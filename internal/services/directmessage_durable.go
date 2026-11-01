// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	connect "connectrpc.com/connect/v2"
	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/durable"
)

// DirectMessageWorkflow is the registered name of the workflow that delivers a direct message on
// the org-partitioned queue (DB2), so one org's chat burst is rate limited and does not delay
// another org's work.
const DirectMessageWorkflow = "koinon.direct-message"

const stepDeliverDirectMessage = "deliver-direct-message"

var _ = durable.DeclareSteps(DirectMessageWorkflow, stepDeliverDirectMessage)

// directMessageWait bounds how long a request waits for its message to leave the queue. The work
// is not lost when it runs out: the message ID makes the client's retry the same workflow.
const directMessageWait = 30 * time.Second

// DirectMessageQueue delivers direct messages through DBOS (api.durable.enabled). Create it with
// RegisterDirectMessages before the runtime launches and pass it in Options.DirectMessages; New
// binds the Server that the workflow runs on.
type DirectMessageQueue struct {
	work  *durable.OrgWork
	srv   atomic.Pointer[Server]
	ready chan struct{}
}

type dmInput struct {
	Org, Agent, Task, User, MessageID, Text string
}

// dmOutput carries the result, and the API errors as data, because a workflow's error loses its
// code on the way through Postgres.
type dmOutput struct {
	Task, Event string
	Seq         int64
	Created     bool
	Reopened    bool
	ErrCode     uint32
	ErrMsg      string
}

// RegisterDirectMessages registers the direct-message workflow on rt.
func RegisterDirectMessages(rt *durable.Runtime, work *durable.OrgWork) (*DirectMessageQueue, error) {
	d := &DirectMessageQueue{work: work, ready: make(chan struct{})}
	err := rt.Register(func(c dbos.Context) error {
		dbos.RegisterWorkflow(c, d.workflow, dbos.WithWorkflowName(DirectMessageWorkflow))
		return nil
	})
	return d, err
}

func (d *DirectMessageQueue) bind(s *Server) {
	if d.srv.CompareAndSwap(nil, s) {
		close(d.ready)
	}
}

func (d *DirectMessageQueue) workflow(c dbos.Context, in dmInput) (dmOutput, error) {
	return dbos.RunAsStep(c, func(ctx context.Context) (dmOutput, error) {
		select {
		case <-d.ready: // a workflow recovered at start can run before the Server exists
		case <-ctx.Done():
			return dmOutput{}, ctx.Err()
		}
		var m DirectMessage
		var err error
		if m.Org, err = parseID("org", in.Org); err != nil {
			return dmOutput{}, err
		}
		if m.Agent, err = parseID("agent", in.Agent); err != nil {
			return dmOutput{}, err
		}
		if in.Task != "" {
			if m.Task, err = parseID("task", in.Task); err != nil {
				return dmOutput{}, err
			}
		}
		if in.User != "" {
			if m.User, err = parseID("user", in.User); err != nil {
				return dmOutput{}, err
			}
		}
		m.MessageID, m.Text = in.MessageID, in.Text
		res, err := d.srv.Load().DeliverDirectMessage(ctx, m)
		if errors.Is(err, pgx.ErrNoRows) {
			err = connect.NewError(connect.CodeNotFound, "agent or task not found")
		}
		var ce *connect.Error
		if errors.As(err, &ce) {
			return dmOutput{ErrCode: uint32(ce.Code()), ErrMsg: ce.Message()}, nil
		}
		if err != nil {
			return dmOutput{}, err
		}
		out := dmOutput{Task: idString(res.Task.ID), Created: res.Created, Reopened: res.Reopened}
		if res.Event != nil {
			out.Seq = res.Event.Seq
		}
		return out, nil
	}, dbos.WithStepName(stepDeliverDirectMessage), dbos.WithStepMaxRetries(3))
}

// deliver enqueues the message for its org and waits for the result. The workflow ID is derived
// from the org and the message ID, so a retried request attaches to the same workflow.
func (d *DirectMessageQueue) deliver(ctx context.Context, in DirectMessage) (DirectMessageResult, error) {
	org := idString(in.Org)
	input := dmInput{Org: org, Agent: idString(in.Agent), Task: idString(in.Task), User: idString(in.User), MessageID: in.MessageID, Text: in.Text}
	wfID := "dm:" + org + ":" + in.MessageID
	h, err := durable.EnqueueForOrg[dmOutput](ctx, d.work, org, DirectMessageWorkflow, input, dbos.WithEnqueueWorkflowID(wfID))
	if err != nil {
		return DirectMessageResult{}, err
	}
	out, err := durable.AwaitResult(d.work.Runtime(), h, wfID, dbos.WithHandleTimeout(directMessageWait))
	if err != nil {
		if errors.Is(err, dbos.ErrTimeout) {
			return DirectMessageResult{}, connect.NewError(connect.CodeUnavailable, "the organisation's queue is busy; send the message again in a moment")
		}
		return DirectMessageResult{}, err
	}
	if out.ErrCode != 0 {
		return DirectMessageResult{}, connect.NewError(connect.Code(out.ErrCode), out.ErrMsg)
	}
	var id pgtype.UUID
	if id, err = parseID("task", out.Task); err != nil {
		return DirectMessageResult{}, err
	}
	res := DirectMessageResult{Created: out.Created, Reopened: out.Reopened}
	if res.Task, err = d.srv.Load().q.GetTask(ctx, store.GetTaskParams{OrgID: in.Org, ID: id}); err != nil {
		return DirectMessageResult{}, err
	}
	if out.Seq > 0 {
		evs, err := d.srv.Load().q.ListTaskEvents(ctx, store.ListTaskEventsParams{OrgID: in.Org, TaskID: id, AfterSeq: out.Seq - 1, MaxEvents: 1})
		if err == nil && len(evs) == 1 && evs[0].Seq == out.Seq {
			res.Event = &evs[0]
		}
	}
	return res, nil
}
