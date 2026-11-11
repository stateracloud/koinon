// SPDX-License-Identifier: Apache-2.0

// Package restorestatus builds the "restore status" report (ADR 0110 decision 7, issue #516): what a
// restored copy would do outward, and what a release will start. It reads only the database and never
// calls a target, so it is safe on a held install and on one whose outward targets are the source's.
//
// This is the part of the report that the database answers: the install binding, the credentials
// the binding refuses, every outward target with its switch, and the work a release picks up (queued
// runs, enabled schedules). The importer plan needs the Kubernetes Secrets and is exported by the
// held importer as the Events and koinon_declarative_held_changes (internal/declarative). The due
// schedule fires and the DBOS backlog are not in this report yet.
package restorestatus

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/installbinding"
)

// Report is the whole status. It is the JSON the command prints with --json.
type Report struct {
	Binding    Binding     `json:"binding"`
	Quarantine Quarantine  `json:"quarantine"`
	Targets    []Target    `json:"outwardTargets"`
	Release    ReleasePlan `json:"release"`
}

// Binding is the install_binding row and this install's state against it.
type Binding struct {
	// State is same, copy or unknown; "unknown" also when no install mark was given.
	State string `json:"state"`
	// Reason says in a line why the state is what it is.
	Reason       string     `json:"reason"`
	HeldSince    *time.Time `json:"heldSince,omitempty"`
	ReleasedAt   *time.Time `json:"releasedAt,omitempty"`
	RecordedHost struct {
		PublicURL          string `json:"publicUrl,omitempty"`
		OIDCRedirectURL    string `json:"oidcRedirectUrl,omitempty"`
		CheckpointEndpoint string `json:"checkpointEndpoint,omitempty"`
		CheckpointBucket   string `json:"checkpointBucket,omitempty"`
	} `json:"recordedTargets"`
}

// Quarantine counts the live credentials this install refuses because another install issued them
// (ADR 0110 decision 3). A credential is live when it is neither revoked nor expired.
type Quarantine struct {
	Tokens   int `json:"tokens"`
	Sessions int `json:"sessions"`
}

// Target is one outward target and whether an admin switched it off.
type Target struct {
	Kind     string `json:"kind"`
	Org      string `json:"org,omitempty"`
	Name     string `json:"name"`
	Disabled bool   `json:"adminDisabled"`
}

// ReleasePlan is the work a release will start.
type ReleasePlan struct {
	// QueuedRuns are runs in state queued, starting, running or parked, by org ID.
	QueuedRuns map[string]int `json:"runsByOrg"`
}

// Build reads the report. ownerID is execution.OwnerID of this install's mark; "" when the caller has
// no mark, in which case the state is unknown and no credential is counted as refused.
func Build(ctx context.Context, pool *pgxpool.Pool, ownerID string) (Report, error) {
	var r Report
	b := installbinding.New(pool, ownerID, false, nil)
	row, st, err := b.Read(ctx)
	if err != nil {
		return r, err
	}
	r.Binding.State, r.Binding.Reason = string(st), reason(st, ownerID != "")
	if ownerID == "" {
		r.Binding.State = string(installbinding.StateUnknown)
	}
	r.Binding.HeldSince, r.Binding.ReleasedAt = row.HeldSince, row.ReleasedAt
	r.Binding.RecordedHost.PublicURL = row.Targets.PublicURL
	r.Binding.RecordedHost.OIDCRedirectURL = row.Targets.OIDCRedirectURL
	r.Binding.RecordedHost.CheckpointEndpoint = row.Targets.CheckpointEndpoint
	r.Binding.RecordedHost.CheckpointBucket = row.Targets.CheckpointBucket

	if ownerID != "" && st != installbinding.StateSame {
		// Not "same": a credential with no mark or another mark is refused (decision 3).
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM api_tokens
			WHERE revoked_at IS NULL AND expires_at > now() AND install_mark IS DISTINCT FROM $1`, ownerID).Scan(&r.Quarantine.Tokens); err != nil {
			return r, fmt.Errorf("count refused tokens: %w", err)
		}
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM sessions
			WHERE revoked_at IS NULL AND expires_at > now() AND idle_expires_at > now() AND install_mark IS DISTINCT FROM $1`, ownerID).Scan(&r.Quarantine.Sessions); err != nil {
			return r, fmt.Errorf("count refused sessions: %w", err)
		}
	}

	rows, err := pool.Query(ctx, `
		SELECT 'mcp-server', org_id::text, name, admin_disabled_at IS NOT NULL FROM mcp_servers
		UNION ALL SELECT 'model-provider', org_id::text, name, admin_disabled_at IS NOT NULL FROM providers
		UNION ALL SELECT 'connection', org_id::text, display_name, admin_disabled_at IS NOT NULL FROM connections
		UNION ALL SELECT 'trace-destination', org_id::text, name, admin_disabled_at IS NOT NULL FROM trace_destinations
		UNION ALL SELECT 'platform-trace-destination', '', 'platform', admin_disabled_at IS NOT NULL FROM platform_trace_destination
		ORDER BY 1, 2, 3`)
	if err != nil {
		return r, fmt.Errorf("list outward targets: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var t Target
		if err := rows.Scan(&t.Kind, &t.Org, &t.Name, &t.Disabled); err != nil {
			return r, err
		}
		r.Targets = append(r.Targets, t)
	}
	if err := rows.Err(); err != nil {
		return r, err
	}

	r.Release.QueuedRuns = map[string]int{}
	runs, err := pool.Query(ctx, `SELECT org_id::text, count(*) FROM runs
		WHERE state IN ('queued','starting','running','parked') GROUP BY 1`)
	if err != nil {
		return r, fmt.Errorf("count runs: %w", err)
	}
	defer runs.Close()
	for runs.Next() {
		var org string
		var n int
		if err := runs.Scan(&org, &n); err != nil {
			return r, err
		}
		r.Release.QueuedRuns[org] = n
	}
	return r, runs.Err()
}

func reason(st installbinding.State, haveMark bool) string {
	switch {
	case !haveMark:
		return "no install mark given (set KOINON_INSTALL_MARK): the state cannot be told"
	case st == installbinding.StateSame:
		return "the database belongs to this install; tokens and sessions are accepted"
	case st == installbinding.StateCopy:
		return "the database was bound to another install or to none: tokens and sessions of other installs are refused until adopt-restore"
	}
	return "the database has no install_binding row yet: a held start writes an unbound row, an unheld start claims it"
}

// Print writes the report for a person.
func Print(w io.Writer, r Report) {
	p := func(f string, a ...any) { _, _ = fmt.Fprintf(w, f, a...) }
	p("binding:    %s (%s)\n", r.Binding.State, r.Binding.Reason)
	if r.Binding.HeldSince != nil {
		p("held since: %s\n", r.Binding.HeldSince.UTC().Format(time.RFC3339))
	}
	if r.Binding.ReleasedAt != nil {
		p("released:   %s\n", r.Binding.ReleasedAt.UTC().Format(time.RFC3339))
	}
	rt := r.Binding.RecordedHost
	p("recorded outward targets of the binding install: public url %q, oidc redirect %q, checkpoints %q bucket %q\n",
		rt.PublicURL, rt.OIDCRedirectURL, rt.CheckpointEndpoint, rt.CheckpointBucket)
	p("refused:     %d live API tokens, %d live sessions\n", r.Quarantine.Tokens, r.Quarantine.Sessions)
	p("outward targets (%d):\n", len(r.Targets))
	for _, t := range r.Targets {
		state := "ENABLED"
		if t.Disabled {
			state = "disabled"
		}
		p("  %-8s %-26s org=%-36s %s\n", state, t.Kind, t.Org, t.Name)
	}
	n := 0
	for _, c := range r.Release.QueuedRuns {
		n += c
	}
	p("a release picks up %d runs (queued, starting, running or parked) in %d orgs\n", n, len(r.Release.QueuedRuns))
}
