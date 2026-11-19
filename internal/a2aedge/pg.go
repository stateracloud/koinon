// SPDX-License-Identifier: Apache-2.0

package a2aedge

import (
	"context"
	"errors"
	"fmt"

	connect "connectrpc.com/connect/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/services"
	"github.com/stateracloud/koinon/internal/triggers"
	"github.com/stateracloud/koinon/templates"
)

const maxTimelineEvents = 10_000

// PG is the [Backend] over Postgres and the services package.
type PG struct {
	Pool *pgxpool.Pool
	Svc  *services.Server
}

var _ Backend = PG{}

// Agent implements [Backend].
func (b PG) Agent(ctx context.Context, sc Scope) (Agent, error) {
	q := store.New(b.Pool)
	a, err := q.GetAgent(ctx, store.GetAgentParams{OrgID: sc.Org, ID: sc.Agent})
	if errors.Is(err, pgx.ErrNoRows) {
		return Agent{}, ErrNotFound
	}
	if err != nil {
		return Agent{}, err
	}
	if a.Status != store.AgentStatusActive {
		return Agent{}, fmt.Errorf("%w: the agent is not active", ErrRefused)
	}
	tr, err := triggers.ForAgent(ctx, q, sc.Org, sc.Agent, templates.TriggerDirectMessage)
	if err != nil {
		return Agent{}, err
	}
	if len(tr) == 0 {
		return Agent{}, fmt.Errorf("%w: the agent takes no direct messages", ErrRefused)
	}
	return Agent{Name: a.DisplayName}, nil
}

// Deliver implements [Backend].
func (b PG) Deliver(ctx context.Context, sc Scope, task pgtype.UUID, messageID, text string) (services.DirectMessageResult, error) {
	res, err := b.Svc.DeliverDirectMessage(ctx, services.DirectMessage{
		Org: sc.Org, Agent: sc.Agent, Task: task, User: sc.User, MessageID: messageID, Text: text,
	})
	return res, mapService(err)
}

// mapService maps the connect errors of the services package to the errors of the edge.
func mapService(err error) error {
	if err == nil {
		return nil
	}
	switch connect.CodeOf(err) {
	case connect.CodeNotFound:
		return ErrNotFound
	case connect.CodeFailedPrecondition, connect.CodeInvalidArgument, connect.CodeAlreadyExists:
		return fmt.Errorf("%w: %s", ErrRefused, err.Error())
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// isThread reports whether the task is a thread of the agent that a message through the edge (or a
// direct message) started. The edge shows no other task.
func isThread(t store.Task, agent pgtype.UUID) bool {
	return t.AgentID == agent && t.CreatedByKind == store.TaskCreatorKindTrigger &&
		t.CreatedByTriggerID == services.DirectMessageTriggerID(agent)
}

// Thread implements [Backend].
func (b PG) Thread(ctx context.Context, sc Scope, id pgtype.UUID) (store.Task, []store.TaskEvent, error) {
	q := store.New(b.Pool)
	t, err := q.GetTask(ctx, store.GetTaskParams{OrgID: sc.Org, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Task{}, nil, ErrNotFound
	}
	if err != nil {
		return store.Task{}, nil, err
	}
	if !isThread(t, sc.Agent) {
		return store.Task{}, nil, ErrNotFound
	}
	ev, err := q.ListTaskEvents(ctx, store.ListTaskEventsParams{OrgID: sc.Org, TaskID: id, AfterSeq: 0, MaxEvents: maxTimelineEvents})
	return t, ev, err
}

// Cancel implements [Backend].
func (b PG) Cancel(ctx context.Context, sc Scope, id pgtype.UUID) (store.Task, error) {
	if _, _, err := b.Thread(ctx, sc, id); err != nil {
		return store.Task{}, err
	}
	t, err := b.Svc.CancelTaskAs(ctx, sc.Org, sc.User, id)
	return t, mapService(err)
}
