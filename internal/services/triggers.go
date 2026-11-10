// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"errors"
	"fmt"
	"time"

	connect "connectrpc.com/connect/v2"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/durable"
	agentv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/agent/v1alpha1"
	"github.com/stateracloud/koinon/internal/scheduler"
	"github.com/stateracloud/koinon/internal/triggers"
	"github.com/stateracloud/koinon/templates"
)

// RunScheduleTrigger fires a schedule trigger of the agent's active version now. It is the
// scheduler's fire without the clock: the same trigger lookup, the same CreateTriggeredTask (a task
// created by the trigger, carrying the trigger's instruction, with a queued scheduled-origin run),
// so the agent's status, health, concurrency limits and budgets apply exactly as they do to a
// scheduled fire; a spent budget stops the run where it stops a scheduled one. What differs is
// recorded: the task's events say the fire was manual, and by whom. It needs no DBOS: the task is
// created in the caller's transaction, whether or not the durable runtime is on.
//
// With api.durable.enabled the fire goes through the org's partition of the org queue
// (RunNowQueue), like direct messages, so the org's trigger_concurrency and trigger_rate_per_minute
// apply to it as they do to scheduled fires; with it off the fire is inline.
func (s *Server) RunScheduleTrigger(ctx context.Context, in triggers.RunNow) (store.Task, error) {
	// A held install starts no work: a manual run is an admin starting something (ADR 0107).
	if err := s.hold.Err(); err != nil {
		return store.Task{}, err
	}
	if in.Now.IsZero() {
		in.Now = time.Now()
	}
	if s.runNow != nil {
		return s.runNow.fire(ctx, in)
	}
	return s.fireNow(ctx, in)
}

// fireNow creates the task in the caller's transaction: RunScheduleTrigger's body, and what the
// run-now workflow runs on the queue.
func (s *Server) fireNow(ctx context.Context, in triggers.RunNow) (store.Task, error) {
	now := in.Now
	list, err := triggers.ForAgent(ctx, s.q, in.Org, in.Agent, templates.TriggerSchedule)
	if err != nil {
		return store.Task{}, err
	}
	var tr *triggers.Active
	for i := range list {
		if list[i].ID == in.Trigger {
			tr = &list[i]
		}
	}
	if tr == nil {
		// Not in effect: the agent may be a draft, paused or archived, or the trigger only proposed.
		a, err := s.q.GetAgent(ctx, store.GetAgentParams{OrgID: in.Org, ID: in.Agent})
		if err != nil {
			return store.Task{}, err
		}
		if a.Status != store.AgentStatusActive {
			return store.Task{}, fmt.Errorf("%w: it is %s", triggers.ErrCannotRun, a.Status)
		}
		return store.Task{}, triggers.ErrTriggerNotFound
	}
	_, loc, err := templates.ParseSchedule(tr.Filter.Cron, tr.Filter.Timezone)
	if err != nil {
		loc = time.UTC
	}
	trig := triggers.UUID(in.Agent, in.Trigger)
	task, _, err := s.CreateTriggeredTask(ctx, TriggeredTask{
		Org: in.Org, Agent: in.Agent, Trigger: trig,
		Source: durable.SourceManualTrigger, Key: durable.TriggerFireKey(trig.String(), now),
		Title:   triggers.ManualTaskTitle(in.Trigger, now, loc),
		Message: tr.Instruction,
		// Run now uses the trigger's notify target, not the person who fired it (ADR 0072 amendment
		// 2026-10-28): a test fire behaves like the real one.
		Notify: tr.Notify,
		Origin: store.RunOriginScheduled,
		Fire:   triggers.FireManual, FiredBy: in.User,
		OnCreate: func(ctx context.Context, q *store.Queries, b audit.Batch, t store.Task) error {
			b.Org(audit.Event{
				Org: in.Org, Actor: in.Actor, Action: "agent.trigger.run_now", TargetKind: "agent", TargetID: in.Agent.String(),
				After: map[string]any{"trigger": in.Trigger, "version": tr.Version, "taskId": t.ID.String()},
			})
			return nil
		},
	})
	switch {
	case errors.Is(err, ErrAgentInactive), errors.Is(err, scheduler.ErrAgentNotActive):
		return store.Task{}, fmt.Errorf("%w: it is not active", triggers.ErrCannotRun)
	case errors.Is(err, scheduler.ErrAgentUnhealthy):
		return store.Task{}, fmt.Errorf("%w: %w", triggers.ErrCannotRun, err)
	}
	return task, err
}

// RunAgentTrigger implements AgentService. Admins and owners.
func (s *Server) RunAgentTrigger(ctx context.Context, req *agentv1alpha1.RunAgentTriggerRequest) (*agentv1alpha1.RunAgentTriggerResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	id, err := parseID("agent_id", req.GetAgentId())
	if err != nil {
		return nil, err
	}
	if req.GetTriggerId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, "trigger_id is required")
	}
	task, err := s.RunScheduleTrigger(requestCtx(ctx), triggers.RunNow{
		Org: c.org, Agent: id, Trigger: req.GetTriggerId(), Actor: c.auditActor(), User: c.actor(),
	})
	switch {
	case errors.Is(err, triggers.ErrTriggerNotFound):
		return nil, connect.NewError(connect.CodeNotFound, err.Error())
	case errors.Is(err, triggers.ErrCannotRun):
		return nil, connect.NewError(connect.CodeFailedPrecondition, err.Error())
	case err != nil:
		return nil, s.dbError(err, "agent")
	}
	s.log.Info("schedule trigger fired by hand", "org", c.org.String(), "agent", id.String(), "trigger", req.GetTriggerId(),
		"task", task.ID.String(), "by", c.actor().String())
	return &agentv1alpha1.RunAgentTriggerResponse{TaskId: idString(task.ID)}, nil
}
