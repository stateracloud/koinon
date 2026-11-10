// SPDX-License-Identifier: Apache-2.0

package durable

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
)

// Ordering (ADR 0066, practice B2): state changes (a park, a child task, an inbox item) commit
// first; only then does anything notify or start a sandbox. A notification is a DBOS workflow
// started after the commit, with an ID derived from the effect, so
//   - a rollback sends nothing (the workflow is never started),
//   - a failure after the commit and before or during the notification is retried as a DBOS step
//     and recovered after a restart, and the same ID started twice is one workflow,
//   - the notification happens once, as the step is checkpointed when it succeeds.
//
// A handler must still be idempotent: a crash between its side effect and its checkpoint runs it
// again.

// NotifyWorkflow is the registered name of the notification workflow.
const NotifyWorkflow = "koinon.notify"

// notifyStep names the one step of the notification workflow: "deliver:<kind>", one per handler kind.
// TestWorkflowSteps records the name with a placeholder, because a new kind is a new workflow input, not a
// change to the steps of workflows already recorded.
const notifyStepPrefix = "deliver:"

// notifyRetries is how often a failing handler is retried inside the step, with DBOS's default
// backoff (100 ms doubling to 5 s); after that the workflow ends in error and is visible in DBOS.
const notifyRetries = 8

// A Notification is one thing to tell or start after a commit.
type Notification struct {
	// ID is unique per effect and stable across retries, for example "wake.agent:task-created:<task>".
	// It becomes the DBOS workflow ID, which is what makes a second Send a no-op.
	ID string
	// Kind picks the Handler.
	Kind string
	// Payload is the handler's input, JSON.
	Payload string
}

// A Handler delivers a Notification: it wakes a sandbox, posts to a chat connector, and so on.
type Handler func(ctx context.Context, n Notification) error

// A Sender sends notifications after a commit.
type Sender interface {
	Send(ctx context.Context, n Notification) error
}

// A Notifier delivers Notifications as DBOS steps when it has a Runtime, and inline (once, no
// retry, errors logged) when it has none, so a deployment without DBOS (api.durable.enabled off)
// keeps its best-effort behaviour.
type Notifier struct {
	rt  *Runtime
	log *slog.Logger

	mu       sync.RWMutex
	handlers map[string]Handler
}

// NewNotifier returns a Notifier on rt, or an inline one when rt is nil. Add it to the runtime
// with rt.Register(n.Registrar()) before Launch.
func NewNotifier(rt *Runtime, log *slog.Logger) *Notifier {
	if log == nil {
		log = slog.Default()
	}
	return &Notifier{rt: rt, log: log, handlers: map[string]Handler{}}
}

// Handle sets the handler for kind. Register handlers before Launch so a workflow recovered at
// start finds its handler.
func (n *Notifier) Handle(kind string, h Handler) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.handlers[kind] = h
}

func (n *Notifier) handler(kind string) (Handler, error) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	h, ok := n.handlers[kind]
	if !ok {
		return nil, fmt.Errorf("durable: no handler for notification kind %q", kind)
	}
	return h, nil
}

// Registrar registers the notification workflow for Runtime.Register.
func (n *Notifier) Registrar() Registrar {
	return func(c dbos.Context) error {
		dbos.RegisterWorkflow(c, n.workflow, dbos.WithWorkflowName(NotifyWorkflow))
		return nil
	}
}

func (n *Notifier) workflow(c dbos.Context, note Notification) (string, error) {
	_, err := dbos.RunAsStep(c, func(ctx context.Context) (string, error) {
		h, err := n.handler(note.Kind)
		if err != nil {
			return "", err
		}
		return "", h(ctx, note)
	}, dbos.WithStepName(notifyStepPrefix+note.Kind), dbos.WithStepMaxRetries(notifyRetries))
	return "", err
}

// Send starts the notification. Call it after the transaction that made the state change has
// committed. With a Runtime it returns once the workflow is recorded in Postgres (the delivery is
// then durable); without one it runs the handler now. Sending an ID that was sent before does
// nothing.
func (n *Notifier) Send(ctx context.Context, note Notification) error {
	if note.ID == "" || note.Kind == "" {
		return errors.New("durable: a notification needs an ID and a kind")
	}
	if n.rt == nil || n.rt.Context() == nil {
		h, err := n.handler(note.Kind)
		if err != nil {
			return err
		}
		return h(ctx, note)
	}
	_, err := dbos.RunWorkflow(n.rt.Context(), n.workflow, note, dbos.WithWorkflowID(note.ID))
	if err != nil {
		return fmt.Errorf("durable: start notification %s: %w", note.ID, err)
	}
	return nil
}

// A SendError reports that a notification could not be sent after its transaction committed. The
// state change stands; the caller may log it, or send again with the same IDs.
type SendError struct{ Err error }

func (e *SendError) Error() string { return "durable: send after commit: " + e.Err.Error() }
func (e *SendError) Unwrap() error { return e.Err }

// Commit runs fn in a transaction on pool. fn returns the notifications that its state changes
// call for; they are sent only after the transaction committed, never on a rollback. fn also gets
// the audit batch: it adds the audit events of its change to b, and the audit module writes them in
// lock order just before the commit (ADR 0100 decision 1.7). A change with nothing to audit leaves
// b empty. A failed send comes back as a *SendError (and a failed transaction as its own error), so
// a caller can tell "nothing happened" from "committed, not yet notified". With a Runtime a recorded
// workflow retries itself; only the start of the workflow can fail here.
func Commit(ctx context.Context, pool *pgxpool.Pool, send Sender, fn func(q *store.Queries, b audit.Batch) ([]Notification, error)) error {
	var notes []Notification
	_, err := audit.InTx(ctx, pool, func(q *store.Queries, b audit.Batch) error {
		var err error
		notes, err = fn(q, b)
		return err
	})
	return afterCommit(ctx, send, notes, err)
}

// afterCommit sends notes when the transaction committed (err is nil) and reports a failed send as a
// *SendError.
func afterCommit(ctx context.Context, send Sender, notes []Notification, err error) error {
	if err != nil {
		return err
	}
	var errs []error
	for _, note := range notes {
		if err := send.Send(ctx, note); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return &SendError{Err: errors.Join(errs...)}
	}
	return nil
}
