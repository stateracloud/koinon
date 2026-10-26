// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"unicode/utf8"

	connect "connectrpc.com/connect/v2"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
	runv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/run/v1alpha1"
	taskv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/task/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/task/v1alpha1/taskv1alpha1connect"
	"github.com/stateracloud/koinon/internal/scheduler"
)

// Task event kinds written by the API.
const (
	KindTaskCreated   = "task.created"
	KindMessage       = "message"
	KindStatusChanged = "status.changed"
)

const (
	maxMessageBytes  = 64 << 10
	maxTimelineItems = 10_000
)

func checkMessage(text string, required bool) (string, error) {
	if strings.TrimSpace(text) == "" {
		if required {
			return "", connect.NewError(connect.CodeInvalidArgument, "text is required")
		}
		return "", nil
	}
	if len(text) > maxMessageBytes {
		return "", connect.NewError(connect.CodeInvalidArgument, "message is longer than 64 KiB")
	}
	if !utf8.ValidString(text) {
		return "", connect.NewError(connect.CodeInvalidArgument, "message is not valid UTF-8")
	}
	return text, nil
}

func jsonPayload(v map[string]any) []byte {
	b, _ := json.Marshal(v) // plain values only
	return b
}

// endStates are the states a task ends in.
var endStates = []store.TaskState{store.TaskStateCompleted, store.TaskStateCanceled, store.TaskStateFailed}

func isEnded(s store.TaskState) bool { return slices.Contains(endStates, s) }

// errTaskEnded means the task has ended and takes no messages.
var errTaskEnded = errors.New("task has ended")

// enqueue maps scheduler errors for the API.
func enqueue(ctx context.Context, q *store.Queries, t store.Task) (*runv1alpha1.Run, error) {
	run, queued, err := scheduler.Enqueue(ctx, q, t)
	if errors.Is(err, scheduler.ErrAgentNotActive) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, "the task's agent is not active")
	}
	if errors.Is(err, scheduler.ErrAgentUnhealthy) {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			"the task's agent is unhealthy and takes no new runs until an admin resumes it ("+strings.TrimPrefix(err.Error(), scheduler.ErrAgentUnhealthy.Error()+": ")+")")
	}
	if err != nil || !queued {
		return nil, err
	}
	return runProto(run), nil
}

// createTask creates a task owned by an active agent, created by the caller, with its first
// message and a queued run. It is what both TaskService and the ticket compatibility layer call.
func (s *Server) createTask(ctx context.Context, c caller, agentID pgtype.UUID, title, msg string) (store.Task, *runv1alpha1.Run, error) {
	var (
		task store.Task
		run  *runv1alpha1.Run
	)
	err := s.inTx(ctx, func(q *store.Queries) error {
		agent, err := q.GetAgent(ctx, store.GetAgentParams{OrgID: c.org, ID: agentID})
		if err != nil {
			return err
		}
		if agent.Status != store.AgentStatusActive {
			return connect.NewError(connect.CodeFailedPrecondition, "agent is not active")
		}
		t, err := q.CreateTask(ctx, store.CreateTaskParams{
			OrgID: c.org, Title: title, AgentID: agent.ID,
			CreatedByKind: store.TaskCreatorKindUser, CreatedByUserID: c.actor(),
		})
		if err != nil {
			return err
		}
		if _, err := q.AppendTaskEvent(ctx, store.AppendTaskEventParams{
			OrgID: c.org, TaskID: t.ID, Kind: KindTaskCreated,
			ActorKind: store.TaskActorKindUser, ActorUserID: c.actor(),
			Payload: jsonPayload(map[string]any{"title": title}),
		}); err != nil {
			return err
		}
		if msg != "" {
			if _, err := q.AppendTaskEvent(ctx, store.AppendTaskEventParams{
				OrgID: c.org, TaskID: t.ID, Kind: KindMessage,
				ActorKind: store.TaskActorKindUser, ActorUserID: c.actor(),
				Payload: jsonPayload(map[string]any{"text": msg}),
			}); err != nil {
				return err
			}
		}
		if run, err = enqueue(ctx, q, t); err != nil {
			return err
		}
		task, err = q.GetTask(ctx, store.GetTaskParams{OrgID: c.org, ID: t.ID})
		return err
	})
	return task, run, err
}

// taskView is a task with its timeline and runs, read in one snapshot.
type taskView struct {
	task   store.Task
	events []store.TaskEvent
	runs   []*runv1alpha1.Run
	cost   *runv1alpha1.Cost
}

func (s *Server) loadTask(ctx context.Context, c caller, id pgtype.UUID) (taskView, error) {
	var v taskView
	err := s.inSnapshot(ctx, func(q *store.Queries) error {
		t, err := q.GetTask(ctx, store.GetTaskParams{OrgID: c.org, ID: id})
		if err != nil {
			return err
		}
		v.task = t
		if v.events, err = q.ListTaskEvents(ctx, store.ListTaskEventsParams{
			OrgID: c.org, TaskID: id, AfterSeq: 0, MaxEvents: maxTimelineItems,
		}); err != nil {
			return err
		}
		runs, err := q.ListRunsForTask(ctx, store.ListRunsForTaskParams{OrgID: c.org, TaskID: id})
		if err != nil {
			return err
		}
		v.runs = runsProto(runs)
		if err := setRunCosts(ctx, q, c.org, v.runs); err != nil {
			return err
		}
		costs, err := sumTaskCosts(ctx, q, c.org, []pgtype.UUID{id})
		v.cost = costs[idString(id)]
		return err
	})
	return v, err
}

// listTasks lists the org's tasks, in any of states if given, with their costs by task ID.
func (s *Server) listTasks(ctx context.Context, c caller, states []store.TaskState) ([]store.Task, map[string]*runv1alpha1.Cost, error) {
	var (
		tasks []store.Task
		err   error
	)
	if len(states) == 0 {
		tasks, err = s.q.ListTasks(ctx, c.org)
	} else {
		names := make([]string, len(states))
		for i, st := range states {
			names[i] = string(st)
		}
		tasks, err = s.q.ListTasksByStates(ctx, store.ListTasksByStatesParams{OrgID: c.org, States: names})
	}
	if err != nil {
		return nil, nil, err
	}
	ids := make([]pgtype.UUID, 0, len(tasks))
	for _, t := range tasks {
		ids = append(ids, t.ID)
	}
	costs, err := sumTaskCosts(ctx, s.q, c.org, ids)
	return tasks, costs, err
}

// appendMessage adds a user message to a task and queues a run unless one is queued already.
func (s *Server) appendMessage(ctx context.Context, c caller, id pgtype.UUID, msg string) (store.TaskEvent, *runv1alpha1.Run, error) {
	var (
		ev  store.TaskEvent
		run *runv1alpha1.Run
	)
	err := s.inTx(ctx, func(q *store.Queries) error {
		// The row lock serialises this with concurrent messages (and so the queued-run check in
		// Enqueue) and with setState, so a task that ends concurrently takes no message.
		t, err := q.GetTaskForUpdate(ctx, store.GetTaskForUpdateParams{OrgID: c.org, ID: id})
		if err != nil {
			return err
		}
		if isEnded(t.State) {
			return errTaskEnded
		}
		ev, err = q.AppendTaskEvent(ctx, store.AppendTaskEventParams{
			OrgID: c.org, TaskID: t.ID, Kind: KindMessage,
			ActorKind: store.TaskActorKindUser, ActorUserID: c.actor(),
			Payload: jsonPayload(map[string]any{"text": msg}),
		})
		if err != nil {
			return err
		}
		run, err = enqueue(ctx, q, t)
		return err
	})
	return ev, run, err
}

// setState moves a task to a state on behalf of the caller and records the change on the timeline.
// Moving to an end state cancels the task's queued runs. A task already in the state is returned
// unchanged and the canceled runs are still swept, as the ticket API always did.
//
// cancelMsg is the outcome of the queued runs it cancels.
//
// pick chooses the state from the task's current one. It runs in the transaction, after the task
// row is locked, so concurrent changes of one task are serialised and pick never sees a stale state.
func (s *Server) setState(ctx context.Context, c caller, id pgtype.UUID, cancelMsg string, pick func(cur store.TaskState) (store.TaskState, error)) (store.Task, []store.Run, error) {
	var (
		task store.Task
		runs []store.Run
	)
	err := s.inTx(ctx, func(q *store.Queries) error {
		t, err := q.GetTaskForUpdate(ctx, store.GetTaskForUpdateParams{OrgID: c.org, ID: id})
		if err != nil {
			return err
		}
		to, err := pick(t.State)
		if err != nil {
			return err
		}
		if from := t.State; from != to {
			if t, err = q.SetTaskState(ctx, store.SetTaskStateParams{OrgID: c.org, ID: id, State: to}); err != nil {
				return err
			}
			if _, err := q.AppendTaskEvent(ctx, store.AppendTaskEventParams{
				OrgID: c.org, TaskID: id, Kind: KindStatusChanged,
				ActorKind: store.TaskActorKindUser, ActorUserID: c.actor(),
				Payload: jsonPayload(map[string]any{"from": string(from), "to": string(to)}),
			}); err != nil {
				return err
			}
		}
		if isEnded(to) {
			if runs, err = scheduler.CancelQueuedForTask(ctx, q, t, cancelMsg, scheduler.Actor{UserID: c.actor()}); err != nil {
				return err
			}
		}
		task, err = q.GetTask(ctx, store.GetTaskParams{OrgID: c.org, ID: id})
		return err
	})
	return task, runs, err
}

// taskAPI implements TaskService. It is a type of its own because AppendMessage is also a method
// of TicketService, with other types, on Server.
type taskAPI struct{ *Server }

var _ taskv1alpha1connect.TaskServiceHandler = taskAPI{}

// CreateTask implements TaskService.
func (a taskAPI) CreateTask(ctx context.Context, req *taskv1alpha1.CreateTaskRequest) (*taskv1alpha1.CreateTaskResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleMember)
	if err != nil {
		return nil, err
	}
	agentID, err := parseID("agent_id", req.GetAgentId())
	if err != nil {
		return nil, err
	}
	title := strings.TrimSpace(req.GetTitle())
	if title == "" || utf8.RuneCountInString(title) > 500 {
		return nil, connect.NewError(connect.CodeInvalidArgument, "title must be 1 to 500 characters")
	}
	msg, err := checkMessage(req.GetMessage(), false)
	if err != nil {
		return nil, err
	}
	t, run, err := a.createTask(ctx, c, agentID, title, msg)
	if err != nil {
		return nil, a.dbError(err, "agent")
	}
	return &taskv1alpha1.CreateTaskResponse{Task: taskProto(t), Run: run}, nil
}

// ListTasks implements TaskService.
func (a taskAPI) ListTasks(ctx context.Context, req *taskv1alpha1.ListTasksRequest) (*taskv1alpha1.ListTasksResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleViewer)
	if err != nil {
		return nil, err
	}
	var states []store.TaskState
	if req.GetState() != taskv1alpha1.TaskState_TASK_STATE_UNSPECIFIED {
		st, ok := taskStateFromProto(req.GetState())
		if !ok {
			return nil, connect.NewError(connect.CodeInvalidArgument, "unknown state")
		}
		states = []store.TaskState{st}
	}
	tasks, costs, err := a.listTasks(ctx, c, states)
	if err != nil {
		return nil, a.dbError(err, "tasks")
	}
	res := &taskv1alpha1.ListTasksResponse{}
	for _, t := range tasks {
		p := taskProto(t)
		p.Cost = costOrNone(costs, p.GetId())
		res.Tasks = append(res.Tasks, p)
	}
	return res, nil
}

// GetTask implements TaskService.
func (a taskAPI) GetTask(ctx context.Context, req *taskv1alpha1.GetTaskRequest) (*taskv1alpha1.GetTaskResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleViewer)
	if err != nil {
		return nil, err
	}
	id, err := parseID("task_id", req.GetTaskId())
	if err != nil {
		return nil, err
	}
	v, err := a.loadTask(ctx, c, id)
	if err != nil {
		return nil, a.dbError(err, "task")
	}
	res := &taskv1alpha1.GetTaskResponse{Task: taskProto(v.task), Runs: v.runs}
	res.Task.Cost = v.cost
	if res.Task.Cost == nil {
		res.Task.Cost = noCost()
	}
	res.Task.AgentSummary = v.task.AgentSummary.String
	res.Task.AgentSummaryAt = tsProto(v.task.AgentSummaryAt)
	for _, e := range v.events {
		res.Events = append(res.Events, taskEventProto(e))
	}
	return res, nil
}

// AppendMessage implements TaskService.
func (a taskAPI) AppendMessage(ctx context.Context, req *taskv1alpha1.AppendMessageRequest) (*taskv1alpha1.AppendMessageResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleMember)
	if err != nil {
		return nil, err
	}
	id, err := parseID("task_id", req.GetTaskId())
	if err != nil {
		return nil, err
	}
	msg, err := checkMessage(req.GetText(), true)
	if err != nil {
		return nil, err
	}
	ev, run, err := a.appendMessage(ctx, c, id, msg)
	if errors.Is(err, errTaskEnded) {
		err = connect.NewError(connect.CodeFailedPrecondition, "task has ended")
	}
	if err != nil {
		return nil, a.dbError(err, "task")
	}
	return &taskv1alpha1.AppendMessageResponse{Event: taskEventProto(ev), Run: run}, nil
}

// CancelTask implements TaskService.
func (a taskAPI) CancelTask(ctx context.Context, req *taskv1alpha1.CancelTaskRequest) (*taskv1alpha1.CancelTaskResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleMember)
	if err != nil {
		return nil, err
	}
	id, err := parseID("task_id", req.GetTaskId())
	if err != nil {
		return nil, err
	}
	// Ended tasks stay as they ended; only an open task can be canceled. The check runs in
	// setState's transaction, on the locked row.
	t, runs, err := a.setState(ctx, c, id, "task ended", func(cur store.TaskState) (store.TaskState, error) {
		if isEnded(cur) {
			return cur, connect.NewError(connect.CodeFailedPrecondition, "task has already ended")
		}
		return store.TaskStateCanceled, nil
	})
	if err != nil {
		return nil, a.dbError(err, "task")
	}
	return &taskv1alpha1.CancelTaskResponse{Task: taskProto(t), CanceledRuns: runsProto(runs)}, nil
}
