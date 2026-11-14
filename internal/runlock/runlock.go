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
// Lock order. The rule is run rows, then the task row, and not every path keeps it yet:
//
//   - Enqueue (scheduler.Enqueue) locks the task row only; it inserts a new run row.
//   - Owner.Requeue locks its own run (LockRun), then the task row, then the queued run of the task
//     with SKIP LOCKED (CarrySandboxWaitToQueued). It never waits for the queued run after the
//     task lock.
//   - The task-first paths (setState, cancel_task, ContinueAfterTimeLimit) lock the task row, then
//     touch only queued runs (CancelQueuedForTask).
//   - A statement that locks more than one run row sorts by id (CancelOpenRunsForAgent,
//     CancelRunsOverWallClock, ClearSandboxWaitForAgent).
//
// Some older run-first cancel paths (archive, CancelUnowned on a queued run, CancelAtLimit) lock a
// run row, which can be a queued run, and then the task row. They can still meet a task-first path
// in a cycle (queued run, then task, against task, then queued run). That is not fixed here; it is
// tracked in issue #1015. So there is no claim here that the order is free of cycles.
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
