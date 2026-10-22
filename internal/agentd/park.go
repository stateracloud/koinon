// SPDX-License-Identifier: Apache-2.0

package agentd

// Parking on ask-tier approvals (plan item P1; ADRs 0030, 0042, 0049).
//
// An ask-tier tool call is recorded by the tool server, which answers "approval requested"
// at once; the harness ends its turn. agentd then decides, under the run's row lock, whether
// the run is done or must wait: if the run has approval requests whose outcome the agent has
// not been sent, the run parks (reason approval) instead of finishing, the ticket moves to
// Waiting on you, and the worker lets the harness go. Koinon's approval service resolves the
// park once every request is decided and executed; a worker then claims the run, loads the
// session and sends the outcomes as the next prompt.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/procoduck/koinon/internal/db/store"
)

// kindParked is the run and ticket event of a run parking on approvals.
const kindParked = "run.parked"

// parkResumeNote opens the prompt of a run resumed after its approvals were decided.
const parkResumeNote = "[Koinon] Your run was paused while a human decided on your approval requests, " +
	"and has now resumed. The outcomes:"

func openApproval(s store.ApprovalStatus) bool {
	return s == store.ApprovalStatusPending || s == store.ApprovalStatusApproved
}

// outcomes returns the decided requests of the run the agent has not been sent yet, as text
// for the prompt, and remembers them so the turn's end marks them delivered.
func (r *runner) outcomes(ctx context.Context) (string, error) {
	reqs, err := r.q.ListUndeliveredApprovalRequestsForRun(ctx, store.ListUndeliveredApprovalRequestsForRunParams{
		OrgID: r.run.OrgID, RunID: r.run.ID,
	})
	if err != nil {
		return "", err
	}
	r.delivered = r.delivered[:0]
	var b strings.Builder
	for _, a := range reqs {
		if openApproval(a.Status) {
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
		if len(reqs) == 0 {
			return r.finishIn(ctx, q, src, store.RunStateSucceeded, "")
		}
		parked = true
		ids := make([]string, 0, len(reqs))
		var expires pgtype.Timestamptz
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
		// What the agent said before it parked goes on the ticket now, before the park note
		// (and before run.parked, after which reply starts afresh).
		if err := r.postReply(ctx, q); err != nil {
			return err
		}
		payload, _ := json.Marshal(map[string]any{"reason": string(store.RunParkReasonApproval), "approvals": ids})
		if _, err := q.AppendRunEventOnce(ctx, store.AppendRunEventOnceParams{
			OrgID: r.run.OrgID, RunID: r.run.ID, Epoch: r.run.Epoch, Kind: kindParked, Payload: payload, SourceSeq: src,
		}); err != nil {
			return err
		}
		if _, err := q.ParkRun(ctx, store.ParkRunParams{
			OrgID: r.run.OrgID, ID: r.run.ID, Epoch: r.run.Epoch,
			ParkReason: store.RunParkReasonApproval, ParkExpiresAt: expires,
		}); err != nil {
			return err
		}
		if _, err := q.AppendTicketEvent(ctx, store.AppendTicketEventParams{
			OrgID: r.run.OrgID, TicketID: r.run.TicketID, RunID: r.run.ID,
			Kind: kindParked, ActorKind: store.TicketActorKindSystem, Payload: payload,
		}); err != nil {
			return err
		}
		if waiting {
			_, err := q.MoveTicketFrom(ctx, store.MoveTicketFromParams{
				OrgID: r.run.OrgID, ID: r.run.TicketID, ToStatus: store.TicketStatusWaitingOnYou,
				FromStatuses: []string{string(store.TicketStatusTodo), string(store.TicketStatusInProgress)},
			})
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			return nil
		}
		for _, a := range reqs {
			if openApproval(a.Status) {
				return nil // approved, being executed: the executor resolves the park
			}
		}
		// Every request was decided while the turn ran: resume at once.
		_, err = q.ResolveRunPark(ctx, store.ResolveRunParkParams{OrgID: r.run.OrgID, ID: r.run.ID})
		return err
	})
	if err != nil {
		return r.dbErr(ctx, "end the turn", err)
	}
	if parked {
		r.log.Info("run parked on approvals", "waiting_on_human", waiting)
	} else {
		r.log.Info("run finished", "outcome", store.RunStateSucceeded)
	}
	return nil
}
