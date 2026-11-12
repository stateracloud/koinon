// SPDX-License-Identifier: Apache-2.0

package agentd

// The run's spans (ADR 0074, decisions 2 and 3): one trace per turn, a root span
// "invoke_agent {agent}" and a child "execute_tool {tool}" for every ACP tool call, a koinon.park
// event where a turn ends in a park, and a span link and koinon.wait event on the turn that resumes
// it. Tracing never fails or delays a run: every error here is logged and the turn goes untraced.
// No database call is made while the mutex that guards the turn is held, and the ones on the run's
// path are cut short when the worker is fenced.

import (
	"cmp"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/stateracloud/koinon/internal/acp"
	"github.com/stateracloud/koinon/internal/agentd/turnpolicy"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/runoutcome"
	"github.com/stateracloud/koinon/internal/tracedest"
	"github.com/stateracloud/koinon/internal/tracing/export"
	"github.com/stateracloud/koinon/internal/tracing/semconv"
)

// runTrace is the tracing state of one traced run under one epoch. A nil *runTrace is an untraced
// run: every method is a no-op on it.
type runTrace struct {
	sink    *traceSink
	q       *store.Queries
	run     store.Run
	log     interface{ Warn(msg string, args ...any) }
	decider TraceDecider
	// fenced closes when the worker may no longer write; database calls on the run's path end then.
	fenced <-chan struct{}

	mu sync.Mutex
	id identity
	// mcpMissing is set before the first turn: the names of the allowed MCP servers that were not
	// serving tools when the harness started, and the check that found them (mcpwait.go).
	mcpMissing []string
	mcpAttempt int
	turn       *turnTrace
	parkedOn   string // the park reason endTurn chose, until finish
}

// identity is what the root span says about the agent and the task.
type identity struct {
	agentID, agentName, agentVersion, model, task string
	requestedKind, requestedID                    string
}

// turnTrace is the turn in progress.
type turnTrace struct {
	row     store.RunTurn
	root    trace.Span
	rootCtx context.Context
	reason  string // how the turn ended, as far as turn() knows
	tools   map[string]*toolTrace
	done    map[string]bool
	// content: this turn carries content, by the org's level read when the turn began.
	content bool
	parts   []msgPart
	size    int
	input   string
}

type toolTrace struct {
	span       trace.Span
	name, kind string
	status     string
	// inputJSON and outputJSON are the capped JSON of the arguments and result, kept only above
	// content level none. The raw values are not kept.
	inputJSON, outputJSON string
	// target and childRun are what ask_agent's span needs (provisional, see endTool).
	target, childRun string
}

// msgPart is a part of an assembled message: text or reasoning.
type msgPart struct {
	Type    string `json:"type"`
	Content string `json:"content"`
}

// chatMessage is a message of the GenAI messages JSON schema (gen_ai.input.messages and
// gen_ai.output.messages at the pinned commit).
type chatMessage struct {
	Role         string    `json:"role"`
	Parts        []msgPart `json:"parts"`
	FinishReason string    `json:"finish_reason,omitempty"`
}

// traceStart decides, once per run, whether it is traced (decision 1). It runs when the worker has
// claimed the run, before anything else of the run happens.
func (p *Pool) traceStart(ctx context.Context, run store.Run, fenced <-chan struct{}, log interface{ Warn(msg string, args ...any) }) *runTrace {
	if p.sink == nil {
		return nil
	}
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	traced, err := p.cfg.Tracing.Decider.DecideRun(dctx, run.OrgID, run.ID)
	if err != nil {
		log.Warn("deciding whether the run is traced failed; it is not traced", "err", err)
		return nil
	}
	if !traced {
		return nil
	}
	return &runTrace{sink: p.sink, q: p.q, run: run, log: log, fenced: fenced, decider: p.cfg.Tracing.Decider}
}

// dbctx bounds a database call of the run's path: d at most, and until the worker is fenced.
func (t *runTrace) dbctx(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(parent, d)
	go func() {
		select {
		case <-t.fenced:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

// identify records what the root span says about the agent, once the run has loaded it.
func (t *runTrace) identify(agent store.Agent, version store.AgentVersion, task store.Task, model string) {
	if t == nil {
		return
	}
	id := identity{
		agentID: agent.ID.String(), agentName: agent.DisplayName, agentVersion: fmt.Sprint(version.Version),
		model: model, task: task.ID.String(), requestedKind: string(task.CreatedByKind),
	}
	switch task.CreatedByKind {
	case store.TaskCreatorKindUser:
		id.requestedID = uuidString(task.CreatedByUserID)
	case store.TaskCreatorKindAgent:
		id.requestedID = uuidString(task.CreatedByAgentID)
	case store.TaskCreatorKindTrigger:
		id.requestedID = uuidString(task.CreatedByTriggerID)
	}
	t.mu.Lock()
	t.id = id
	t.mu.Unlock()
}

// noteMCPMissing records the allowed MCP servers found missing before the harness started; the next
// root span carries them, once. Only the recovered case (attempt 1) reaches a root span: a run that
// fails on the second miss (attempt 2) ends before its first turn, so it has no root span and the
// run.mcp_server_missing event (attempt 2) is its record.
func (t *runTrace) noteMCPMissing(names []string, attempt int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.mcpMissing, t.mcpAttempt = names, attempt
	t.mu.Unlock()
}

func uuidString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return u.String()
}

func (t *runTrace) common(id identity, turn int32) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String(semconv.GenAIConversationID, t.run.ID.String()),
		attribute.String(semconv.SessionID, t.run.ID.String()),
		attribute.String(semconv.KoinonRun, t.run.ID.String()),
		attribute.String(semconv.KoinonOrg, t.run.OrgID.String()),
		attribute.String(semconv.KoinonTask, id.task),
		attribute.Int(semconv.KoinonTurn, int(turn)),
	}
}

// traceparentMeta is the _meta of session/prompt: a hint for harnesses that honour it, never the
// source of attribution (decision 2).
func traceparentMeta(trace [16]byte, span [8]byte) map[string]any {
	return map[string]any{"traceparent": "00-" + hex.EncodeToString(trace[:]) + "-" + hex.EncodeToString(span[:]) + "-01"}
}

// beginTurn starts the trace of a turn: a new row with a new trace ID, and the root span. It returns
// the _meta to send with the prompt (nil when the turn is not traced). A turn still open is ended
// with the reason turn() recorded for it. Turn rows the previous owner left open are ended as
// interrupted by the insert, which also fails for a worker that is no longer the run's owner.
func (t *runTrace) beginTurn(ctx context.Context, prompt string) map[string]any {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	prev, id, missing, attempt := t.turn, t.id, t.mcpMissing, t.mcpAttempt
	// The notice belongs to the first turn's root span only.
	t.mcpMissing, t.mcpAttempt = nil, 0
	t.turn, t.parkedOn = nil, ""
	t.mu.Unlock()
	if prev != nil {
		t.closeTurn(ctx, prev, &turnEnd{reason: cmp.Or(prev.reason, "continued")}, true)
	}

	traceID := export.NewTraceID()
	dctx, cancel := t.dbctx(ctx, 5*time.Second)
	defer cancel()
	row, err := t.q.InsertOwnedRunTurn(dctx, store.InsertOwnedRunTurnParams{
		OrgID: t.run.OrgID, RunID: t.run.ID, Epoch: t.run.Epoch, TraceID: traceID[:],
	})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) && dctx.Err() == nil {
			t.log.Warn("recording the turn failed; it is not traced", "err", err)
		}
		return nil
	}
	now := time.Now()
	rootID := RootSpanID(traceID)
	attrs := append(t.common(id, row.Seq),
		attribute.String(semconv.GenAIOperationName, semconv.OperationInvokeAgent),
		attribute.String(semconv.GenAIAgentID, id.agentID),
		attribute.String(semconv.GenAIAgentName, id.agentName),
		attribute.String(semconv.GenAIAgentVersion, id.agentVersion),
		attribute.String(semconv.GenAIRequestModel, id.model),
		attribute.String(semconv.KoinonRequestedByKind, id.requestedKind),
		attribute.String(semconv.KoinonRequestedByID, id.requestedID),
	)
	if len(missing) > 0 {
		attrs = append(attrs,
			attribute.String(semconv.KoinonMCPMissingServers, strings.Join(missing, ",")),
			attribute.Int(semconv.KoinonMCPMissingAttempts, attempt))
	}
	opts := []trace.SpanStartOption{trace.WithSpanKind(trace.SpanKindInternal), trace.WithTimestamp(now), trace.WithAttributes(attrs...)}
	var wait *time.Duration
	if row.Seq > 1 {
		if p, err := t.q.GetRunTurn(dctx, store.GetRunTurnParams{OrgID: t.run.OrgID, RunID: t.run.ID, Seq: row.Seq - 1}); err == nil {
			opts = append(opts, trace.WithLinks(trace.Link{SpanContext: spanContext(p.TraceID)}))
			if strings.HasPrefix(p.EndReason.String, "park") && p.EndedAt.Valid {
				w := max(now.Sub(p.EndedAt.Time), 0)
				wait = &w
			}
		}
	}
	rctx := context.WithValue(context.WithoutCancel(ctx), rootIDsKey{}, rootIDs{trace: traceID, span: rootID})
	rctx, root := t.sink.tracer.Start(rctx, semconv.SpanName(semconv.OperationInvokeAgent, id.agentName), opts...)
	if wait != nil {
		root.AddEvent(semconv.EventWait, trace.WithTimestamp(now), trace.WithAttributes(attribute.Float64(semconv.KoinonWaitSeconds, wait.Seconds())))
	}
	tt := &turnTrace{row: row, root: root, rootCtx: rctx, tools: map[string]*toolTrace{}, done: map[string]bool{}}
	// The level is read for every turn: a run parked for days must not send content once the org or
	// the platform ceiling has been lowered. Without the level, content is left out.
	level, err := t.decider.ContentLevel(dctx, t.run.OrgID)
	if err != nil {
		t.log.Warn("reading the org's trace content level failed; no content is traced", "err", err)
		level = tracedest.LevelNone
	}
	tt.content = tracedest.LevelRank(level) > tracedest.LevelRank(tracedest.LevelNone)
	if tt.content {
		tt.input = t.sink.messagesJSON(chatMessage{Role: "user", Parts: []msgPart{{Type: "text", Content: t.sink.clip(prompt)}}})
	}
	t.mu.Lock()
	t.turn = tt
	t.mu.Unlock()
	return traceparentMeta(traceID, rootID)
}

// spanContext is a link target: the root span of the turn with this trace ID.
func spanContext(traceID []byte) trace.SpanContext {
	var tid trace.TraceID
	copy(tid[:], traceID)
	return trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: tid, SpanID: trace.SpanID(RootSpanID(tid)), TraceFlags: trace.FlagsSampled, Remote: true,
	})
}

// noteTurnReason records how the prompt ended (turnReason's vocabulary).
func (t *runTrace) noteTurnReason(reason string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.turn != nil {
		t.turn.reason = reason
	}
}

// reason is how the last prompt ended, for an ending: the policy's outcome when it ended the turns,
// else the error.
func (e ending) reason() string {
	if t := e.res; t != nil {
		switch t.out.Kind {
		case turnpolicy.OutCapReached:
			return "canceled"
		case turnpolicy.OutHarnessCancelled, turnpolicy.OutRetriesExhausted, turnpolicy.OutRefusalsExceeded, turnpolicy.OutLoop:
			return "failed"
		case turnpolicy.OutFenced:
			return "fenced"
		case turnpolicy.OutShutdown:
			return "shutdown"
		}
		return turnReason(t.err) // OutFailed, OutPassthrough
	}
	return turnReason(e.err)
}

func turnReason(err error) string {
	var f *failure
	var c *canceled
	var ref *refused
	switch {
	case err == nil:
		return "end_turn"
	case errors.As(err, &ref):
		return "refused"
	case errors.As(err, &f):
		return "failed"
	case errors.As(err, &c):
		return "canceled"
	case errors.Is(err, errFenced):
		return "fenced"
	case errors.Is(err, errShutdown):
		return "shutdown"
	case errors.Is(err, errDropped):
		return "interrupted"
	default:
		return "error"
	}
}

// parked records the park endTurn made, so the turn ends with a koinon.park event.
func (t *runTrace) parked(reason store.RunParkReason) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.parkedOn = string(reason)
	t.mu.Unlock()
}

// openRow is the row of the turn in progress, for endTurn to end it inside the park transaction.
func (t *runTrace) openRow() (pgtype.UUID, bool) {
	if t == nil {
		return pgtype.UUID{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.turn == nil {
		return pgtype.UUID{}, false
	}
	return t.turn.row.ID, true
}

// endFor is how a turn ends for the run's outcome (decision 3: koinon.outcome and koinon.failure_class
// on the last turn). e is how execute ended, fk why the worker was fenced, if it was, and
// parkedOn the park endTurn made.
func endFor(e ending, fk fence, parkedOn string) *turnEnd {
	var f *failure
	var c *canceled
	end := &turnEnd{reason: e.reason()}
	err := e.err
	if t := e.res; t != nil {
		switch t.out.Kind {
		case turnpolicy.OutCapReached:
			end.outcome = string(store.RunStateCanceled)
			return end
		case turnpolicy.OutHarnessCancelled, turnpolicy.OutRetriesExhausted, turnpolicy.OutRefusalsExceeded, turnpolicy.OutLoop:
			_, class, _ := t.fails()
			end.outcome, end.class = string(store.RunStateFailed), string(cmp.Or(class, classFatal))
			return end
		case turnpolicy.OutFenced:
			if fk == fenceCanceled {
				// The control plane canceled the run (a person, the budget, a cap).
				end.reason, end.outcome = "canceled", string(store.RunStateCanceled)
			}
			return end
		case turnpolicy.OutShutdown:
			return end
		}
		err = t.err // OutFailed, OutPassthrough
	}
	switch {
	case err == nil && parkedOn != "":
		end.reason, end.park = "park:"+parkedOn, parkedOn
	case err == nil:
		end.outcome = string(store.RunStateSucceeded)
	case errors.Is(err, errFenced) && fk == fenceCanceled:
		// The control plane canceled the run (a person, the budget, a cap).
		end.reason, end.outcome = "canceled", string(store.RunStateCanceled)
	case errors.As(err, &f) && f.sandbox == nil:
		end.outcome, end.class = string(store.RunStateFailed), string(cmp.Or(f.class, classFatal))
	case errors.As(err, &c):
		end.outcome, end.class = string(store.RunStateCanceled), string(c.class)
	}
	return end
}

// turnRow is the open turn row and why it ends with the run, for the run's owner to end it with the
// ending verb (internal/runoutcome). A worker that lost the run or gave it up gets none: the new owner
// ends the row.
func (t *runTrace) turnRow(e ending, fk fence) runoutcome.Turn {
	if t == nil || fk == fenceLost || fk == fenceAbandon {
		return runoutcome.Turn{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.turn == nil {
		return runoutcome.Turn{}
	}
	return runoutcome.Turn{ID: t.turn.row.ID, Reason: endFor(e, fk, t.parkedOn).reason}
}

// finish ends the spans of the last turn of this execution with the run's outcome. The turn's row
// is the run owner's to end.
func (t *runTrace) finish(ctx context.Context, e ending, fk fence) {
	if t == nil {
		return
	}
	t.mu.Lock()
	tt, parkedOn := t.turn, t.parkedOn
	t.turn = nil
	t.mu.Unlock()
	if tt == nil {
		return
	}
	t.closeTurn(ctx, tt, endFor(e, fk, parkedOn), false)
}

type turnEnd struct {
	reason, park, outcome, class string
}

// closeTurn ends a turn that is no longer reachable from the runTrace: its open tool spans, its
// root span and, when writeRow, its row (a turn that continues into a new one; a run's last turn
// is ended by the run's owner).
func (t *runTrace) closeTurn(ctx context.Context, tt *turnTrace, end *turnEnd, writeRow bool) {
	now := time.Now()
	for id, tool := range tt.tools {
		t.endTool(tt, tool, now)
		delete(tt.tools, id)
	}
	var attrs []attribute.KeyValue
	if end.outcome != "" {
		attrs = append(attrs, attribute.String(semconv.KoinonOutcome, end.outcome))
		if end.class != "" {
			attrs = append(attrs, attribute.String(semconv.KoinonFailureClass, end.class))
		}
	}
	var content []string
	if tt.content {
		if tt.input != "" {
			attrs = append(attrs, attribute.String(semconv.GenAIInputMessages, tt.input))
			content = append(content, semconv.GenAIInputMessages)
		}
		if len(tt.parts) > 0 {
			finish := "stop"
			if end.outcome == string(store.RunStateFailed) || end.reason == "failed" || end.reason == "error" {
				finish = "error"
			}
			attrs = append(attrs, attribute.String(semconv.GenAIOutputMessages,
				t.sink.messagesJSON(chatMessage{Role: "assistant", Parts: tt.parts, FinishReason: finish})))
			content = append(content, semconv.GenAIOutputMessages)
		}
	}
	if len(content) > 0 {
		attrs = append(attrs, attribute.StringSlice(semconv.KoinonContentAttributes, content))
	}
	tt.root.SetAttributes(attrs...)
	if end.park != "" {
		tt.root.AddEvent(semconv.EventPark, trace.WithTimestamp(now), trace.WithAttributes(attribute.String(semconv.KoinonParkReason, end.park)))
	}
	if end.outcome == string(store.RunStateFailed) || end.reason == "failed" || end.reason == "error" {
		tt.root.SetStatus(codes.Error, "")
	}
	tt.root.End(trace.WithTimestamp(now))
	if !writeRow {
		return
	}
	ectx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, err := t.q.EndRunTurn(ectx, store.EndRunTurnParams{OrgID: t.run.OrgID, ID: tt.row.ID, EndReason: pgtype.Text{String: end.reason, Valid: true}})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) { // no row: it is ended already
		t.log.Warn("ending the turn row failed", "err", err)
	}
}

// update traces a session update of the turn in progress. at is when the update arrived, taken
// before anything slow (the event write) happened.
func (t *runTrace) update(n acp.SessionNotification, at time.Time) {
	if t == nil {
		return
	}
	t.mu.Lock()
	tt := t.turn
	var fin *toolTrace
	if tt != nil {
		u := n.Update
		switch {
		case u.ToolCall != nil:
			c := u.ToolCall
			fin = t.toolUpdate(tt, string(c.ToolCallId), c.Title, string(c.Kind), string(c.Status), c.RawInput, c.RawOutput, at)
		case u.ToolCallUpdate != nil:
			c := u.ToolCallUpdate
			var title, kind, status string
			if c.Title != nil {
				title = *c.Title
			}
			if c.Kind != nil {
				kind = string(*c.Kind)
			}
			if c.Status != nil {
				status = string(*c.Status)
			}
			fin = t.toolUpdate(tt, string(c.ToolCallId), title, kind, status, c.RawInput, c.RawOutput, at)
		case u.AgentMessageChunk != nil && tt.content:
			t.addPart(tt, "text", acp.Flatten(n).Text)
		case u.AgentThoughtChunk != nil && tt.content:
			t.addPart(tt, "reasoning", acp.Flatten(n).Text)
		}
	}
	t.mu.Unlock()
	if fin != nil {
		t.endTool(tt, fin, at) // outside the lock: ask_agent's span reads the database
	}
}

// addPart appends text to the assembled message, to its last part when that has the same type.
func (t *runTrace) addPart(tt *turnTrace, typ, text string) {
	if text == "" || tt.size >= t.sink.cap {
		return
	}
	if room := t.sink.cap - tt.size; len(text) > room {
		text = truncateUTF8(text, room) + truncMark
	}
	tt.size += len(text)
	if n := len(tt.parts); n > 0 && tt.parts[n-1].Type == typ {
		tt.parts[n-1].Content += text
		return
	}
	tt.parts = append(tt.parts, msgPart{Type: typ, Content: text})
}

// builtinTools are the harness's own tool names (OpenCode's built-ins). A tool's name is shown only
// if it is one of these, a Koinon tool (koinon_*) or an MCP tool (mcp__*): an ACP title is free text,
// and OpenCode fills in a file path or a URL as the title of a finished call. The shell tool is "bash" and
// "shell": OpenCode 2.2.5 (#60687) runs a call named bash as shell, and both stay until no supported pin uses
// bash (ADR 0093 decision 5).
var builtinTools = map[string]bool{
	"bash": true, "shell": true, "read": true, "write": true, "edit": true, "multiedit": true, "patch": true, "apply_patch": true,
	"glob": true, "grep": true, "list": true, "todowrite": true, "todoread": true, "task": true, "skill": true,
	"webfetch": true, "websearch": true, "codesearch": true, "lsp": true, "question": true, "plan": true,
}

// prefixedToolRE is the rest of the name of a koinon_* or mcp__* tool: an identifier, no separators.
var prefixedToolRE = regexp.MustCompile(`^(koinon_|mcp__)[A-Za-z0-9_.-]{1,80}$`)

// toolName is the name a tool call is traced under: its title if that is a known tool name, else
// its ACP kind.
func toolName(title, kind string) string {
	if builtinTools[title] || prefixedToolRE.MatchString(title) {
		return title
	}
	return cmp.Or(kind, string(acp.ToolKindOther))
}

// toolUpdate returns the tool when this update is its last, for the caller to end outside the lock.
func (t *runTrace) toolUpdate(tt *turnTrace, id, title, kind, status string, in, out any, at time.Time) *toolTrace {
	tool := tt.tools[id]
	if tool == nil {
		if tt.done[id] {
			return nil // an update after the final status
		}
		// The first message about a call names it, and the name never changes.
		tool = &toolTrace{name: toolName(title, kind), kind: kind}
		_, tool.span = t.sink.tracer.Start(trace.ContextWithSpan(tt.rootCtx, tt.root), semconv.SpanName(semconv.OperationExecuteTool, tool.name),
			trace.WithSpanKind(trace.SpanKindInternal), trace.WithTimestamp(at),
			trace.WithAttributes(append(t.common(t.id, tt.row.Seq),
				attribute.String(semconv.GenAIOperationName, semconv.OperationExecuteTool),
				attribute.String(semconv.GenAIToolCallID, id),
				attribute.String(semconv.GenAIToolType, semconv.ToolTypeFunction))...))
		tt.tools[id] = tool
	}
	tool.kind = cmp.Or(kind, tool.kind)
	tool.status = cmp.Or(status, tool.status)
	if tt.content {
		if in != nil {
			tool.inputJSON = t.sink.contentJSON(in)
			tool.target = cmp.Or(askAgentTarget(in), tool.target)
		}
		if out != nil {
			tool.outputJSON = t.sink.contentJSON(out)
		}
	}
	if out != nil {
		tool.childRun = cmp.Or(childRunID(out), tool.childRun)
	}
	if status == string(acp.ToolCompleted) || status == string(acp.ToolFailed) {
		delete(tt.tools, id)
		tt.done[id] = true
		return tool
	}
	return nil
}

// endTool sets the tool span's final attributes and ends it.
func (t *runTrace) endTool(tt *turnTrace, tool *toolTrace, at time.Time) {
	attrs := []attribute.KeyValue{
		attribute.String(semconv.GenAIToolName, tool.name),
		attribute.String(semconv.KoinonToolKind, cmp.Or(tool.kind, string(acp.ToolKindOther))),
		attribute.String(semconv.KoinonToolStatus, cmp.Or(tool.status, "unknown")),
	}
	var content []string
	if tool.inputJSON != "" {
		attrs = append(attrs, attribute.String(semconv.GenAIToolCallArguments, tool.inputJSON))
		content = append(content, semconv.GenAIToolCallArguments)
	}
	if tool.outputJSON != "" {
		attrs = append(attrs, attribute.String(semconv.GenAIToolCallResult, tool.outputJSON))
		content = append(content, semconv.GenAIToolCallResult)
	}
	// PROVISIONAL: koinon_ask_agent does not exist yet (ADR 0065). Its argument and result shapes
	// below are guesses to be replaced when the tool is built. The target is the callee's name, so it
	// is traced only above content level none, like the arguments it comes from.
	if tool.name == "koinon_ask_agent" {
		attrs = append(attrs, attribute.String(semconv.GenAITransferMode, semconv.GenAITransferModeReturnCall))
		if tool.target != "" {
			// Agent-written free text, so content: the router scrubs or strips it by level.
			attrs = append(attrs, attribute.String(semconv.GenAITransferTargetName, tool.target))
			content = append(content, semconv.GenAITransferTargetName)
		}
		if link, ok := t.childLink(tool.childRun); ok {
			tool.span.AddLink(link)
		}
	}
	if len(content) > 0 {
		attrs = append(attrs, attribute.StringSlice(semconv.KoinonContentAttributes, content))
	}
	tool.span.SetAttributes(attrs...)
	if tool.status == string(acp.ToolFailed) {
		tool.span.SetStatus(codes.Error, "")
	}
	tool.span.End(trace.WithTimestamp(at))
}

// askAgentTarget is the callee's name in ask_agent's arguments: "agent" (or "to"). Provisional.
func askAgentTarget(in any) string {
	m, ok := in.(map[string]any)
	if !ok {
		return ""
	}
	for _, k := range []string{"agent", "to"} {
		if s, ok := m[k].(string); ok && prefixedOrPlain.MatchString(s) {
			return s
		}
	}
	return ""
}

var prefixedOrPlain = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.:/ -]{0,95}$`)

// childRunID is the child run's ID in ask_agent's result ("runId" or "run"). Provisional.
func childRunID(out any) string {
	m, ok := out.(map[string]any)
	if !ok {
		return ""
	}
	for _, k := range []string{"runId", "run"} {
		if s, ok := m[k].(string); ok {
			if _, err := uuid.Parse(s); err == nil {
				return s
			}
		}
	}
	return ""
}

// childLink links an ask_agent span to the first turn of the child run, when that run is traced.
// Provisional, with the rest of ask_agent's handling.
func (t *runTrace) childLink(run string) (trace.Link, bool) {
	id, err := uuid.Parse(run)
	if err != nil {
		return trace.Link{}, false
	}
	ctx, cancel := t.dbctx(context.Background(), 2*time.Second)
	defer cancel()
	row, err := t.q.GetRunTurn(ctx, store.GetRunTurnParams{OrgID: t.run.OrgID, RunID: pgtype.UUID{Bytes: id, Valid: true}, Seq: 1})
	if err != nil {
		return trace.Link{}, false
	}
	return trace.Link{SpanContext: spanContext(row.TraceID)}, true
}

// truncMark ends a value cut by the pre-cap.
const truncMark = "…[truncated]"

func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// clip bounds a string at the pre-cap.
func (s *traceSink) clip(v string) string {
	if len(v) <= s.cap {
		return v
	}
	return truncateUTF8(v, s.cap) + truncMark
}

// contentJSON is an opaque tool value as JSON for a content attribute. A value over the pre-cap
// becomes a JSON string holding its cut-off text, so the attribute is still valid JSON; the router
// truncates precisely.
func (s *traceSink) contentJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		b, _ = json.Marshal(fmt.Sprint(v))
	}
	if len(b) <= s.cap {
		return string(b)
	}
	cut, _ := json.Marshal(truncateUTF8(string(b), s.cap) + truncMark)
	return string(cut)
}

// messagesJSON is one message as the messages array of the GenAI conventions. Over the pre-cap, the
// leaf strings (the parts' content) are halved until it fits, so the structure is always kept.
func (s *traceSink) messagesJSON(m chatMessage) string {
	m.Parts = append([]msgPart(nil), m.Parts...)
	for range 32 {
		b, _ := json.Marshal([]chatMessage{m})
		if len(b) <= s.cap {
			return string(b)
		}
		shrunk := false
		for i := range m.Parts {
			if c := strings.TrimSuffix(m.Parts[i].Content, truncMark); len(c) > 32 {
				m.Parts[i].Content = truncateUTF8(c, len(c)/2) + truncMark
				shrunk = true
			}
		}
		if !shrunk {
			break
		}
	}
	for i := range m.Parts {
		m.Parts[i].Content = ""
	}
	b, _ := json.Marshal([]chatMessage{m})
	return string(b)
}
