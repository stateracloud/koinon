// SPDX-License-Identifier: Apache-2.0

// Package loopguard finds an agent that is stuck in a loop by reading the harness's ACP stream
// (ADR 0030 amendment 2026-11-11, issue 119).
//
// Four patterns are watched. Each counts steps since the last reset:
//
//   - same_action: the same tool call with the same result, ActionObservation times in a row;
//   - same_error: the same tool call failing, ActionError times in a row;
//   - monologue: agent messages with no tool call between them, Monologue in a row;
//   - alternating: two different tool calls, each with the same result every time, taking turns,
//     Alternating steps in a row (A B A B A B).
//
// A step is a batch of tool calls: calls that are announced before any of them has finished are
// parallel calls of one model response and count once. ACP carries no response id for tool calls,
// so a harness that announces the second call of a response only after the first has finished is
// counted as two steps; that can only make a loop show up a step late, never earlier.
//
// Reset is called when a prompt goes out: a new user message, which includes agentd's own nudge,
// starts the count again. The Detector sends the pattern on Signal once and forgets what it has
// seen, so the same stretch of the stream is never reported twice.
//
// The thresholds start at OpenHands' defaults (StuckDetectionThresholds: 4, 3, 3 and 6; software-
// agent-sdk, openhands-sdk/openhands/sdk/conversation/{types,stuck_detector}.py, MIT). Nothing here
// is copied from it.
package loopguard

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/stateracloud/koinon/internal/acp"
)

// Pattern names a stuck pattern. The values are the fixed label values of the loop metric (ADR 0023).
type Pattern string

// The patterns.
const (
	SameAction  Pattern = "same_action"
	SameError   Pattern = "same_error"
	Monologue   Pattern = "monologue"
	Alternating Pattern = "alternating"
)

// Patterns lists every pattern, for the metric's fixed label set.
var Patterns = []Pattern{SameAction, SameError, Monologue, Alternating}

// Describe is the pattern in a sentence, for the nudge and the run's failure reason.
func (p Pattern) Describe() string {
	switch p {
	case SameAction:
		return "repeated the same action and got the same result"
	case SameError:
		return "repeated an action that keeps failing"
	case Monologue:
		return "kept writing messages without calling a tool"
	case Alternating:
		return "alternated between two actions without making progress"
	}
	return "repeated itself"
}

// Thresholds are how many steps of a pattern make a loop.
type Thresholds struct {
	// ActionObservation: the same action with the same result.
	ActionObservation int
	// ActionError: the same action failing.
	ActionError int
	// Monologue: agent messages in a row.
	Monologue int
	// Alternating: steps of two actions taking turns.
	Alternating int
}

// DefaultThresholds are OpenHands' defaults.
var DefaultThresholds = Thresholds{ActionObservation: 4, ActionError: 3, Monologue: 3, Alternating: 6}

// maxSteps bounds the history; the widest pattern needs Alternating steps.
const maxSteps = 64

// step is one batch of tool calls that has finished.
type step struct {
	action, result string
	failed         bool
}

type call struct {
	kind, title string
	input       any
	output      any
	content     any
	status      acp.ToolCallStatus
}

func (c *call) done() bool { return c.status == acp.ToolCompleted || c.status == acp.ToolFailed }

// Detector reads one run's stream. It is safe for concurrent use.
type Detector struct {
	th  Thresholds
	sig chan Pattern

	mu    sync.Mutex
	calls map[string]*call // the batch in progress, by tool call id
	order []string
	steps []step
	// msgs counts message bursts since the last tool call; prev is the kind of the last update that
	// matters and msgID the id of the current message, if the harness sends one.
	msgs  int
	prev  string
	msgID string
}

// New returns a Detector with the given thresholds. A threshold of zero or less turns its pattern off.
func New(th Thresholds) *Detector {
	return &Detector{th: th, sig: make(chan Pattern, 1), calls: map[string]*call{}}
}

// Signal delivers a detected pattern. It holds at most one.
func (d *Detector) Signal() <-chan Pattern { return d.sig }

// Reset forgets what was seen and drops a pending signal: a new prompt starts the count again.
func (d *Detector) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.forget()
	select {
	case <-d.sig:
	default:
	}
}

func (d *Detector) forget() {
	clear(d.calls)
	d.order = d.order[:0]
	d.steps = d.steps[:0]
	d.msgs, d.prev, d.msgID = 0, "", ""
}

// Observe reads one session update.
func (d *Detector) Observe(n acp.SessionNotification) {
	u := n.Update
	d.mu.Lock()
	defer d.mu.Unlock()
	var found Pattern
	switch {
	case u.AgentMessageChunk != nil:
		id := ""
		if m := u.AgentMessageChunk.MessageId; m != nil {
			id = *m
		}
		if d.prev != "message" || id != d.msgID {
			d.msgs++
			found = d.monologue()
		}
		d.prev, d.msgID = "message", id
	case u.AgentThoughtChunk != nil:
		d.prev = "thought"
	case u.ToolCall != nil:
		t := u.ToolCall
		d.msgs, d.prev = 0, "tool"
		c := d.call(string(t.ToolCallId))
		c.kind, c.title = string(t.Kind), t.Title
		c.input, c.output, c.content = pick(c.input, t.RawInput), pick(c.output, t.RawOutput), pickContent(c.content, t.Content)
		if t.Status != "" {
			c.status = t.Status
		}
		found = d.finish()
	case u.ToolCallUpdate != nil:
		t := u.ToolCallUpdate
		d.prev = "tool"
		c := d.call(string(t.ToolCallId))
		if t.Kind != nil {
			c.kind = string(*t.Kind)
		}
		if t.Title != nil {
			c.title = *t.Title
		}
		c.input, c.output, c.content = pick(c.input, t.RawInput), pick(c.output, t.RawOutput), pickContent(c.content, t.Content)
		if t.Status != nil {
			c.status = *t.Status
		}
		found = d.finish()
	}
	if found != "" {
		d.forget()
		select {
		case d.sig <- found:
		default:
		}
	}
}

func pick(old, new any) any {
	if new != nil {
		return new
	}
	return old
}

func pickContent[T any](old any, new []T) any {
	if len(new) > 0 {
		return new
	}
	return old
}

// call returns the call of the batch in progress, adding it when it is new.
func (d *Detector) call(id string) *call {
	if c, ok := d.calls[id]; ok {
		return c
	}
	c := &call{}
	d.calls[id] = c
	d.order = append(d.order, id)
	return c
}

// finish closes the batch when every call in it has finished, and looks for a pattern.
func (d *Detector) finish() Pattern {
	for _, c := range d.calls {
		if !c.done() {
			return ""
		}
	}
	actions, results := make([]string, 0, len(d.calls)), make([]string, 0, len(d.calls))
	failed := true
	for _, c := range d.calls {
		actions = append(actions, digest(c.kind, c.title, c.input))
		results = append(results, digest(string(c.status), c.output, c.content))
		failed = failed && c.status == acp.ToolFailed
	}
	// Parallel calls are one step whatever order they were announced or finished in.
	sort.Strings(actions)
	sort.Strings(results)
	d.steps = append(d.steps, step{action: strings.Join(actions, ","), result: strings.Join(results, ","), failed: failed})
	if len(d.steps) > maxSteps {
		d.steps = slices.Delete(d.steps, 0, len(d.steps)-maxSteps)
	}
	clear(d.calls)
	d.order = d.order[:0]
	return d.tools()
}

// digest is a short, stable hash of values: results can be large.
func digest(parts ...any) string {
	h := sha256.New()
	for _, p := range parts {
		b, err := json.Marshal(p) // maps are marshalled with sorted keys
		if err != nil {
			b = []byte("unmarshalable")
		}
		h.Write(b)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:12])
}

func (d *Detector) monologue() Pattern {
	if d.th.Monologue > 0 && d.msgs >= d.th.Monologue {
		return Monologue
	}
	return ""
}

// tools checks the tool patterns against the end of the history, the most telling first.
func (d *Detector) tools() Pattern {
	s := d.steps
	if n := d.th.ActionError; n > 0 && len(s) >= n {
		tail := s[len(s)-n:]
		if all(tail, func(x step) bool { return x.failed && x.action == tail[0].action }) {
			return SameError
		}
	}
	if n := d.th.ActionObservation; n > 0 && len(s) >= n {
		tail := s[len(s)-n:]
		if all(tail, func(x step) bool { return x == tail[0] }) {
			return SameAction
		}
	}
	if n := d.th.Alternating; n >= 4 && len(s) >= n {
		tail := s[len(s)-n:]
		ok := tail[0] != tail[1]
		for i := 2; ok && i < len(tail); i++ {
			ok = tail[i] == tail[i-2]
		}
		if ok {
			return Alternating
		}
	}
	return ""
}

func all(s []step, f func(step) bool) bool {
	for _, x := range s {
		if !f(x) {
			return false
		}
	}
	return true
}
