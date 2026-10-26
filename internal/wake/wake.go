// SPDX-License-Identifier: Apache-2.0

// Package wake carries the pre-wake signals of plan item LT1 (practice B5): events that say work
// for an agent is about to arrive, before a run exists to say so.
//
// Activity, as the reconciler counts it, is the latest of
//   - the sandbox's last start,
//   - the last run of the agent that finished or parked,
//   - the last pre-wake signal (this package),
//
// and an active or claimable run, which keeps the sandbox up for as long as it lasts. A sandbox
// stops once IdleTimeout has passed since its last activity; one whose run parked on a2a or
// input_required stays up for the longer ParkWarm window from the park (internal/orgsettings).
// A signal on a stopped sandbox starts it; on a running one it restarts the idle clock.
//
// The signals:
//   - TaskCreated: a task was created for the agent (a run follows, but the sandbox can start
//     before the run is queued and claimed);
//   - Typing: a person is typing in the task's conversation, so the sandbox is up when they send;
//   - CronDue: a schedule is about to fire; TR1 calls it a lead time (with jitter) ahead.
package wake

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
)

// Agent signals that work for the agent is about to arrive. It reports whether the agent has a
// sandbox record to wake; an agent without one gets its sandbox created, ready, on activation.
func Agent(ctx context.Context, q *store.Queries, org, agent pgtype.UUID) (bool, error) {
	_, err := q.WakeAgentSandbox(ctx, store.WakeAgentSandboxParams{OrgID: org, AgentID: agent})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("wake agent sandbox: %w", err)
	}
	// The write is not a runs or agents change, so no trigger wakes the reconciler for it.
	if err := q.NotifyReconciler(ctx, org); err != nil {
		return true, fmt.Errorf("notify reconciler: %w", err)
	}
	return true, nil
}

// TaskCreated is the signal for a new task assigned to an agent.
func TaskCreated(ctx context.Context, q *store.Queries, task store.Task) (bool, error) {
	if !task.AgentID.Valid {
		return false, nil
	}
	return Agent(ctx, q, task.OrgID, task.AgentID)
}

// Typing is the signal for a person typing in a task's conversation: the API hook the web UI and
// chat connectors call (throttled by the caller).
func Typing(ctx context.Context, q *store.Queries, org, taskID pgtype.UUID) (bool, error) {
	task, err := q.GetTask(ctx, store.GetTaskParams{OrgID: org, ID: taskID})
	if err != nil {
		return false, fmt.Errorf("load task: %w", err)
	}
	return TaskCreated(ctx, q, task)
}

// CronDue is the signal for a schedule about to fire for the agent; TR1 calls it a lead time
// ahead of the fire time, with jitter.
func CronDue(ctx context.Context, q *store.Queries, org, agent pgtype.UUID) (bool, error) {
	return Agent(ctx, q, org, agent)
}
