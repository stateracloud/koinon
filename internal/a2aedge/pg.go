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

const (
	maxTimelineEvents = 10_000
	timelinePage      = 1_000
)

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
	res, err := b.Svc.SendDirectMessageAs(ctx, services.DirectMessage{
		Org: sc.Org, Agent: sc.Agent, Task: task, User: sc.User, MessageID: messageID, Text: text,
		// A2A: a task that has ended takes no message.
		NoReopen: task.Valid,
	})
	return res, mapService(err)
}

// mapService maps the connect errors of the services package to the errors of the edge.
func mapService(err error) error {
	if err == nil {
		return nil
	}
	// The message of a connect error is written for the client. Other errors are not, and pass
	// on as they are (the handler hides them).
	var ce *connect.Error
	if errors.As(err, &ce) {
		switch ce.Code() {
		case connect.CodeNotFound:
			return ErrNotFound
		case connect.CodeInvalidArgument:
			return fmt.Errorf("%w: %s", ErrInvalid, ce.Message())
		case connect.CodeFailedPrecondition, connect.CodeAlreadyExists, connect.CodeUnavailable:
			return fmt.Errorf("%w: %s", ErrRefused, ce.Message())
		}
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
func (b PG) Thread(ctx context.Context, sc Scope, id pgtype.UUID, after int64) (store.Task, []store.TaskEvent, error) {
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
	// The timeline is read in pages up to a cap. A thread longer than the cap shows its first
	// events only (ADR 0122, consequences); a wait reads after its message, so it is not affected.
	var all []store.TaskEvent
	for len(all) < maxTimelineEvents {
		ev, err := q.ListTaskEvents(ctx, store.ListTaskEventsParams{OrgID: sc.Org, TaskID: id, AfterSeq: after, MaxEvents: timelinePage})
		if err != nil {
			return store.Task{}, nil, err
		}
		all = append(all, ev...)
		if len(ev) < timelinePage {
			break
		}
		after = ev[len(ev)-1].Seq
	}
	return t, all, nil
}

// Cancel implements [Backend].
func (b PG) Cancel(ctx context.Context, sc Scope, id pgtype.UUID) (store.Task, error) {
	if _, _, err := b.Thread(ctx, sc, id, 1<<62); err != nil {
		return store.Task{}, err
	}
	t, err := b.Svc.CancelTaskAs(ctx, sc.Org, sc.User, id)
	return t, mapService(err)
}
