// SPDX-License-Identifier: Apache-2.0

package agentd

// Parking on ask-tier approvals (plan item P1; ADRs 0030, 0042, 0049) and on ask_user questions
// (plan item TK3; ADR 0065).
//
// An ask-tier tool call is recorded by the tool server, which answers "approval requested"
// at once; the harness ends its turn. agentd then decides, under the run's row lock, whether
// the run is done or must wait: if the run has approval requests whose outcome the agent has
// not been sent, the run parks (reason approval) instead of finishing, the task moves to
// Waiting on you, and the worker lets the harness go. Koinon's approval service resolves the
// park once every request is decided and executed; a worker then claims the run, loads the
// session and sends the outcomes as the next prompt.
//
// A question the agent asked with ask_user parks the run the same way (reason input_required): the
// turn's end opens the run's asked questions, so they appear in their addressees' inboxes, in the
// same transaction that parks the run and moves the task to input-required. An answer, a dismissal
// or an expiry resolves the park (package questions) and the outcome is the next prompt, with the
// safe default named when it was applied.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/questions"
	"github.com/stateracloud/koinon/internal/runreply"
)

// kindParked is the run and task event of a run parking on approvals.
const kindParked = runreply.KindParked

// parkResumeNote opens the prompt of a run resumed after its approvals were decided.
const parkResumeNote = "[Koinon] Your run was paused while a human decided on your approval requests, " +
	"and has now resumed. The outcomes:"

// outcomes returns the decided requests of the run the agent has not been sent yet, as text
// for the prompt, and remembers them so the turn's end marks them delivered.
func (r *runner) outcomes(ctx context.Context) (string, error) {
	reqs, err := r.q.ListUndeliveredApprovalRequestsForRun(ctx, store.ListUndeliveredApprovalRequestsForRunParams{
		OrgID: r.run.OrgID, RunID: r.run.ID,
	})
	if err != nil {
		return "", err
	}
	qs, err := r.q.ListUndeliveredQuestionsForRun(ctx, store.ListUndeliveredQuestionsForRunParams{
		OrgID: r.run.OrgID, RunID: r.run.ID,
	})
	if err != nil {
		return "", err
	}
	text, deliveredQ, delivered := renderOutcomes(qs, reqs)
	r.delivered, r.deliveredQ = delivered, deliveredQ
	return text, nil
}

// renderOutcomes is the pure part of outcomes: the prompt text for the resolved questions and
// decided requests, and the IDs of the ones it covers. It reads no clock and makes no IDs, so its
// output is a function of the rows (golden files in testdata).
func renderOutcomes(qs []store.Question, reqs []store.ApprovalRequest) (text string, deliveredQ, delivered []pgtype.UUID) {
	var b strings.Builder
	noAnswer := false
	for _, row := range qs {
		if row.State == store.QuestionStateAsked || row.State == store.QuestionStateOpen {
			continue
		}
		noAnswer = noAnswer || row.State == store.QuestionStateExpired
		deliveredQ = append(deliveredQ, row.ID)
		fmt.Fprintf(&b, "\n- ask_user (question id=%s): %q: %s", row.ID.String(), row.Question, questionOutcomeText(row))
	}
	for _, a := range reqs {
		if a.Status.Open() {
			continue
		}
		delivered = append(delivered, a.ID)
		fmt.Fprintf(&b, "\n- %s (approval id=%s, %s): %s", a.Tool, a.ID.String(), a.Target, outcomeText(a))
	}
	if b.Len() == 0 {
		return "", deliveredQ, delivered
	}
	b.WriteString("\nThese outcomes are final: do not call the tools again for them. Continue the task from here, " +
		"and tell the user what happened.")
	if noAnswer {
		b.WriteString(noAnswerNote)
	}
	return b.String(), deliveredQ, delivered
}

// noAnswerNote is the system instruction of the "no answer" run (ADR 0065 decision 10, plan item
// TK4): a question expired, Koinon applied its default, and this is the one short run the agent gets
// to react.
const noAnswerNote = "\n\n[Koinon system] This is a short \"no answer\" run: at least one question above expired " +
	"because nobody answered, and Koinon applied its default. Decide now and keep it brief. Either carry on with " +
	"the default, cancel the task with cancel_task (give the reason), or ask someone else with ask_user (not the " +
	"same person about the same thing). This question will not come back."

// questionOutcomeText describes one resolved question to the agent. A default that was applied
// because the question was dismissed or expired is said so (ADR 0065 decision 10a).
func questionOutcomeText(row store.Question) string {
	switch row.State {
	case store.QuestionStateDismissed:
		return fmt.Sprintf("the person dismissed the question, so your default was applied: %q. Treat that as the answer.", row.Answer.String)
	case store.QuestionStateExpired:
		return fmt.Sprintf("nobody answered in time, so your default was applied: %q. Treat that as the answer.", row.Answer.String)
	default:
		return fmt.Sprintf("the person answered: %q", row.Answer.String)
	}
}

// outcomeText describes one decided request to the agent.
func outcomeText(a store.ApprovalRequest) string {
	switch a.Status {
	case store.ApprovalStatusExecuted:
		var res map[string]any
		_ = json.Unmarshal(a.Result, &res)
		if url, _ := res["url"].(string); url != "" {
			return "approved and executed by Koinon: " + url
		}
		return "approved and executed by Koinon: " + string(a.Result)
	case store.ApprovalStatusFailed:
		return "approved, but executing it failed: " + a.Error.String
	case store.ApprovalStatusDenied:
		if note := strings.TrimSpace(a.DecisionNote.String); note != "" {
			return "denied by the approver, who wrote: " + note
		}
		return "denied by the approver (no note)"
	case store.ApprovalStatusExpired:
		return "expired without a decision"
	default:
		return string(a.Status)
	}
}

// endTurn ends a turn that stopped normally: the run succeeds, or parks while approval
// requests the agent made are open or their outcomes unsent. Both happen in one transaction
// under the run's row lock, which orders the decision against the approval service's
// resolution of the park.
func (r *runner) endTurn(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.srcSeq++
	src := r.srcSeq
	parked, waiting := false, false
	var parkReason store.RunParkReason
	err := pgx.BeginFunc(ctx, r.p.db, func(tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		if len(r.delivered) > 0 {
			if _, err := q.MarkApprovalRequestsDelivered(ctx, store.MarkApprovalRequestsDeliveredParams{
				OrgID: r.run.OrgID, Ids: r.delivered,
			}); err != nil {
				return err
			}
		}
		if len(r.deliveredQ) > 0 {
			if _, err := q.MarkQuestionsDelivered(ctx, store.MarkQuestionsDeliveredParams{
				OrgID: r.run.OrgID, Ids: r.deliveredQ,
			}); err != nil {
				return err
			}
		}
		run, err := q.LockRun(ctx, store.LockRunParams{OrgID: r.run.OrgID, ID: r.run.ID})
		if err != nil {
			return err
		}
		if run.Epoch != r.run.Epoch || run.State != store.RunStateRunning {
			return pgx.ErrNoRows // fenced
		}
		// The agent canceled the task in this turn (cancel_task): there is nothing to wait for. The
		// questions it asked are canceled instead of opened, and the approvals are canceled by the
		// approvals sweep once the run has ended.
		task, err := q.GetTask(ctx, store.GetTaskParams{OrgID: r.run.OrgID, ID: r.run.TaskID})
		if err != nil {
			return err
		}
		if task.State == store.TaskStateCanceled {
			if _, err := questions.CancelForRun(ctx, q, r.run.OrgID, r.run.ID, "the task was canceled"); err != nil {
				return err
			}
			return r.finishIn(ctx, q, src, store.RunStateSucceeded, "", "", nil)
		}
		reqs, err := q.ListUndeliveredApprovalRequestsForRun(ctx, store.ListUndeliveredApprovalRequestsForRunParams{
			OrgID: r.run.OrgID, RunID: r.run.ID,
		})
		if err != nil {
			return err
		}
		qs, err := q.ListUndeliveredQuestionsForRun(ctx, store.ListUndeliveredQuestionsForRunParams{
			OrgID: r.run.OrgID, RunID: r.run.ID,
		})
		if err != nil {
			return err
		}
		if len(reqs) == 0 && len(qs) == 0 {
			return r.finishIn(ctx, q, src, store.RunStateSucceeded, "", "", nil)
		}
		parked = true
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
		waiting = asked
		for _, a := range reqs {
			ids = append(ids, a.ID.String())
			if a.Status == store.ApprovalStatusPending {
				waiting = true
				if !expires.Valid || a.ExpiresAt.Time.Before(expires.Time) {
					expires = a.ExpiresAt
				}
			}
		}
		if !expires.Valid {
			// Nothing waits for a human; the park is resolved below or by the executor.
			expires = pgtype.Timestamptz{Time: r.p.cfg.Now().Add(7 * 24 * time.Hour), Valid: true}
		}
		// What the agent said before it parked goes on the task now, before the park note
		// (and before run.parked, after which reply starts afresh).
		if err := r.postReply(ctx, q); err != nil {
			return err
		}
		reason := store.RunParkReasonApproval
		if asked {
			reason = store.RunParkReasonInputRequired
		}
		parkReason = reason
		qids := make([]string, 0, len(qs))
		for _, row := range qs {
			qids = append(qids, row.ID.String())
		}
		payload, _ := json.Marshal(map[string]any{"reason": string(reason), "approvals": ids, "questions": qids})
		if _, err := q.AppendRunEventOnce(ctx, store.AppendRunEventOnceParams{
			OrgID: r.run.OrgID, RunID: r.run.ID, Epoch: r.run.Epoch, Kind: kindParked, Payload: payload, SourceSeq: src,
		}); err != nil {
			return err
		}
		if _, err := q.ParkRun(ctx, store.ParkRunParams{
			OrgID: r.run.OrgID, ID: r.run.ID, Epoch: r.run.Epoch,
			ParkReason: reason, ParkExpiresAt: expires,
		}); err != nil {
			return err
		}
		if asked {
			if _, err := questions.OpenForRun(ctx, q, r.run.OrgID, r.run.ID, r.run.TaskID); err != nil {
				return err
			}
		}
		if _, err := q.AppendTaskEvent(ctx, store.AppendTaskEventParams{
			OrgID: r.run.OrgID, TaskID: r.run.TaskID, RunID: r.run.ID,
			Kind: kindParked, ActorKind: store.TaskActorKindSystem, Payload: payload,
		}); err != nil {
			return err
		}
		if waiting {
			_, err := q.SetTaskStateFrom(ctx, store.SetTaskStateFromParams{
				OrgID: r.run.OrgID, ID: r.run.TaskID, ToState: store.TaskStateInputRequired,
				FromStates: []string{string(store.TaskStateSubmitted), string(store.TaskStateWorking)},
			})
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			return nil
		}
		for _, a := range reqs {
			if a.Status.Open() {
				return nil // approved, being executed: the executor resolves the park
			}
		}
		// Every request and question was decided while the turn ran: resume at once.
		_, err = q.ResolveRunPark(ctx, store.ResolveRunParkParams{OrgID: r.run.OrgID, ID: r.run.ID})
		return err
	})
	if err != nil {
		return r.dbErr(ctx, "end the turn", err)
	}
	if parked {
		r.tr.parked(parkReason)
		r.p.cfg.Metrics.Finished.WithLabelValues(string(store.RunStateParked)).Inc()
		r.log.Info("run parked on approvals", "waiting_on_human", waiting)
	} else {
		r.p.cfg.Metrics.Finished.WithLabelValues(string(store.RunStateSucceeded)).Inc()
		r.log.Info("run finished", "outcome", store.RunStateSucceeded)
	}
	return nil
}
