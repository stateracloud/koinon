// SPDX-License-Identifier: Apache-2.0

package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
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
// agentd. A kind the feed does not know (other, a missing kind, or a kind newer than this list;
// not Koinon's MCP tools, whose legs the gateway sets) sets both legs: ADR 0091 reads what is unclassified as the most exposed.

// The guard must not decide a call on legs that miss a tool result the harness finished before the
// call (ADR 0091 decision 4, amendment 2026-11-03: procoduck, option A). The harness's session updates
// reach agentd over ACP while its MCP calls reach the guard through the gateway, so a call can
// overtake the update that finished a shell read. agentd therefore counts each finished tool call
// twice: when its reader reads the update off the harness's stream (read, before the SDK queues it)
// and once the handler has reported it (done). The guard waits for done to reach read before it
// reads the legs, and takes both legs when it does not within a bound.
//
// The counts live in memory (feedCounts) and one writer per harness copies them to the database,
// in order and as absolute values. The reader only adds to the count and never waits for the
// database: while a slow report holds the handler, the next result in the pipe is still read and
// counted, so the guard sees that the session is behind.

// TaintFeed records one tool result of an agent's sandbox, and the counts the guard waits on
// (mcpguard.Feed).
type TaintFeed interface {
	Report(ctx context.Context, org, agent pgtype.UUID, legs mcpguard.Legs, texts []string) error
	// Start starts the run's counts for this worker's epoch, before the harness runs.
	Start(ctx context.Context, org, run pgtype.UUID, epoch int64) error
	// Counts records the worker's counts: finished tool calls read from the harness, and the first
	// done of them reported. They are absolute and only grow; mcpguard.ErrFeedFenced when a newer
	// epoch owns the feed.
	Counts(ctx context.Context, org, run pgtype.UUID, epoch, read, done int64) error
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

// feedTimeout bounds one write of the counts.
const feedTimeout = 10 * time.Second

// feedCounts are one harness's counts for the leak guard, and their writer.
type feedCounts struct {
	r    *runner
	feed TaintFeed
	wake chan struct{}

	mu         sync.Mutex
	read, done int64
}

// add counts finished tool calls read (by the reader) or done (by the handler) and wakes the
// writer. It never waits for the database.
func (c *feedCounts) add(read, done int64) {
	c.mu.Lock()
	c.read += read
	c.done += done
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *feedCounts) snapshot() (int64, int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.read, c.done
}

// write copies the counts to the database until ctx ends, the latest values each time: a write
// that lags covers every count added meanwhile. A write that fails taintTries times abandons the
// run (the guard must not wait on counts that are wrong, or it would pass a call too early); a
// newer epoch owning the feed ends the writer, as the worker is fenced.
func (c *feedCounts) write(ctx context.Context) {
	var wrote [2]int64
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.wake:
		}
		read, done := c.snapshot()
		if wrote == [2]int64{read, done} {
			continue
		}
		var err error
		wait := 200 * time.Millisecond
		for try := 1; try <= taintTries; try++ {
			wctx, cancel := context.WithTimeout(ctx, feedTimeout)
			err = c.feed.Counts(wctx, c.r.run.OrgID, c.r.run.ID, c.r.run.Epoch, read, done)
			cancel()
			if err == nil || errors.Is(err, mcpguard.ErrFeedFenced) || ctx.Err() != nil || try == taintTries || permanentFeedErr(err) {
				break
			}
			t := time.NewTimer(wait)
			select {
			case <-t.C:
			case <-ctx.Done():
				t.Stop()
			}
			wait *= 2
		}
		switch {
		case err == nil:
			wrote = [2]int64{read, done}
		case ctx.Err() != nil:
			return
		case errors.Is(err, mcpguard.ErrFeedFenced):
			c.r.log.Warn("the leak guard's feed belongs to a newer worker of the run; this worker stops counting")
			c.r.own.Fence(runoutcome.FenceLost)
			return
		case c.r.feedGiveUp(err):
			return
		default:
			c.r.log.Error("the leak guard's feed could not count tool results; giving the run up", "err", err)
			c.r.own.Fence(runoutcome.FenceAbandon)
			return
		}
	}
}

// UpdateRead is acp.ReadObserver: the reader read a session update off the harness's stream. A
// finished tool call is counted before the SDK queues it, so the count is ahead of anything the
// harness could send through the gateway after it.
func (h handler) UpdateRead(line []byte) {
	c := h.r.feedCounts.Load()
	if c == nil || !lineFinishesTool(line) {
		return
	}
	c.add(1, 0)
}

// feedStart starts the run's counts before the harness runs, and their writer until ctx ends.
func (r *runner) feedStart(ctx context.Context) error {
	feed := r.p.cfg.Taint
	if feed == nil {
		return nil
	}
	if err := feed.Start(ctx, r.run.OrgID, r.run.ID, r.run.Epoch); err != nil {
		return err
	}
	c := &feedCounts{r: r, feed: feed, wake: make(chan struct{}, 1)}
	r.feedCounts.Store(c)
	go c.write(ctx)
	return nil
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

// An MCP tool's result does not set legs here. It passed the gateway, and the guard set the
// session's legs there from the tool's own declaration (ADR 0091 decision 4, old issue 643): reporting the
// ACP kind other as well would hold both legs for the first MCP call of a home generation and
// undo every declaration. The call is recognised by the harness's title prefix
// (harness.Descriptor.MCPToolPrefix); a call of kind other without it still sets both legs.

// unknownLegs are the legs of a finished tool call whose kind is not in taintLegs: both.
var unknownLegs = mcpguard.Legs{UntrustedInput: true, PrivateData: true}

// taintTries is how often a report is tried before the feed gives it up (and counts it).
const taintTries = 3

// taintCalls follows the run's tool calls until they finish: a tool_call_update may leave out the
// kind and the content its tool_call or an earlier update carried.
type taintCalls struct {
	mu        sync.Mutex
	calls     map[string]*taintCall
	mcpPrefix string
	mcpRE     *regexp.Regexp
}

// setMCPPrefix sets the title prefix of the harness's MCP tool calls ("" for none).
func (t *taintCalls) setMCPPrefix(p string) {
	t.mu.Lock()
	t.mcpPrefix = p
	t.mcpRE = nil
	if p != "" {
		t.mcpRE = regexp.MustCompile("^" + regexp.QuoteMeta(p) + `[A-Za-z0-9_.-]+$`)
	}
	t.mu.Unlock()
}

type taintCall struct {
	kind  acp.ToolKind
	title string
	named bool
	texts []string
}

// taintUpdate is the part of a tool_call or tool_call_update that the feed reads.
type taintUpdate struct {
	Update  string  `json:"sessionUpdate"`
	ID      string  `json:"toolCallId"`
	Kind    *string `json:"kind"`
	Title   *string `json:"title"`
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
	// Only the opening tool_call names the tool (OpenCode); a later update's title is free text the
	// model can write, so it never replaces the first one.
	if u.Update == "tool_call" && !c.named {
		c.named = true
		if u.Title != nil {
			c.title = *u.Title
		}
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
	if c.kind == acp.ToolKindOther && t.mcpRE != nil && t.mcpRE.MatchString(c.title) {
		return mcpguard.Legs{}, nil, false
	}
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
		if try == taintTries || permanentFeedErr(err) {
			break // a refusal that cannot change is not retried
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
	if r.feedGiveUp(err) {
		return err
	}
	r.log.Error("the leak guard's feed could not record a tool result; giving the run up", "err", err)
	r.own.Fence(runoutcome.FenceAbandon)
	return err
}

// permanentFeedErr reports whether a feed error cannot succeed on retry (a constraint or
// cardinality violation, bad input). Failing closed stays: such a run is failed, not run unwatched.
func permanentFeedErr(err error) bool {
	_, ok := runoutcome.PermanentSQLState(err)
	return ok
}

// feedGiveUp fails the run once, with a named reason, when err is permanent: a takeover would feed
// the same data to the same statement and meet the same refusal (koinon#123, ADR 0091 decision 4).
// It reports whether it did; a transient error is left to the caller, which abandons the run for
// another worker to resume.
func (r *runner) feedGiveUp(err error) bool {
	code, ok := runoutcome.PermanentSQLState(err)
	if !ok {
		return false
	}
	r.log.Error("the leak guard's feed refused a tool result for good; failing the run", "sqlstate", code, "err", err)
	r.feedFailure.CompareAndSwap(nil, &failure{reason: feedFailReason(code), class: classFatal})
	r.own.Fence(runoutcome.FenceFailed)
	return true
}

// feedFailReason is the run's failure reason for a permanent feed error: the SQLSTATE only, never the
// error's text, which may quote what the session read.
func feedFailReason(code string) string {
	return "the leak guard could not record a tool result and retrying cannot help (database error " + code + "); the run is failed instead of retried"
}
