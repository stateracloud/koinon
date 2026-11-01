// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"errors"
	"strconv"
	"sync/atomic"
	"time"

	connect "connectrpc.com/connect/v2"
	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/jackc/pgx/v5"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/durable"
	"github.com/stateracloud/koinon/internal/triggers"
)

// RunNowWorkflow is the registered name of the workflow that fires a schedule trigger by hand on the
// org-partitioned queue, the queue scheduled fires use, so the org's trigger_concurrency and
// trigger_rate_per_minute bound manual fires too.
const RunNowWorkflow = "koinon.trigger-run-now"

// runNowWait bounds how long a request waits for its fire to leave the queue. The work is not lost
// when it runs out: the workflow ID makes the person's retry within the same second the same fire.
const runNowWait = 30 * time.Second

// RunNowQueue fires schedule triggers by hand through DBOS (api.durable.enabled). Create it with
// RegisterRunNow before the runtime launches and pass it in Options.RunNow; New binds the Server
// the workflow runs on. Without it RunScheduleTrigger fires inline.
type RunNowQueue struct {
	work  *durable.OrgWork
	srv   atomic.Pointer[Server]
	ready chan struct{}
}

type runNowInput struct {
	Org, Agent, Trigger, ActorKind, ActorID, User string
	// Now is the time of the fire in Unix seconds, fixed when the request is made so that a replay
	// is the same fire and creates the same dedupe key.
	Now int64
}

// runNowOutput carries the result and the errors as data, because a workflow's error loses its type
// on the way through Postgres.
type runNowOutput struct {
	Task string
	// Err is "", "not_found", "cannot_run" or "no_rows"; Msg is the error text.
	Err, Msg string
}

const stepRunNow = "run-trigger-now"

var _ = durable.DeclareSteps(RunNowWorkflow, stepRunNow)

// RegisterRunNow registers the run-now workflow on rt.
func RegisterRunNow(rt *durable.Runtime, work *durable.OrgWork) (*RunNowQueue, error) {
	q := &RunNowQueue{work: work, ready: make(chan struct{})}
	err := rt.Register(func(c dbos.Context) error {
		dbos.RegisterWorkflow(c, q.workflow, dbos.WithWorkflowName(RunNowWorkflow))
		return nil
	})
	return q, err
}

func (q *RunNowQueue) bind(s *Server) {
	if q.srv.CompareAndSwap(nil, s) {
		close(q.ready)
	}
}

func (q *RunNowQueue) workflow(c dbos.Context, in runNowInput) (runNowOutput, error) {
	return dbos.RunAsStep(c, func(ctx context.Context) (runNowOutput, error) {
		select {
		case <-q.ready: // a workflow recovered at start can run before the Server exists
		case <-ctx.Done():
			return runNowOutput{}, ctx.Err()
		}
		var r triggers.RunNow
		var err error
		if r.Org, err = parseID("org", in.Org); err != nil {
			return runNowOutput{}, err
		}
		if r.Agent, err = parseID("agent", in.Agent); err != nil {
			return runNowOutput{}, err
		}
		if in.User != "" {
			if r.User, err = parseID("user", in.User); err != nil {
				return runNowOutput{}, err
			}
		}
		r.Trigger, r.Actor, r.Now = in.Trigger, audit.Actor{Kind: in.ActorKind, ID: in.ActorID}, time.Unix(in.Now, 0)
		task, err := q.srv.Load().fireNow(ctx, r)
		switch {
		case errors.Is(err, triggers.ErrTriggerNotFound):
			return runNowOutput{Err: "not_found", Msg: err.Error()}, nil
		case errors.Is(err, triggers.ErrCannotRun):
			return runNowOutput{Err: "cannot_run", Msg: err.Error()}, nil
		case errors.Is(err, pgx.ErrNoRows):
			return runNowOutput{Err: "no_rows", Msg: err.Error()}, nil
		case err != nil:
			return runNowOutput{}, err
		}
		return runNowOutput{Task: idString(task.ID)}, nil
	}, dbos.WithStepName(stepRunNow), dbos.WithStepMaxRetries(3))
}

// remoteError is an error that came back as data from the workflow and still matches its sentinel.
type remoteError struct {
	is  error
	msg string
}

func (e *remoteError) Error() string        { return e.msg }
func (e *remoteError) Is(target error) bool { return target == e.is }

// fire enqueues the fire for its org and waits for the result. The workflow ID names the trigger
// and the second, like the dedupe key, so a retried request attaches to the same workflow.
func (q *RunNowQueue) fire(ctx context.Context, in triggers.RunNow) (store.Task, error) {
	org := idString(in.Org)
	input := runNowInput{
		Org: org, Agent: idString(in.Agent), Trigger: in.Trigger, ActorKind: in.Actor.Kind, ActorID: in.Actor.ID,
		User: idString(in.User), Now: in.Now.Unix(),
	}
	id := "run-now:" + org + ":" + triggers.UUID(in.Agent, in.Trigger).String() + ":" + strconv.FormatInt(input.Now, 10)
	h, err := durable.EnqueueForOrg[runNowOutput](ctx, q.work, org, RunNowWorkflow, input, dbos.WithEnqueueWorkflowID(id))
	if err != nil {
		return store.Task{}, err
	}
	out, err := durable.AwaitResult(q.work.Runtime(), h, id, dbos.WithHandleTimeout(runNowWait))
	if err != nil {
		if errors.Is(err, dbos.ErrTimeout) {
			return store.Task{}, connect.NewError(connect.CodeUnavailable, "the organisation's queue is busy; run the trigger again in a moment")
		}
		return store.Task{}, err
	}
	switch out.Err {
	case "not_found":
		return store.Task{}, &remoteError{is: triggers.ErrTriggerNotFound, msg: out.Msg}
	case "cannot_run":
		return store.Task{}, &remoteError{is: triggers.ErrCannotRun, msg: out.Msg}
	case "no_rows":
		return store.Task{}, pgx.ErrNoRows
	}
	task, err := parseID("task", out.Task)
	if err != nil {
		return store.Task{}, err
	}
	return q.srv.Load().q.GetTask(ctx, store.GetTaskParams{OrgID: in.Org, ID: task})
}
