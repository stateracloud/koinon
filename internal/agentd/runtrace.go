// SPDX-License-Identifier: Apache-2.0

package agentd

// The run's spans (ADR 0074, decisions 2 and 3): one trace per turn, a root span
// "invoke_agent {agent}" and a child "execute_tool {tool}" for every ACP tool call, a koinon.park
// event where a turn ends in a park, and a span link and koinon.wait event on the turn that resumes
// it. Tracing never fails or delays a run: every error here is logged and the turn goes untraced.

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
	"github.com/jackc/pgx/v5/pgtype"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/stateracloud/koinon/internal/acp"
	"github.com/stateracloud/koinon/internal/db/store"
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
	content bool

	mu       sync.Mutex
	id       identity
	turn     *turnTrace
	parkedOn string // the park reason endTurn chose, until finish
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
	parts   []msgPart
	size    int
	input   string
}

type toolTrace struct {
	span          trace.Span
	name, kind    string
	status        string
	input, output any
}

// msgPart is a part of an assembled assistant message: text or reasoning.
type msgPart struct {
	Type    string `json:"type"`
	Content string `json:"content"`
}

// traceStart decides, once per run, whether it is traced (decision 1). It runs when the worker has
// claimed the run, before anything else of the run happens.
func (p *Pool) traceStart(ctx context.Context, run store.Run, log interface{ Warn(msg string, args ...any) }) *runTrace {
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
	level, err := p.cfg.Tracing.Decider.ContentLevel(dctx, run.OrgID)
	if err != nil {
		// Without the level, content is left out: the safe side.
		log.Warn("reading the org's trace content level failed; no content is traced", "err", err)
		level = tracedest.LevelNone
	}
	return &runTrace{sink: p.sink, q: p.q, run: run, log: log, content: tracedest.LevelRank(level) > tracedest.LevelRank(tracedest.LevelNone)}
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

func uuidString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return u.String()
}

func (t *runTrace) common(turn int32) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String(semconv.GenAIConversationID, t.run.ID.String()),
		attribute.String(semconv.SessionID, t.run.ID.String()),
		attribute.String(semconv.KoinonRun, t.run.ID.String()),
		attribute.String(semconv.KoinonOrg, t.run.OrgID.String()),
		attribute.String(semconv.KoinonTask, t.id.task),
		attribute.Int(semconv.KoinonTurn, int(turn)),
	}
}

// traceparentMeta is the _meta of session/prompt: a hint for harnesses that honour it, never the
// source of attribution (decision 2).
func traceparentMeta(trace [16]byte, span [8]byte) map[string]any {
	return map[string]any{"traceparent": "00-" + hex.EncodeToString(trace[:]) + "-" + hex.EncodeToString(span[:]) + "-01"}
}

// beginTurn starts the trace of a turn: a new row with a new trace ID, and the root span. It returns
// the _meta to send with the prompt (nil when the turn is not traced). A turn still open is ended.
func (t *runTrace) beginTurn(ctx context.Context, prompt string) map[string]any {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.endTurnLocked(ctxBg(ctx), "continued", nil)
	t.parkedOn = ""
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	traceID := export.NewTraceID()
	row, err := t.q.InsertRunTurn(dctx, store.InsertRunTurnParams{OrgID: t.run.OrgID, RunID: t.run.ID, TraceID: traceID[:]})
	if err != nil {
		t.log.Warn("recording the turn failed; it is not traced", "err", err)
		return nil
	}
	now := time.Now()
	rootID := RootSpanID(traceID)
	attrs := append(t.common(row.Seq),
		attribute.String(semconv.GenAIOperationName, semconv.OperationInvokeAgent),
		attribute.String(semconv.GenAIAgentID, t.id.agentID),
		attribute.String(semconv.GenAIAgentName, t.id.agentName),
		attribute.String(semconv.GenAIAgentVersion, t.id.agentVersion),
		attribute.String(semconv.GenAIRequestModel, t.id.model),
		attribute.String(semconv.KoinonRequestedByKind, t.id.requestedKind),
		attribute.String(semconv.KoinonRequestedByID, t.id.requestedID),
	)
	opts := []trace.SpanStartOption{trace.WithSpanKind(trace.SpanKindInternal), trace.WithTimestamp(now), trace.WithAttributes(attrs...)}
	var wait *time.Duration
	if row.Seq > 1 {
		if prev, err := t.q.GetRunTurn(dctx, store.GetRunTurnParams{OrgID: t.run.OrgID, RunID: t.run.ID, Seq: row.Seq - 1}); err == nil {
			opts = append(opts, trace.WithLinks(trace.Link{SpanContext: spanContext(prev.TraceID, nil)}))
			if strings.HasPrefix(prev.EndReason.String, "park") && prev.EndedAt.Valid {
				w := max(now.Sub(prev.EndedAt.Time), 0)
				wait = &w
			}
		}
	}
	rctx := context.WithValue(context.WithoutCancel(ctx), rootIDsKey{}, rootIDs{trace: traceID, span: rootID})
	rctx, root := t.sink.tracer.Start(rctx, semconv.SpanName(semconv.OperationInvokeAgent, t.id.agentName), opts...)
	if wait != nil {
		root.AddEvent(semconv.EventWait, trace.WithTimestamp(now), trace.WithAttributes(attribute.Float64(semconv.KoinonWaitSeconds, wait.Seconds())))
	}
	tt := &turnTrace{row: row, root: root, rootCtx: rctx, tools: map[string]*toolTrace{}, done: map[string]bool{}}
	if t.content {
		tt.input = t.sink.contentJSON([]map[string]any{{"role": "user", "parts": []map[string]string{{"type": "text", "content": t.sink.clip(prompt)}}}})
	}
	t.turn = tt
	return traceparentMeta(traceID, rootID)
}

func ctxBg(ctx context.Context) context.Context { return context.WithoutCancel(ctx) }

// spanContext is a link target: the root span of the turn with this trace ID.
func spanContext(traceID []byte, _ any) trace.SpanContext {
	var tid trace.TraceID
	copy(tid[:], traceID)
	return trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: tid, SpanID: trace.SpanID(RootSpanID(tid)), TraceFlags: trace.FlagsSampled, Remote: true,
	})
}

// noteTurnResult records how the prompt ended, from turn()'s error.
func (t *runTrace) noteTurnResult(err error) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.turn != nil {
		t.turn.reason = turnReason(err)
	}
}

func turnReason(err error) string {
	var f *failure
	var c *canceled
	var ref *refused
	switch {
	case err == nil, errors.Is(err, errEndTurn):
		return "end_turn"
	case errors.Is(err, errWrapUp):
		return "wrap_up"
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

// finish ends the last turn of this execution with the run's outcome (decision 3: koinon.outcome and
// koinon.failure_class on the last turn). err is what execute returned.
func (t *runTrace) finish(ctx context.Context, err error) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.turn == nil {
		return
	}
	var f *failure
	var c *canceled
	end := &turnEnd{reason: turnReason(err)}
	switch {
	case err == nil && t.parkedOn != "":
		end.reason, end.park = "park:"+t.parkedOn, t.parkedOn
	case err == nil:
		end.outcome = string(store.RunStateSucceeded)
	case errors.As(err, &f) && f.sandbox == nil:
		end.outcome, end.class = string(store.RunStateFailed), string(cmp.Or(f.class, classFatal))
	case errors.As(err, &c):
		end.outcome, end.class = string(store.RunStateCanceled), string(c.class)
	}
	t.endTurnLocked(ctxBg(ctx), end.reason, end)
}

type turnEnd struct {
	reason, park, outcome, class string
}

func (t *runTrace) endTurnLocked(ctx context.Context, reason string, end *turnEnd) {
	tt := t.turn
	if tt == nil {
		return
	}
	t.turn = nil
	now := time.Now()
	if end == nil {
		end = &turnEnd{reason: reason}
	}
	for id, tool := range tt.tools {
		t.endTool(tool, now)
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
	if t.content {
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
				t.sink.contentJSON([]map[string]any{{"role": "assistant", "parts": tt.parts, "finish_reason": finish}})))
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
	if end.outcome == string(store.RunStateFailed) {
		tt.root.SetStatus(codes.Error, "")
	}
	tt.root.End(trace.WithTimestamp(now))
	ectx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := t.q.EndRunTurn(ectx, store.EndRunTurnParams{OrgID: t.run.OrgID, ID: tt.row.ID, EndReason: pgtype.Text{String: end.reason, Valid: true}}); err != nil {
		t.log.Warn("ending the turn row failed", "err", err)
	}
}

// update traces a session update of the turn in progress.
func (t *runTrace) update(n acp.SessionNotification) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	tt := t.turn
	if tt == nil {
		return
	}
	u := n.Update
	switch {
	case u.ToolCall != nil:
		c := u.ToolCall
		t.toolUpdate(tt, string(c.ToolCallId), c.Title, string(c.Kind), string(c.Status), c.RawInput, c.RawOutput)
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
		t.toolUpdate(tt, string(c.ToolCallId), title, kind, status, c.RawInput, c.RawOutput)
	case u.AgentMessageChunk != nil && t.content:
		t.addPart(tt, "text", acp.Flatten(n).Text)
	case u.AgentThoughtChunk != nil && t.content:
		t.addPart(tt, "reasoning", acp.Flatten(n).Text)
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

// toolNameRE is a tool's name as an identifier. An ACP title is free text and may carry arguments
// ("Read /home/x/secret.txt"); only a title that is a plain name becomes gen_ai.tool.name.
var toolNameRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.:/-]{0,95}$`)

func toolName(title, kind string) string {
	if toolNameRE.MatchString(title) {
		return title
	}
	return cmp.Or(kind, string(acp.ToolKindOther))
}

func (t *runTrace) toolUpdate(tt *turnTrace, id, title, kind, status string, in, out any) {
	tool := tt.tools[id]
	if tool == nil {
		if tt.done[id] {
			return // an update after the final status
		}
		tool = &toolTrace{name: toolName(title, kind), kind: kind}
		_, tool.span = t.sink.tracer.Start(tt.rootCtxWithSpan(), semconv.SpanName(semconv.OperationExecuteTool, tool.name),
			trace.WithSpanKind(trace.SpanKindInternal),
			trace.WithAttributes(append(t.common(tt.row.Seq),
				attribute.String(semconv.GenAIOperationName, semconv.OperationExecuteTool),
				attribute.String(semconv.GenAIToolCallID, id),
				attribute.String(semconv.GenAIToolType, semconv.ToolTypeFunction))...))
		tt.tools[id] = tool
	}
	if title != "" {
		if name := toolName(title, kind); name != tool.name && toolNameRE.MatchString(title) {
			tool.name = name
			tool.span.SetName(semconv.SpanName(semconv.OperationExecuteTool, name))
		}
	}
	tool.kind = cmp.Or(kind, tool.kind)
	tool.status = cmp.Or(status, tool.status)
	if in != nil {
		tool.input = in
	}
	if out != nil {
		tool.output = out
	}
	if status == string(acp.ToolCompleted) || status == string(acp.ToolFailed) {
		t.endTool(tool, time.Now())
		delete(tt.tools, id)
		tt.done[id] = true
	}
}

func (tt *turnTrace) rootCtxWithSpan() context.Context {
	return trace.ContextWithSpan(tt.rootCtx, tt.root)
}

// endTool sets the tool span's final attributes and ends it.
func (t *runTrace) endTool(tool *toolTrace, at time.Time) {
	attrs := []attribute.KeyValue{
		attribute.String(semconv.GenAIToolName, tool.name),
		attribute.String(semconv.KoinonToolKind, cmp.Or(tool.kind, string(acp.ToolKindOther))),
		attribute.String(semconv.KoinonToolStatus, cmp.Or(tool.status, "unknown")),
	}
	var content []string
	if t.content {
		if tool.input != nil {
			attrs = append(attrs, attribute.String(semconv.GenAIToolCallArguments, t.sink.contentJSON(tool.input)))
			content = append(content, semconv.GenAIToolCallArguments)
		}
		if tool.output != nil {
			attrs = append(attrs, attribute.String(semconv.GenAIToolCallResult, t.sink.contentJSON(tool.output)))
			content = append(content, semconv.GenAIToolCallResult)
		}
	}
	if len(content) > 0 {
		attrs = append(attrs, attribute.StringSlice(semconv.KoinonContentAttributes, content))
	}
	if tool.name == "koinon_ask_agent" {
		attrs = append(attrs, attribute.String(semconv.GenAITransferMode, semconv.GenAITransferModeReturnCall))
		if target := askAgentTarget(tool.input); target != "" {
			attrs = append(attrs, attribute.String(semconv.GenAITransferTargetName, target))
		}
		if link, ok := t.childLink(tool.output); ok {
			tool.span.AddLink(link)
		}
	}
	tool.span.SetAttributes(attrs...)
	if tool.status == string(acp.ToolFailed) {
		tool.span.SetStatus(codes.Error, "")
	}
	tool.span.End(trace.WithTimestamp(at))
}

// askAgentTarget is the callee's name in ask_agent's arguments: "agent" (or "to").
func askAgentTarget(in any) string {
	m, ok := in.(map[string]any)
	if !ok {
		return ""
	}
	for _, k := range []string{"agent", "to"} {
		if s, ok := m[k].(string); ok && toolNameRE.MatchString(s) {
			return s
		}
	}
	return ""
}

// childLink links an ask_agent span to the first turn of the child run, when its result names the
// run ("run" or "runId") and that run is traced.
func (t *runTrace) childLink(out any) (trace.Link, bool) {
	m, ok := out.(map[string]any)
	if !ok {
		return trace.Link{}, false
	}
	var id uuid.UUID
	for _, k := range []string{"runId", "run"} {
		if s, ok := m[k].(string); ok {
			if u, err := uuid.Parse(s); err == nil {
				id = u
				break
			}
		}
	}
	if id == uuid.Nil {
		return trace.Link{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	row, err := t.q.GetRunTurn(ctx, store.GetRunTurnParams{OrgID: t.run.OrgID, RunID: pgtype.UUID{Bytes: id, Valid: true}, Seq: 1})
	if err != nil {
		return trace.Link{}, false
	}
	return trace.Link{SpanContext: spanContext(row.TraceID, nil)}, true
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

// contentJSON is v as JSON for a content attribute. A value over the pre-cap becomes a JSON string
// holding its cut-off text, so the attribute is still valid JSON; the router truncates precisely.
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
