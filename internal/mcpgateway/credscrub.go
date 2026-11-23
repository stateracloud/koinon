// SPDX-License-Identifier: Apache-2.0

package mcpgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/tracing/scrub"
)

// CredentialScrub removes the credential values that the gateway opened from everything the gateway emits: log
// lines, span names, span attributes, span events, recorded errors and the results it gives to an agent (#1174,
// ADR 0097 decision 12). Only the gateway holds an opened v2 credential, so only the gateway can remove it: the
// trace router cannot read the value.
//
// It matches by exact value with [scrub.Secrets]: the value as sent (prefix and token), the token alone, the
// decoded "user:password" and password of a Basic value (not the user name), and the encoded forms NewSecrets adds (JSON-escaped, URL-encoded, base64). A
// value shorter than [scrub.MinSecretLen] is not matched: it would redact ordinary words.
//
// A value stays in the set for Retain after the gateway last opened it, so a late error about a rotated
// credential is still scrubbed. The set holds at most MaxValues values; the oldest go first, in one batch. It is safe for
// concurrent use. The zero value is not usable: call [NewCredentialScrub].
type CredentialScrub struct {
	// Retain is how long a value stays in the set after its last open. Zero means 24 hours.
	Retain time.Duration
	// MaxValues caps the set. One credential adds up to four values. Zero means 65536.
	MaxValues int

	now func() time.Time

	mu        sync.Mutex
	seen      map[string]time.Time // value -> last open
	lastPrune time.Time
	set       atomic.Pointer[scrub.Secrets]
}

// NewCredentialScrub returns an empty CredentialScrub.
func NewCredentialScrub() *CredentialScrub {
	s := &CredentialScrub{now: time.Now, seen: map[string]time.Time{}}
	s.set.Store(scrub.NewSecrets())
	return s
}

// String never prints a value.
func (s *CredentialScrub) String() string { return "mcpgateway.CredentialScrub" }

// GoString never prints a value.
func (s *CredentialScrub) GoString() string { return "mcpgateway.CredentialScrub" }

func (s *CredentialScrub) retain() time.Duration {
	if s.Retain > 0 {
		return s.Retain
	}
	return 24 * time.Hour
}

func (s *CredentialScrub) maxValues() int {
	if s.MaxValues > 0 {
		return s.MaxValues
	}
	return 65536
}

// Add records opened credential values. Each value also adds the token after a known auth scheme
// ([scrub.AuthParts]). Empty values are ignored.
func (s *CredentialScrub) Add(values ...string) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for _, v := range values {
		for _, p := range scrub.AuthParts(v) {
			p = strings.TrimSpace(p)
			if len(p) < scrub.MinSecretLen {
				continue
			}
			if _, ok := s.seen[p]; !ok {
				changed = true
			}
			s.seen[p] = now
		}
	}
	if now.Sub(s.lastPrune) >= time.Minute || len(s.seen) > s.maxValues() {
		s.lastPrune = now
		changed = s.pruneLocked(now) || changed
	}
	if changed {
		vals := make([]string, 0, len(s.seen))
		for v := range s.seen {
			vals = append(vals, v)
		}
		s.set.Store(scrub.NewSecrets(vals...))
	}
}

// pruneLocked drops values not opened within Retain. Over MaxValues, it drops the oldest values in one batch
// down to nine tenths of MaxValues, so a full set is not sorted and rebuilt at each open.
func (s *CredentialScrub) pruneLocked(now time.Time) bool {
	changed := false
	for v, t := range s.seen {
		if now.Sub(t) > s.retain() {
			delete(s.seen, v)
			changed = true
		}
	}
	if limit := s.maxValues(); len(s.seen) > limit {
		keep := limit - limit/10
		vals := make([]string, 0, len(s.seen))
		for v := range s.seen {
			vals = append(vals, v)
		}
		slices.SortFunc(vals, func(a, b string) int { return s.seen[a].Compare(s.seen[b]) })
		for _, v := range vals[:len(vals)-keep] {
			delete(s.seen, v)
		}
		changed = true
	}
	return changed
}

// Text returns in with every recorded value replaced by [redacted:secret].
func (s *CredentialScrub) Text(in string) string { return s.set.Load().Redact(in) }

// Error returns err unchanged when its text holds no recorded value, else an error with the scrubbed text.
// The new error does not wrap err: an Unwrap would give the value back.
func (s *CredentialScrub) Error(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if out := s.Text(msg); out != msg {
		return errors.New(out)
	}
	return err
}

// Source returns src with every credential it gives recorded before the caller can use it. The gateway wraps
// its own source with it; a [Builtin] that opens a credential takes its source from here too.
func (s *CredentialScrub) Source(src CredentialSource) CredentialSource {
	if r, ok := src.(recordingSource); ok && r.s == s {
		return src
	}
	return recordingSource{src: src, s: s}
}

type recordingSource struct {
	src CredentialSource
	s   *CredentialScrub
}

func (r recordingSource) Credential(ctx context.Context, t gateway.MCPTarget, agent gateway.AgentRef) (Credential, error) {
	c, err := r.src.Credential(ctx, t, agent)
	if c.Value != "" {
		r.s.Add(c.Value, strings.TrimPrefix(c.Value, t.CredentialPrefix))
	}
	return c, r.s.Error(err)
}

// Result scrubs everything an agent gets from a tool call. It scrubs the bytes of each blob (an embedded
// resource, an image, an audio clip) as text, then the JSON form of the whole result, so every field is covered,
// including fields a later SDK adds. A result with no match is returned unchanged. If the scrubbed JSON does not
// decode, the agent gets an error result rather than the value.
func (s *CredentialScrub) Result(res *mcp.CallToolResult) *mcp.CallToolResult {
	if res == nil || s.set.Load().Len() == 0 {
		return res
	}
	for _, c := range res.Content {
		switch c := c.(type) {
		case *mcp.EmbeddedResource:
			if c.Resource != nil {
				c.Resource.Blob = s.bytes(c.Resource.Blob)
			}
		case *mcp.ImageContent:
			c.Data = s.bytes(c.Data)
		case *mcp.AudioContent:
			c.Data = s.bytes(c.Data)
		}
	}
	raw, err := json.Marshal(res)
	if err != nil {
		return scrubbedResult()
	}
	out := s.Text(string(raw))
	if out == string(raw) {
		return res
	}
	var clean mcp.CallToolResult
	if json.Unmarshal([]byte(out), &clean) != nil {
		return scrubbedResult()
	}
	return &clean
}

// bytes returns b with every recorded value in its text replaced, or b itself when it holds none.
func (s *CredentialScrub) bytes(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	if out := s.Text(string(b)); out != string(b) {
		return []byte(out)
	}
	return b
}

func scrubbedResult() *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{
		Text: "koinon-gateway removed a credential from this tool result and could not rebuild it",
	}}}
}

// Handler returns h with every log record scrubbed before h sees it: the message, every attribute value and key,
// the attributes bound with WithAttrs and the group names. Bound attributes are scrubbed at each record, so a
// value opened after the bind is removed too.
func (s *CredentialScrub) Handler(h slog.Handler) slog.Handler {
	if sh, ok := h.(*scrubHandler); ok && sh.s == s {
		return h
	}
	return &scrubHandler{s: s, inner: h}
}

type handlerOp struct {
	group string
	attrs []slog.Attr
}

type scrubHandler struct {
	s     *CredentialScrub
	inner slog.Handler
	ops   []handlerOp

	cache atomic.Pointer[builtHandler]
}

type builtHandler struct {
	set *scrub.Secrets
	h   slog.Handler
}

func (h *scrubHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *scrubHandler) Handle(ctx context.Context, r slog.Record) error {
	set := h.s.set.Load()
	inner := h.inner
	if len(h.ops) > 0 {
		if b := h.cache.Load(); b != nil && b.set == set {
			inner = b.h
		} else {
			for _, op := range h.ops {
				if op.group != "" {
					inner = inner.WithGroup(set.Redact(op.group))
				} else {
					inner = inner.WithAttrs(scrubAttrs(set, op.attrs))
				}
			}
			h.cache.Store(&builtHandler{set: set, h: inner})
		}
	}
	out := slog.NewRecord(r.Time, r.Level, set.Redact(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(scrubAttr(set, a))
		return true
	})
	return inner.Handle(ctx, out)
}

func (h *scrubHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	return h.with(handlerOp{attrs: attrs})
}

func (h *scrubHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return h.with(handlerOp{group: name})
}

func (h *scrubHandler) with(op handlerOp) *scrubHandler {
	ops := make([]handlerOp, 0, len(h.ops)+1)
	ops = append(append(ops, h.ops...), op)
	return &scrubHandler{s: h.s, inner: h.inner, ops: ops}
}

func scrubAttrs(set *scrub.Secrets, attrs []slog.Attr) []slog.Attr {
	out := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		out[i] = scrubAttr(set, a)
	}
	return out
}

func scrubAttr(set *scrub.Secrets, a slog.Attr) slog.Attr {
	a.Key = set.Redact(a.Key)
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		a.Value = slog.StringValue(set.Redact(v.String()))
	case slog.KindGroup:
		a.Value = slog.GroupValue(scrubAttrs(set, v.Group())...)
	case slog.KindAny:
		a.Value = scrubAny(set, v)
	default:
		a.Value = v
	}
	return a
}

// scrubAny turns a value that would print a recorded credential into its scrubbed text. A value that prints none
// is kept as it is, so a handler still formats it in its own way.
func scrubAny(set *scrub.Secrets, v slog.Value) slog.Value {
	var text string
	switch x := v.Any().(type) {
	case error:
		text = x.Error()
	case []byte:
		// A text handler writes a []byte as a string; %+v would print numbers and hide the match.
		text = string(x)
	case json.RawMessage:
		text = string(x)
	case fmt.Stringer:
		text = x.String()
	default:
		text = fmt.Sprintf("%+v", x)
	}
	if out := set.Redact(text); out != text {
		return slog.StringValue(out)
	}
	return v
}

// Tracer returns t with every span scrubbed: its name, attributes, status text, events, links and recorded
// errors. A span that t starts is put in the returned context in its scrubbed form, so code that takes the span
// from the context writes through the scrub too.
func (s *CredentialScrub) Tracer(t trace.Tracer) trace.Tracer {
	if t == nil {
		return nil
	}
	if st, ok := t.(scrubTracer); ok && st.s == s {
		return t
	}
	return scrubTracer{Tracer: t, s: s}
}

type scrubTracer struct {
	trace.Tracer
	s *CredentialScrub
}

func (t scrubTracer) Start(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	set := t.s.set.Load()
	cfg := trace.NewSpanStartConfig(opts...)
	clean := []trace.SpanStartOption{trace.WithSpanKind(cfg.SpanKind())}
	if attrs := cfg.Attributes(); len(attrs) > 0 {
		clean = append(clean, trace.WithAttributes(scrubKVs(set, attrs)...))
	}
	if links := cfg.Links(); len(links) > 0 {
		clean = append(clean, trace.WithLinks(scrubLinks(set, links)...))
	}
	if cfg.NewRoot() {
		clean = append(clean, trace.WithNewRoot())
	}
	if ts := cfg.Timestamp(); !ts.IsZero() {
		clean = append(clean, trace.WithTimestamp(ts))
	}
	ctx, sp := t.Tracer.Start(ctx, set.Redact(name), clean...)
	wrapped := scrubSpan{Span: sp, s: t.s}
	return trace.ContextWithSpan(ctx, wrapped), wrapped
}

type scrubSpan struct {
	trace.Span
	s *CredentialScrub
}

func (sp scrubSpan) SetName(name string) { sp.Span.SetName(sp.s.Text(name)) }

func (sp scrubSpan) SetAttributes(kv ...attribute.KeyValue) {
	sp.Span.SetAttributes(scrubKVs(sp.s.set.Load(), kv)...)
}

func (sp scrubSpan) SetStatus(code codes.Code, description string) {
	sp.Span.SetStatus(code, sp.s.Text(description))
}

func (sp scrubSpan) AddEvent(name string, opts ...trace.EventOption) {
	set := sp.s.set.Load()
	sp.Span.AddEvent(set.Redact(name), scrubEventOptions(set, opts)...)
}

func (sp scrubSpan) RecordError(err error, opts ...trace.EventOption) {
	set := sp.s.set.Load()
	if err != nil {
		if msg := err.Error(); set.Redact(msg) != msg {
			err = errors.New(set.Redact(msg))
		}
	}
	sp.Span.RecordError(err, scrubEventOptions(set, opts)...)
}

func (sp scrubSpan) AddLink(l trace.Link) {
	l.Attributes = scrubKVs(sp.s.set.Load(), l.Attributes)
	sp.Span.AddLink(l)
}

func (sp scrubSpan) TracerProvider() trace.TracerProvider {
	return scrubProvider{TracerProvider: sp.Span.TracerProvider(), s: sp.s}
}

type scrubProvider struct {
	trace.TracerProvider
	s *CredentialScrub
}

func (p scrubProvider) Tracer(name string, opts ...trace.TracerOption) trace.Tracer {
	return p.s.Tracer(p.TracerProvider.Tracer(name, opts...))
}

func scrubEventOptions(set *scrub.Secrets, opts []trace.EventOption) []trace.EventOption {
	if len(opts) == 0 {
		return nil
	}
	cfg := trace.NewEventConfig(opts...)
	clean := []trace.EventOption{trace.WithTimestamp(cfg.Timestamp())}
	if attrs := cfg.Attributes(); len(attrs) > 0 {
		clean = append(clean, trace.WithAttributes(scrubKVs(set, attrs)...))
	}
	if cfg.StackTrace() {
		clean = append(clean, trace.WithStackTrace(true))
	}
	return clean
}

func scrubLinks(set *scrub.Secrets, links []trace.Link) []trace.Link {
	out := make([]trace.Link, len(links))
	for i, l := range links {
		l.Attributes = scrubKVs(set, l.Attributes)
		out[i] = l
	}
	return out
}

func scrubKVs(set *scrub.Secrets, kvs []attribute.KeyValue) []attribute.KeyValue {
	if len(kvs) == 0 || set.Len() == 0 {
		return kvs
	}
	out := make([]attribute.KeyValue, len(kvs))
	for i, kv := range kvs {
		key := attribute.Key(set.Redact(string(kv.Key)))
		switch kv.Value.Type() {
		case attribute.STRING:
			out[i] = key.String(set.Redact(kv.Value.AsString()))
		case attribute.STRINGSLICE:
			ss := kv.Value.AsStringSlice()
			for j := range ss {
				ss[j] = set.Redact(ss[j])
			}
			out[i] = key.StringSlice(ss)
		default:
			out[i] = attribute.KeyValue{Key: key, Value: kv.Value}
		}
	}
	return out
}
