// SPDX-License-Identifier: Apache-2.0

package mcpgateway

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
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
// lines, span names, span attributes, span events, recorded errors, audit rows, and the tool lists and tool
// results it gives to an agent (#1174, ADR 0097 decision 12). Only the gateway holds an opened v2 credential, so
// only the gateway can remove it: the trace router cannot read the value.
//
// It matches by exact value with [scrub.Secrets]: the value as sent (prefix and token), the token alone, the
// decoded "user:password" and password of a Basic value (and the user name when the password is empty or a
// dummy), and the encoded forms NewSecrets adds (JSON-escaped, URL-encoded, base64). A value shorter than
// [scrub.MinSecretLen] is not matched: it would redact ordinary words. Such a value is counted and logged once
// for each org and target, so an operator sees it.
//
// The scrub is a backstop for a target that echoes its credential by accident. It is not a defence against a
// target that hides the value on purpose (hex, a split over two fields, a hash): such a target already holds the
// credential it was sent. The gateway keeps upstream text out of its own telemetry by construction; the scrub
// covers the few fields that can carry it.
//
// Scope. Each value is recorded for the org of the agent whose call opened it. An org chooses its own credential
// bytes, so every text that belongs to one org is scrubbed only with that org's values: the tool lists and tool
// results an agent gets, and the logs, spans and audit rows of a request (the org travels in the context, see
// [WithScrubOrg]). Otherwise one org could change or hide the text of another org. A log record or a span with no
// org in its context is process telemetry: it is scrubbed with the values of every org, in its values and free
// text only, never in its keys or group names, which are code constants.
//
// Lifetime and limits. A value stays for Retain after the gateway last opened it, so a late error about a
// rotated credential is still scrubbed. Each org holds at most MaxOrgValues values and the process at most
// MaxValues; over a cap the oldest values go first, in one batch. A value that is already known costs no lock and
// no rebuild. The matcher of all orgs is rebuilt at most once a second; until then the matchers of the orgs that
// changed are applied after it, so no value is ever missed. It is safe for concurrent use. The zero value is not
// usable: call [NewCredentialScrub].
type CredentialScrub struct {
	// Retain is how long a value stays after its last open. Zero means 24 hours.
	Retain time.Duration
	// MaxOrgValues caps the values of one org. One credential adds up to six values. Zero means 256.
	MaxOrgValues int
	// MaxValues caps the values of all orgs. Zero means 8192.
	MaxValues int

	now func() time.Time

	mu        sync.Mutex // serialises changes of sets
	sets      atomic.Pointer[scrubSets]
	lastPrune atomic.Int64 // unix nanoseconds
	metrics   atomic.Pointer[Metrics]
	log       atomic.Pointer[slog.Logger]
	warned    sync.Map // scopedTarget -> struct{}: a short value was logged
}

// scopedTarget names the target and org of a short credential that was logged.
type scopedTarget struct{ org, target string }

// orgSet is the immutable matcher of one org and the last open of each of its values.
type orgSet struct {
	secrets *scrub.Secrets
	last    map[string]*atomic.Int64 // value -> last open, unix nanoseconds
}

// scrubSets is an immutable snapshot. all matches the values of every org as they were when it was built;
// pending names the orgs whose values changed since then, whose matchers are applied after all.
type scrubSets struct {
	byOrg    map[string]*orgSet
	all      *scrub.Secrets
	pending  []string
	allBuilt time.Time
}

var emptySecrets = scrub.NewSecrets()

const (
	allRebuildEvery = time.Second
	pruneEvery      = time.Minute
)

// orgRedactor returns the matcher of org, or an empty one.
func (ss *scrubSets) orgRedactor(org string) redactor {
	if o, ok := ss.byOrg[org]; ok {
		return redactor{o.secrets}
	}
	return redactor{emptySecrets}
}

// allRedactor returns the matchers of every org: all, then the orgs not yet in all.
func (ss *scrubSets) allRedactor() redactor {
	r := make(redactor, 0, 1+len(ss.pending))
	r = append(r, ss.all)
	for _, org := range ss.pending {
		if o, ok := ss.byOrg[org]; ok {
			r = append(r, o.secrets)
		}
	}
	return r
}

// redactor applies one or more matchers in turn.
type redactor []*scrub.Secrets

func (r redactor) Redact(in string) string {
	for _, s := range r {
		in = s.Redact(in)
	}
	return in
}

func (r redactor) empty() bool {
	for _, s := range r {
		if s.Len() > 0 {
			return false
		}
	}
	return true
}

type scrubOrgKey struct{}

// WithScrubOrg returns ctx marked as the context of a request of an agent of org. The logs and spans written
// with it, and the audit rows of the request, are scrubbed only with the values of org. The gateway marks each
// agent request; a [Builtin] or an [Audit] gets the marked context.
func WithScrubOrg(ctx context.Context, org string) context.Context {
	return context.WithValue(ctx, scrubOrgKey{}, org)
}

// scrubOrgOf returns the org that ctx is marked with.
func scrubOrgOf(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	org, ok := ctx.Value(scrubOrgKey{}).(string)
	return org, ok
}

// scope is where a text goes: the org of a request, or the process.
type scope struct {
	org    string
	scoped bool
}

func scopeOf(ctx context.Context) scope {
	org, ok := scrubOrgOf(ctx)
	return scope{org: org, scoped: ok}
}

func (s *CredentialScrub) redactorFor(sc scope) redactor {
	ss := s.sets.Load()
	if sc.scoped {
		return ss.orgRedactor(sc.org)
	}
	return ss.allRedactor()
}

// NewCredentialScrub returns an empty CredentialScrub.
func NewCredentialScrub() *CredentialScrub {
	s := &CredentialScrub{now: time.Now}
	s.sets.Store(&scrubSets{byOrg: map[string]*orgSet{}, all: emptySecrets})
	return s
}

// String never prints a value.
func (s *CredentialScrub) String() string { return "mcpgateway.CredentialScrub" }

// GoString never prints a value.
func (s *CredentialScrub) GoString() string { return "mcpgateway.CredentialScrub" }

// use sets the counters and the logger of the short-value warning. [New] calls it.
func (s *CredentialScrub) use(m *Metrics, log *slog.Logger) {
	if m != nil {
		s.metrics.Store(m)
	}
	if log != nil {
		s.log.Store(log)
	}
}

func (s *CredentialScrub) retain() time.Duration {
	if s.Retain > 0 {
		return s.Retain
	}
	return 24 * time.Hour
}

func (s *CredentialScrub) maxOrgValues() int {
	if s.MaxOrgValues > 0 {
		return s.MaxOrgValues
	}
	return 256
}

func (s *CredentialScrub) maxValues() int {
	if s.MaxValues > 0 {
		return s.MaxValues
	}
	return 8192
}

// Add records opened credential values for no org: they are scrubbed from process telemetry, and from the
// texts of the org "".
func (s *CredentialScrub) Add(values ...string) { s.AddFor("", values...) }

// AddFor records credential values that a call of an agent of org opened. Each value also adds the token after a
// known auth scheme ([scrub.AuthParts]). Values shorter than [scrub.MinSecretLen] are ignored.
func (s *CredentialScrub) AddFor(org string, values ...string) {
	var parts []string
	for _, v := range values {
		for _, p := range scrub.AuthParts(v) {
			if p = strings.TrimSpace(p); len(p) >= scrub.MinSecretLen && !slices.Contains(parts, p) {
				parts = append(parts, p)
			}
		}
	}
	now := s.now()
	// The fast path: every value is known, so only its last open changes. It takes no lock and builds nothing.
	if o, ok := s.sets.Load().byOrg[org]; ok && now.UnixNano()-s.lastPrune.Load() < int64(pruneEvery) {
		known := true
		for _, p := range parts {
			if t, ok := o.last[p]; ok {
				t.Store(now.UnixNano())
			} else {
				known = false
			}
		}
		if known {
			return
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := s.sets.Load()
	next := &scrubSets{byOrg: maps.Clone(ss.byOrg), all: ss.all, pending: slices.Clone(ss.pending), allBuilt: ss.allBuilt}
	changed := map[string]bool{}
	removed := false
	if len(parts) > 0 {
		last := map[string]*atomic.Int64{}
		if o, ok := next.byOrg[org]; ok {
			last = maps.Clone(o.last)
		}
		for _, p := range parts {
			if t, ok := last[p]; ok {
				t.Store(now.UnixNano())
				continue
			}
			t := &atomic.Int64{}
			t.Store(now.UnixNano())
			last[p] = t
			changed[org] = true
		}
		if len(last) > s.maxOrgValues() {
			dropOldest(last, s.maxOrgValues())
			removed = true
		}
		if changed[org] {
			next.byOrg[org] = &orgSet{last: last}
		}
	}
	if now.UnixNano()-s.lastPrune.Load() >= int64(pruneEvery) {
		s.lastPrune.Store(now.UnixNano())
		removed = s.pruneLocked(next, now, changed) || removed
	}
	if total := countValues(next); total > s.maxValues() {
		s.capLocked(next, changed)
		removed = true
	}
	if len(changed) == 0 {
		return
	}
	for o := range changed {
		set, ok := next.byOrg[o]
		if !ok {
			continue
		}
		if len(set.last) == 0 {
			delete(next.byOrg, o)
			continue
		}
		next.byOrg[o] = &orgSet{secrets: scrub.NewSecrets(slices.Collect(maps.Keys(set.last))...), last: set.last}
		if !slices.Contains(next.pending, o) {
			next.pending = append(next.pending, o)
		}
	}
	// The matcher of all orgs is rebuilt at most once a second, and at once when a value was removed, so a removed
	// value does not stay in it. Until then the changed orgs are pending: their own matchers are applied after it.
	if removed || now.Sub(next.allBuilt) >= allRebuildEvery {
		all := map[string]bool{}
		for _, o := range next.byOrg {
			for v := range o.last {
				all[v] = true
			}
		}
		next.all = scrub.NewSecrets(slices.Collect(maps.Keys(all))...)
		next.pending = nil
		next.allBuilt = now
	}
	s.sets.Store(next)
}

// dropOldest removes the values opened longest ago from last until keep minus a tenth remain, so a full set is not
// sorted and rebuilt at each open.
func dropOldest(last map[string]*atomic.Int64, keep int) {
	keep -= keep / 10
	type entry struct {
		value string
		at    int64
	}
	// The fast path can change a time during the sort: sort a copy of the times.
	all := make([]entry, 0, len(last))
	for v, t := range last {
		all = append(all, entry{v, t.Load()})
	}
	slices.SortFunc(all, func(a, b entry) int { return cmp.Compare(a.at, b.at) })
	for _, e := range all[:max(0, len(all)-keep)] {
		delete(last, e.value)
	}
}

func countValues(ss *scrubSets) int {
	n := 0
	for _, o := range ss.byOrg {
		n += len(o.last)
	}
	return n
}

// pruneLocked drops from next the values not opened within Retain and marks the orgs it changed. It reports
// whether it removed a value.
func (s *CredentialScrub) pruneLocked(next *scrubSets, now time.Time, changed map[string]bool) bool {
	removed := false
	for org, o := range next.byOrg {
		var last map[string]*atomic.Int64
		for v, t := range o.last {
			if now.Sub(time.Unix(0, t.Load())) <= s.retain() {
				continue
			}
			if last == nil {
				last = maps.Clone(o.last)
			}
			delete(last, v)
		}
		if last != nil {
			next.byOrg[org] = &orgSet{last: last}
			changed[org] = true
			removed = true
		}
	}
	return removed
}

// capLocked drops the oldest values of all orgs until MaxValues minus a tenth remain, and marks the orgs it
// changed.
func (s *CredentialScrub) capLocked(next *scrubSets, changed map[string]bool) {
	type entry struct {
		org, value string
		at         int64
	}
	var all []entry
	for org, o := range next.byOrg {
		for v, t := range o.last {
			all = append(all, entry{org, v, t.Load()})
		}
	}
	keep := s.maxValues() - s.maxValues()/10
	slices.SortFunc(all, func(a, b entry) int { return cmp.Compare(a.at, b.at) })
	cloned := map[string]bool{}
	for _, e := range all[:max(0, len(all)-keep)] {
		o := next.byOrg[e.org]
		if !cloned[e.org] {
			o = &orgSet{last: maps.Clone(o.last)}
			next.byOrg[e.org] = o
			cloned[e.org] = true
		}
		delete(o.last, e.value)
		changed[e.org] = true
	}
}

// Text returns in with every recorded value of every org replaced by [redacted:secret]. It is for process
// telemetry that belongs to no org.
func (s *CredentialScrub) Text(in string) string { return s.textIn(scope{}, in) }

// TextFor returns in with every value recorded for org replaced. It is for a text that belongs to one org: an
// audit row of a call of an agent of org.
func (s *CredentialScrub) TextFor(org, in string) string {
	return s.textIn(scope{org: org, scoped: true}, in)
}

func (s *CredentialScrub) textIn(sc scope, in string) string {
	out := s.redactorFor(sc).Redact(in)
	if out != in {
		s.metrics.Load().redacted(redactTelemetry)
	}
	return out
}

// Error returns err unchanged when its text holds no recorded value of any org, else an error with the scrubbed
// text. The new error does not give the old text back: it has no Unwrap. errors.Is still sees the old chain, and
// errors.As still finds an [*OpenError], as a copy with its reason and key ID only.
func (s *CredentialScrub) Error(err error) error { return s.errorIn(scope{}, err) }

func (s *CredentialScrub) errorIn(sc scope, err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if out := s.textIn(sc, msg); out != msg {
		return &scrubbedError{msg: out, err: err}
	}
	return err
}

// scrubbedError is an error whose text held a credential.
type scrubbedError struct {
	msg string
	err error
}

func (e *scrubbedError) Error() string { return e.msg }

// Is reports whether the old error matches target. It gives no text back.
func (e *scrubbedError) Is(target error) bool { return errors.Is(e.err, target) }

// As finds an [*OpenError] in the old chain and gives a copy of it with no wrapped error: its text is fixed.
func (e *scrubbedError) As(target any) bool {
	p, ok := target.(**OpenError)
	if !ok {
		return false
	}
	var oe *OpenError
	if !errors.As(e.err, &oe) {
		return false
	}
	*p = &OpenError{Reason: oe.Reason, KeyID: oe.KeyID}
	return true
}

// Source returns src with every credential it gives recorded before the caller can use it. The gateway wraps
// its own source with it, and gives the wrapped source to every [CredentialBuiltin].
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
		token := strings.TrimSpace(strings.TrimPrefix(c.Value, t.CredentialPrefix))
		r.s.AddFor(agent.Org, c.Value, token)
		if len(token) < scrub.MinSecretLen {
			r.s.short(agent.Org, t.Name)
		}
	}
	return c, r.s.errorIn(scope{org: agent.Org, scoped: true}, err)
}

// short counts a credential too short to scrub, and logs it once for each org and target. The line names the
// target, never the value.
func (s *CredentialScrub) short(org, target string) {
	s.metrics.Load().shortCredential()
	if _, logged := s.warned.LoadOrStore(scopedTarget{org: org, target: target}, struct{}{}); logged {
		return
	}
	if log := s.log.Load(); log != nil {
		log.Warn("credential too short to scrub from logs and results", "org", org, "target", target,
			"min_bytes", scrub.MinSecretLen)
	}
}

// Result scrubs everything an agent of org gets from a tool call, with the values recorded for org. It scrubs the
// bytes of each blob (an embedded resource, an image, an audio clip) as text, then the JSON form of the whole
// result, so every field is covered, including fields a later SDK adds. A result with no match is returned
// unchanged. If the scrubbed JSON does not decode, the agent gets an error result rather than the value.
func (s *CredentialScrub) Result(org string, res *mcp.CallToolResult) *mcp.CallToolResult {
	set := s.sets.Load().orgRedactor(org)
	if res == nil || set.empty() {
		return res
	}
	changed := false
	for _, c := range res.Content {
		switch c := c.(type) {
		case *mcp.EmbeddedResource:
			if c.Resource != nil {
				c.Resource.Blob = redactBytes(set, c.Resource.Blob, &changed)
			}
		case *mcp.ImageContent:
			c.Data = redactBytes(set, c.Data, &changed)
		case *mcp.AudioContent:
			c.Data = redactBytes(set, c.Data, &changed)
		}
	}
	clean, ok := scrubJSON(set, res, &changed)
	if changed {
		s.metrics.Load().redacted(redactAgent)
	}
	if !ok {
		return scrubbedResult()
	}
	return clean
}

// Tools scrubs the tools a target of org listed, with the values recorded for org: each tool's name, title,
// description, schemas, annotations and metadata, through its JSON form. A target can quote the credential it was
// sent in any of them. A tool whose scrubbed JSON does not decode is left out, so nobody gets the value. The
// gateway calls it as soon as a target answers, so the guard, the pin hash, the prober and the agent all see one
// text.
func (s *CredentialScrub) Tools(org string, tools []*mcp.Tool) []*mcp.Tool {
	set := s.sets.Load().orgRedactor(org)
	if set.empty() {
		return tools
	}
	changed := false
	out := make([]*mcp.Tool, 0, len(tools))
	for _, t := range tools {
		if t == nil {
			continue
		}
		if clean, ok := scrubJSON(set, t, &changed); ok {
			out = append(out, clean)
		}
	}
	if changed {
		s.metrics.Load().redacted(redactAgent)
	}
	return out
}

// ToolList scrubs the tool list an agent of org gets, as [CredentialScrub.Tools] does, and its metadata. It
// changes res in place and returns it.
func (s *CredentialScrub) ToolList(org string, res *mcp.ListToolsResult) *mcp.ListToolsResult {
	if res == nil {
		return res
	}
	res.Tools = s.Tools(org, res.Tools)
	if res.Meta != nil {
		changed := false
		if m, ok := scrubJSON(s.sets.Load().orgRedactor(org), &res.Meta, &changed); ok {
			res.Meta = *m
		} else {
			res.Meta = nil
		}
		if changed {
			s.metrics.Load().redacted(redactAgent)
		}
	}
	return res
}

// scrubJSON returns v unchanged when its JSON form holds no value of set, else a new value decoded from the
// scrubbed JSON, and sets *changed. ok is false when v does not encode or the scrubbed JSON does not decode.
func scrubJSON[T any](set redactor, v *T, changed *bool) (_ *T, ok bool) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	out := set.Redact(string(raw))
	if out == string(raw) {
		return v, true
	}
	*changed = true
	clean := new(T)
	if json.Unmarshal([]byte(out), clean) != nil {
		return nil, false
	}
	return clean, true
}

// redactBytes returns b with every value of set in its text replaced, or b itself when it holds none.
func redactBytes(set redactor, b []byte, changed *bool) []byte {
	if len(b) == 0 {
		return b
	}
	if out := set.Redact(string(b)); out != string(b) {
		*changed = true
		return []byte(out)
	}
	return b
}

func scrubbedResult() *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{
		Text: "koinon-gateway removed a credential from this tool result and could not rebuild it",
	}}}
}

// Handler returns h with every log record scrubbed before h sees it. A record whose context is marked with an
// org ([WithScrubOrg]) is scrubbed with that org's values: the message, every attribute key and value, the
// attributes bound with WithAttrs and the group names. Any other record is scrubbed with the values of every org,
// in the message and the attribute values only. Bound attributes are scrubbed at each record, so a value opened
// after the bind is removed too.
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
	sets *scrubSets
	sc   scope
	h    slog.Handler
}

func (h *scrubHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *scrubHandler) Handle(ctx context.Context, r slog.Record) error {
	sc := scopeOf(ctx)
	ss := h.s.sets.Load()
	set := ss.allRedactor()
	if sc.scoped {
		set = ss.orgRedactor(sc.org)
	}
	a := attrScrub{set: set, keys: sc.scoped}
	inner := h.inner
	if len(h.ops) > 0 {
		if b := h.cache.Load(); b != nil && b.sets == ss && b.sc == sc {
			inner = b.h
		} else {
			for _, op := range h.ops {
				if op.group != "" {
					inner = inner.WithGroup(a.key(op.group))
				} else {
					inner = inner.WithAttrs(a.attrs(op.attrs))
				}
			}
			h.cache.Store(&builtHandler{sets: ss, sc: sc, h: inner})
		}
	}
	msg := set.Redact(r.Message)
	a.changed = a.changed || msg != r.Message
	out := slog.NewRecord(r.Time, r.Level, msg, r.PC)
	r.Attrs(func(x slog.Attr) bool {
		out.AddAttrs(a.attr(x))
		return true
	})
	if a.changed {
		h.s.metrics.Load().redacted(redactTelemetry)
	}
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

// attrScrub scrubs log attributes with set. keys is false for process records: their keys and group names are
// code constants and are kept.
type attrScrub struct {
	set     redactor
	keys    bool
	changed bool
}

func (a *attrScrub) key(k string) string {
	if !a.keys {
		return k
	}
	out := a.set.Redact(k)
	a.changed = a.changed || out != k
	return out
}

func (a *attrScrub) text(v string) string {
	out := a.set.Redact(v)
	a.changed = a.changed || out != v
	return out
}

func (a *attrScrub) attrs(attrs []slog.Attr) []slog.Attr {
	out := make([]slog.Attr, len(attrs))
	for i, x := range attrs {
		out[i] = a.attr(x)
	}
	return out
}

func (a *attrScrub) attr(x slog.Attr) slog.Attr {
	x.Key = a.key(x.Key)
	v := x.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		x.Value = slog.StringValue(a.text(v.String()))
	case slog.KindGroup:
		x.Value = slog.GroupValue(a.attrs(v.Group())...)
	case slog.KindAny:
		x.Value = a.any(v)
	default:
		x.Value = v
	}
	return x
}

// any turns a value that would print a recorded credential into its scrubbed text. A value that prints none is
// kept as it is, so a handler still formats it in its own way. A value is read as its error text, its bytes, its
// String, its JSON form and its %+v form, because a handler prints it in one of those.
func (a *attrScrub) any(v slog.Value) slog.Value {
	x := v.Any()
	var texts []string
	switch x := x.(type) {
	case error:
		texts = append(texts, x.Error())
	case []byte:
		// A text handler writes a []byte as a string; %+v would print numbers and hide the match.
		texts = append(texts, string(x))
	case json.RawMessage:
		texts = append(texts, string(x))
	case fmt.Stringer:
		texts = append(texts, x.String())
	}
	if raw, err := json.Marshal(x); err == nil {
		texts = append(texts, string(raw))
	}
	texts = append(texts, fmt.Sprintf("%+v", x))
	for _, t := range texts {
		if out := a.set.Redact(t); out != t {
			a.changed = true
			return slog.StringValue(out)
		}
	}
	return v
}

// Tracer returns t with every span scrubbed: its name, attributes, status text, events, links and recorded
// errors. A span started with a context marked with an org ([WithScrubOrg]) is scrubbed with that org's values,
// keys included; any other span with the values of every org, in its values and free text only. A span that t
// starts is put in the returned context in its scrubbed form, so code that takes the span from the context writes
// through the scrub too.
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
	sp := scrubSpan{s: t.s, sc: scopeOf(ctx)}
	k := sp.kv()
	cfg := trace.NewSpanStartConfig(opts...)
	clean := []trace.SpanStartOption{trace.WithSpanKind(cfg.SpanKind())}
	if attrs := cfg.Attributes(); len(attrs) > 0 {
		clean = append(clean, trace.WithAttributes(k.kvs(attrs)...))
	}
	if links := cfg.Links(); len(links) > 0 {
		clean = append(clean, trace.WithLinks(k.links(links)...))
	}
	if cfg.NewRoot() {
		clean = append(clean, trace.WithNewRoot())
	}
	if ts := cfg.Timestamp(); !ts.IsZero() {
		clean = append(clean, trace.WithTimestamp(ts))
	}
	ctx, sp.Span = t.Tracer.Start(ctx, k.text(name), clean...)
	k.count()
	return trace.ContextWithSpan(ctx, sp), sp
}

type scrubSpan struct {
	trace.Span
	s  *CredentialScrub
	sc scope
}

// kv returns the scrub of one span write, with the matchers as they are now.
func (sp scrubSpan) kv() *kvScrub {
	return &kvScrub{attrScrub: attrScrub{set: sp.s.redactorFor(sp.sc), keys: sp.sc.scoped}, s: sp.s}
}

func (sp scrubSpan) SetName(name string) {
	k := sp.kv()
	sp.Span.SetName(k.text(name))
	k.count()
}

func (sp scrubSpan) SetAttributes(kv ...attribute.KeyValue) {
	k := sp.kv()
	sp.Span.SetAttributes(k.kvs(kv)...)
	k.count()
}

func (sp scrubSpan) SetStatus(code codes.Code, description string) {
	k := sp.kv()
	sp.Span.SetStatus(code, k.text(description))
	k.count()
}

func (sp scrubSpan) AddEvent(name string, opts ...trace.EventOption) {
	k := sp.kv()
	sp.Span.AddEvent(k.text(name), k.eventOptions(opts)...)
	k.count()
}

func (sp scrubSpan) RecordError(err error, opts ...trace.EventOption) {
	k := sp.kv()
	if err != nil {
		if msg := err.Error(); k.text(msg) != msg {
			err = errors.New(k.text(msg))
		}
	}
	sp.Span.RecordError(err, k.eventOptions(opts)...)
	k.count()
}

func (sp scrubSpan) AddLink(l trace.Link) {
	k := sp.kv()
	l.Attributes = k.kvs(l.Attributes)
	sp.Span.AddLink(l)
	k.count()
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

// kvScrub scrubs span texts and attributes.
type kvScrub struct {
	attrScrub
	s *CredentialScrub
}

func (k *kvScrub) count() {
	if k.changed {
		k.s.metrics.Load().redacted(redactTelemetry)
	}
}

func (k *kvScrub) eventOptions(opts []trace.EventOption) []trace.EventOption {
	if len(opts) == 0 {
		return nil
	}
	cfg := trace.NewEventConfig(opts...)
	clean := []trace.EventOption{trace.WithTimestamp(cfg.Timestamp())}
	if attrs := cfg.Attributes(); len(attrs) > 0 {
		clean = append(clean, trace.WithAttributes(k.kvs(attrs)...))
	}
	if cfg.StackTrace() {
		clean = append(clean, trace.WithStackTrace(true))
	}
	return clean
}

func (k *kvScrub) links(links []trace.Link) []trace.Link {
	out := make([]trace.Link, len(links))
	for i, l := range links {
		l.Attributes = k.kvs(l.Attributes)
		out[i] = l
	}
	return out
}

func (k *kvScrub) kvs(kvs []attribute.KeyValue) []attribute.KeyValue {
	if len(kvs) == 0 || k.set.empty() {
		return kvs
	}
	out := make([]attribute.KeyValue, len(kvs))
	for i, kv := range kvs {
		out[i] = attribute.KeyValue{Key: attribute.Key(k.key(string(kv.Key))), Value: k.value(kv.Value)}
	}
	return out
}

// value scrubs every attribute type that holds text or bytes: a string, a string slice, a byte slice, and the
// values in a slice or a map.
func (k *kvScrub) value(v attribute.Value) attribute.Value {
	switch v.Type() {
	case attribute.STRING:
		return attribute.StringValue(k.text(v.AsString()))
	case attribute.STRINGSLICE:
		ss := v.AsStringSlice()
		for j := range ss {
			ss[j] = k.text(ss[j])
		}
		return attribute.StringSliceValue(ss)
	case attribute.BYTESLICE:
		b := v.AsByteSlice()
		if out := k.text(string(b)); out != string(b) {
			return attribute.ByteSliceValue([]byte(out))
		}
		return v
	case attribute.SLICE:
		vs := v.AsSlice()
		for j := range vs {
			vs[j] = k.value(vs[j])
		}
		return attribute.SliceValue(vs...)
	case attribute.MAP:
		return attribute.MapValue(k.kvs(v.AsMap())...)
	}
	return v
}
