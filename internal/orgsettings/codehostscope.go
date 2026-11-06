// SPDX-License-Identifier: Apache-2.0

package orgsettings

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/sinkclass"
)

// The org's code-host scope (ADR 0091 decision 3): the repositories of its code-host connections
// with the visibility the connector last read. The code-host derivation calls a write internal only
// for a repository that is listed here and not public. The connector is the writer (ReplaceCodeHostRepos
// takes one connection's whole list, with the time it read it); admins read the scope. A repository
// whose visibility is unknown, or whose reading is older than the freshness window, is not internal.
//
// The freshness window is DefaultVisibilityMaxAge. ADR 0091 decision 3 (settled in #115): unknown is
// external-high, and a reading older than an hour counts as unknown, because a repository that
// turned public after the reading is a leak the guard would not see.

// DefaultVisibilityMaxAge is how old a repository's visibility reading may be and still count.
const DefaultVisibilityMaxAge = sinkclass.DefaultMaxAge

// MaxCodeHostRepos bounds the repositories one connection reports.
const MaxCodeHostRepos = 50000

// CodeHostRepo is one repository of the scope as a connector reports it.
type CodeHostRepo struct {
	Owner, Name string
	// Visibility is "private", "internal", "public" or "unknown".
	Visibility string
}

// CodeHostScopeRepos converts the stored scope to the guard's form.
func codeHostScopeOf(rows []store.OrgCodeHostRepo) []sinkclass.Repo {
	out := make([]sinkclass.Repo, 0, len(rows))
	for _, r := range rows {
		out = append(out, sinkclass.Repo{Connection: r.ConnectionID.String(), Owner: r.Owner, Name: r.Repo, Visibility: sinkclass.Visibility(r.Visibility), CheckedAt: r.CheckedAt.Time})
	}
	return out
}

// Scope returns the org's code-host scope as the derivation reads it, with the default freshness window.
func (l LeakGuard) Scope() sinkclass.Scope {
	return sinkclass.NewScope(l.CodeHostRepos, DefaultVisibilityMaxAge)
}

// ReplaceCodeHostRepos replaces the repositories of one code-host connection with repos, read at
// checkedAt: a repository the connection no longer reports leaves the scope. Names are lower-cased.
// ErrNotFound when the connection is not the org's.
//
// Replacements of one connection are serialised, and a reading older than the stored one is dropped
// (nil, nothing written): the newest reading wins whatever order the replicas finish in.
// Known gap (#244): a reading is not tied to the credential that took it, and an empty newest
// reading leaves nothing to compare with, so an older reading (one taken under a replaced secret
// included) can still land; it ages out with the freshness window.
//
// Audited as org.code_host_scope.replace with counts, not the list: always for a person's (or any
// non-system) actor, and for the system's periodic refresh only when the repositories or their
// visibilities changed.
func (s *Service) ReplaceCodeHostRepos(ctx context.Context, org pgtype.UUID, actor audit.Actor, connectionID string, repos []CodeHostRepo, checkedAt time.Time) error {
	var cid pgtype.UUID
	if err := cid.Scan(connectionID); err != nil || !cid.Valid {
		return ErrNotFound
	}
	if len(repos) > MaxCodeHostRepos {
		return &InvalidError{Msg: fmt.Sprintf("a connection reports at most %d repositories", MaxCodeHostRepos)}
	}
	if checkedAt.After(time.Now().Add(sinkclass.MaxClockSkew)) {
		return &InvalidError{Msg: "the visibility reading is dated in the future"}
	}
	var owners, names, vis []string
	seen := map[string]bool{}
	counts := map[string]int{}
	for _, r := range repos {
		// Validate before lower-casing: U+212A (Kelvin sign) lower-cases to an ASCII k.
		if !sinkclass.ValidRepoPart(strings.TrimSpace(r.Owner)) || !sinkclass.ValidRepoPart(strings.TrimSpace(r.Name)) {
			return &InvalidError{Msg: fmt.Sprintf("repository %q/%q is not an owner and a name", r.Owner, r.Name)}
		}
		o, n := strings.ToLower(strings.TrimSpace(r.Owner)), strings.ToLower(strings.TrimSpace(r.Name))
		v := sinkclass.Visibility(r.Visibility)
		switch v {
		case sinkclass.VisPrivate, sinkclass.VisInternal, sinkclass.VisPublic, sinkclass.VisUnknown:
		default:
			return &InvalidError{Msg: fmt.Sprintf("repository visibility must be private, internal, public or unknown, not %q", r.Visibility)}
		}
		if len(o) > 255 || len(n) > 255 {
			return &InvalidError{Msg: fmt.Sprintf("repository %q/%q is not an owner and a name", r.Owner, r.Name)}
		}
		if seen[o+"/"+n] {
			return &InvalidError{Msg: fmt.Sprintf("repository %s/%s is listed twice", o, n)}
		}
		seen[o+"/"+n] = true
		owners, names, vis = append(owners, o), append(names, n), append(vis, string(v))
		counts[string(v)]++
	}
	err := s.tx(ctx, func(q *store.Queries) error {
		// The connection must be the org's, also for an empty report, which would write nothing the
		// foreign key could refuse. Its row is locked first (FOR KEY SHARE), before the scope lock:
		// SetConnectionSecret and DeleteConnection lock the row and then the scope, so the reverse
		// order here would deadlock with them.
		if _, err := q.KeyShareConnection(ctx, store.KeyShareConnectionParams{OrgID: org, ID: cid}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		// Every replica and every RPC may read the same connection at once: one replacement at a
		// time, and an older reading that arrives last does not overwrite a newer one.
		if err := q.LockCodeHostConnectionScope(ctx, store.LockCodeHostConnectionScopeParams{OrgID: org, ConnectionID: cid}); err != nil {
			return err
		}
		stored, err := q.ListCodeHostConnectionRepos(ctx, store.ListCodeHostConnectionReposParams{OrgID: org, ConnectionID: cid})
		if err != nil {
			return err
		}
		for _, r := range stored {
			if r.CheckedAt.Time.After(checkedAt) {
				return nil
			}
		}
		changed := len(stored) != len(owners)
		if !changed {
			idx := make(map[string]string, len(owners))
			for i := range owners {
				idx[owners[i]+"/"+names[i]] = vis[i]
			}
			for _, r := range stored {
				if v, ok := idx[r.Owner+"/"+r.Repo]; !ok || v != string(r.Visibility) {
					changed = true
					break
				}
			}
		}
		before, err := q.DeleteCodeHostConnectionRepos(ctx, store.DeleteCodeHostConnectionReposParams{OrgID: org, ConnectionID: cid})
		if err != nil {
			return err
		}
		if len(owners) > 0 {
			if _, err := q.UpsertCodeHostRepos(ctx, store.UpsertCodeHostReposParams{
				OrgID: org, ConnectionID: cid, CheckedAt: pgtype.Timestamptz{Time: checkedAt, Valid: true}, Owners: owners, Repos: names, Visibilities: vis,
			}); err != nil {
				return err
			}
		}
		if !changed && actor.Kind == audit.KindSystem {
			// The periodic refresh of an unchanged list: its freshness shows in ScopeSummary and the log.
			return nil
		}
		return audit.RecordErr(ctx, q, audit.Event{
			Org: org, Actor: actor, Action: "org.code_host_scope.replace", TargetKind: "connection", TargetID: connectionID,
			Before: map[string]any{"repositories": before},
			After: map[string]any{
				"repositories": len(owners), "private": counts["private"], "internal": counts["internal"],
				"public": counts["public"], "unknown": counts["unknown"],
			},
		})
	})
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23503" {
		return ErrNotFound
	}
	return err
}

// ScopeSummary counts the scope by visibility, for the console. The repository list itself is not
// served: an org can have thousands.
type ScopeSummary struct {
	Repositories, Private, Internal, Public, Unknown int
	// Stale counts the repositories whose reading is older than MaxAge or dated in the future: the derivation treats them as unknown.
	Stale int
	// OldestCheckedAt is the oldest reading, nil for an empty scope.
	OldestCheckedAt *time.Time
	MaxAge          time.Duration
}

// ScopeSummary summarises the org's code-host scope at now.
func (l LeakGuard) ScopeSummary(now time.Time) ScopeSummary {
	out := ScopeSummary{MaxAge: DefaultVisibilityMaxAge}
	for _, r := range l.CodeHostRepos {
		out.Repositories++
		switch r.Visibility {
		case sinkclass.VisPrivate:
			out.Private++
		case sinkclass.VisInternal:
			out.Internal++
		case sinkclass.VisPublic:
			out.Public++
		default:
			out.Unknown++
		}
		if now.Sub(r.CheckedAt) > out.MaxAge {
			out.Stale++
		}
		if out.OldestCheckedAt == nil || r.CheckedAt.Before(*out.OldestCheckedAt) {
			t := r.CheckedAt
			out.OldestCheckedAt = &t
		}
	}
	return out
}
