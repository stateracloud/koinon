// SPDX-License-Identifier: Apache-2.0

package a2aedge_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/a2aedge"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/services"
)

var taskUUID = pgtype.UUID{Bytes: [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}, Valid: true}

// contextID is the thread. A task is one turn of it: the first turn is taskID.
const (
	contextID = "01020304-0506-0708-090a-0b0c0d0e0f10"
	taskID    = contextID + ".1"
	taskID2   = contextID + ".2"
)

// fake is an in-memory Backend: one thread whose timeline a spec moves forward.
type fake struct {
	mu      sync.Mutex
	task    store.Task
	events  []store.TaskEvent
	seen    map[string]bool
	cancels int
	sc      a2aedge.Scope
}

func newFake() *fake {
	return &fake{seen: map[string]bool{}, task: store.Task{ID: taskUUID, State: store.TaskStateSubmitted, Title: "t"}}
}

func (f *fake) add(kind, payload string) *store.TaskEvent {
	e := store.TaskEvent{Seq: int64(len(f.events) + 1), Kind: kind, Payload: []byte(payload)}
	f.events = append(f.events, e)
	return &f.events[len(f.events)-1]
}

// move records a status change like the services package does.
func (f *fake) move(to store.TaskState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.task.State = to
	f.add("status.changed", `{"to":"`+string(to)+`"}`)
}

func (f *fake) Agent(context.Context, a2aedge.Scope) (a2aedge.Agent, error) {
	return a2aedge.Agent{Name: "scout"}, nil
}

func (f *fake) Deliver(_ context.Context, sc a2aedge.Scope, task pgtype.UUID, reopen bool, mid, text string) (services.DirectMessageResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sc = sc
	if f.seen[mid] {
		return services.DirectMessageResult{Task: f.task}, nil
	}
	// The rules of the real service: a thread that failed or was canceled takes no message. A
	// completed thread is reopened (a new turn) only when the caller says so.
	if task.Valid {
		switch f.task.State {
		case store.TaskStateCanceled, store.TaskStateFailed:
			return services.DirectMessageResult{}, a2aedge.ErrRefused
		case store.TaskStateCompleted:
			if !reopen {
				return services.DirectMessageResult{}, a2aedge.ErrRefused
			}
			f.task.State = store.TaskStateSubmitted
			f.add("status.changed", `{"from":"completed","to":"submitted","reason":"follow_up"}`)
		}
	}
	f.seen[mid] = true
	b, _ := json.Marshal(map[string]string{"text": text})
	e := f.add("message", string(b))
	e.ActorKind = store.TaskActorKindUser
	if !task.Valid {
		f.task.State = store.TaskStateSubmitted
	}
	return services.DirectMessageResult{Task: f.task, Event: e, Created: !task.Valid}, nil
}

func (f *fake) Thread(_ context.Context, _ a2aedge.Scope, id pgtype.UUID, after int64) (store.Task, []store.TaskEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id != taskUUID {
		return store.Task{}, nil, a2aedge.ErrNotFound
	}
	var ev []store.TaskEvent
	for _, e := range f.events {
		if e.Seq > after {
			ev = append(ev, e)
		}
	}
	return f.task, ev, nil
}

// park moves the task to input_required the way a run does: a run.parked event and the state, with
// no status.changed event.
func (f *fake) park(question string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if question != "" {
		f.add("question.asked", `{"question":"`+question+`"}`).ActorKind = store.TaskActorKindAgent
	}
	f.add("run.parked", `{}`)
	f.task.State = store.TaskStateInputRequired
}

func (f *fake) Cancel(_ context.Context, _ a2aedge.Scope, id pgtype.UUID) (store.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id != taskUUID {
		return store.Task{}, a2aedge.ErrNotFound
	}
	if a2aedge.Ended(f.task.State) {
		return store.Task{}, a2aedge.ErrRefused
	}
	f.cancels++
	f.task.State = store.TaskStateCanceled
	f.add("status.changed", `{"to":"canceled"}`)
	return f.task, nil
}

func userMsg(id, body string) *a2a.Message {
	return &a2a.Message{ID: id, Role: a2a.MessageRoleUser, Parts: a2a.ContentParts{a2a.NewTextPart(body)}}
}

func inTask(m *a2a.Message, id string) *a2a.Message {
	m.TaskID = a2a.TaskID(id)
	return m
}

var _ = Describe("Handler", func() {
	var (
		f   *fake
		h   *a2aedge.Handler
		ctx context.Context
	)
	BeforeEach(func() {
		f = newFake()
		h = a2aedge.NewHandler(f, 2*time.Second, 5*time.Millisecond)
		ctx = a2aedge.WithScope(context.Background(), a2aedge.Scope{Agent: pgtype.UUID{Valid: true}})
	})

	send := func(m *a2a.Message, cfg *a2a.SendMessageConfig) (a2a.SendMessageResult, error) {
		return h.SendMessage(ctx, &a2a.SendMessageRequest{Message: m, Config: cfg})
	}

	It("refuses a call without a scope", func() {
		_, err := h.SendMessage(context.Background(), &a2a.SendMessageRequest{Message: userMsg("m1", "hi")})
		Expect(err).To(HaveOccurred())
	})

	It("returns at once with ReturnImmediately", func() {
		res, err := send(userMsg("m1", "hi"), &a2a.SendMessageConfig{ReturnImmediately: true})
		Expect(err).NotTo(HaveOccurred())
		t := res.(*a2a.Task)
		Expect(t.Status.State).To(Equal(a2a.TaskStateSubmitted))
		Expect(string(t.ID)).To(Equal(taskID))
	})

	It("waits for completed and returns the reply as an artifact", func() {
		go func() {
			defer GinkgoRecover()
			time.Sleep(30 * time.Millisecond)
			f.mu.Lock()
			f.add("message", `{"text":"the answer"}`).ActorKind = store.TaskActorKindAgent
			f.mu.Unlock()
			f.move(store.TaskStateCompleted)
		}()
		res, err := send(userMsg("m1", "question"), nil)
		Expect(err).NotTo(HaveOccurred())
		t := res.(*a2a.Task)
		Expect(t.Status.State).To(Equal(a2a.TaskStateCompleted))
		Expect(t.Artifacts).To(HaveLen(1))
		Expect(t.Artifacts[0].Parts[0].Text()).To(Equal("the answer"))
	})

	It("settles on input-required and shows the question", func() {
		go func() {
			time.Sleep(30 * time.Millisecond)
			f.park("which region?")
		}()
		res, err := send(userMsg("m1", "deploy"), nil)
		Expect(err).NotTo(HaveOccurred())
		t := res.(*a2a.Task)
		Expect(t.Status.State).To(Equal(a2a.TaskStateInputRequired))
		Expect(t.Status.Message).NotTo(BeNil())
		Expect(t.Status.Message.Parts[0].Text()).To(ContainSubstring("which region?"))
	})

	It("does not settle on an input-required state from before the reply", func() {
		f.park("which region?")
		f.task.State = store.TaskStateInputRequired
		go func() {
			time.Sleep(60 * time.Millisecond)
			f.move(store.TaskStateCompleted)
		}()
		res, err := send(inTask(userMsg("m2", "eu-west"), taskID), nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.(*a2a.Task).Status.State).To(Equal(a2a.TaskStateCompleted))
	})

	It("gives the answer to a retried reply of a task that has ended since", func() {
		f.park("which region?")
		_, err := send(inTask(userMsg("m2", "eu-west"), taskID), &a2a.SendMessageConfig{ReturnImmediately: true})
		Expect(err).NotTo(HaveOccurred())
		f.move(store.TaskStateCompleted)
		res, err := send(inTask(userMsg("m2", "eu-west"), taskID), nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.(*a2a.Task).Status.State).To(Equal(a2a.TaskStateCompleted))
	})

	It("shows a note when the task waits on a decision and no question was asked", func() {
		f.park("")
		t, err := h.GetTask(ctx, &a2a.GetTaskRequest{ID: a2a.TaskID(taskID)})
		Expect(err).NotTo(HaveOccurred())
		Expect(t.Status.Message.Parts[0].Text()).To(ContainSubstring("decision"))
	})

	It("hides an error of the backend that is not for the client", func() {
		_, err := h.SendMessage(a2aedge.WithScope(context.Background(), a2aedge.Scope{}), &a2a.SendMessageRequest{Message: inTask(userMsg("m", "x"), "01020304-0506-0708-090a-0b0c0d0e0f11.1")})
		Expect(err).To(MatchError(a2a.ErrTaskNotFound))
		f2 := &failing{fake: newFake()}
		h2 := a2aedge.NewHandler(f2, time.Second, time.Millisecond)
		_, err = h2.SendMessage(ctx, &a2a.SendMessageRequest{Message: userMsg("m", "x")})
		Expect(err).To(MatchError(a2a.ErrInternalError))
		Expect(err.Error()).NotTo(ContainSubstring("SQLSTATE"))
	})

	It("returns the task as it stands when the wait ends", func() {
		h = a2aedge.NewHandler(f, 40*time.Millisecond, 5*time.Millisecond)
		res, err := send(userMsg("m1", "slow"), nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.(*a2a.Task).Status.State).To(Equal(a2a.TaskStateSubmitted))
	})

	It("does not write a message twice for one message ID", func() {
		_, err := send(userMsg("same", "once"), &a2a.SendMessageConfig{ReturnImmediately: true})
		Expect(err).NotTo(HaveOccurred())
		_, err = send(userMsg("same", "once"), &a2a.SendMessageConfig{ReturnImmediately: true})
		Expect(err).NotTo(HaveOccurred())
		n := 0
		for _, e := range f.events {
			if e.Kind == "message" {
				n++
			}
		}
		Expect(n).To(Equal(1))
	})

	It("refuses bad input", func() {
		_, err := send(nil, nil)
		Expect(err).To(MatchError(a2a.ErrInvalidParams))
		_, err = send(userMsg("", "x"), nil)
		Expect(err).To(MatchError(a2a.ErrInvalidParams))
		_, err = send(userMsg(strings.Repeat("a", 129), "x"), nil)
		Expect(err).To(MatchError(a2a.ErrInvalidParams))
		agent := userMsg("m", "x")
		agent.Role = a2a.MessageRoleAgent
		_, err = send(agent, nil)
		Expect(err).To(MatchError(a2a.ErrInvalidParams))
	})

	It("refuses a message to an ended task and an unknown task", func() {
		f.move(store.TaskStateCompleted)
		_, err := send(inTask(userMsg("m", "x"), taskID), nil)
		Expect(err).To(MatchError(a2a.ErrUnsupportedOperation))
		_, err = send(inTask(userMsg("m", "x"), "00000000-0000-0000-0000-000000000009.1"), nil)
		Expect(err).To(MatchError(a2a.ErrTaskNotFound))
		_, err = send(inTask(userMsg("m", "x"), contextID), nil)
		Expect(err).To(MatchError(a2a.ErrTaskNotFound))
	})

	Describe("turns", func() {
		// A thread is one context. Each turn is a task. The first turn is answered and completed.
		firstTurn := func() {
			_, err := send(userMsg("m1", "first"), &a2a.SendMessageConfig{ReturnImmediately: true})
			Expect(err).NotTo(HaveOccurred())
			f.mu.Lock()
			f.add("message", `{"text":"first answer"}`).ActorKind = store.TaskActorKindAgent
			f.mu.Unlock()
			f.move(store.TaskStateCompleted)
		}
		next := func(id, body string) *a2a.Task {
			m := userMsg(id, body)
			m.ContextID = contextID
			res, err := send(m, &a2a.SendMessageConfig{ReturnImmediately: true})
			Expect(err).NotTo(HaveOccurred())
			return res.(*a2a.Task)
		}

		It("refuses a message to a completed task", func() {
			firstTurn()
			_, err := send(inTask(userMsg("m2", "more"), taskID), nil)
			Expect(err).To(MatchError(a2a.ErrUnsupportedOperation))
			Expect(f.task.State).To(Equal(store.TaskStateCompleted))
		})

		It("starts a new task in the same thread for a message with the context and no task", func() {
			firstTurn()
			t := next("m2", "second")
			Expect(string(t.ID)).To(Equal(taskID2))
			Expect(t.ContextID).To(Equal(contextID))
			Expect(t.Status.State).To(Equal(a2a.TaskStateSubmitted))
			Expect(t.History).To(HaveLen(1))
			Expect(t.History[0].Parts[0].Text()).To(Equal("second"))
			Expect(f.task.ID).To(Equal(taskUUID))
		})

		It("points the new task at the task before it", func() {
			firstTurn()
			t := next("m2", "second")
			Expect(t.History[0].ReferenceTasks).To(Equal([]a2a.TaskID{taskID}))
			// The first turn has no reference.
			old, err := h.GetTask(ctx, &a2a.GetTaskRequest{ID: taskID})
			Expect(err).NotTo(HaveOccurred())
			Expect(old.History[0].ReferenceTasks).To(BeEmpty())
		})

		It("keeps the task of an earlier turn completed, with its own history and reply", func() {
			firstTurn()
			next("m2", "second")
			old, err := h.GetTask(ctx, &a2a.GetTaskRequest{ID: taskID})
			Expect(err).NotTo(HaveOccurred())
			Expect(old.Status.State).To(Equal(a2a.TaskStateCompleted))
			Expect(old.History).To(HaveLen(2))
			Expect(old.Artifacts[0].Parts[0].Text()).To(Equal("first answer"))
			_, err = send(inTask(userMsg("m3", "late"), taskID), nil)
			Expect(err).To(MatchError(a2a.ErrUnsupportedOperation))
			_, err = h.CancelTask(ctx, &a2a.CancelTaskRequest{ID: taskID})
			Expect(err).To(MatchError(a2a.ErrTaskNotCancelable))
			_, err = h.GetTask(ctx, &a2a.GetTaskRequest{ID: contextID + ".3"})
			Expect(err).To(MatchError(a2a.ErrTaskNotFound))
		})

		It("answers a question inside the same task", func() {
			_, err := send(userMsg("m1", "deploy"), &a2a.SendMessageConfig{ReturnImmediately: true})
			Expect(err).NotTo(HaveOccurred())
			f.park("which region?")
			res, err := send(inTask(userMsg("m2", "eu-west"), taskID), &a2a.SendMessageConfig{ReturnImmediately: true})
			Expect(err).NotTo(HaveOccurred())
			Expect(string(res.(*a2a.Task).ID)).To(Equal(taskID))
		})

		It("knows no task ID that is not a thread and a plain turn number", func() {
			for _, id := range []string{contextID, contextID + ".0", contextID + ".01", contextID + ".+1", contextID + ".x", ".1"} {
				_, err := h.GetTask(ctx, &a2a.GetTaskRequest{ID: a2a.TaskID(id)})
				Expect(err).To(MatchError(a2a.ErrTaskNotFound), id)
			}
		})

		It("refuses a context of another thread and a context with a task that does not match", func() {
			m := userMsg("m2", "x")
			m.ContextID = "00000000-0000-0000-0000-000000000009"
			_, err := send(m, nil)
			Expect(err).To(MatchError(a2a.ErrTaskNotFound))
			m = inTask(userMsg("m3", "x"), taskID)
			m.ContextID = "00000000-0000-0000-0000-000000000009"
			_, err = send(m, nil)
			Expect(err).To(MatchError(a2a.ErrInvalidParams))
		})
	})

	It("gets and cancels a task", func() {
		t, err := h.GetTask(ctx, &a2a.GetTaskRequest{ID: a2a.TaskID(taskID)})
		Expect(err).NotTo(HaveOccurred())
		Expect(t.Status.State).To(Equal(a2a.TaskStateSubmitted))
		t, err = h.CancelTask(ctx, &a2a.CancelTaskRequest{ID: a2a.TaskID(taskID)})
		Expect(err).NotTo(HaveOccurred())
		Expect(t.Status.State).To(Equal(a2a.TaskStateCanceled))
		_, err = h.CancelTask(ctx, &a2a.CancelTaskRequest{ID: a2a.TaskID(taskID)})
		Expect(err).To(MatchError(a2a.ErrTaskNotCancelable))
	})

	It("reports an unsupported operation for list and push configs", func() {
		_, err := h.ListTasks(ctx, &a2a.ListTasksRequest{})
		Expect(err).To(HaveOccurred())
		_, err = h.CreateTaskPushConfig(ctx, &a2a.PushConfig{})
		Expect(err).To(MatchError(a2a.ErrPushNotificationNotSupported))
	})
})

// failing returns an error with database detail from Deliver.
type failing struct{ *fake }

func (f *failing) Deliver(context.Context, a2aedge.Scope, pgtype.UUID, bool, string, string) (services.DirectMessageResult, error) {
	return services.DirectMessageResult{}, errors.New("ERROR: invalid byte sequence for encoding UTF8: 0x00 (SQLSTATE 22021)")
}
