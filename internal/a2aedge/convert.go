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

// historyMessage maps a timeline event to an A2A message, or nil when the event is not a
// conversation entry. Messages of people are user messages; messages of the agent and its
// questions are agent messages.
func historyMessage(taskID string, e store.TaskEvent) *a2a.Message {
	var (
		role a2a.MessageRole
		text string
	)
	switch {
	case e.Kind == kindMessage && e.ActorKind == store.TaskActorKindUser:
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
	m.ID = taskID + ":" + strconv.FormatInt(e.Seq, 10)
	m.TaskID = a2a.TaskID(taskID)
	m.ContextID = taskID
	return m
}

// Task maps a Koinon task and its timeline to an A2A task.
//
//   - History is the conversation, oldest first, cut to the last historyLength entries when
//     historyLength is set (0 means none).
//   - While the task waits, the status message is the open question (input-required) or the last
//     agent reply.
//   - A completed task carries the last agent reply as the artifact "reply".
func Task(t store.Task, events []store.TaskEvent, historyLength *int) *a2a.Task {
	id := uuidString(t.ID)
	out := &a2a.Task{ID: a2a.TaskID(id), ContextID: id, Status: a2a.TaskStatus{State: State(t.State)}}
	if t.UpdatedAt.Valid {
		ts := t.UpdatedAt.Time.UTC()
		out.Status.Timestamp = &ts
	}
	var history []*a2a.Message
	for _, e := range events {
		if m := historyMessage(id, e); m != nil {
			history = append(history, m)
		}
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
	if t.State == store.TaskStateInputRequired && out.Status.Message == nil {
		// A run parked on an approval has no question. The client cannot answer it here.
		out.Status.Message = &a2a.Message{
			ID: id + ":wait", Role: a2a.MessageRoleAgent, TaskID: a2a.TaskID(id), ContextID: id,
			Parts: a2a.ContentParts{a2a.NewTextPart("The agent waits for a decision in Koinon.")},
		}
	}
	if t.State == store.TaskStateCompleted && out.Status.Message != nil {
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
