// SPDX-License-Identifier: Apache-2.0

// Package scopesync keeps the org's code-host scope (ADR 0091 decision 3) current: it lists the
// repositories of each code-host connection with their visibility and hands the whole list to
// orgsettings.ReplaceCodeHostRepos. Until a connection has been read, or once its reading is older
// than the freshness window, every code-host write on it is external-high; so a failed read is
// never papered over, the old rows simply age out.
//
// Every koinon-api replica runs the periodic refresh, but one round runs at a time across them:
// the replica that holds a Postgres session advisory lock (see lead) is the round's leader, and the
// others skip that tick. A round also skips a connection read less than MinAge ago, so the
// replicas' rounds, which drift apart with the jitter, do not read the same connection twice in
// one interval. The readings themselves are the record of when a connection was last read.
package scopesync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/codehost"
	"github.com/stateracloud/koinon/internal/connections"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/orgsettings"
	"github.com/stateracloud/koinon/internal/sinkclass"
)

// DefaultInterval is how often Run re-reads every connection. It is a third of the freshness
// window (orgsettings.DefaultVisibilityMaxAge), so two failed rounds in a row still leave the
// last reading fresh, and a repository that turned public is seen well inside the window.
const DefaultInterval = orgsettings.DefaultVisibilityMaxAge / 3

// DefaultTimeout bounds the reading of one connection, so one slow code host does not hold up
// the other connections and orgs of the round.
const DefaultTimeout = 2 * time.Minute

// MinAge is how recent a connection's reading may be for a round to skip it: half the interval,
// so a reading another replica's round took a moment ago is not repeated, and every connection is
// still read at least once per interval.
const MinAge = DefaultInterval / 2

// roundLock names the advisory lock the round's leader holds.
const roundLock = "koinon.code-host-scope.round"

// Actor is the audit actor of a refresh.
var Actor = audit.System("koinon-api:code-host-scope")

// Syncer reads connections' repositories into the code-host scope.
type Syncer struct {
	pool     *pgxpool.Pool
	q        *store.Queries
	scope    *orgsettings.Service
	hosts    *codehost.Registry
	keys     connections.Opener
	log      *slog.Logger
	now      func() time.Time
	timeout  time.Duration
	minAge   time.Duration
	maxRepos int
}

// New returns a Syncer. keys opens the connections' secret fields; nil (Koinon without
// key-encryption keys) makes every sync report connections.ErrNoKeyring.
func New(pool *pgxpool.Pool, hosts *codehost.Registry, keys connections.Opener, log *slog.Logger) *Syncer {
	if log == nil {
		log = slog.Default()
	}
	return &Syncer{
		pool: pool, q: store.New(pool), scope: orgsettings.NewService(pool), hosts: hosts, keys: keys, log: log, now: time.Now,
		timeout: DefaultTimeout, minAge: MinAge, maxRepos: orgsettings.MaxCodeHostRepos,
	}
}

// Sync reads one connection's repositories and replaces its part of the scope, audited as actor
// (orgsettings.ReplaceCodeHostRepos: a person's sync is always audited, the refresh's only on a
// change). The reading is dated at the start of the read, so a slow listing does not look
// fresher than it is. A connection that is not a code host is skipped without error. On error the
// stored scope is left as it was.
func (s *Syncer) Sync(ctx context.Context, row store.Connection, actor audit.Actor) error {
	p, ok := s.hosts.Get(row.Connector)
	if !ok {
		return nil
	}
	if s.keys == nil {
		return connections.ErrNoKeyring
	}
	readAt := s.now()
	conn, err := connections.Load(ctx, s.keys, row)
	if err == nil {
		err = p.Validate(conn)
	}
	if err != nil {
		return fmt.Errorf("load connection: %w", err)
	}
	// Metadata read only, the least any credential has; no repositories named, so the whole grant.
	acc, err := p.Open(ctx, conn, p.MetadataScope(nil))
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	var repos []orgsettings.CodeHostRepo
	skipped := 0
	for page := 1; len(repos) < s.maxRepos; page++ {
		got, err := acc.Repositories(ctx, page, codehost.MaxPerPage)
		if err != nil {
			return fmt.Errorf("list repositories: %w", err)
		}
		for _, r := range got.Repositories {
			parsed, err := p.ParseRepo(r.FullName)
			if err != nil {
				skipped++ // a name Koinon cannot address is not in the scope: external-high
				continue
			}
			repos = append(repos, orgsettings.CodeHostRepo{Owner: parsed.Owner, Name: parsed.Name, Visibility: visibility(r)})
		}
		if len(got.Repositories) == 0 || int64(page)*codehost.MaxPerPage >= got.Total {
			break
		}
	}
	if skipped > 0 {
		s.log.Warn("code-host repositories with unparsable names left out of the scope", "connection", row.ID.String(), "count", skipped)
	}
	repos = dedupe(repos)
	if len(repos) > s.maxRepos {
		// Whatever page crossed the bound: the rest stay out of the scope, which is the safe side,
		// and the connection keeps syncing instead of failing on a list it cannot store.
		s.log.Warn("code-host scope truncated", "connection", row.ID.String(), "max", s.maxRepos)
		repos = repos[:s.maxRepos]
	}
	return s.scope.ReplaceCodeHostRepos(ctx, row.OrgID, actor, row.ID.String(), repos, readAt)
}

// visibility is what the connector reported; anything it did not say is unknown (external-high).
func visibility(r codehost.Repository) string {
	switch r.Visibility {
	case codehost.VisibilityPrivate, codehost.VisibilityPublic:
		return r.Visibility
	}
	return string(sinkclass.VisUnknown)
}

// dedupe drops a repository listed twice (a rename mid-listing), keeping the least private
// reading, because ReplaceCodeHostRepos refuses duplicates and the doubtful one must win.
func dedupe(repos []orgsettings.CodeHostRepo) []orgsettings.CodeHostRepo {
	seen := map[[2]string]int{}
	out := repos[:0:0]
	for _, r := range repos {
		k := [2]string{strings.ToLower(r.Owner), strings.ToLower(r.Name)}
		if i, ok := seen[k]; ok {
			if r.Visibility != codehost.VisibilityPrivate {
				out[i].Visibility = r.Visibility
			}
			continue
		}
		seen[k] = len(out)
		out = append(out, r)
	}
	return out
}

// SyncOrg syncs every code-host connection of the org, logging each failure.
func (s *Syncer) SyncOrg(ctx context.Context, org pgtype.UUID) {
	s.syncOrg(ctx, org, 0)
}

// syncOrg syncs the org's live code-host connections whose reading is at least minAge old, each
// under its own timeout.
func (s *Syncer) syncOrg(ctx context.Context, org pgtype.UUID, minAge time.Duration) {
	for _, id := range s.hosts.IDs() {
		rows, err := s.q.ListConnectionsByConnector(ctx, store.ListConnectionsByConnectorParams{OrgID: org, Connector: id})
		if err != nil {
			s.log.Warn("code-host scope: listing connections failed", "org", org.String(), "connector", id, "err", err)
			continue
		}
		for _, row := range rows {
			if ctx.Err() != nil {
				return
			}
			if row.RemovedAt.Valid || row.CredentialLostAt.Valid {
				continue
			}
			if minAge > 0 && s.readWithin(ctx, row, minAge) {
				continue
			}
			cctx, cancel := context.WithTimeout(ctx, s.timeout)
			err := s.Sync(cctx, row, Actor)
			cancel()
			if err != nil && ctx.Err() == nil {
				s.log.Warn("code-host scope: refresh failed; the stored reading ages out", "org", org.String(), "connection", row.ID.String(), "connector", id, "err", err)
			}
		}
	}
}

// readWithin reports whether the connection's stored reading is younger than d. A failed lookup
// reads it again.
func (s *Syncer) readWithin(ctx context.Context, row store.Connection, d time.Duration) bool {
	at, err := s.q.CodeHostConnectionReadAt(ctx, store.CodeHostConnectionReadAtParams{OrgID: row.OrgID, ConnectionID: row.ID})
	if err != nil || !at.Valid || at.InfinityModifier != pgtype.Finite {
		return false
	}
	return s.now().Sub(at.Time) < d
}

// RunOnce runs one refresh round over every org's code-host connections, when no other replica
// is running one; it then returns nil without reading anything.
func (s *Syncer) RunOnce(ctx context.Context) error {
	release, ok, err := s.lead(ctx)
	if err != nil {
		return fmt.Errorf("code-host scope: round lock: %w", err)
	}
	if !ok {
		s.log.Debug("code-host scope: another replica runs this round")
		return nil
	}
	defer release()
	orgs, err := s.q.ListOrgsWithCodeHostConnections(ctx, s.hosts.IDs())
	if err != nil {
		return err
	}
	for _, org := range orgs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.syncOrg(ctx, org, s.minAge)
	}
	return nil
}

// lead takes the round's lock: a session-level pg_try_advisory_lock on a connection held for the
// round. Postgres drops it the moment that session ends, so a leader that crashes or is killed
// mid-round frees the next round at once, with no lease to expire, no clock to trust and no table
// (a migration) to keep; the readings record what was done (MinAge). release unlocks and returns
// the connection; a connection that could not unlock is closed, never pooled with the lock held.
func (s *Syncer) lead(ctx context.Context) (release func(), ok bool, err error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1, 0))`, roundLock).Scan(&ok); err != nil || !ok {
		conn.Release()
		return nil, false, err
	}
	return func() {
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		var unlocked bool
		if err := conn.QueryRow(uctx, `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, roundLock).Scan(&unlocked); err != nil || !unlocked {
			_ = conn.Hijack().Close(uctx)
			return
		}
		conn.Release()
	}, true, nil
}

// Run refreshes the scope about every interval (DefaultInterval when <= 0, give or take a tenth,
// so the replicas' ticks spread out) until ctx ends, and once at the start.
func (s *Syncer) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultInterval
	}
	t := time.NewTimer(jitter(interval))
	defer t.Stop()
	for {
		if err := s.RunOnce(ctx); err != nil && ctx.Err() == nil && !errors.Is(err, context.Canceled) {
			s.log.Warn("code-host scope: refresh round failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			t.Reset(jitter(interval))
		}
	}
}

// jitter is d give or take a tenth.
func jitter(d time.Duration) time.Duration {
	spread := d / 5
	if spread <= 0 {
		return d
	}
	return d - spread/2 + rand.N(spread) //nolint:gosec // jitter, not security
}
