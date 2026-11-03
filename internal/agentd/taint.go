// SPDX-License-Identifier: Apache-2.0

package agentd

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/acp"
	"github.com/stateracloud/koinon/internal/mcpguard"
	"github.com/stateracloud/koinon/internal/runoutcome"
)

// The leak guard's feed from inside the sandbox (ADR 0084 decision 2, ADR 0091 decision 4): the
// gateway shows the guard only MCP calls, so agentd reports the results of the harness's own tools.
// A finished repository read, search, shell command or file edit sets the private-data leg of the
// agent's session, and its output is fingerprinted with the session's key, in this process; a
// finished fetch sets the untrusted-input leg. Only fingerprints are stored: no tool output leaves
// agentd. A kind the feed does not know (other, which covers MCP tools, a missing kind, or a kind
// newer than this list) sets both legs: ADR 0091 reads what is unclassified as the most exposed.

// The guard must not decide a call on legs that miss a tool result the harness finished before the
// call (ADR 0091 decision 4, amendment 2026-11-03: Andre, option A). The harness's session updates
// reach agentd over ACP while its MCP calls reach the guard through the gateway, so a call can
// overtake the update that finished a shell read. agentd therefore counts each finished tool call
// twice: when its reader reads the update off the harness's stream (FeedRead, before the SDK queues
// it) and once the handler has reported it (FeedDone). The guard waits for the second count to
// reach the first before it reads the legs, and takes both legs when it does not within a bound.

// TaintFeed records one tool result of an agent's sandbox, and the counts the guard waits on
// (mcpguard.Feed).
type TaintFeed interface {
	Report(ctx context.Context, org, agent pgtype.UUID, legs mcpguard.Legs, texts []string) error
	// Start starts the run's counts for this worker's epoch, before the harness runs.
	Start(ctx context.Context, org, run pgtype.UUID, epoch int64) error
	// Read counts a finished tool call read from the harness; Done counts it as reported.
	Read(ctx context.Context, org, run pgtype.UUID, epoch int64) error
	Done(ctx context.Context, org, run pgtype.UUID, epoch int64) error
}

// finishesTool reports whether a session update (its "update" object) finishes a tool call: the
// updates the feed counts. The reader and the handler apply it to the same update.
func finishesTool(update []byte) bool {
	var u struct {
		Update string  `json:"sessionUpdate"`
		ID     string  `json:"toolCallId"`
		Status *string `json:"status"`
	}
	if json.Unmarshal(update, &u) != nil || (u.Update != "tool_call" && u.Update != "tool_call_update") || u.ID == "" || u.Status == nil {
		return false
	}
	return *u.Status == string(acp.ToolCompleted) || *u.Status == string(acp.ToolFailed)
}

// lineFinishesTool is finishesTool for a session/update notification as read off the stream.
func lineFinishesTool(line []byte) bool {
	var m struct {
		Params struct {
			Update json.RawMessage `json:"update"`
		} `json:"params"`
	}
	return json.Unmarshal(line, &m) == nil && finishesTool(m.Params.Update)
}

// feedTimeout bounds one count written from the reader, which has no context of its own.
const feedTimeout = 10 * time.Second

// feedCount writes one count with feedTaint's retries. A count that cannot be written abandons the
// run: the guard must not wait on counts that are wrong (it would pass a call too early).
func (r *runner) feedCount(ctx context.Context, what string, count func(context.Context, pgtype.UUID, pgtype.UUID, int64) error) error {
	var err error
	wait := 200 * time.Millisecond
	for try := 1; try <= taintTries; try++ {
		if err = count(ctx, r.run.OrgID, r.run.ID, r.run.Epoch); err == nil {
			return nil
		}
		if try == taintTries {
			break
		}
		t := time.NewTimer(wait)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			try = taintTries
		}
		wait *= 2
	}
	r.log.Error("the leak guard's feed could not count a tool result; giving the run up", "count", what, "err", err)
	r.own.Fence(runoutcome.FenceAbandon)
	return err
}

// UpdateRead is acp.ReadObserver: the reader read a session update off the harness's stream. A
// finished tool call is counted before the SDK queues it, so the count is ahead of anything the
// harness could send through the gateway after it.
func (h handler) UpdateRead(line []byte) {
	feed := h.r.p.cfg.Taint
	if feed == nil || !lineFinishesTool(line) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), feedTimeout)
	defer cancel()
	_ = h.r.feedCount(ctx, "read", feed.Read) // a failure has fenced the run
}

// feedStart starts the run's counts before the harness runs.
func (r *runner) feedStart(ctx context.Context) error {
	feed := r.p.cfg.Taint
	if feed == nil {
		return nil
	}
	return feed.Start(ctx, r.run.OrgID, r.run.ID, r.run.Epoch)
}

// taintLegs are the legs a finished tool call of each known kind sets; a known kind with no legs
// (switch_mode changes the harness's mode and carries no data) reports nothing. Any other kind
// sets unknownLegs.
var taintLegs = map[acp.ToolKind]mcpguard.Legs{
	acp.ToolKindRead:       {PrivateData: true},
	acp.ToolKindSearch:     {PrivateData: true},
	acp.ToolKindExecute:    {PrivateData: true},
	acp.ToolKindEdit:       {PrivateData: true},
	acp.ToolKindDelete:     {PrivateData: true},
	acp.ToolKindMove:       {PrivateData: true},
	acp.ToolKindThink:      {PrivateData: true},
	acp.ToolKindFetch:      {UntrustedInput: true},
	acp.ToolKindSwitchMode: {},
}

// unknownLegs are the legs of a finished tool call whose kind is not in taintLegs: both.
var unknownLegs = mcpguard.Legs{UntrustedInput: true, PrivateData: true}

// taintTries is how often a report is tried before the feed gives it up (and counts it).
const taintTries = 3

// taintCalls follows the run's tool calls until they finish: a tool_call_update may leave out the
// kind and the content its tool_call or an earlier update carried.
type taintCalls struct {
	mu    sync.Mutex
	calls map[string]*taintCall
}

type taintCall struct {
	kind  acp.ToolKind
	texts []string
}

// taintUpdate is the part of a tool_call or tool_call_update that the feed reads.
type taintUpdate struct {
	Update  string  `json:"sessionUpdate"`
	ID      string  `json:"toolCallId"`
	Kind    *string `json:"kind"`
	Status  *string `json:"status"`
	Content []struct {
		Content *struct {
			Text string `json:"text"`
		} `json:"content"`
		OldText *string `json:"oldText"`
		NewText string  `json:"newText"`
	} `json:"content"`
	RawOutput json.RawMessage `json:"rawOutput"`
}

// finished takes one session update and returns the legs and texts to report when it finishes a
// tool call the feed covers.
func (t *taintCalls) finished(update []byte) (mcpguard.Legs, []string, bool) {
	var u taintUpdate
	if json.Unmarshal(update, &u) != nil || (u.Update != "tool_call" && u.Update != "tool_call_update") || u.ID == "" {
		return mcpguard.Legs{}, nil, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.calls == nil {
		t.calls = map[string]*taintCall{}
	}
	c := t.calls[u.ID]
	if c == nil {
		c = &taintCall{}
		t.calls[u.ID] = c
	}
	if u.Kind != nil {
		c.kind = acp.ToolKind(*u.Kind)
	}
	// Content, when present, replaces what the call showed before (ACP); the raw output adds to it.
	if u.Content != nil {
		c.texts = c.texts[:0]
		for _, b := range u.Content {
			if b.Content != nil && b.Content.Text != "" {
				c.texts = append(c.texts, b.Content.Text)
			}
			if b.OldText != nil && *b.OldText != "" {
				c.texts = append(c.texts, *b.OldText)
			}
			if b.NewText != "" {
				c.texts = append(c.texts, b.NewText)
			}
		}
	}
	if len(u.RawOutput) > 0 {
		var raw any
		if json.Unmarshal(u.RawOutput, &raw) == nil {
			c.texts = appendStrings(c.texts, raw)
		}
	}
	if u.Status == nil || (*u.Status != string(acp.ToolCompleted) && *u.Status != string(acp.ToolFailed)) {
		return mcpguard.Legs{}, nil, false
	}
	delete(t.calls, u.ID)
	legs, ok := taintLegs[c.kind]
	if !ok {
		legs = unknownLegs
	}
	if !legs.PrivateData && !legs.UntrustedInput {
		return mcpguard.Legs{}, nil, false
	}
	return legs, c.texts, true
}

// appendStrings appends every string in a decoded JSON value.
func appendStrings(out []string, v any) []string {
	switch x := v.(type) {
	case string:
		if x != "" {
			out = append(out, x)
		}
	case []any:
		for _, e := range x {
			out = appendStrings(out, e)
		}
	case map[string]any:
		for _, e := range x {
			out = appendStrings(out, e)
		}
	}
	return out
}

// feedTaint reports a finished tool call to the leak guard. A report that fails taintTries times
// abandons the run, as a business event that cannot be written does (runoutcome): the guard must
// not go on deciding without what this session read. Another worker resumes the session, and its
// replay reports the call again (fingerprints and legs are idempotent).
func (r *runner) feedTaint(ctx context.Context, update []byte) error {
	feed := r.p.cfg.Taint
	if feed == nil {
		return nil
	}
	legs, texts, ok := r.taint.finished(update)
	if !ok {
		return nil
	}
	var err error
	wait := 200 * time.Millisecond
	for try := 1; try <= taintTries; try++ {
		if err = feed.Report(ctx, r.run.OrgID, r.run.AgentID, legs, texts); err == nil {
			r.p.cfg.Metrics.taintReport(legs, true)
			return nil
		}
		if try == taintTries {
			break
		}
		t := time.NewTimer(wait)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			try = taintTries
		}
		wait *= 2
	}
	r.p.cfg.Metrics.taintReport(legs, false)
	r.log.Error("the leak guard's feed could not record a tool result; giving the run up", "err", err)
	r.own.Fence(runoutcome.FenceAbandon)
	return err
}
