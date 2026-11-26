// SPDX-License-Identifier: Apache-2.0

package a2aedge

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/stateracloud/koinon/internal/db/store"
)

// Event kinds the edge reads. They are the values the writers use (internal/services,
// internal/questions); the edge only reads them.
const (
	kindMessage       = "message"
	kindStatusChanged = "status.changed"
	kindQuestionAsked = "question.asked"
	kindParked        = "run.parked"
	artifactReplyID   = "reply"
)

type payload struct {
	Text     string `json:"text"`
	Display  string `json:"display"`
	Question string `json:"question"`
	To       string `json:"to"`
	// MessageID is the ID the sender chose.
	MessageID string `json:"message_id"`
}

func decode(b []byte) payload {
	var p payload
	_ = json.Unmarshal(b, &p)
	return p
}

// text is the text a message event shows: the display text when the instruction of a trigger was
// added to the text the agent sees.
func (p payload) text() string {
	if p.Display != "" {
		return p.Display
	}
	return p.Text
}

// statusTo reports the state a status.changed event moved the task to.
func statusTo(e store.TaskEvent) (store.TaskState, bool) {
	if e.Kind != kindStatusChanged {
		return "", false
	}
	var p struct {
		To string `json:"to"`
	}
	if json.Unmarshal(e.Payload, &p) != nil || p.To == "" {
		return "", false
	}
	return store.TaskState(p.To), true
}

// personMessage reports whether an event is a message of a person: a Koinon user, or a person
// without a Koinon account (actor kind external, ADR 0130). Both open and answer turns, so the turns
// agree with the history that historyMessage shows.
func personMessage(e store.TaskEvent) bool {
	return e.Kind == kindMessage && (e.ActorKind == store.TaskActorKindUser || e.ActorKind == store.TaskActorKindExternal)
}

// historyMessage maps a timeline event to an A2A message, or nil when the event is not a
// conversation entry. Messages of people are user messages, also those of a person without a
// Koinon account (actor kind external, ADR 0130); messages of the agent and its questions are agent
// messages.
func historyMessage(thread string, task a2a.TaskID, e store.TaskEvent) *a2a.Message {
	var (
		role a2a.MessageRole
		text string
	)
	switch {
	case personMessage(e):
		role, text = a2a.MessageRoleUser, decode(e.Payload).text()
	case e.Kind == kindMessage && e.ActorKind == store.TaskActorKindAgent:
		role, text = a2a.MessageRoleAgent, decode(e.Payload).text()
	case e.Kind == kindQuestionAsked:
		role, text = a2a.MessageRoleAgent, decode(e.Payload).Question
	default:
		return nil
	}
	if strings.TrimSpace(text) == "" {
		return nil
	}
	m := a2a.NewMessage(role, a2a.NewTextPart(text))
	// The message ID is stable, so a client sees the same message in every GetTask.
	m.ID = thread + ":" + strconv.FormatInt(e.Seq, 10)
	m.TaskID = task
	m.ContextID = thread
	return m
}

// reopenedFrom reports whether the event moved a completed task back to work: a follow-up message
// follows it (internal/services, direct messages).
func reopenedFrom(e store.TaskEvent) bool {
	if e.Kind != kindStatusChanged {
		return false
	}
	var p struct {
		From string `json:"from"`
	}
	return json.Unmarshal(e.Payload, &p) == nil && p.From == string(store.TaskStateCompleted)
}

// turnStarts returns the index of the first message of each turn. A thread is one A2A context and
// each turn is one A2A task (ADR 0123 decision 2). The first message of a person opens turn 1. A
// message of a person after the thread was reopened from completed opens the next turn. Any other
// message of a person answers the open turn (the task waited for input).
func turnStarts(events []store.TaskEvent) []int {
	var starts []int
	reopened := false
	for i, e := range events {
		switch {
		case reopenedFrom(e):
			reopened = true
		case personMessage(e):
			if len(starts) == 0 || reopened {
				starts = append(starts, i)
			}
			reopened = false
		}
	}
	return starts
}

// turnOfSeq returns the turn (counted from 1) that holds the event with sequence number seq, and
// the sequence number of the first message of the turn.
func turnOfSeq(events []store.TaskEvent, seq int64) (turn int, first int64) {
	for n, i := range turnStarts(events) {
		if events[i].Seq > seq {
			break
		}
		turn, first = n+1, events[i].Seq
	}
	return max(turn, 1), first
}

// Turns reports how many turns (A2A tasks) the timeline holds. A thread has at least one.
func Turns(events []store.TaskEvent) int { return max(len(turnStarts(events)), 1) }

// Task maps turn n of a Koinon thread (counted from 1; 0 means the last turn) to an A2A task.
// The thread is the context. An earlier turn ended in completed, because only a completed thread
// takes a follow-up.
//
//   - History is the conversation of the turn, oldest first, cut to the last historyLength entries
//     when historyLength is set (0 means none).
//   - The message that opens a turn after the first references the task of the turn before.
//   - While the task waits, the status message is the open question (input-required) or the last
//     agent reply.
//   - A completed task carries the last agent reply as the artifact "reply".
func Task(t store.Task, events []store.TaskEvent, turn int, historyLength *int) *a2a.Task {
	thread := uuidString(t.ID)
	starts := turnStarts(events)
	last := max(len(starts), 1)
	if turn < 1 || turn > last {
		turn = last
	}
	state := State(t.State)
	if turn < last {
		state = a2a.TaskStateCompleted
	}
	id := taskID(t.ID, turn)
	out := &a2a.Task{ID: id, ContextID: thread, Status: a2a.TaskStatus{State: state}}
	// The events of the turn: from its first message up to the first message of the next turn.
	from, to := 0, len(events)
	if len(starts) > 0 {
		from = starts[turn-1]
		if turn < len(starts) {
			to = starts[turn]
		}
	}
	events = events[from:to]
	if ts := t.UpdatedAt; turn == last && ts.Valid {
		v := ts.Time.UTC()
		out.Status.Timestamp = &v
	} else if n := len(events); n > 0 && events[n-1].CreatedAt.Valid {
		v := events[n-1].CreatedAt.Time.UTC()
		out.Status.Timestamp = &v
	}
	var history []*a2a.Message
	for _, e := range events {
		if m := historyMessage(thread, id, e); m != nil {
			history = append(history, m)
		}
	}
	if turn > 1 && len(history) > 0 && history[0].Role == a2a.MessageRoleUser {
		history[0].ReferenceTasks = []a2a.TaskID{taskID(t.ID, turn-1)}
	}
	// The last agent entry is the status message, when it came after the last user message.
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == a2a.MessageRoleUser {
			break
		}
		if out.Status.Message == nil {
			out.Status.Message = history[i]
		}
	}
	if state == a2a.TaskStateInputRequired && out.Status.Message == nil {
		// A run parked on an approval has no question. The client cannot answer it here.
		out.Status.Message = &a2a.Message{
			ID: thread + ":wait", Role: a2a.MessageRoleAgent, TaskID: id, ContextID: thread,
			Parts: a2a.ContentParts{a2a.NewTextPart("The agent waits for a decision in Koinon.")},
		}
	}
	if state == a2a.TaskStateCompleted && out.Status.Message != nil {
		out.Artifacts = []*a2a.Artifact{{ID: artifactReplyID, Name: "reply", Parts: out.Status.Message.Parts}}
	}
	if historyLength != nil {
		n := max(*historyLength, 0)
		if len(history) > n {
			history = history[len(history)-n:]
		}
	}
	out.History = history
	return out
}
