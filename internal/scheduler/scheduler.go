// SPDX-License-Identifier: Apache-2.0

// Package scheduler queues and cancels runs (plan item A4, ADRs 0030, 0049).
//
//   - One active run per agent; further runs queue in FIFO order. Queueing
//     only inserts a queued run: agentd claims runs with store.ClaimNextRun,
//     which enforces the order and the one-active-run rule.
//   - A run gets the limits of the agent version it executes (from the template, or the
//     agent's override): a wall-clock limit enforced by Sweeper, and a model-request limit and a
//     spend limit enforced by CheckRunLimits, which whoever records a ledger entry calls (ADR
//     0081). Tokens are reported by RecordUsage, not enforced.
//   - Every cancel writes a run event and a task event saying why.
//
// Functions taking a *store.Queries run inside the caller's transaction.
package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/budget"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/runlimits"
	"github.com/stateracloud/koinon/internal/runoutcome"
)

// KindRunQueued is the run and task event of a queued run. The cancels write
// runoutcome.KindRunCanceled.
const KindRunQueued = "run.queued"

// ErrAgentNotActive means the task's agent cannot take runs.
var ErrAgentNotActive = errors.New("agent is not active")

// ErrAgentUnhealthy means the agent's sandbox keeps getting stuck and an admin has not resumed it
// (ADR 0054). The error wraps the reason.
var ErrAgentUnhealthy = errors.New("agent is unhealthy")

// Enqueue queues a run of the task's agent for the task, with the caps of
// the agent's active version. It queues nothing, and returns false, when the
// task has no agent or already has a queued run (that run will see every
// message appended before it starts). The caller must hold the task's row
// lock (for example by having appended a task event in the same
// transaction), so concurrent calls for one task queue at most one run.
func Enqueue(ctx context.Context, q *store.Queries, task store.Task) (store.Run, bool, error) {
	return EnqueueOrigin(ctx, q, task, store.RunOriginInteractive)
}

// EnqueueOrigin is Enqueue for a run of the given origin. Interactive runs are claimed before
// scheduled ones (LT1, ADR 0066); triggers and schedules (TR1) pass RunOriginScheduled.
func EnqueueOrigin(ctx context.Context, q *store.Queries, task store.Task, origin store.RunOrigin) (store.Run, bool, error) {
	if !task.AgentID.Valid {
		return store.Run{}, false, nil
	}
	org := task.OrgID
	queued, err := q.HasQueuedRunForTask(ctx, store.HasQueuedRunForTaskParams{OrgID: org, TaskID: task.ID})
	if err != nil {
		return store.Run{}, false, err
	}
	if queued {
		return store.Run{}, false, nil
	}
	agent, err := q.GetAgent(ctx, store.GetAgentParams{OrgID: org, ID: task.AgentID})
	if err != nil {
		return store.Run{}, false, fmt.Errorf("load agent: %w", err)
	}
	if agent.Status != store.AgentStatusActive || !agent.ActiveVersionID.Valid {
		return store.Run{}, false, ErrAgentNotActive
	}
	if agent.UnhealthyAt.Valid {
		return store.Run{}, false, fmt.Errorf("%w: %s", ErrAgentUnhealthy, agent.UnhealthyReason.String)
	}
	version, err := q.GetAgentVersion(ctx, store.GetAgentVersionParams{OrgID: org, ID: agent.ActiveVersionID})
	if err != nil {
		return store.Run{}, false, fmt.Errorf("load agent version: %w", err)
	}
	cfg, err := agentconfig.Parse(version.Config)
	if err != nil {
		return store.Run{}, false, err
	}
	limits, err := runlimits.ForAgent(ctx, q, org, agent.ID, cfg)
	if err != nil {
		return store.Run{}, false, err
	}
	run, err := q.CreateRun(ctx, store.CreateRunParams{
		OrgID: org, AgentID: agent.ID, TaskID: task.ID,
		Origin:              store.NullRunOrigin{RunOrigin: origin, Valid: true},
		MaxWallClockSeconds: int32(limits.WallClock / time.Second), //nolint:gosec // bounded by templates.MaxRunDuration
		MaxModelRequests:    pgtype.Int8{Int64: limits.MaxModelRequests, Valid: true},
		MaxSpendUsd:         pgtype.Text{String: limits.MaxSpend.Numeric(), Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Run{}, false, ErrAgentNotActive
	}
	if err != nil {
		return store.Run{}, false, fmt.Errorf("create run: %w", err)
	}
	payload := mustJSON(map[string]any{
		"max_wall_clock_seconds": run.MaxWallClockSeconds,
		"max_model_requests":     run.MaxModelRequests.Int64,
		"max_spend_usd":          limits.MaxSpend.String(),
	})
	ev, err := q.AppendRunControlEvent(ctx, store.AppendRunControlEventParams{
		OrgID: org, RunID: run.ID, Kind: KindRunQueued, Payload: payload,
	})
	if err != nil {
		return store.Run{}, false, fmt.Errorf("append run event: %w", err)
	}
	run.LastEventSeq = ev.Seq
	if _, err := q.AppendTaskEvent(ctx, store.AppendTaskEventParams{
		OrgID: org, TaskID: task.ID, Kind: KindRunQueued, ActorKind: store.TaskActorKindSystem,
		RunID: run.ID, Payload: payload,
	}); err != nil {
		return store.Run{}, false, fmt.Errorf("append task event: %w", err)
	}
	return run, true, nil
}

// DB is what the scheduler needs from a pool to open transactions.
type DB interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	store.DBTX
}

// RecordUsage adds tokens to a run's reported total and checks its limits (CheckRunLimits).
// Whoever learns a run's token usage (the spend ledger, from agentgateway's access log) calls it
// with the tokens used since the last report. Tokens are reported, not enforced (ADR 0081): the
// limits that can end the run are its model requests and its spend. It returns the updated run;
// pgx.ErrNoRows means the run has finished or does not exist in the org, and the usage is dropped.
func RecordUsage(ctx context.Context, db DB, org, runID pgtype.UUID, tokens int64) (store.Run, error) {
	if tokens < 0 {
		return store.Run{}, fmt.Errorf("negative token usage %d", tokens)
	}
	var out store.Run
	err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		q := store.New(tx)
		run, err := q.AddRunTokens(ctx, store.AddRunTokensParams{OrgID: org, ID: runID, Tokens: tokens})
		if err != nil {
			return err
		}
		out, err = checkRunLimits(ctx, tx, run)
		return err
	})
	return out, err
}

// CheckRunLimits cancels the run if it has reached its model-request limit or a spend limit of
// its scope (ADR 0081). Whoever records or changes a ledger entry of the run calls it after the
// write, in the same transaction: a new entry, and a settlement that raises an entry's cost. It
// returns the run, canceled or not; pgx.ErrNoRows means the run has finished or does not exist.
//
// The run row is locked first and the lock is held to the end of the enclosing transaction, so
// that concurrent writers of one run's entries are serialised: each counts the entries the
// others committed before it. Without the lock two requests recorded at once could both count
// N-1 entries and overshoot the request limit.
func CheckRunLimits(ctx context.Context, db DB, org, runID pgtype.UUID) (store.Run, error) {
	var out store.Run
	err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		q := store.New(tx)
		run, err := q.LockRun(ctx, store.LockRunParams{OrgID: org, ID: runID})
		if err != nil {
			return err
		}
		if !isOpen(run) {
			return pgx.ErrNoRows
		}
		out, err = checkRunLimits(ctx, tx, run)
		return err
	})
	return out, err
}

func isOpen(r store.Run) bool {
	switch r.State {
	case store.RunStateQueued, store.RunStateStarting, store.RunStateRunning, store.RunStateParked:
		return true
	}
	return false
}

// spendScope reads one spend limit that covers a run, as a budget level, with the spend inside it.
// ok is false when the run is not in that scope. Run is the scope of rc.8; the per-task cap of
// old issue 183 is another entry of spendScopes (a level named for the task, summing the task's entries),
// and the check and the cancel below need no change.
type spendScope func(ctx context.Context, q *store.Queries, run store.Run) (l budget.Level, ok bool, err error)

var spendScopes = []spendScope{budget.RunLevel}

func checkRunLimits(ctx context.Context, tx pgx.Tx, run store.Run) (store.Run, error) {
	q := store.New(tx)
	limits, err := runlimits.Of(run)
	if err != nil {
		return run, err
	}
	usage, err := runlimits.ReadUsage(ctx, q, run.OrgID, run.ID)
	if err != nil {
		return run, err
	}
	reason, msg, extra := runoutcome.CancelReason(""), "", map[string]any(nil)
	if r, hit := limits.CheckRequests(usage); hit {
		reason, msg, extra = runoutcome.ReasonRequestCap, r.Message(), r.Detail()
	}
	if reason == "" {
		for _, scope := range spendScopes {
			l, ok, err := scope(ctx, q, run)
			if err != nil {
				return run, err
			}
			if ok && l.Reached() {
				reason, msg, extra = runoutcome.ReasonRunSpendCap, l.StopMessage(), l.StopDetail()
				break
			}
		}
	}
	if reason == "" {
		return run, nil
	}
	extra["model_requests"], extra["spent_usd"] = usage.Requests, usage.Spend.String()
	// What the agent last said travels with the cancel, as at the time limit (ADR 0066 amendment
	// 2026-10-28).
	canceled, err := runoutcome.CancelAtLimit(ctx, tx, run.OrgID, run.ID, runoutcome.Cancellation{
		Reason: reason, Message: msg, Class: store.RunFailureClassBudget, Actor: runoutcome.System, Extra: extra,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return run, nil // finished meanwhile
	}
	return canceled, err
}

// SweepWallClock cancels every owned run past its wall-clock cap, org by org,
// and returns how many it canceled. Several API replicas may sweep at once: a
// run canceled by one no longer matches the others.
func SweepWallClock(ctx context.Context, db DB) (int, error) {
	orgs, err := store.New(db).ListOrgIDs(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, org := range orgs {
		err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
			runs, err := runoutcome.CancelOverTimeLimit(ctx, tx, org, ContinueAfterTimeLimit)
			n += len(runs)
			return err
		})
		if err != nil {
			return n, fmt.Errorf("sweep org %s: %w", org.String(), err)
		}
	}
	return n, nil
}

// Sweeper runs SweepWallClock every interval until ctx ends.
func Sweeper(ctx context.Context, db DB, interval time.Duration, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		n, err := SweepWallClock(ctx, db)
		if err != nil && ctx.Err() == nil {
			log.Error("wall-clock sweep failed", "err", err)
		} else if n > 0 {
			log.Info("canceled runs over their wall-clock cap", "runs", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // only maps of plain values are passed
	}
	return b
}
