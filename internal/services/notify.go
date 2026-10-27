// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/durable"
	"github.com/stateracloud/koinon/internal/questions"
	"github.com/stateracloud/koinon/internal/scheduler"
	"github.com/stateracloud/koinon/internal/wake"
)

// ErrAgentInactive is returned when a task is created for an agent that is not active. It is a
// permanent failure: callers drop the event instead of retrying it.
var ErrAgentInactive = errors.New("agent is not active")

// NotifyWakeAgent is the notification kind that pre-wakes an agent's sandbox (plan item LT1).
const NotifyWakeAgent = "wake.agent"

type wakePayload struct {
	Org   string `json:"org"`
	Agent string `json:"agent"`
}

// wakeNotification is the pre-wake for a task, to send after the commit that made the task or the
// message. effect names the effect, so a repeat of it is the same workflow (ADR 0066, B2).
// A task without an agent (old rows predate the tasks_owned_by_agent check) has nothing to wake,
// so it yields no notification.
func wakeNotification(effect string, task store.Task) []durable.Notification {
	if !task.AgentID.Valid {
		return nil
	}
	p, _ := json.Marshal(wakePayload{Org: idString(task.OrgID), Agent: idString(task.AgentID)})
	return []durable.Notification{{ID: NotifyWakeAgent + ":" + effect, Kind: NotifyWakeAgent, Payload: string(p)}}
}

// RegisterNotifications registers the handlers of the notifications that services sends. Call it
// on a Notifier before the DBOS runtime launches, so workflows recovered at start find them.
func RegisterNotifications(n *durable.Notifier, pool *pgxpool.Pool) {
	questions.RegisterNotifications(n, pool)
	n.Handle(NotifyWakeAgent, func(ctx context.Context, note durable.Notification) error {
		var p wakePayload
		if err := json.Unmarshal([]byte(note.Payload), &p); err != nil {
			return fmt.Errorf("decode %s payload: %w", note.Kind, err)
		}
		org, err := parseID("org", p.Org)
		if err != nil {
			return err
		}
		agent, err := parseID("agent", p.Agent)
		if err != nil {
			return err
		}
		_, err = wake.Agent(ctx, store.New(pool), org, agent)
		return err
	})
}

// commit runs fn in a transaction and sends the notifications it returns after the commit
// (durable.Commit). A failed send is returned as *durable.SendError.
func (s *Server) commit(ctx context.Context, fn func(q *store.Queries) ([]durable.Notification, error)) error {
	return durable.Commit(ctx, s.pool, s.notifier, fn)
}

// sendFailed logs a *durable.SendError, which leaves the committed change standing and the
// request successful (the notification is best effort for the caller), and returns any other error.
func (s *Server) sendFailed(err error, msg string) error {
	var se *durable.SendError
	if errors.As(err, &se) {
		s.log.Warn(msg, "err", se.Err)
		return nil
	}
	return err
}

// TriggeredTask is a task to create for a trigger fire or an inbound event.
type TriggeredTask struct {
	Org, Agent pgtype.UUID
	// Trigger is the ID of the trigger that fired; it is recorded as the task's creator.
	Trigger pgtype.UUID
	// Source and Key are the dedupe key (durable.Source*, from the source: a delivery ID, an
	// event_id, durable.TriggerFireKey).
	Source, Key string
	Title       string
	Message     string
	// Origin is the run's origin; empty means scheduled, which interactive work outranks.
	Origin store.RunOrigin
}

// CreateTriggeredTask creates a task for a trigger fire at most once per (org, Source, Key): the
// dedupe record, the task, its events and its queued run commit in one transaction, so a replayed
// delivery or a duplicate fire finds the record and creates nothing. The sandbox pre-wake is sent
// after the commit. The returned task is the existing one for a replay, and created is false.
func (s *Server) CreateTriggeredTask(ctx context.Context, in TriggeredTask) (task store.Task, created bool, err error) {
	origin := in.Origin
	if origin == "" {
		origin = store.RunOriginScheduled
	}
	err = s.commit(ctx, func(q *store.Queries) ([]durable.Notification, error) {
		var notes []durable.Notification
		out, err := durable.Once(ctx, q, in.Org, in.Source, in.Key, func(q *store.Queries) (string, pgtype.UUID, error) {
			agent, err := q.GetAgent(ctx, store.GetAgentParams{OrgID: in.Org, ID: in.Agent})
			if err != nil {
				return "", pgtype.UUID{}, err
			}
			if agent.Status != store.AgentStatusActive {
				return "", pgtype.UUID{}, ErrAgentInactive
			}
			t, err := q.CreateTask(ctx, store.CreateTaskParams{
				OrgID: in.Org, Title: in.Title, AgentID: agent.ID,
				CreatedByKind: store.TaskCreatorKindTrigger, CreatedByTriggerID: in.Trigger,
			})
			if err != nil {
				return "", pgtype.UUID{}, err
			}
			if _, err := q.AppendTaskEvent(ctx, store.AppendTaskEventParams{
				OrgID: in.Org, TaskID: t.ID, Kind: KindTaskCreated, ActorKind: store.TaskActorKindSystem,
				Payload: jsonPayload(map[string]any{"title": in.Title, "source": in.Source}),
			}); err != nil {
				return "", pgtype.UUID{}, err
			}
			if in.Message != "" {
				if _, err := q.AppendTaskEvent(ctx, store.AppendTaskEventParams{
					OrgID: in.Org, TaskID: t.ID, Kind: KindMessage, ActorKind: store.TaskActorKindSystem,
					Payload: jsonPayload(map[string]any{"text": in.Message, "source": in.Source}),
				}); err != nil {
					return "", pgtype.UUID{}, err
				}
			}
			if _, _, err := scheduler.EnqueueOrigin(ctx, q, t, origin); err != nil {
				return "", pgtype.UUID{}, err
			}
			return "task", t.ID, nil
		})
		if err != nil {
			return nil, err
		}
		if !out.First && (out.EffectKind != "task" || !out.EffectID.Valid) {
			return nil, fmt.Errorf("dedupe key %s/%s already used by %q", in.Source, in.Key, out.EffectKind)
		}
		created = out.First
		task, err = q.GetTask(ctx, store.GetTaskParams{OrgID: in.Org, ID: out.EffectID})
		if err != nil {
			return nil, err
		}
		if out.First {
			notes = append(notes, wakeNotification("task-created:"+idString(task.ID), task)...)
		}
		return notes, nil
	})
	return task, created, s.sendFailed(err, "waking the agent's sandbox for a triggered task failed")
}
