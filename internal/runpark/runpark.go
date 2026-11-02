// SPDX-License-Identifier: Apache-2.0

// Package runpark resolves the park of a run that waits on people: the approval service and the
// question service both resume a run through it once nothing it waits for is open. It depends on
// the store alone, so both services, and internal/runoutcome above them, can use it.
package runpark

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
)

// Resolve makes a run parked on approvals or questions (reasons approval and input_required)
// claimable once none of its approval requests is open and no question of it waits for a person,
// and moves its task back from Waiting on you. It locks the run row first; call it in the
// transaction that resolved a request.
func Resolve(ctx context.Context, q *store.Queries, org, runID pgtype.UUID) error {
	run, err := q.LockRun(ctx, store.LockRunParams{OrgID: org, ID: runID})
	if err != nil {
		return err
	}
	reason := run.ParkReason.RunParkReason
	if run.State != store.RunStateParked || run.ReadyAt.Valid ||
		(reason != store.RunParkReasonApproval && reason != store.RunParkReasonInputRequired) {
		// Still running: agentd sees the outcome when it ends the turn.
		return nil
	}
	reqs, err := q.ListUndeliveredApprovalRequestsForRun(ctx, store.ListUndeliveredApprovalRequestsForRunParams{OrgID: org, RunID: runID})
	if err != nil {
		return err
	}
	for _, r := range reqs {
		if r.Status.Open() {
			return nil
		}
	}
	if n, err := q.CountOpenQuestionsForRun(ctx, store.CountOpenQuestionsForRunParams{OrgID: org, RunID: runID}); err != nil || n > 0 {
		return err
	}
	if _, err := q.ResolveRunPark(ctx, store.ResolveRunParkParams{OrgID: org, ID: runID}); err != nil {
		return err
	}
	_, err = q.SetTaskStateFrom(ctx, store.SetTaskStateFromParams{
		OrgID: org, ID: run.TaskID, ToState: store.TaskStateWorking,
		FromStates: []string{string(store.TaskStateInputRequired)},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}
