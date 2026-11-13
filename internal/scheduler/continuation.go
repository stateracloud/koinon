// SPDX-License-Identifier: Apache-2.0

package scheduler

// Continuation runs after a time cap (issue #99, ADR 0081 amendment 2026-11-12).
//
// A run that ends at its wall-clock cap is followed by a new run of the same task, a continuation,
// while the task has had fewer than its maximum of them (the template's, or the agent's override;
// default 3) and has spent less than its USD cap (the org's default, or the agent's override). When
// either is reached the task goes to input_required, "blocked on you", with the reason in the
// status.changed event. A continuation count of 0 switches the whole thing off for the agent and
// leaves the task as a cancel leaves it.
//
// Both writers of a time-cap cancel call ContinueAfterTimeLimit in the cancel's transaction (the
// run's owner, agentd, through runoutcome.Config.AfterTimeLimit, and the sweep), so a task never
// has a canceled run and neither a continuation nor a blocked state.

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/runlimits"
)

// Reasons a task is blocked instead of continued; the "reason" of the status.changed event.
const (
	ReasonContinuationLimit = "continuation_limit"
	ReasonTaskSpendCap      = "task_spend_cap"
	ReasonContinuationAgent = "continuation_agent_unavailable"
)

const kindMessage = "message"

// ContinueAfterTimeLimit decides what follows a run that was canceled at its time limit, in the
// transaction of q: a queued continuation run, a blocked task, or nothing. It is a
// runoutcome.RunStep.
func ContinueAfterTimeLimit(ctx context.Context, q *store.Queries, run store.Run) error {
	task, err := q.GetTaskForUpdate(ctx, store.GetTaskForUpdateParams{OrgID: run.OrgID, ID: run.TaskID})
	if err != nil {
		return fmt.Errorf("load the task: %w", err)
	}
	switch task.State {
	case store.TaskStateCompleted, store.TaskStateCanceled, store.TaskStateFailed:
		return nil
	}
	if queued, err := q.HasQueuedRunForTask(ctx, store.HasQueuedRunForTaskParams{OrgID: run.OrgID, TaskID: run.TaskID}); err != nil {
		return err
	} else if queued {
		return nil // a follow-up message already asked for the next run
	}
	if !task.AgentID.Valid || task.AgentID != run.AgentID {
		return nil // reassigned: the cap belonged to the run of another agent
	}
	agent, err := q.GetAgent(ctx, store.GetAgentParams{OrgID: run.OrgID, ID: task.AgentID})
	if err != nil {
		return fmt.Errorf("load the agent: %w", err)
	}
	cfg := agentconfig.Config{}
	if agent.ActiveVersionID.Valid {
		v, err := q.GetAgentVersion(ctx, store.GetAgentVersionParams{OrgID: run.OrgID, ID: agent.ActiveVersionID})
		if err != nil {
			return fmt.Errorf("load the agent version: %w", err)
		}
		if cfg, err = agentconfig.Parse(v.Config); err != nil {
			return err
		}
	}
	pol, err := runlimits.ContinuationFor(ctx, q, run.OrgID, agent.ID, cfg)
	if err != nil {
		return err
	}
	if pol.Max <= 0 {
		return nil // continuation is off for this agent
	}
	spent, err := runlimits.ReadTaskSpend(ctx, q, run.OrgID, run.TaskID)
	if err != nil {
		return fmt.Errorf("read the task's spend: %w", err)
	}
	detail := map[string]any{
		"continuations": task.ContinuationCount, "max_continuations": pol.Max,
		"task_spent_usd": spent.String(), "task_spend_cap_usd": pol.TaskSpend.String(),
	}
	switch {
	case int(task.ContinuationCount) >= pol.Max:
		return blockTask(ctx, q, task, ReasonContinuationLimit, detail)
	case spent >= pol.TaskSpend:
		return blockTask(ctx, q, task, ReasonTaskSpendCap, detail)
	}
	origin := store.RunOriginInteractive
	if run.Origin != "" {
		origin = run.Origin
	}
	queued, err := queueContinuation(ctx, q, task, origin)
	switch {
	case errors.Is(err, ErrAgentNotActive), errors.Is(err, ErrAgentUnhealthy):
		return blockTask(ctx, q, task, ReasonContinuationAgent, map[string]any{"error": err.Error()})
	case err != nil:
		return err
	case !queued:
		return nil
	}
	n, err := q.BumpTaskContinuation(ctx, store.BumpTaskContinuationParams{OrgID: run.OrgID, ID: run.TaskID, Max: int32(pol.Max)}) //nolint:gosec // bounded by the template schema and the database's CHECK
	if err != nil {
		return fmt.Errorf("count the continuation: %w", err)
	}
	_, err = q.AppendTaskEvent(ctx, store.AppendTaskEventParams{
		OrgID: run.OrgID, TaskID: run.TaskID, RunID: run.ID, Kind: kindMessage, ActorKind: store.TaskActorKindSystem,
		Payload: mustJSON(map[string]any{
			"source":       "continuation",
			"continuation": n.ContinuationCount,
			"of":           pol.Max,
			"text": fmt.Sprintf("Your previous run stopped at its time limit. This is continuation %d of %d: "+
				"continue the task where you left off, and wrap up when you are done or cannot go further.",
				n.ContinuationCount, pol.Max),
		}),
	})
	return err
}

// queueContinuation queues the continuation run and says whether it did. The count and the message
// are written after it, in the same transaction, so a refusal leaves neither behind.
func queueContinuation(ctx context.Context, q *store.Queries, task store.Task, origin store.RunOrigin) (bool, error) {
	_, queued, err := EnqueueOrigin(ctx, q, task, origin)
	return queued, err
}

// blockTask moves the task to input_required, blocked on its owner, with the reason on its
// timeline. A task a person moved elsewhere in the meantime (to an end state) stays where it is. One that is
// input_required already (a person's message queued the run that just hit its cap) is blocked again.
func blockTask(ctx context.Context, q *store.Queries, task store.Task, reason string, detail map[string]any) error {
	t, err := q.SetTaskStateFrom(ctx, store.SetTaskStateFromParams{
		OrgID: task.OrgID, ID: task.ID, ToState: store.TaskStateInputRequired,
		FromStates: []string{string(store.TaskStateSubmitted), string(store.TaskStateWorking), string(store.TaskStateInputRequired)},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	p := map[string]any{"from": string(task.State), "to": string(t.State), "reason": reason}
	for k, v := range detail {
		p[k] = v
	}
	_, err = q.AppendTaskEvent(ctx, store.AppendTaskEventParams{
		OrgID: task.OrgID, TaskID: task.ID, Kind: "status.changed", ActorKind: store.TaskActorKindSystem, Payload: mustJSON(p),
	})
	return err
}
