// SPDX-License-Identifier: Apache-2.0

// Package runlock holds the row-lock helper of the paths that queue or end runs. It is a package
// of its own so that scheduler and runoutcome (which scheduler imports) share it.
package runlock

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
)

// LockTask takes the task's row lock and returns the task as it is now. It is the step of every
// path that queues a run of the task (scheduler.Enqueue) or ends a run (runoutcome.Owner.Requeue)
// that takes the task lock. The lock lasts to the end of the transaction, so the caller must be in
// one: LockTask checks that, and returns an error when the lock did not outlast the statement. It
// asserts only the transaction. It does not check the lock order, and it cannot: nothing here knows
// which other locks the transaction holds.
//
// Lock order, kept by every caller (there is no runtime check):
//
//  1. run rows, in id order. A statement that locks more than one run row sorts by id
//     (LockRunsForRequeue, CancelOpenRunsForAgent, CancelRunsOverWallClock,
//     ClearSandboxWaitForAgent).
//  2. then the task row (LockTask, or the task event a cancel appends).
//
// An enqueue path locks the task row and no existing run row (it inserts a new one). Owner.Requeue
// locks its run and the queued run of the task (LockRunsForRequeue), then the task, so the update
// of the queued run after the task lock waits for nothing. Because the run rows come first, in one
// global order, and the task row last, no two paths wait on each other in a cycle.
func LockTask(ctx context.Context, q *store.Queries, org, id pgtype.UUID) (store.Task, error) {
	task, err := q.GetTaskForUpdate(ctx, store.GetTaskForUpdateParams{OrgID: org, ID: id})
	if err != nil {
		return store.Task{}, fmt.Errorf("lock task: %w", err)
	}
	held, err := q.TaskLockHeld(ctx, store.TaskLockHeldParams{OrgID: org, ID: id})
	if err != nil {
		return store.Task{}, fmt.Errorf("check the task lock: %w", err)
	}
	if !held.Bool {
		return store.Task{}, errors.New("runlock: the task lock is held outside a transaction; queue runs inside one")
	}
	return task, nil
}
