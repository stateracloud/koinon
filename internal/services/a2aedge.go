// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"

	connect "connectrpc.com/connect/v2"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/auth"
	"github.com/stateracloud/koinon/internal/db/store"
)

// CancelTaskAs cancels an open task on behalf of user, for a caller that is not a Connect session
// (the A2A edge, ADR 0122). The caller has checked the role. It does what CancelTask does: queued
// runs are canceled and the task moves to canceled; a task that has ended is refused.
func (s *Server) CancelTaskAs(ctx context.Context, org, user, id pgtype.UUID) (store.Task, error) {
	c := caller{org: org, p: &auth.Principal{UserID: user}}
	t, _, err := s.setState(ctx, c, id, "task ended", func(cur store.TaskState) (store.TaskState, error) {
		if isEnded(cur) {
			return cur, connect.NewError(connect.CodeFailedPrecondition, "task has already ended")
		}
		return store.TaskStateCanceled, nil
	})
	if err != nil {
		return store.Task{}, s.dbError(err, "task")
	}
	return t, nil
}

// SendDirectMessageAs delivers a direct message for a caller that is not a Connect session (the A2A
// edge, ADR 0122), with the checks and the path of SendDirectMessage: the message ID and text are
// bounded, and with durable on the message goes through the org's rate-limited queue (DB2). The
// caller has checked the role.
func (s *Server) SendDirectMessageAs(ctx context.Context, in DirectMessage) (DirectMessageResult, error) {
	if err := s.refuseWhileHeld(); err != nil {
		return DirectMessageResult{}, err
	}
	if err := checkMessageID(in.MessageID); err != nil {
		return DirectMessageResult{}, err
	}
	text, err := checkMessage(in.Text, true)
	if err != nil {
		return DirectMessageResult{}, err
	}
	in.Text = text
	if s.dm != nil {
		return s.dm.deliver(ctx, in)
	}
	return s.DeliverDirectMessage(ctx, in)
}
