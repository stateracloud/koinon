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
)

// kindParked is the run and task event of a run parking on approvals.
const kindParked = "run.parked"

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
	r.delivered, r.deliveredQ = r.delivered[:0], r.deliveredQ[:0]
	var b strings.Builder
	for _, row := range qs {
		if row.State == store.QuestionStateAsked || row.State == store.QuestionStateOpen {
			continue
		}
		r.deliveredQ = append(r.deliveredQ, row.ID)
		fmt.Fprintf(&b, "\n- ask_user (question id=%s): %q: %s", row.ID.String(), row.Question, questionOutcomeText(row))
	}
	for _, a := range reqs {
		if a.Status.Open() {
			continue
		}
		r.delivered = append(r.delivered, a.ID)
		fmt.Fprintf(&b, "\n- %s (approval id=%s, %s): %s", a.Tool, a.ID.String(), a.Target, outcomeText(a))
	}
	if b.Len() == 0 {
		return "", nil
	}
	b.WriteString("\nThese outcomes are final: do not call the tools again for them. Continue the task from here, " +
		"and tell the user what happened.")
	return b.String(), nil
}

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
			return r.finishIn(ctx, q, src, store.RunStateSucceeded, "", "")
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
			expires = pgtype.Timestamptz{Time: time.Now().Add(7 * 24 * time.Hour), Valid: true}
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
		r.p.cfg.Metrics.Finished.WithLabelValues(string(store.RunStateParked)).Inc()
		r.log.Info("run parked on approvals", "waiting_on_human", waiting)
	} else {
		r.p.cfg.Metrics.Finished.WithLabelValues(string(store.RunStateSucceeded)).Inc()
		r.log.Info("run finished", "outcome", store.RunStateSucceeded)
	}
	return nil
}
