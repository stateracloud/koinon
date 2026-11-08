// SPDX-License-Identifier: Apache-2.0

// Package checkpointscli is "koinon-api checkpoints", the home checkpoint tools of the restore
// runbook (ADR 0085 decision 17).
package checkpointscli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
)

// RequestedAction is the audit action of checkpoints resync, one event per agent, in the agent's
// org. koinon-agentd audits the outcome (home.checkpoints_resynced or
// home.checkpoints_resync_failed).
const RequestedAction = "home.checkpoints_resync_requested"

// actor names the command in the audit log (actor kind cli).
var actor = audit.CLI("checkpoints resync")

// DefaultPoll is how often Resync reads the requests while it waits.
const DefaultPoll = 2 * time.Second

// Tool runs the checkpoints commands against the database.
type Tool struct {
	Pool *pgxpool.Pool
	Out  io.Writer
	// Poll is how often a waiting Resync reads its requests (default DefaultPoll).
	Poll time.Duration
}

// target is an agent with a home repository, and the request made for it.
type target struct {
	org, agent, request pgtype.UUID
}

// outcome is one request as koinon-agentd left it.
type outcome struct {
	target
	attempts                           int32
	err                                string
	finished, failed, replaced         bool
	snapshots, added, missing, skipped int32
	taken                              int32 // prefixes holding a repository the database has no password for (#262)
}

// Resync asks koinon-agentd to rebuild the home checkpoints of the given agents, or of every agent
// with all, from the snapshots in the bucket: the restore runbook's step after a database restore
// (ADR 0085 decision 17). koinon-agentd adds a checkpoint for each snapshot the database lacks and
// marks failed each complete checkpoint whose snapshot is gone; it never deletes one.
//
// Each agent's request is recorded in one transaction with its audit event ([RequestedAction],
// actor cli); a request replaces the agent's last one. With wait, Resync reads the requests until
// each is done or given up, prints progress and a summary, and fails when one failed, was replaced
// by a newer request, or timeout passed first (the requests stay: koinon-agentd finishes them).
// Agents without a home repository have no checkpoints and are skipped.
func (t *Tool) Resync(ctx context.Context, all bool, agents []string, wait bool, timeout time.Duration) error {
	if all == (len(agents) > 0) {
		return errors.New("name the agents with --agent, or pass --all for every agent")
	}
	if wait && timeout <= 0 {
		return errors.New("--timeout must be above 0 (to not wait, pass --wait=false)")
	}
	if t.Pool == nil {
		return errors.New("checkpoints resync needs --database-url")
	}
	targets, err := t.targets(ctx, all, agents)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		_, _ = fmt.Fprintln(t.Out, "No agent has a home repository; nothing to resync.")
		return nil
	}
	var requested []target
	var errs []error
	for _, h := range targets {
		r, err := t.request(ctx, h)
		if err != nil {
			errs = append(errs, fmt.Errorf("agent %s (org %s): %w", h.agent.String(), h.org.String(), err))
			continue
		}
		requested = append(requested, r)
	}
	_, _ = fmt.Fprintf(t.Out, "Requested a checkpoint resync for %d agent(s).\n", len(requested))
	if len(requested) == 0 {
		return errors.Join(errs...)
	}
	if !wait {
		_, _ = fmt.Fprintln(t.Out, "koinon-agentd resyncs each agent from the bucket; koinon_agentd_home_checkpoint_resyncs{state=\"pending\"} falls to 0 when it is done,")
		_, _ = fmt.Fprintln(t.Out, "and {state=\"failed\"} counts the resyncs it gave up on (the audit log names each, action home.checkpoints_resync_failed).")
		return errors.Join(errs...)
	}
	if err := t.wait(ctx, requested, timeout); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// targets lists the agents with a home repository: all of them, or those named, each of which must
// have one.
func (t *Tool) targets(ctx context.Context, all bool, agents []string) ([]target, error) {
	var ids []pgtype.UUID
	for _, a := range agents {
		var id pgtype.UUID
		if err := id.Scan(strings.TrimSpace(a)); err != nil {
			return nil, fmt.Errorf("%q is not an agent ID", a)
		}
		ids = append(ids, id)
	}
	rows, err := t.Pool.Query(ctx, `SELECT org_id, agent_id FROM home_repositories
		WHERE $1 OR agent_id = ANY($2::uuid[]) ORDER BY org_id, agent_id`, all, ids)
	if err != nil {
		return nil, err
	}
	targets, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (target, error) {
		var h target
		err := r.Scan(&h.org, &h.agent)
		return h, err
	})
	if err != nil || all {
		return targets, err
	}
	found := map[pgtype.UUID]bool{}
	for _, h := range targets {
		found[h.agent] = true
	}
	var missing []string
	for _, id := range ids {
		if !found[id] {
			missing = append(missing, id.String())
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("no home repository for agent(s) %s: nothing was requested", strings.Join(missing, ", "))
	}
	return targets, nil
}

// request records the agent's resync request and its audit event in one transaction.
func (t *Tool) request(ctx context.Context, h target) (target, error) {
	_, err := audit.InTx(ctx, t.Pool, func(q *store.Queries, b audit.Batch) error {
		r, err := q.RequestHomeCheckpointResync(ctx, store.RequestHomeCheckpointResyncParams{OrgID: h.org, AgentID: h.agent})
		if err != nil {
			return err
		}
		h.request = r.RequestID
		b.Org(audit.Event{
			Org: h.org, Actor: actor, Action: RequestedAction, TargetKind: "agent", TargetID: h.agent.String(),
			After: map[string]any{"requestId": r.RequestID.String()},
		})
		return nil
	})
	return h, err
}

// wait reads the requests until each is done, given up or replaced, or timeout passes, printing
// progress when it changes and a summary at the end.
func (t *Tool) wait(ctx context.Context, requested []target, timeout time.Duration) error {
	poll := t.Poll
	if poll <= 0 {
		poll = DefaultPoll
	}
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	tick := time.NewTicker(poll)
	defer tick.Stop()
	last := -1
	for {
		got, err := t.outcomes(ctx, requested)
		if err != nil {
			if ctx.Err() != nil {
				return t.stopped(parent, timeout, len(requested), nil)
			}
			return err
		}
		done := 0
		for _, o := range got {
			if o.finished || o.failed || o.replaced {
				done++
			}
		}
		if done != last {
			_, _ = fmt.Fprintf(t.Out, "%d of %d agent(s) resynced or given up.\n", done, len(got))
			last = done
		}
		if done == len(got) {
			return t.summary(got)
		}
		select {
		case <-ctx.Done():
			return t.stopped(parent, timeout, len(requested), got)
		case <-tick.C:
		}
	}
}

// outcomes reads the requests. A request whose row now carries another request ID was replaced.
func (t *Tool) outcomes(ctx context.Context, requested []target) ([]outcome, error) {
	orgs := make([]pgtype.UUID, len(requested))
	agents := make([]pgtype.UUID, len(requested))
	for i, r := range requested {
		orgs[i], agents[i] = r.org, r.agent
	}
	byAgent := map[[2]pgtype.UUID]outcome{}
	rows, err := t.Pool.Query(ctx, `SELECT r.org_id, r.agent_id, r.request_id, r.attempts, coalesce(r.error, ''),
			r.finished_at IS NOT NULL, r.failed_at IS NOT NULL,
			coalesce(r.snapshots, 0), coalesce(r.added, 0), coalesce(r.missing, 0), coalesce(r.skipped, 0), coalesce(r.taken, 0)
		FROM home_checkpoint_resyncs r
		JOIN unnest($1::uuid[], $2::uuid[]) AS t(org_id, agent_id) USING (org_id, agent_id)`, orgs, agents)
	if err != nil {
		return nil, err
	}
	var o outcome
	if _, err := pgx.ForEachRow(rows, []any{
		&o.org, &o.agent, &o.request, &o.attempts, &o.err, &o.finished, &o.failed,
		&o.snapshots, &o.added, &o.missing, &o.skipped, &o.taken,
	}, func() error {
		byAgent[[2]pgtype.UUID{o.org, o.agent}] = o
		return nil
	}); err != nil {
		return nil, err
	}
	out := make([]outcome, 0, len(requested))
	for _, r := range requested {
		o, ok := byAgent[[2]pgtype.UUID{r.org, r.agent}]
		if !ok || o.request != r.request {
			// The agent was deleted, or a newer request replaced this one: its outcome is not ours.
			o = outcome{replaced: true}
		}
		o.target = r
		out = append(out, o)
	}
	return out, nil
}

// summary prints the totals, a line per agent whose checkpoints need a look, and fails when a
// resync was given up or replaced.
func (t *Tool) summary(got []outcome) error {
	var snapshots, added, missing, skipped int32
	var failed, replaced int
	var taken int32
	for _, o := range got {
		taken += o.taken
		snapshots += o.snapshots
		added += o.added
		missing += o.missing
		skipped += o.skipped
		switch {
		case o.failed:
			failed++
		case o.replaced:
			replaced++
		}
	}
	_, _ = fmt.Fprintf(t.Out, "Resynced %d of %d agent(s): %d snapshot(s) in the bucket, %d checkpoint(s) added, %d marked failed because their snapshot is gone, %d snapshot(s) left out.\n",
		len(got)-failed-replaced, len(got), snapshots, added, missing, skipped)
	if taken > 0 {
		_, _ = fmt.Fprintf(t.Out, "%d repository prefix(es) hold a repository this database has no password for, started after the dump: "+
			"the agents' next repositories start past them (audit home.repository_prefix_taken, docs/ops/secret-keys.md).\n", taken)
	}
	for _, o := range got {
		who := fmt.Sprintf("agent %s (org %s)", o.agent.String(), o.org.String())
		switch {
		case o.failed:
			_, _ = fmt.Fprintf(t.Out, "  %s: given up after %d attempt(s), its checkpoints are as they were: %s\n", who, o.attempts, o.err)
		case o.replaced:
			_, _ = fmt.Fprintf(t.Out, "  %s: a newer request replaced this one\n", who)
		case o.missing > 0 || o.skipped > 0:
			_, _ = fmt.Fprintf(t.Out, "  %s: %d checkpoint(s) marked failed (snapshot gone), %d snapshot(s) left out\n", who, o.missing, o.skipped)
		}
		if !o.failed && !o.replaced && o.taken > 0 {
			_, _ = fmt.Fprintf(t.Out, "  %s: %d repository prefix(es) taken\n", who, o.taken)
		}
	}
	if failed+replaced > 0 {
		return fmt.Errorf("%d resync(s) given up and %d replaced; the audit log names each (action home.checkpoints_resync_failed)", failed, replaced)
	}
	return nil
}

// stopped reports the requests still pending when the wait ended, at the timeout or because the
// command was interrupted (parent done); they stay, and koinon-agentd finishes them.
func (t *Tool) stopped(parent context.Context, timeout time.Duration, requested int, got []outcome) error {
	pending := requested
	for _, o := range got {
		if o.finished || o.failed || o.replaced {
			pending--
		}
	}
	if err := parent.Err(); err != nil {
		return fmt.Errorf("stopped waiting (%w) with %d agent(s) not resynced yet; the requests stay and koinon-agentd finishes them (koinon_agentd_home_checkpoint_resyncs)", err, pending)
	}
	return fmt.Errorf("timed out after %s with %d agent(s) not resynced yet; the requests stay and koinon-agentd finishes them (koinon_agentd_home_checkpoint_resyncs): is koinon-agentd running?", timeout, pending)
}
