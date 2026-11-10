// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	connect "connectrpc.com/connect/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/durable"
	taskv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/task/v1alpha1"
	"github.com/stateracloud/koinon/internal/triggers"
	"github.com/stateracloud/koinon/templates"
)

// Direct messages (plan item TR3, ADR 0066 decision 3 and the 2026-10-27 TR3 amendment). A direct
// message to an agent in the inbox chat is a trigger firing: with a direct_message trigger on the
// agent's active version it starts a task, or, in an existing thread, reaches that thread's task.
// The thread is the task; there is no table of its own.

// KindStatusChanged payload reason of a completed task reopened by a follow-up message.
const reasonFollowUp = "follow_up"

const (
	maxMessageIDLen = 128
	maxThreadTitle  = 80
)

// dmTriggerNamespace derives the creator trigger ID of direct-message tasks. Triggers are slugs in
// the agent's versioned config and tasks.created_by_trigger_id is a uuid, so the ID is a v5 UUID
// of the agent: an agent has at most one direct_message trigger (TR1), so the ID is stable across
// versions and renames of the trigger.
var dmTriggerNamespace = uuid.MustParse("6f0e1d6e-3f0a-5c53-9a37-5b6c2a4f7d10")

// DirectMessageTriggerID is the creator trigger ID recorded on tasks that a direct message to the
// agent started.
func DirectMessageTriggerID(agent pgtype.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: uuid.NewSHA1(dmTriggerNamespace, agent.Bytes[:]), Valid: true}
}

// errNoDirectMessages is returned for an agent without a direct_message trigger in effect.
func errNoDirectMessages() error {
	return connect.NewError(connect.CodeFailedPrecondition, "this agent does not take direct messages: it has no direct-message trigger")
}

// DirectMessage is one message to an agent in the inbox chat.
type DirectMessage struct {
	Org, Agent pgtype.UUID
	// Task is the thread to reply in; invalid starts a new thread.
	Task pgtype.UUID
	// User is the sender.
	User pgtype.UUID
	// MessageID is chosen by the client and is the dedupe key.
	MessageID string
	Text      string
}

// DirectMessageResult is what a direct message did.
type DirectMessageResult struct {
	Task store.Task
	// Event is the message on the timeline; nil for a replay.
	Event *store.TaskEvent
	// Created is true when this call created the task; Reopened when it reopened a completed one.
	Created, Reopened bool
}

// threadTitle is the first line of the message, shortened.
func threadTitle(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	line = strings.TrimSpace(line)
	if utf8.RuneCountInString(line) > maxThreadTitle {
		line = string([]rune(line)[:maxThreadTitle-1]) + "…"
	}
	return line
}

// DeliverDirectMessage delivers the message at most once per (org, message ID): the dedupe record,
// the task or follow-up, the message and the queued run (origin interactive, since a person is
// waiting) commit in one transaction, and the sandbox pre-wake follows the commit. It refuses an
// agent that is not active or has no direct_message trigger in effect.
func (s *Server) DeliverDirectMessage(ctx context.Context, in DirectMessage) (DirectMessageResult, error) {
	// A held install queues no new work, also for a message queued before the hold (ADR 0107).
	if err := s.hold.Err(); err != nil {
		return DirectMessageResult{}, err
	}
	var res DirectMessageResult
	err := s.commit(ctx, func(q *store.Queries, _ audit.Batch) ([]durable.Notification, error) {
		res = DirectMessageResult{}
		out, err := durable.Once(ctx, q, in.Org, durable.SourceDirectMessage, in.MessageID, func(q *store.Queries) (string, pgtype.UUID, error) {
			id, err := s.deliverDirectMessage(ctx, q, in, &res)
			return "task", id, err
		})
		if err != nil {
			return nil, err
		}
		if !out.First {
			if out.EffectKind != "task" || !out.EffectID.Valid {
				return nil, connect.NewError(connect.CodeAlreadyExists, "message_id was used for something else")
			}
			res.Task, err = q.GetTask(ctx, store.GetTaskParams{OrgID: in.Org, ID: out.EffectID})
			if err != nil {
				return nil, err
			}
			// A replay returns the task recorded under the ID; another agent's or thread's task is
			// a reused ID, not a replay.
			if res.Task.AgentID != in.Agent || (in.Task.Valid && res.Task.ID != in.Task) {
				res = DirectMessageResult{}
				return nil, connect.NewError(connect.CodeAlreadyExists, "message_id was used for another agent or thread")
			}
			return nil, nil
		}
		return wakeNotification("dm:"+idString(res.Task.ID)+":"+in.MessageID, res.Task), nil
	})
	return res, s.sendFailed(err, "waking the agent's sandbox for a direct message failed")
}

func (s *Server) deliverDirectMessage(ctx context.Context, q *store.Queries, in DirectMessage, res *DirectMessageResult) (pgtype.UUID, error) {
	agent, err := q.GetAgent(ctx, store.GetAgentParams{OrgID: in.Org, ID: in.Agent})
	if err != nil {
		return pgtype.UUID{}, err
	}
	if agent.Status != store.AgentStatusActive {
		return pgtype.UUID{}, connect.NewError(connect.CodeFailedPrecondition, "agent is not active")
	}
	// A trigger is in effect when it is in the active version of an active agent (TR1).
	tr, err := triggers.ForAgent(ctx, q, in.Org, in.Agent, templates.TriggerDirectMessage)
	if err != nil {
		return pgtype.UUID{}, err
	}
	if len(tr) == 0 {
		return pgtype.UUID{}, errNoDirectMessages()
	}
	var task store.Task
	text := in.Text
	if !in.Task.Valid {
		if ins := strings.TrimSpace(tr[0].Instruction); ins != "" {
			text = ins + "\n\n" + in.Text
		}
		title := threadTitle(in.Text)
		if task, err = q.CreateTask(ctx, store.CreateTaskParams{
			OrgID: in.Org, Title: title, AgentID: agent.ID,
			CreatedByKind: store.TaskCreatorKindTrigger, CreatedByTriggerID: DirectMessageTriggerID(agent.ID),
		}); err != nil {
			return pgtype.UUID{}, err
		}
		if _, err := q.AppendTaskEvent(ctx, store.AppendTaskEventParams{
			OrgID: in.Org, TaskID: task.ID, Kind: KindTaskCreated, ActorKind: store.TaskActorKindSystem,
			Payload: jsonPayload(map[string]any{"title": title, "source": durable.SourceDirectMessage, "trigger": tr[0].ID}),
		}); err != nil {
			return pgtype.UUID{}, err
		}
		res.Created = true
	} else {
		// The row lock serialises this with other messages and with setState.
		if task, err = q.GetTaskForUpdate(ctx, store.GetTaskForUpdateParams{OrgID: in.Org, ID: in.Task}); err != nil {
			return pgtype.UUID{}, err
		}
		if task.AgentID != agent.ID {
			return pgtype.UUID{}, connect.NewError(connect.CodeInvalidArgument, "the thread belongs to another agent")
		}
		if task.CreatedByKind != store.TaskCreatorKindTrigger || task.CreatedByTriggerID != DirectMessageTriggerID(agent.ID) {
			return pgtype.UUID{}, connect.NewError(connect.CodeFailedPrecondition, "the task is not a direct-message thread; add the message to the task instead")
		}
		switch task.State {
		case store.TaskStateCanceled, store.TaskStateFailed:
			return pgtype.UUID{}, connect.NewError(connect.CodeFailedPrecondition, "the thread's task has "+string(task.State)+"; start a new thread")
		case store.TaskStateCompleted:
			// A follow-up reopens the task (ADR 0066 TR3 amendment): same task, same timeline, same
			// harness session when it is still there.
			if task, err = q.SetTaskState(ctx, store.SetTaskStateParams{OrgID: in.Org, ID: task.ID, State: store.TaskStateSubmitted}); err != nil {
				return pgtype.UUID{}, err
			}
			if _, err := q.AppendTaskEvent(ctx, store.AppendTaskEventParams{
				OrgID: in.Org, TaskID: task.ID, Kind: KindStatusChanged, ActorKind: store.TaskActorKindUser, ActorUserID: in.User,
				Payload: jsonPayload(map[string]any{"from": string(store.TaskStateCompleted), "to": string(store.TaskStateSubmitted), "reason": reasonFollowUp}),
			}); err != nil {
				return pgtype.UUID{}, err
			}
			res.Reopened = true
		}
	}
	// The message is the person's: agentd's prompt carries user messages. "display" is what the
	// timeline shows, without the trigger's instruction.
	payload := map[string]any{"text": text, "source": durable.SourceDirectMessage, "message_id": in.MessageID}
	if text != in.Text {
		payload["display"] = in.Text
	}
	ev, err := q.AppendTaskEvent(ctx, store.AppendTaskEventParams{
		OrgID: in.Org, TaskID: task.ID, Kind: KindMessage, ActorKind: store.TaskActorKindUser, ActorUserID: in.User,
		Payload: jsonPayload(payload),
	})
	if err != nil {
		return pgtype.UUID{}, err
	}
	// Interactive origin: a person is waiting. Enqueue queues nothing if a run is queued already;
	// that run will see this message.
	if _, err := enqueue(ctx, q, task); err != nil {
		return pgtype.UUID{}, err
	}
	if res.Task, err = q.GetTask(ctx, store.GetTaskParams{OrgID: in.Org, ID: task.ID}); err != nil {
		return pgtype.UUID{}, err
	}
	res.Event = &ev
	return task.ID, nil
}

// checkMessageID bounds the client's message ID.
func checkMessageID(id string) error {
	if id == "" || len(id) > maxMessageIDLen || !utf8.ValidString(id) || strings.ContainsAny(id, "\x00\n") {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Sprintf("message_id is required and at most %d characters", maxMessageIDLen))
	}
	return nil
}

// SendDirectMessage implements TaskService.
func (a taskAPI) SendDirectMessage(ctx context.Context, req *taskv1alpha1.SendDirectMessageRequest) (*taskv1alpha1.SendDirectMessageResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleMember)
	if err != nil {
		return nil, err
	}
	// A held install queues no new work (ADR 0107); the queued path would only retry the refusal.
	if err := a.refuseWhileHeld(); err != nil {
		return nil, err
	}
	agentID, err := parseID("agent_id", req.GetAgentId())
	if err != nil {
		return nil, err
	}
	in := DirectMessage{Org: c.org, Agent: agentID, User: c.actor(), MessageID: req.GetMessageId()}
	if req.GetTaskId() != "" {
		if in.Task, err = parseID("task_id", req.GetTaskId()); err != nil {
			return nil, err
		}
	}
	if err := checkMessageID(in.MessageID); err != nil {
		return nil, err
	}
	if in.Text, err = checkMessage(req.GetText(), true); err != nil {
		return nil, err
	}
	// With durable on, creation goes through the org's rate-limited queue (DB2); without it the
	// message is delivered inline in one transaction.
	var res DirectMessageResult
	if a.dm != nil {
		res, err = a.dm.deliver(ctx, in)
	} else {
		res, err = a.DeliverDirectMessage(ctx, in)
	}
	if err != nil {
		return nil, a.dbError(err, "agent")
	}
	out := &taskv1alpha1.SendDirectMessageResponse{Task: taskProto(res.Task), Created: res.Created, Reopened: res.Reopened}
	if res.Event != nil {
		out.Event = taskEventProto(*res.Event)
	}
	return out, nil
}
