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

// TaintFeed records one tool result of an agent's sandbox (mcpguard.Feed).
type TaintFeed interface {
	Report(ctx context.Context, org, agent pgtype.UUID, legs mcpguard.Legs, texts []string) error
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
