// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"encoding/json"

	connect "connectrpc.com/connect/v2"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/auth"
	"github.com/stateracloud/koinon/internal/db/store"
)

// CancelTaskAs cancels an open task on behalf of user, for a caller that is not a Connect session
// (the A2A edge, ADR 0123). The caller has checked the role. It does what CancelTask does: queued
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
// edge, ADR 0123), with the checks and the path of SendDirectMessage: the message ID and text are
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

// maxReopenScan bounds the events that reopenedAfter reads: 10 pages of 1000.
const maxReopenScan = 10_000

// reopenedAfter reports whether a follow-up reopened the completed task after the event with
// sequence number seq. Each such reopening starts a new turn of the thread (ADR 0123). The caller
// holds the lock of the task.
func reopenedAfter(ctx context.Context, q *store.Queries, org, task pgtype.UUID, seq int64) (bool, error) {
	for scanned := 0; scanned < maxReopenScan; {
		ev, err := q.ListTaskEvents(ctx, store.ListTaskEventsParams{OrgID: org, TaskID: task, AfterSeq: seq, MaxEvents: 1000})
		if err != nil {
			return false, err
		}
		for _, e := range ev {
			var p struct {
				From string `json:"from"`
			}
			if e.Kind == KindStatusChanged && json.Unmarshal(e.Payload, &p) == nil && p.From == string(store.TaskStateCompleted) {
				return true, nil
			}
		}
		if len(ev) < 1000 {
			break
		}
		seq = ev[len(ev)-1].Seq
		scanned += len(ev)
	}
	return false, nil
}
