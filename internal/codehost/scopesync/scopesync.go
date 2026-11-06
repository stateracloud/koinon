// SPDX-License-Identifier: Apache-2.0

// Package scopesync keeps the org's code-host scope (ADR 0091 decision 3) current: it lists the
// repositories of each code-host connection with their visibility and hands the whole list to
// orgsettings.ReplaceCodeHostRepos. Until a connection has been read, or once its reading is older
// than the freshness window, every code-host write on it is external-high; so a failed read is
// never papered over, the old rows simply age out.
package scopesync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

// Actor is the audit actor of a refresh.
var Actor = audit.Actor{Kind: audit.KindSystem, ID: "code-host-scope"}

// Syncer reads connections' repositories into the code-host scope.
type Syncer struct {
	q     *store.Queries
	scope *orgsettings.Service
	hosts *codehost.Registry
	keys  connections.Opener
	log   *slog.Logger
	now   func() time.Time
}

// New returns a Syncer. keys opens the connections' secret fields; nil (Koinon without
// key-encryption keys) makes every sync report connections.ErrNoKeyring.
func New(pool *pgxpool.Pool, hosts *codehost.Registry, keys connections.Opener, log *slog.Logger) *Syncer {
	if log == nil {
		log = slog.Default()
	}
	return &Syncer{q: store.New(pool), scope: orgsettings.NewService(pool), hosts: hosts, keys: keys, log: log, now: time.Now}
}

// Sync reads one connection's repositories and replaces its part of the scope. The reading is
// dated at the start of the read, so a slow listing does not look fresher than it is. A
// connection that is not a code host is skipped without error. On error the stored scope is left
// as it was.
func (s *Syncer) Sync(ctx context.Context, row store.Connection) error {
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
	for page := 1; ; page++ {
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
		if len(repos) >= orgsettings.MaxCodeHostRepos {
			// The rest stay out of the scope, which is the safe side.
			s.log.Warn("code-host scope truncated", "connection", row.ID.String(), "max", orgsettings.MaxCodeHostRepos)
			repos = repos[:orgsettings.MaxCodeHostRepos]
			break
		}
	}
	if skipped > 0 {
		s.log.Warn("code-host repositories with unparsable names left out of the scope", "connection", row.ID.String(), "count", skipped)
	}
	return s.scope.ReplaceCodeHostRepos(ctx, row.OrgID, Actor, row.ID.String(), dedupe(repos), readAt)
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
	for _, id := range s.hosts.IDs() {
		rows, err := s.q.ListConnectionsByConnector(ctx, store.ListConnectionsByConnectorParams{OrgID: org, Connector: id})
		if err != nil {
			s.log.Warn("code-host scope: listing connections failed", "org", org.String(), "connector", id, "err", err)
			continue
		}
		for _, row := range rows {
			if row.RemovedAt.Valid || row.CredentialLostAt.Valid {
				continue
			}
			if err := s.Sync(ctx, row); err != nil && !errors.Is(err, context.Canceled) {
				s.log.Warn("code-host scope: refresh failed; the stored reading ages out", "org", org.String(), "connection", row.ID.String(), "connector", id, "err", err)
			}
		}
	}
}

// RunOnce syncs every org's code-host connections.
func (s *Syncer) RunOnce(ctx context.Context) error {
	orgs, err := s.q.ListOrgsWithCodeHostConnections(ctx, s.hosts.IDs())
	if err != nil {
		return err
	}
	for _, org := range orgs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.SyncOrg(ctx, org)
	}
	return nil
}

// Run refreshes the scope every interval (DefaultInterval when <= 0) until ctx ends, and once
// at the start. Every replica may run it: a replace is a whole-list write.
func (s *Syncer) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := s.RunOnce(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("code-host scope: refresh round failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
