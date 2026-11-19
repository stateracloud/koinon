// SPDX-License-Identifier: Apache-2.0

package a2aedge

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
)

const (
	// DefaultWait is how long a blocking SendMessage waits for the task to settle. After that it
	// returns the task as it stands, and the client polls GetTask (or sets returnImmediately).
	DefaultWait = 30 * time.Second
	// DefaultPoll is how often the wait reads the task.
	DefaultPoll = 500 * time.Millisecond

	maxMessageBytes   = 64 << 10
	maxMessageIDBytes = 128
	// maxWaiters bounds the blocking sends in flight. Over the bound a send returns at once, as with
	// returnImmediately, and the client reads the task with GetTask.
	maxWaiters = 64
)

type scopeKey struct{}

// WithScope returns ctx carrying the scope of the request.
func WithScope(ctx context.Context, sc Scope) context.Context {
	return context.WithValue(ctx, scopeKey{}, sc)
}

func scopeOf(ctx context.Context) (Scope, error) {
	sc, ok := ctx.Value(scopeKey{}).(Scope)
	if !ok {
		return Scope{}, a2a.ErrUnauthenticated
	}
	return sc, nil
}

// Handler is the A2A request handler of the edge. It implements [a2asrv.RequestHandler] over a
// [Backend] and holds no state.
type Handler struct {
	be      Backend
	wait    time.Duration
	poll    time.Duration
	waiters chan struct{}
}

var _ a2asrv.RequestHandler = (*Handler)(nil)

// NewHandler returns a handler. A wait or poll of zero takes the default.
func NewHandler(be Backend, wait, poll time.Duration) *Handler {
	if wait <= 0 {
		wait = DefaultWait
	}
	if poll <= 0 {
		poll = DefaultPoll
	}
	return &Handler{be: be, wait: wait, poll: poll, waiters: make(chan struct{}, maxWaiters)}
}

func unsupported(what string) error {
	return fmt.Errorf("%w: %s", a2a.ErrUnsupportedOperation, what)
}

// mapErr turns a backend error into an A2A error.
func mapErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrNotFound):
		return a2a.ErrTaskNotFound
	case errors.Is(err, ErrRefused):
		return fmt.Errorf("%w: %s", a2a.ErrUnsupportedOperation, strings.TrimPrefix(err.Error(), ErrRefused.Error()+": "))
	case errors.Is(err, ErrInvalid):
		return fmt.Errorf("%w: %s", a2a.ErrInvalidParams, strings.TrimPrefix(err.Error(), ErrInvalid.Error()+": "))
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	}
	// The text of any other error can carry database detail: log it, and tell the client nothing.
	slog.Error("a2a edge: request failed", "err", err)
	return a2a.ErrInternalError
}

// text joins the text parts of a message. A part of another kind is refused: the agent takes text.
func text(m *a2a.Message) (string, error) {
	var b strings.Builder
	for _, p := range m.Parts {
		t, ok := p.Content.(a2a.Text)
		if !ok {
			return "", fmt.Errorf("%w: the agent takes text parts only", a2a.ErrInvalidParams)
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(string(t))
	}
	s := b.String()
	switch {
	case strings.TrimSpace(s) == "":
		return "", fmt.Errorf("%w: the message has no text", a2a.ErrInvalidParams)
	case len(s) > maxMessageBytes:
		return "", fmt.Errorf("%w: the message is longer than 64 KiB", a2a.ErrInvalidParams)
	}
	return s, nil
}

// SendMessage implements [a2asrv.RequestHandler]. A message without a task ID starts a thread. A
// message with one continues that thread: it answers an input-required task. A task that ended
// takes no message (A2A: a terminal task cannot restart; the client starts a new task).
func (h *Handler) SendMessage(ctx context.Context, req *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	sc, err := scopeOf(ctx)
	if err != nil {
		return nil, err
	}
	m := req.Message
	switch {
	case m == nil:
		return nil, fmt.Errorf("%w: message is required", a2a.ErrInvalidParams)
	case m.Role != a2a.MessageRoleUser:
		return nil, fmt.Errorf("%w: the role of a sent message is ROLE_USER", a2a.ErrInvalidParams)
	case m.ID == "" || len(m.ID) > maxMessageIDBytes:
		return nil, fmt.Errorf("%w: messageId is required and at most %d bytes", a2a.ErrInvalidParams, maxMessageIDBytes)
	}
	body, err := text(m)
	if err != nil {
		return nil, err
	}
	var thread pgtype.UUID
	var reopen bool
	var since int64
	switch {
	case m.TaskID != "":
		// A message to a task answers the open turn. A turn that ended takes no message (A2A: a
		// terminal task cannot restart); the client names the context instead.
		id, n, ok := parseTaskID(string(m.TaskID))
		if !ok {
			return nil, a2a.ErrTaskNotFound
		}
		if m.ContextID != "" && m.ContextID != uuidString(id) {
			return nil, fmt.Errorf("%w: contextId does not match the task", a2a.ErrInvalidParams)
		}
		_, ev, err := h.be.Thread(ctx, sc, id, 0)
		if err != nil {
			return nil, mapErr(err)
		}
		switch last := Turns(ev); {
		case n > last:
			return nil, a2a.ErrTaskNotFound
		case n < last:
			return nil, fmt.Errorf("%w: the task has ended; send a new message (with a new messageId) with its contextId to start a new task", a2a.ErrUnsupportedOperation)
		}
		// The last turn may have completed too. The delivery refuses it under the lock of the task,
		// so that a retry of a message that was taken still gets its answer. It also refuses the
		// message when a follow-up opened a later turn since this check.
		thread = id
		if starts := turnStarts(ev); len(starts) > 0 {
			since = ev[starts[n-1]].Seq
		}
	case m.ContextID != "":
		// A message with a context and no task starts the next turn: a new task in the thread.
		id, ok := parseUUID(m.ContextID)
		if !ok {
			return nil, a2a.ErrTaskNotFound
		}
		if _, _, err := h.be.Thread(ctx, sc, id, 1<<62); err != nil {
			return nil, mapErr(err)
		}
		thread, reopen = id, true
	}
	res, err := h.be.Deliver(ctx, sc, thread, reopen, since, m.ID, body)
	if err != nil {
		return nil, mapErr(err)
	}
	// The message is written. From here the wait only reads.
	marker := int64(0)
	turn := 0
	if res.Event != nil {
		marker = res.Event.Seq
	} else if _, ev, err := h.be.Thread(ctx, sc, res.Task.ID, 0); err == nil {
		// A replay: the answer is the task that holds the message, which an earlier turn may be.
		for _, e := range ev {
			if e.Kind == kindMessage && e.ActorKind == store.TaskActorKindUser && decode(e.Payload).MessageID == m.ID {
				marker = e.Seq
				if t, _ := turnOfSeq(ev, e.Seq); t < Turns(ev) {
					turn = t
				}
				break
			}
		}
	}
	var hist *int
	if req.Config != nil {
		hist = req.Config.HistoryLength
	}
	if turn != 0 || (req.Config != nil && req.Config.ReturnImmediately) {
		return h.view(ctx, sc, res.Task.ID, turn, hist)
	}
	select {
	case h.waiters <- struct{}{}:
		defer func() { <-h.waiters }()
	default:
		return h.view(ctx, sc, res.Task.ID, 0, hist)
	}
	return h.settle(ctx, sc, res.Task.ID, marker, hist)
}

// view reads turn n of a thread as it stands (0 is the last turn).
func (h *Handler) view(ctx context.Context, sc Scope, id pgtype.UUID, n int, hist *int) (*a2a.Task, error) {
	t, ev, err := h.be.Thread(ctx, sc, id, 0)
	if err != nil {
		return nil, mapErr(err)
	}
	if n > Turns(ev) {
		return nil, a2a.ErrTaskNotFound
	}
	return Task(t, ev, n, hist), nil
}

// settle waits until the task is in a state the client has to act on or an end state, reached
// after the message (marker is the sequence number of the message). When the wait ends first, it
// returns the task as it stands. It reads the timeline after the marker only, so a long thread
// costs no more than a short one.
func (h *Handler) settle(ctx context.Context, sc Scope, id pgtype.UUID, marker int64, hist *int) (*a2a.Task, error) {
	deadline := time.NewTimer(h.wait)
	defer deadline.Stop()
	tick := time.NewTicker(h.poll)
	defer tick.Stop()
	for {
		t, ev, err := h.be.Thread(ctx, sc, id, marker)
		if err != nil {
			return nil, mapErr(err)
		}
		if settledAfter(t.State, ev) {
			return h.view(ctx, sc, id, 0, hist)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return h.view(ctx, sc, id, 0, hist)
		case <-tick.C:
		}
	}
}

// settledAfter reports whether the task is in a state the client acts on, reached after the message.
// ev holds the events after the message. An end state counts at once: a task that has ended takes no
// message, so the message did not leave it there. The task state input-required is not enough: it
// can be the state from before a reply. The run that parks on a question or an approval writes
// run.parked (or question.asked) without a status.changed event, so those count as the proof.
func settledAfter(state store.TaskState, ev []store.TaskEvent) bool {
	switch {
	case Ended(state):
		return true
	case state != store.TaskStateInputRequired:
		return false
	}
	for _, e := range ev {
		if e.Kind == kindParked || e.Kind == kindQuestionAsked {
			return true
		}
		if to, ok := statusTo(e); ok && to == store.TaskStateInputRequired {
			return true
		}
	}
	return false
}

// GetTask implements [a2asrv.RequestHandler].
func (h *Handler) GetTask(ctx context.Context, req *a2a.GetTaskRequest) (*a2a.Task, error) {
	sc, err := scopeOf(ctx)
	if err != nil {
		return nil, err
	}
	id, n, ok := parseTaskID(string(req.ID))
	if !ok {
		return nil, a2a.ErrTaskNotFound
	}
	return h.view(ctx, sc, id, n, req.HistoryLength)
}

// CancelTask implements [a2asrv.RequestHandler].
func (h *Handler) CancelTask(ctx context.Context, req *a2a.CancelTaskRequest) (*a2a.Task, error) {
	sc, err := scopeOf(ctx)
	if err != nil {
		return nil, err
	}
	id, n, ok := parseTaskID(string(req.ID))
	if !ok {
		return nil, a2a.ErrTaskNotFound
	}
	_, ev, err := h.be.Thread(ctx, sc, id, 0)
	if err != nil {
		return nil, mapErr(err)
	}
	switch last := Turns(ev); {
	case n > last:
		return nil, a2a.ErrTaskNotFound
	case n < last:
		// An earlier turn ended.
		return nil, a2a.ErrTaskNotCancelable
	}
	if _, err := h.be.Cancel(ctx, sc, id); err != nil {
		if errors.Is(err, ErrRefused) {
			return nil, a2a.ErrTaskNotCancelable
		}
		return nil, mapErr(err)
	}
	return h.view(ctx, sc, id, n, nil)
}

// ListTasks implements [a2asrv.RequestHandler]. The edge does not list: a client keeps the IDs of
// the tasks it started.
func (h *Handler) ListTasks(context.Context, *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	return nil, unsupported("ListTasks")
}

// SubscribeToTask implements [a2asrv.RequestHandler]. Streaming is not in the first slice.
func (h *Handler) SubscribeToTask(context.Context, *a2a.SubscribeToTaskRequest) iter.Seq2[a2a.Event, error] {
	return failSeq(unsupported("SubscribeToTask"))
}

// SendStreamingMessage implements [a2asrv.RequestHandler].
func (h *Handler) SendStreamingMessage(context.Context, *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error] {
	return failSeq(unsupported("SendStreamingMessage"))
}

func failSeq(err error) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) { yield(nil, err) }
}

// GetTaskPushConfig implements [a2asrv.RequestHandler].
func (h *Handler) GetTaskPushConfig(context.Context, *a2a.GetTaskPushConfigRequest) (*a2a.PushConfig, error) {
	return nil, a2a.ErrPushNotificationNotSupported
}

// ListTaskPushConfigs implements [a2asrv.RequestHandler].
func (h *Handler) ListTaskPushConfigs(context.Context, *a2a.ListTaskPushConfigRequest) (*a2a.ListTaskPushConfigResponse, error) {
	return nil, a2a.ErrPushNotificationNotSupported
}

// CreateTaskPushConfig implements [a2asrv.RequestHandler].
func (h *Handler) CreateTaskPushConfig(context.Context, *a2a.PushConfig) (*a2a.PushConfig, error) {
	return nil, a2a.ErrPushNotificationNotSupported
}

// DeleteTaskPushConfig implements [a2asrv.RequestHandler].
func (h *Handler) DeleteTaskPushConfig(context.Context, *a2a.DeleteTaskPushConfigRequest) error {
	return a2a.ErrPushNotificationNotSupported
}

// GetExtendedAgentCard implements [a2asrv.RequestHandler].
func (h *Handler) GetExtendedAgentCard(context.Context, *a2a.GetExtendedAgentCardRequest) (*a2a.AgentCard, error) {
	return nil, a2a.ErrExtendedCardNotConfigured
}
