// SPDX-License-Identifier: Apache-2.0

// Package restorestatus builds the "restore status" report (ADR 0110 decision 7, issue #516): what a
// restored copy would do outward, and what a release will start. It reads only the database and never
// calls a target, so it is safe on a held install and on one whose outward targets are the source's.
//
// The report is what the database answers: the install binding and whether a release is pending, the
// credentials the binding refuses, every outward target with its switch, the importer's removal list,
// and the work a release picks up (due schedule fires, DBOS queue backlog, runs, the org limits that apply to
// them). The importer plan needs the Kubernetes Secrets, so the caller passes it in (Options.PlanRemovals).
package restorestatus

import (
	"context"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/installbinding"
	"github.com/stateracloud/koinon/internal/orgsettings"
	"github.com/stateracloud/koinon/internal/triggers/schedule"
)

// Options are what the caller knows that the database does not.
type Options struct {
	// AcceptImporterDeletions is restore.acceptImporterDeletions as this process has it.
	AcceptImporterDeletions bool
	// PlanRemovals returns what the importer would remove now (declarative.PlanRemovals); nil leaves
	// the importer section without a plan.
	PlanRemovals func(ctx context.Context) ([]installbinding.Removal, error)
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// BestEffort makes a failure of a section that gates nothing (the refused credentials, the outward
	// targets, the release preview) a line in Report.Warnings instead of an error. The importer section
	// and the binding row stay errors: the release gate decides on them.
	BestEffort bool
}

// Report is the whole status. It is the JSON the command prints with --json.
type Report struct {
	Binding    Binding     `json:"binding"`
	Quarantine Quarantine  `json:"quarantine"`
	Targets    []Target    `json:"outwardTargets"`
	Importer   Importer    `json:"importer"`
	Release    ReleasePlan `json:"release"`
	// Warnings are the sections a BestEffort build could not read.
	Warnings []string `json:"warnings,omitempty"`
}

// Importer is the declarative importer's part of the release (ADR 0110 decision 1).
type Importer struct {
	// Planned is what the importer would remove now; nil when the caller gave no plan.
	Planned []installbinding.Removal `json:"plannedRemovals,omitempty"`
	// Waiting and Accepted are the rows of restore_accepted_removals.
	Waiting  []installbinding.Removal `json:"waitingRemovals,omitempty"`
	Accepted []installbinding.Removal `json:"acceptedRemovals,omitempty"`
	// AcceptImporterDeletions is restore.acceptImporterDeletions as this process has it.
	AcceptImporterDeletions bool `json:"acceptImporterDeletions"`
	// NeedsAccept is true when a release now would leave removals waiting, because the value is false.
	NeedsAccept bool `json:"needsAccept"`
}

// Workflows counts the DBOS workflows of an org that a release lets run.
type Workflows struct {
	Enqueued int `json:"enqueued"`
	Pending  int `json:"pending"`
}

// Limits are an org's effective run limits (ADR 0066), which apply to the backlog after the release.
type Limits struct {
	MaxConcurrentRuns    int `json:"maxConcurrentRuns"`
	TriggerConcurrency   int `json:"triggerConcurrency"`
	TriggerRatePerMinute int `json:"triggerRatePerMinute"`
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
	// Pending is true while a release is pending (ADR 0110 decision 1): the hold left held_since set,
	// or this is a copy that has not run its release step.
	Pending bool `json:"pending"`
	// QueuedRuns are runs in state queued, starting, running or parked, by org ID.
	QueuedRuns map[string]int `json:"runsByOrg"`
	// ParkedRuns are the parked ones among them, by org ID.
	ParkedRuns map[string]int `json:"parkedRunsByOrg"`
	// DueTriggers are the schedule fires the first tick after the release enqueues (one per trigger, look-back 1 h).
	DueTriggers []schedule.Due `json:"dueScheduleTriggers"`
	// DBOS are the workflows ENQUEUED and PENDING per org ID; empty when the durable runtime is off.
	DBOS map[string]Workflows `json:"dbosWorkflowsByOrg"`
	// OrgLimits are the run limits that apply per org ID.
	OrgLimits map[string]Limits `json:"orgLimits"`
}

// Build reads the report. ownerID is execution.OwnerID of this install's mark; "" when the caller has
// no mark, in which case the state is unknown and no credential is counted as refused.
func Build(ctx context.Context, pool *pgxpool.Pool, ownerID string, optional ...Options) (Report, error) {
	var r Report
	var opt Options
	if len(optional) > 0 {
		opt = optional[0]
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
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
	r.Release.Pending = installbinding.Pending(ownerID, row, st != installbinding.StateUnknown)
	r.Binding.RecordedHost.PublicURL = row.Targets.PublicURL
	r.Binding.RecordedHost.OIDCRedirectURL = row.Targets.OIDCRedirectURL
	r.Binding.RecordedHost.CheckpointEndpoint = row.Targets.CheckpointEndpoint
	r.Binding.RecordedHost.CheckpointBucket = row.Targets.CheckpointBucket

	soft := func(section string, err error) error {
		if err == nil || !opt.BestEffort {
			return err
		}
		r.Warnings = append(r.Warnings, fmt.Sprintf("%s: %v", section, err))
		return nil
	}
	if err := soft("refused credentials", quarantineSection(ctx, pool, &r, ownerID, st)); err != nil {
		return r, err
	}
	if err := soft("outward targets", targetsSection(ctx, pool, &r)); err != nil {
		return r, err
	}

	if err := importerSection(ctx, pool, &r, opt); err != nil {
		return r, err
	}
	if err := soft("release preview", releaseSection(ctx, pool, &r, opt)); err != nil {
		return r, err
	}
	return r, nil
}

func quarantineSection(ctx context.Context, pool *pgxpool.Pool, r *Report, ownerID string, st installbinding.State) error {
	if ownerID != "" && st != installbinding.StateSame {
		// Not "same": a credential with no mark or another mark is refused (decision 3).
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM api_tokens
			WHERE revoked_at IS NULL AND expires_at > now() AND install_mark IS DISTINCT FROM $1`, ownerID).Scan(&r.Quarantine.Tokens); err != nil {
			return fmt.Errorf("count refused tokens: %w", err)
		}
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM sessions
			WHERE revoked_at IS NULL AND expires_at > now() AND idle_expires_at > now() AND install_mark IS DISTINCT FROM $1`, ownerID).Scan(&r.Quarantine.Sessions); err != nil {
			return fmt.Errorf("count refused sessions: %w", err)
		}
	}

	return nil
}

func targetsSection(ctx context.Context, pool *pgxpool.Pool, r *Report) error {
	rows, err := pool.Query(ctx, `
		SELECT 'mcp-server', org_id::text, name, admin_disabled_at IS NOT NULL FROM mcp_servers
		UNION ALL SELECT 'model-provider', org_id::text, name, admin_disabled_at IS NOT NULL FROM providers
		UNION ALL SELECT 'connection', org_id::text, display_name, admin_disabled_at IS NOT NULL FROM connections
		UNION ALL SELECT 'trace-destination', org_id::text, name, admin_disabled_at IS NOT NULL FROM trace_destinations
		UNION ALL SELECT 'platform-trace-destination', '', 'platform', admin_disabled_at IS NOT NULL FROM platform_trace_destination
		ORDER BY 1, 2, 3`)
	if err != nil {
		return fmt.Errorf("list outward targets: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var t Target
		if err := rows.Scan(&t.Kind, &t.Org, &t.Name, &t.Disabled); err != nil {
			return err
		}
		r.Targets = append(r.Targets, t)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	return nil
}

func importerSection(ctx context.Context, pool *pgxpool.Pool, r *Report, opt Options) error {
	r.Importer.AcceptImporterDeletions = opt.AcceptImporterDeletions
	var err error
	if r.Importer.Waiting, r.Importer.Accepted, err = installbinding.ListRemovals(ctx, pool); err != nil {
		return fmt.Errorf("list the restore's removals: %w", err)
	}
	if opt.PlanRemovals != nil {
		if r.Importer.Planned, err = opt.PlanRemovals(ctx); err != nil {
			return fmt.Errorf("plan the importer's removals: %w", err)
		}
	}
	r.Importer.NeedsAccept = len(r.Blocking()) > 0
	return nil
}

// Blocking returns the removals that make a release refuse (the restore-release-check Job fails on
// them): without restore.acceptImporterDeletions, every planned removal while a release is pending,
// and later every planned removal that still waits (ADR 0110 decision 1).
func (r Report) Blocking() []installbinding.Removal {
	if r.Importer.AcceptImporterDeletions {
		return nil
	}
	if r.Release.Pending {
		return r.Importer.Planned
	}
	planned := map[installbinding.Removal]bool{}
	for _, x := range r.Importer.Planned {
		planned[x] = true
	}
	var out []installbinding.Removal
	for _, x := range r.Importer.Waiting {
		if planned[x] {
			out = append(out, x)
		}
	}
	return out
}

func releaseSection(ctx context.Context, pool *pgxpool.Pool, r *Report, opt Options) error {
	r.Release.QueuedRuns = map[string]int{}
	r.Release.ParkedRuns = map[string]int{}
	runs, err := pool.Query(ctx, `SELECT org_id::text, count(*), count(*) FILTER (WHERE state = 'parked') FROM runs
		WHERE state IN ('queued','starting','running','parked') GROUP BY 1`)
	if err != nil {
		return fmt.Errorf("count runs: %w", err)
	}
	for runs.Next() {
		var org string
		var n, parked int
		if err := runs.Scan(&org, &n, &parked); err != nil {
			runs.Close()
			return err
		}
		r.Release.QueuedRuns[org] = n
		if parked > 0 {
			r.Release.ParkedRuns[org] = parked
		}
	}
	runs.Close()
	if err := runs.Err(); err != nil {
		return err
	}

	if r.Release.DueTriggers, err = schedule.PreviewCatchUp(ctx, pool, opt.Now()); err != nil {
		return fmt.Errorf("preview the schedule catch-up: %w", err)
	}
	if r.Release.DueTriggers == nil {
		r.Release.DueTriggers = []schedule.Due{}
	}

	r.Release.DBOS = map[string]Workflows{}
	var have bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('dbos.workflow_status') IS NOT NULL`).Scan(&have); err != nil {
		return err
	}
	if have {
		wf, err := pool.Query(ctx, `SELECT COALESCE(queue_partition_key, ''), count(*) FILTER (WHERE status = 'ENQUEUED'), count(*) FILTER (WHERE status = 'PENDING')
			FROM dbos.workflow_status WHERE status IN ('ENQUEUED', 'PENDING') GROUP BY 1`)
		if err != nil {
			return fmt.Errorf("count DBOS workflows: %w", err)
		}
		for wf.Next() {
			var org string
			var w Workflows
			if err := wf.Scan(&org, &w.Enqueued, &w.Pending); err != nil {
				wf.Close()
				return err
			}
			r.Release.DBOS[org] = w
		}
		wf.Close()
		if err := wf.Err(); err != nil {
			return err
		}
	}

	r.Release.OrgLimits = map[string]Limits{}
	ids, err := store.New(pool).ListOrgIDs(ctx)
	if err != nil {
		return err
	}
	svc := orgsettings.NewService(pool)
	for _, id := range ids {
		l, err := svc.RunLimits(ctx, pgtype.UUID(id))
		if err != nil {
			return fmt.Errorf("read the run limits: %w", err)
		}
		r.Release.OrgLimits[id.String()] = Limits{MaxConcurrentRuns: l.MaxConcurrentRuns, TriggerConcurrency: l.TriggerConcurrency, TriggerRatePerMinute: l.TriggerRatePerMinute}
	}
	return nil
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
	for _, w := range r.Warnings {
		p("warning:    %s\n", w)
	}
	p("refused:     %d live API tokens, %d live sessions\n", r.Quarantine.Tokens, r.Quarantine.Sessions)
	p("outward targets (%d):\n", len(r.Targets))
	for _, t := range r.Targets {
		state := "ENABLED"
		if t.Disabled {
			state = "disabled"
		}
		p("  %-8s %-26s org=%-36s %s\n", state, t.Kind, t.Org, t.Name)
	}
	p("release:     %s\n", map[bool]string{true: "PENDING (the next unheld start of koinon-api runs it)", false: "not pending"}[r.Release.Pending])
	p("importer:    %d removals planned, %d waiting, %d accepted; restore.acceptImporterDeletions=%t\n",
		len(r.Importer.Planned), len(r.Importer.Waiting), len(r.Importer.Accepted), r.Importer.AcceptImporterDeletions)
	for _, g := range []struct {
		name string
		list []installbinding.Removal
	}{{"planned", r.Importer.Planned}, {"waiting", r.Importer.Waiting}, {"accepted", r.Importer.Accepted}} {
		for _, x := range g.list {
			p("  %-8s %-15s org=%s %s\n", g.name, x.Kind, x.Org, x.Secret)
		}
	}
	if r.Importer.NeedsAccept {
		p("  the release needs restore.acceptImporterDeletions=true (or the Secrets back): the removals above wait until then\n")
	}
	n, parked := 0, 0
	for _, c := range r.Release.QueuedRuns {
		n += c
	}
	for _, c := range r.Release.ParkedRuns {
		parked += c
	}
	p("a release picks up %d runs (queued, starting, running or parked; %d parked) in %d orgs\n", n, parked, len(r.Release.QueuedRuns))
	p("the first schedule tick fires %d trigger(s) once (catch-up, look back %s)\n", len(r.Release.DueTriggers), schedule.DefaultCatchUp)
	orgs := make([]string, 0, len(r.Release.DBOS))
	for o := range r.Release.DBOS {
		orgs = append(orgs, o)
	}
	sort.Strings(orgs)
	for _, o := range orgs {
		w := r.Release.DBOS[o]
		p("  dbos org=%s enqueued=%d pending=%d\n", o, w.Enqueued, w.Pending)
	}
	orgs = orgs[:0]
	for o := range r.Release.OrgLimits {
		orgs = append(orgs, o)
	}
	sort.Strings(orgs)
	for _, o := range orgs {
		l := r.Release.OrgLimits[o]
		p("  limits org=%s max concurrent runs=%d, trigger concurrency=%d, trigger rate/min=%d\n", o, l.MaxConcurrentRuns, l.TriggerConcurrency, l.TriggerRatePerMinute)
	}
}
