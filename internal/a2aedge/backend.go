// SPDX-License-Identifier: Apache-2.0

package a2aedge

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/services"
)

// Errors a [Backend] returns. The handler maps them to A2A errors.
var (
	// ErrNotFound: the agent or the thread does not exist for this org.
	ErrNotFound = errors.New("not found")
	// ErrRefused: the agent or the task is in a state that takes no message (the text says why).
	ErrRefused = errors.New("refused")
	// ErrInvalid: the message is not valid (the text says why, for the client).
	ErrInvalid = errors.New("invalid")
)

// Scope is who calls and where. The auth layer sets it; the handler never reads it from the body.
type Scope struct {
	Org, Agent pgtype.UUID
	// User is the user who created the token. Messages are written as this user.
	User pgtype.UUID
}

// Agent is what the agent card shows.
type Agent struct {
	Name string
}

// Backend is what the edge needs from Koinon: the services package for writes, the store for reads.
type Backend interface {
	// Agent returns the agent, or [ErrNotFound]. It returns [ErrRefused] for an agent that is not
	// active or has no direct_message trigger.
	Agent(ctx context.Context, sc Scope) (Agent, error)
	// Deliver writes a message into a thread (new when task is invalid) once per message ID. The
	// result's Event holds the timeline entry of the message; it is nil for a replay. A completed
	// thread is reopened (the next turn) only when reopen is set. Otherwise it is refused
	// ([ErrRefused]), but a replay of a message that was taken before it completed returns the
	// task. A thread that failed or was canceled is always refused.
	Deliver(ctx context.Context, sc Scope, task pgtype.UUID, reopen bool, messageID, text string) (services.DirectMessageResult, error)
	// Thread returns a thread of the agent and its timeline after sequence number after (0 is the
	// whole timeline, up to a cap), or [ErrNotFound] for any other task.
	Thread(ctx context.Context, sc Scope, task pgtype.UUID, after int64) (store.Task, []store.TaskEvent, error)
	// Cancel cancels an open thread of the agent. A thread that has ended gives [ErrRefused].
	Cancel(ctx context.Context, sc Scope, task pgtype.UUID) (store.Task, error)
}
