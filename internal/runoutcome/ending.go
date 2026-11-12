// SPDX-License-Identifier: Apache-2.0

package runoutcome

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/questions"
	"github.com/stateracloud/koinon/internal/runreply"
)

// KindParked is the run and task event of a run parking.
const KindParked = runreply.KindParked

// defaultParkTTL is how long a park that waits for no human may last before it expires.
const defaultParkTTL = 7 * 24 * time.Hour

// begin runs fn in a transaction under the Owner's mutex with the next source seq, which the
// closing run event of the transaction carries.
func (o *Owner) begin(ctx context.Context, fn func(q *store.Queries, b audit.Batch, src int64) error) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	// What the agent was saying when the run ends is cut off: the reply the verb posts reads it.
	if !o.IsFenced() {
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), flushTimeout)
		if err := o.flushLocked(fctx, true, false); err != nil && !errors.Is(err, ErrFenced) {
			o.cfg.Log.Warn("storing the item in progress failed", "err", err)
		}
		cancel()
	}
	src := o.nextSrc()
	_, err := audit.InPgTx(ctx, o.cfg.DB, func(tx pgx.Tx, b audit.Batch) error {
		return fn(o.q.WithTx(tx), b, src)
	})
	return err
}

// Delivered are the outcomes the turn's prompt carried: the decided approval requests and the
// resolved questions. EndTurn marks them delivered.
type Delivered struct {
	Approvals []pgtype.UUID
	Questions []pgtype.UUID
}

// TurnEnd is what EndTurn decided.
type TurnEnd struct {
	// Parked: the run waits (for a human, or for the executor of approved requests) instead of
	// succeeding.
	Parked bool
	// ParkReason is why, when Parked.
	ParkReason store.RunParkReason
	// Waiting: a human has something to decide, so the task moves to input-required.
	Waiting bool
}

// EndTurn ends a turn that stopped normally. In one transaction under the run's row lock, which
// orders the decision against the approval service's resolution of the park, it marks the delivered
// outcomes, then either succeeds the run or parks it while approval requests or questions the agent
// made are open or their outcomes unsent. The turn row ends with the run's row, as "end_turn" or
// "park:<reason>".
//
// Unlike the other verbs, a failed EndTurn does not end the turn row itself (it uses Err, not failed):
// the turn row's reason is not known here. The caller does not know how the turn will end until it
// has read the error (a cancel fence ends it as "canceled", a lost run leaves it to the new owner), so
// it ends the row with CloseTurn and the reason it then knows. Ending it here first, with the reason
// of the caller's Turn, would win over that (EndRunTurn only ends an open row).
func (o *Owner) EndTurn(ctx context.Context, d Delivered, turn Turn) (TurnEnd, error) {
	var end TurnEnd
	err := o.begin(ctx, func(q *store.Queries, b audit.Batch, src int64) error {
		var err error
		end, err = o.endTurnIn(ctx, q, src, d, turn)
		return err
	})
	if err != nil {
		return TurnEnd{}, o.Err(ctx, "end the turn", err)
	}
	if end.Parked {
		o.finished(store.RunStateParked)
		o.cfg.Log.Info("run parked on approvals", "waiting_on_human", end.Waiting)
	} else {
		o.finished(store.RunStateSucceeded)
		o.cfg.Log.Info("run finished", "outcome", store.RunStateSucceeded)
	}
	return end, nil
}

func (o *Owner) endTurnIn(ctx context.Context, q *store.Queries, src int64, d Delivered, turn Turn) (TurnEnd, error) {
	var end TurnEnd
	org := o.run.OrgID
	if len(d.Approvals) > 0 {
		if _, err := q.MarkApprovalRequestsDelivered(ctx, store.MarkApprovalRequestsDeliveredParams{OrgID: org, Ids: d.Approvals}); err != nil {
			return end, err
		}
	}
	if len(d.Questions) > 0 {
		if _, err := q.MarkQuestionsDelivered(ctx, store.MarkQuestionsDeliveredParams{OrgID: org, Ids: d.Questions}); err != nil {
			return end, err
		}
	}
	run, err := q.LockRun(ctx, store.LockRunParams{OrgID: org, ID: o.run.ID})
	if err != nil {
		return end, err
	}
	if run.Epoch != o.run.Epoch || run.State != store.RunStateRunning {
		return end, pgx.ErrNoRows // fenced
	}
	// The agent canceled the task in this turn (cancel_task): there is nothing to wait for. The
	// questions it asked are canceled instead of opened, and the approvals are canceled by the
	// approvals sweep once the run has ended.
	task, err := q.GetTask(ctx, store.GetTaskParams{OrgID: org, ID: o.run.TaskID})
	if err != nil {
		return end, err
	}
	if task.State == store.TaskStateCanceled {
		if err := o.endRow(ctx, q, turn, "end_turn"); err != nil {
			return end, err
		}
		if _, err := questions.CancelForRun(ctx, q, org, o.run.ID, "the task was canceled"); err != nil {
			return end, err
		}
		return end, o.finishIn(ctx, q, src, store.RunStateSucceeded, "", "", nil)
	}
	reqs, err := q.ListUndeliveredApprovalRequestsForRun(ctx, store.ListUndeliveredApprovalRequestsForRunParams{OrgID: org, RunID: o.run.ID})
	if err != nil {
		return end, err
	}
	qs, err := q.ListUndeliveredQuestionsForRun(ctx, store.ListUndeliveredQuestionsForRunParams{OrgID: org, RunID: o.run.ID})
	if err != nil {
		return end, err
	}
	if len(reqs) == 0 && len(qs) == 0 {
		if err := o.endRow(ctx, q, turn, "end_turn"); err != nil {
			return end, err
		}
		return end, o.finishIn(ctx, q, src, store.RunStateSucceeded, "", "", nil)
	}

	end.Parked = true
	ids := make([]string, 0, len(reqs))
	var expires pgtype.Timestamptz
	// Questions the agent asked this turn become visible now, in this transaction.
	asked := false
	for _, row := range qs {
		switch row.State {
		case store.QuestionStateAsked, store.QuestionStateOpen:
			asked = true
			if !expires.Valid || row.ExpiresAt.Time.Before(expires.Time) {
				expires = row.ExpiresAt
			}
		}
	}
	end.Waiting = asked
	for _, a := range reqs {
		ids = append(ids, a.ID.String())
		if a.Status == store.ApprovalStatusPending {
			end.Waiting = true
			if !expires.Valid || a.ExpiresAt.Time.Before(expires.Time) {
				expires = a.ExpiresAt
			}
		}
	}
	if !expires.Valid {
		// Nothing waits for a human; the park is resolved below or by the executor.
		expires = pgtype.Timestamptz{Time: o.cfg.Now().Add(defaultParkTTL), Valid: true}
	}
	reason := store.RunParkReasonApproval
	if asked {
		reason = store.RunParkReasonInputRequired
	}
	end.ParkReason = reason
	// The turn's row ends with the park, before the run's state changes, so a resume on another
	// replica sees it ended and how long the run waited.
	if err := o.endRow(ctx, q, turn, "park:"+string(reason)); err != nil {
		return end, err
	}
	// What the agent said before it parked goes on the task now, before the park note (and before
	// run.parked, after which the reply starts afresh).
	if err := o.postReply(ctx, q); err != nil {
		return end, err
	}
	qids := make([]string, 0, len(qs))
	for _, row := range qs {
		qids = append(qids, row.ID.String())
	}
	payload, _ := json.Marshal(map[string]any{"reason": string(reason), "approvals": ids, "questions": qids})
	if _, err := q.AppendRunEventOnce(ctx, store.AppendRunEventOnceParams{
		OrgID: org, RunID: o.run.ID, Epoch: o.run.Epoch, Kind: KindParked, Payload: payload, SourceSeq: src,
	}); err != nil {
		return end, err
	}
	if _, err := q.ParkRun(ctx, store.ParkRunParams{
		OrgID: org, ID: o.run.ID, Epoch: o.run.Epoch, ParkReason: reason, ParkExpiresAt: expires,
	}); err != nil {
		return end, err
	}
	if asked {
		if _, err := questions.OpenForRun(ctx, q, org, o.run.ID, o.run.TaskID); err != nil {
			return end, err
		}
	}
	if _, err := q.AppendTaskEvent(ctx, store.AppendTaskEventParams{
		OrgID: org, TaskID: o.run.TaskID, RunID: o.run.ID,
		Kind: KindParked, ActorKind: store.TaskActorKindSystem, Payload: payload,
	}); err != nil {
		return end, err
	}
	if end.Waiting {
		_, err := q.SetTaskStateFrom(ctx, store.SetTaskStateFromParams{
			OrgID: org, ID: o.run.TaskID, ToState: store.TaskStateInputRequired,
			FromStates: []string{string(store.TaskStateSubmitted), string(store.TaskStateWorking)},
		})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return end, err
		}
		return end, nil
	}
	for _, a := range reqs {
		if a.Status.Open() {
			return end, nil // approved, being executed: the executor resolves the park
		}
	}
	// Every request and question was decided while the turn ran: resume at once.
	_, err = q.ResolveRunPark(ctx, store.ResolveRunParkParams{OrgID: org, ID: o.run.ID})
	return end, err
}

// Succeed ends the run as succeeded, with the agent's reply on the task, and completes the task.
func (o *Owner) Succeed(ctx context.Context, turn Turn) error {
	return o.end(ctx, turn, store.RunStateSucceeded, "", "", nil)
}

// Supersede ends a run that has nothing left to do as succeeded, without a turn: the task's earlier
// run already read every message this run was queued for (#21). The reason is the run's outcome
// reason. Like any success it completes the task, which the queued run was holding open (EndTaskFrom
// waits for the queue to empty); the run has no reply of its own. The run is still starting (no
// harness session was made), and FinishRun ends a run as succeeded only from running, so in one
// transaction under the run's lock the verb takes the starting -> running step and the end together:
// nothing ever sees a running run without a harness.
func (o *Owner) Supersede(ctx context.Context, turn Turn, reason string) error {
	err := o.begin(ctx, func(q *store.Queries, b audit.Batch, src int64) error {
		run, err := q.LockRun(ctx, store.LockRunParams{OrgID: o.run.OrgID, ID: o.run.ID})
		if err != nil {
			return err
		}
		if run.Epoch != o.run.Epoch {
			return pgx.ErrNoRows
		}
		if err := o.endRow(ctx, q, turn, "superseded"); err != nil {
			return err
		}
		if run.State == store.RunStateStarting {
			if _, err := q.MarkRunRunning(ctx, store.MarkRunRunningParams{OrgID: o.run.OrgID, ID: o.run.ID, Epoch: o.run.Epoch}); err != nil {
				return err
			}
		}
		return o.finishIn(ctx, q, src, store.RunStateSucceeded, reason, "", nil)
	})
	if err != nil {
		return o.failed(ctx, turn, "supersede the run", err)
	}
	o.finished(store.RunStateSucceeded)
	o.cfg.Log.Info("run finished", "outcome", store.RunStateSucceeded, "reason", reason)
	return nil
}

// Fail ends the run as failed with a reason and a failure class. A fatal class fails the task too
// (ADR 0065); any other leaves it open for a person to retry with a message.
func (o *Owner) Fail(ctx context.Context, turn Turn, reason string, class store.RunFailureClass) error {
	return o.end(ctx, turn, store.RunStateFailed, reason, class, nil)
}

// Cancel ends the run as canceled by its owner, for example at the wall-clock cap or on a spent
// budget. class is the failure class recorded with the cancel, if any; extra is merged into the
// payload of the closing events; the failure class wins over an extra field of the same name.
func (o *Owner) Cancel(ctx context.Context, turn Turn, reason string, class store.RunFailureClass, extra map[string]any) error {
	return o.end(ctx, turn, store.RunStateCanceled, reason, class, extra)
}

// CancelAtTimeLimit cancels the run as its owner at its wall-clock cap. The closing events carry
// the note of runreply.TimeLimitNote, the same the control plane's sweep writes when it gets there
// first (CancelOverTimeLimit); reason is the run's outcome reason. A failure to read the agent's
// last reply leaves the summary out of the note.
func (o *Owner) CancelAtTimeLimit(ctx context.Context, turn Turn, reason string) error {
	note, err := runreply.TimeLimitNote(ctx, o.q, o.run)
	if err != nil {
		o.cfg.Log.Warn("reading the agent's last reply for the time-limit note failed", "err", err)
	}
	return o.Cancel(ctx, turn, reason, "", note)
}

func (o *Owner) end(ctx context.Context, turn Turn, outcome store.RunState, reason string, class store.RunFailureClass, extra map[string]any) error {
	err := o.begin(ctx, func(q *store.Queries, b audit.Batch, src int64) error {
		if err := o.endRow(ctx, q, turn, turn.Reason); err != nil {
			return err
		}
		return o.finishIn(ctx, q, src, outcome, reason, class, extra)
	})
	if err != nil {
		return o.failed(ctx, turn, "finish the run", err)
	}
	o.finished(outcome)
	o.cfg.Log.Info("run finished", "outcome", outcome, "reason", reason)
	return nil
}

// finishIn ends the run in the caller's transaction: the closing run event, FinishRun, the reply or
// the failure on the task, and the task's end after it.
func (o *Owner) finishIn(ctx context.Context, q *store.Queries, src int64, outcome store.RunState, reason string, class store.RunFailureClass, extra map[string]any) error {
	body := map[string]any{}
	for k, v := range extra {
		body[k] = v
	}
	if _, ok := body["reason"]; !ok {
		body["reason"] = reason
	}
	if class != "" {
		body["failureClass"] = string(class)
	}
	payload, _ := json.Marshal(body)
	if _, err := q.AppendRunEventOnce(ctx, store.AppendRunEventOnceParams{
		OrgID: o.run.OrgID, RunID: o.run.ID, Epoch: o.run.Epoch,
		Kind: "run." + string(outcome), Payload: payload, SourceSeq: src,
	}); err != nil {
		return err
	}
	if _, err := q.FinishRun(ctx, store.FinishRunParams{
		OrgID: o.run.OrgID, ID: o.run.ID, Epoch: o.run.Epoch, Outcome: outcome,
		OutcomeReason: pgtype.Text{String: reason, Valid: reason != ""},
		FailureClass:  store.NullRunFailureClass{RunFailureClass: class, Valid: class != ""},
	}); err != nil {
		return err
	}
	if outcome == store.RunStateSucceeded {
		if err := o.postReply(ctx, q); err != nil {
			return err
		}
	} else if _, err := q.AppendTaskEvent(ctx, store.AppendTaskEventParams{
		OrgID: o.run.OrgID, TaskID: o.run.TaskID, RunID: o.run.ID,
		Kind: "run." + string(outcome), ActorKind: store.TaskActorKindSystem, Payload: payload,
	}); err != nil {
		return err
	}
	// The task's end comes after the reply or the failure on its timeline.
	return o.endTask(ctx, q, outcome, class)
}

// endTask moves the run's task to the run's outcome (ADR 0065): a run that succeeded completes it, a
// run that failed fatally fails it, whether or not the run parked on the way. Other failures leave the
// task open (a person can retry them with a message), and a task that is canceled, already ended or
// has another run queued (a follow-up message) stays as it is.
func (o *Owner) endTask(ctx context.Context, q *store.Queries, outcome store.RunState, class store.RunFailureClass) error {
	var to store.TaskState
	switch {
	case outcome == store.RunStateSucceeded:
		to = store.TaskStateCompleted
	case outcome == store.RunStateFailed && class == store.RunFailureClassFatal:
		to = store.TaskStateFailed
	default:
		return nil
	}
	t, err := q.EndTaskFrom(ctx, store.EndTaskFromParams{
		OrgID: o.run.OrgID, ID: o.run.TaskID, ToState: to,
		FromStates: []string{string(store.TaskStateSubmitted), string(store.TaskStateWorking), string(store.TaskStateInputRequired)},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]any{"to": string(t.State), "reason": endReason(outcome)})
	_, err = q.AppendTaskEvent(ctx, store.AppendTaskEventParams{
		OrgID: o.run.OrgID, TaskID: o.run.TaskID, RunID: o.run.ID,
		Kind: "status.changed", ActorKind: store.TaskActorKindSystem, Payload: payload,
	})
	return err
}

// ReasonRunSucceeded is the reason of the status.changed event with which a succeeded run completes
// its task. RequestSessionRestartForAgent (queries/stuck.sql) matches it literally to tell a task a run
// completed from one a person completed (#81).
const ReasonRunSucceeded = endReasonPrefix + string(store.RunStateSucceeded)

// endReasonPrefix starts the reason of the status.changed event that ends a task with the run's outcome.
const endReasonPrefix = "run_"

// endReason is the reason of the status.changed event that ends a task with the run's outcome.
func endReason(outcome store.RunState) string { return endReasonPrefix + string(outcome) }

// postReply puts the agent's reply since the run's last park on the task, if it said anything
// (interim narration before a tool call is left out).
func (o *Owner) postReply(ctx context.Context, q *store.Queries) error {
	reply, err := runreply.Reply(ctx, q, o.run.OrgID, o.run.ID)
	if err != nil || reply == "" {
		return err
	}
	text, _ := json.Marshal(map[string]string{"text": reply})
	_, err = q.AppendTaskEvent(ctx, store.AppendTaskEventParams{
		OrgID: o.run.OrgID, TaskID: o.run.TaskID, RunID: o.run.ID,
		Kind: "message", ActorKind: store.TaskActorKindAgent, ActorAgentID: o.run.AgentID, Payload: text,
	})
	return err
}

// Requeue puts the run back in the queue: its sandbox was not ready in time. It is the run's
// owner's last write; the next claim gives the run a new epoch. A run that was taken over, canceled
// or finished meanwhile is ErrFenced.
func (o *Owner) Requeue(ctx context.Context, turn Turn, reason string) error {
	err := o.begin(ctx, func(q *store.Queries, b audit.Batch, src int64) error {
		if err := o.endRow(ctx, q, turn, turn.Reason); err != nil {
			return err
		}
		payload, _ := json.Marshal(map[string]string{"reason": reason})
		if _, err := q.AppendRunEventOnce(ctx, store.AppendRunEventOnceParams{
			OrgID: o.run.OrgID, RunID: o.run.ID, Epoch: o.run.Epoch,
			Kind: "run.requeued", Payload: payload, SourceSeq: src,
		}); err != nil {
			return err
		}
		_, err := q.RequeueRun(ctx, store.RequeueRunParams{OrgID: o.run.OrgID, ID: o.run.ID, Epoch: o.run.Epoch})
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		o.FenceFromDB(ctx)
		if k := o.FenceKind(); k != FenceLost && k != FenceAbandon {
			o.CloseTurn(ctx, turn)
		}
		return ErrFenced
	}
	if err != nil {
		return o.failed(ctx, turn, "requeue the run", err)
	}
	if o.cfg.Metrics != nil {
		o.cfg.Metrics.Requeued()
	}
	o.cfg.Log.Info("run back in the queue", "reason", reason)
	return nil
}

// Release gives the lease up so a worker takes the run over at once. The turn row ends first.
func (o *Owner) Release(ctx context.Context, turn Turn) {
	o.FlushItems(ctx, true)
	o.CloseTurn(ctx, turn)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := o.q.ReleaseRunLease(ctx, store.ReleaseRunLeaseParams{OrgID: o.run.OrgID, ID: o.run.ID, Epoch: o.run.Epoch})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		o.cfg.Log.Warn("releasing the lease failed; it expires on its own", "err", err)
		return
	}
	o.cfg.Log.Info("lease released for a takeover")
}

// Unavailable is why and how long a sandbox never became ready.
type Unavailable struct {
	// Reason is the run's failure reason.
	Reason string
	Cause  execution.SandboxWaitCause
	Detail string
	Waited time.Duration
	// Cap is the org's sandbox wait cap the wait applied, read once; every text about the failure
	// uses it, so they all agree with the run's failure reason.
	Cap time.Duration
	// CauseSeen is how long after the start of the wait the sandbox row first named Cause, when
	// CauseWasSeen. It tells a cause seen early (the cap was only the wait) from one seen just
	// before the cap (issue 573).
	CauseSeen    time.Duration
	CauseWasSeen bool
	// Class is the failure class recorded with the run.
	Class store.RunFailureClass
}

// KindAgentUnhealthy is the task event kind that says an agent was marked unhealthy.
const KindAgentUnhealthy = "agent.unhealthy"

// SandboxUnavailable fails a run whose sandbox never became ready within the org's cap, in one
// transaction: the run's end, the agent's unhealthy mark with its audit event, the notification of
// the org's owners and admins, and a note on every task with a run waiting on the agent (ADR 0066
// amendment 2026-10-28). The agent's other queued runs stop being claimable at the same moment
// (ClaimNextRun skips unhealthy agents), so their tasks must say why they stopped moving.
func (o *Owner) SandboxUnavailable(ctx context.Context, turn Turn, u Unavailable) error {
	err := o.begin(ctx, func(q *store.Queries, b audit.Batch, src int64) error {
		if err := o.endRow(ctx, q, turn, turn.Reason); err != nil {
			return err
		}
		payload := map[string]any{
			"sandboxCause": string(u.Cause), "waitedSeconds": int64(u.Waited.Seconds()), "detail": u.Detail,
		}
		if u.CauseWasSeen {
			payload["causeSeenMs"] = u.CauseSeen.Milliseconds()
		}
		if err := o.finishIn(ctx, q, src, store.RunStateFailed, u.Reason, u.Class, payload); err != nil {
			return err
		}
		a, err := q.GetAgent(ctx, store.GetAgentParams{OrgID: o.run.OrgID, ID: o.run.AgentID})
		if err != nil {
			return err
		}
		reason := fmt.Sprintf("its sandbox never became ready in %s: %s (%s)", runreply.Human(u.Cap), u.Cause.Describe(), u.Detail)
		if _, err := q.MarkAgentUnhealthy(ctx, store.MarkAgentUnhealthyParams{
			OrgID: o.run.OrgID, ID: o.run.AgentID, Reason: reason,
		}); errors.Is(err, pgx.ErrNoRows) {
			return nil // already unhealthy: it was told, audited and noted
		} else if err != nil {
			return err
		}
		b.Org(audit.Event{
			Org: o.run.OrgID, Actor: audit.System("koinon-agentd:run-outcome"), Action: "agent.mark_unhealthy",
			TargetKind: "agent", TargetID: o.run.AgentID.String(),
			After: map[string]any{
				"reason": reason, "cause": "sandbox_unavailable", "sandboxCause": string(u.Cause),
				"runId": o.run.ID.String(), "waitedSeconds": int64(u.Waited.Seconds()),
			},
		})
		// The decision says "the agent's owners". Koinon has no per-agent owner (agents.created_by is
		// only who created it): the people who can act are the ones who can resume an agent, the
		// org's admins and owners.
		message := fmt.Sprintf("%s was marked unhealthy: its sandbox never became ready in %s (%s). Fix the cause, then resume the agent on its page.",
			a.DisplayName, runreply.Human(u.Cap), u.Cause.Describe())
		if err := NotifyUnhealthy(ctx, q, o.run, "agent.unhealthy:"+o.run.AgentID.String()+":sandbox-wait:"+o.run.ID.String(), map[string]string{
			"run_id": o.run.ID.String(), "sandbox_cause": string(u.Cause), "message": message,
		}); err != nil {
			return err
		}
		waiting, err := q.ListTasksWithWaitingRunOfAgent(ctx, store.ListTasksWithWaitingRunOfAgentParams{OrgID: o.run.OrgID, AgentID: o.run.AgentID})
		if err != nil {
			return err
		}
		tasks := map[pgtype.UUID]bool{o.run.TaskID: true}
		for _, t := range waiting {
			tasks[t] = true
		}
		for t := range tasks {
			if err := TaskNote(ctx, q, o.run.OrgID, t, KindAgentUnhealthy,
				message+" Its waiting runs do not start until an admin resumes the agent."); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return o.failed(ctx, turn, "fail the run", err)
	}
	o.finished(store.RunStateFailed)
	if o.cfg.Metrics != nil {
		o.cfg.Metrics.SandboxWaitCap(u.Cause)
	}
	o.cfg.Log.Error("the sandbox never became ready", "cause", u.Cause, "detail", u.Detail, "waited", u.Waited)
	return nil
}

// NotificationAgentUnhealthy is the in-app notification kind for owners and admins.
const NotificationAgentUnhealthy = "agent.unhealthy"

// TaskNote appends a system event with a message to a task's timeline.
func TaskNote(ctx context.Context, q *store.Queries, org, task pgtype.UUID, kind, message string) error {
	payload, err := json.Marshal(map[string]string{"message": message})
	if err != nil {
		return err
	}
	_, err = q.AppendTaskEvent(ctx, store.AppendTaskEventParams{
		OrgID: org, TaskID: task, Kind: kind, ActorKind: store.TaskActorKindSystem, Payload: payload,
	})
	return err
}

// NotifyUnhealthy tells the org's owners and admins, once per dedupe key. The payload carries the
// message and whatever else the notification needs; agent_id is always set.
func NotifyUnhealthy(ctx context.Context, q *store.Queries, run store.Run, dedupe string, fields map[string]string) error {
	users, err := q.ListOrgBudgetRecipients(ctx, run.OrgID)
	if err != nil {
		return err
	}
	payload := map[string]string{}
	for k, v := range fields {
		payload[k] = v
	}
	payload["agent_id"] = run.AgentID.String()
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	for _, u := range users {
		if _, err := q.InsertNotification(ctx, store.InsertNotificationParams{
			OrgID: run.OrgID, UserID: u, Kind: NotificationAgentUnhealthy, DedupeKey: dedupe, Payload: raw,
		}); err != nil {
			return err
		}
	}
	return nil
}
