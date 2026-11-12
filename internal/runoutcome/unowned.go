// SPDX-License-Identifier: Apache-2.0

package runoutcome

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/runreply"
)

// The cancels of this file are the control plane's: a person, a budget stop, the time-limit sweep,
// an expired approval, a task that ended or an agent that was archived ends a run nobody here owns.
// They hold no epoch and are fenced by the run's state instead: only a run that is still live
// (queued, starting, running or parked) is canceled, and its owner's later writes then match no row
// (ADR 0050, ADR 0030). Whichever of the two commits first is the run's outcome.

// KindRunCanceled is the run and task event of a canceled run.
const KindRunCanceled = "run.canceled"

// CancelReason is the machine-readable cause of a cancel, in the event payload's "reason".
type CancelReason string

// Cancel reasons.
const (
	ReasonUser CancelReason = "user"
	// ReasonTaskDone: the task reached an end state. The value is what earlier events stored.
	ReasonTaskDone      CancelReason = "ticket_done"
	ReasonAgentArchived CancelReason = "agent_archived"
	ReasonWallClockCap  CancelReason = "wall_clock_cap"
	// ReasonTokenCap: the token cap of runs before ADR 0081. It cancels nothing now; old events
	// carry it.
	ReasonTokenCap CancelReason = "token_cap"
	// ReasonRequestCap: the run made as many model requests as its limit allows (ADR 0081).
	ReasonRequestCap CancelReason = "request_cap"
	// ReasonRunSpendCap: the run spent its spend limit (ADR 0081). The agent's, org's and platform's
	// budgets cancel with ReasonSpendLimit.
	ReasonRunSpendCap CancelReason = "run_spend_cap"
	// ReasonSpendLimit: the agent reached its spend limit (plan item L1).
	ReasonSpendLimit CancelReason = "spend_limit"
	// ReasonApprovalExpired: the run was parked on an approval nobody decided in time
	// (ADR 0049: 7 days).
	ReasonApprovalExpired CancelReason = "approval_expired"
)

// Actor is who caused a cancel, for the task event.
type Actor struct {
	// UserID is set for a user; otherwise the actor is the system.
	UserID pgtype.UUID
}

// System is the actor of automatic cancels (limits, sweeps, expiries).
var System = Actor{}

// Cancellation is what a cancel by someone other than the run's owner records.
type Cancellation struct {
	Reason CancelReason
	// Message is the run's outcome reason and the events' "message".
	Message string
	// Class is the failure class recorded with the cancel; empty for none.
	Class store.RunFailureClass
	Actor Actor
	// Extra is merged into the events' payload, over "reason" and "message"; the run's failure class
	// wins over an extra field of the same name.
	Extra map[string]any
}

// TxDB is a transaction or a pool, for the cancels that read what the agent last said: the read
// runs in a savepoint (or a transaction of its own), so that a failed read leaves the cancel's
// transaction usable. pgx.Tx and *pgxpool.Pool are both one.
type TxDB interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	store.DBTX
}

// CancelUnowned cancels a live run that the caller does not own. pgx.ErrNoRows: it has finished or
// does not exist in the org. q is the caller's transaction.
func CancelUnowned(ctx context.Context, q *store.Queries, org, runID pgtype.UUID, c Cancellation) (store.Run, error) {
	run, err := q.CancelRun(ctx, store.CancelRunParams{
		OrgID: org, ID: runID, OutcomeReason: c.Message,
		FailureClass: store.NullRunFailureClass{RunFailureClass: c.Class, Valid: c.Class != ""},
	})
	if err != nil {
		return store.Run{}, err
	}
	return recordCanceled(ctx, q, run, c)
}

// CancelAtLimit is CancelUnowned for a run that reached one of its own limits: the events carry
// what the agent last said (summary and summarySource, ADR 0066 amendment 2026-10-28), as at the
// time limit. A failure to read it leaves the summary out; the cancel does not wait on it.
func CancelAtLimit(ctx context.Context, db TxDB, org, runID pgtype.UUID, c Cancellation) (store.Run, error) {
	q := store.New(db)
	run, err := q.CancelRun(ctx, store.CancelRunParams{
		OrgID: org, ID: runID, OutcomeReason: c.Message,
		FailureClass: store.NullRunFailureClass{RunFailureClass: c.Class, Valid: c.Class != ""},
	})
	if err != nil {
		return store.Run{}, err
	}
	summary, serr := readIn(ctx, db, func(q *store.Queries) (summaryOf, error) {
		s, src, err := runreply.Summary(ctx, q, run)
		return summaryOf{s, src}, err
	})
	if serr != nil {
		slog.Warn("reading the agent's last reply for the limit note failed", "run", run.ID.String(), "err", serr)
	}
	if summary.text != "" {
		c.Extra = with(c.Extra, map[string]any{"summary": summary.text, "summarySource": summary.source})
	}
	return recordCanceled(ctx, q, run, c)
}

// CancelQueuedForTask cancels the task's queued runs, when it reaches an end state. msg is the
// outcome the runs record.
func CancelQueuedForTask(ctx context.Context, q *store.Queries, task store.Task, msg string, actor Actor) ([]store.Run, error) {
	runs, err := q.CancelQueuedRunsForTask(ctx, store.CancelQueuedRunsForTaskParams{
		OrgID: task.OrgID, TaskID: task.ID, OutcomeReason: msg,
	})
	if err != nil {
		return nil, err
	}
	return recordAll(ctx, q, runs, Cancellation{Reason: ReasonTaskDone, Message: msg, Actor: actor})
}

// CancelForAgent cancels every live run of an agent, when it is archived.
func CancelForAgent(ctx context.Context, q *store.Queries, org, agentID pgtype.UUID, actor Actor) ([]store.Run, error) {
	const msg = "agent archived"
	runs, err := q.CancelOpenRunsForAgent(ctx, store.CancelOpenRunsForAgentParams{
		OrgID: org, AgentID: agentID, OutcomeReason: msg,
	})
	if err != nil {
		return nil, err
	}
	return recordAll(ctx, q, runs, Cancellation{Reason: ReasonAgentArchived, Message: msg, Actor: actor})
}

// CancelOverTimeLimit cancels every owned run of the org past its wall-clock cap, with the note of
// runreply.TimeLimitNote: the same note the run's owner writes when it stops the run at its time
// limit itself (Owner.CancelAtTimeLimit). A failure to read the summary leaves it out; the cancel
// does not wait on it. after, when not nil, runs for each canceled run in the same transaction
// (see Config.AfterTimeLimit).
func CancelOverTimeLimit(ctx context.Context, db TxDB, org pgtype.UUID, after RunStep) ([]store.Run, error) {
	q := store.New(db)
	runs, err := q.CancelRunsOverWallClock(ctx, store.CancelRunsOverWallClockParams{
		OrgID: org, OutcomeReason: "wall-clock cap exceeded",
	})
	if err != nil {
		return nil, err
	}
	out := make([]store.Run, 0, len(runs))
	for _, r := range runs {
		note, nerr := readIn(ctx, db, func(q *store.Queries) (map[string]any, error) {
			return runreply.TimeLimitNote(ctx, q, r)
		})
		if nerr != nil {
			slog.Warn("reading the agent's last reply for the time-limit note failed", "run", r.ID.String(), "err", nerr)
			note = runreply.TimeLimitNoteBase(r)
		}
		msg, _ := note["message"].(string)
		r, err := recordCanceled(ctx, q, r, Cancellation{Reason: ReasonWallClockCap, Message: msg, Actor: System, Extra: note})
		if err != nil {
			return nil, err
		}
		if after != nil {
			if err := after(ctx, q, r); err != nil {
				return nil, err
			}
		}
		out = append(out, r)
	}
	return out, nil
}

type summaryOf struct{ text, source string }

// readIn runs a read in a savepoint of db (or a transaction of its own, for a pool), so that a
// failed read is rolled back and leaves the caller's transaction usable.
func readIn[T any](ctx context.Context, db TxDB, read func(q *store.Queries) (T, error)) (T, error) {
	var zero T
	sp, err := db.Begin(ctx)
	if err != nil {
		return zero, err
	}
	v, err := read(store.New(sp))
	if err != nil {
		_ = sp.Rollback(ctx)
		return zero, err
	}
	return v, sp.Commit(ctx)
}

func recordAll(ctx context.Context, q *store.Queries, runs []store.Run, c Cancellation) ([]store.Run, error) {
	out := make([]store.Run, 0, len(runs))
	for _, r := range runs {
		r, err := recordCanceled(ctx, q, r, c)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// recordCanceled writes the closing run event and the task event of a run that was just canceled.
// A cancel leaves the task as it is (ADR 0065: only a success or a fatal failure ends it).
func recordCanceled(ctx context.Context, q *store.Queries, run store.Run, c Cancellation) (store.Run, error) {
	p := with(map[string]any{"reason": string(c.Reason), "message": c.Message}, c.Extra)
	if run.FailureClass.Valid {
		p["failureClass"] = string(run.FailureClass.RunFailureClass)
	}
	payload, err := json.Marshal(p)
	if err != nil {
		return store.Run{}, fmt.Errorf("encode the cancel: %w", err)
	}
	ev, err := q.AppendRunControlEvent(ctx, store.AppendRunControlEventParams{
		OrgID: run.OrgID, RunID: run.ID, Kind: KindRunCanceled, Payload: payload,
	})
	if err != nil {
		return store.Run{}, fmt.Errorf("append run event: %w", err)
	}
	run.LastEventSeq = ev.Seq
	te := store.AppendTaskEventParams{
		OrgID: run.OrgID, TaskID: run.TaskID, Kind: KindRunCanceled,
		ActorKind: store.TaskActorKindSystem, RunID: run.ID, Payload: payload,
	}
	if c.Actor.UserID.Valid {
		te.ActorKind, te.ActorUserID = store.TaskActorKindUser, c.Actor.UserID
	}
	if _, err := q.AppendTaskEvent(ctx, te); err != nil {
		return store.Run{}, fmt.Errorf("append task event: %w", err)
	}
	return run, nil
}

// with returns a copy of base with extra merged over it.
func with(base, extra map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}
