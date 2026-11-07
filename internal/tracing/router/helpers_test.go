// SPDX-License-Identifier: Apache-2.0

package router_test

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/stateracloud/koinon/internal/tracedest"
	"github.com/stateracloud/koinon/internal/tracing/export"
	"github.com/stateracloud/koinon/internal/tracing/router"
	"github.com/stateracloud/koinon/internal/tracing/scrub"
	"github.com/stateracloud/koinon/internal/tracing/semconv"
)

// Fake secrets: obviously fake and low-entropy (AGENTS.md).
const (
	fakeGatewayKey = "kgw_koinon_test_not-a-real-key"
	fakeOrgSecret  = "koinon_test_not-a-real-token-orgsecret"
	fakeEmail      = "mara@example.test"
)

// The specs wait on state, never on a time. These deadlines only tell a hang from a slow machine: the
// a loaded 3-CPU external gate has taken seconds for steps that take milliseconds alone (koinon-private#533, koinon-private#612).
const (
	// hangBound ends a wait that would otherwise never end.
	hangBound = time.Minute
	// exportTimeout bounds one export to a fake backend. No spec here tests the timeout itself.
	exportTimeout = 30 * time.Second
)

func uid(s string) pgtype.UUID {
	var id pgtype.UUID
	Expect(id.Scan(s)).To(Succeed())
	return id
}

// backend is a fake OTLP backend that records the requests it gets.
type backend struct {
	*httptest.Server
	mu     sync.Mutex
	bodies [][]byte
	reqs   []*coltracepb.ExportTraceServiceRequest
	status int
}

func newBackend() *backend {
	b := &backend{status: 200}
	b.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req coltracepb.ExportTraceServiceRequest
		_ = proto.Unmarshal(body, &req)
		b.mu.Lock()
		status := b.status
		if status == 200 {
			b.bodies = append(b.bodies, body)
			b.reqs = append(b.reqs, &req)
		}
		b.mu.Unlock()
		if status != 200 {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		out, _ := proto.Marshal(&coltracepb.ExportTraceServiceResponse{})
		_, _ = w.Write(out)
	}))
	DeferCleanup(b.Close)
	return b
}

func (b *backend) setStatus(s int) {
	b.mu.Lock()
	b.status = s
	b.mu.Unlock()
}

// spans returns every span received, ordered by start time then name.
func (b *backend) spans() []*tracepb.Span {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []*tracepb.Span
	for _, r := range b.reqs {
		for _, rs := range r.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				out = append(out, ss.Spans...)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].StartTimeUnixNano != out[j].StartTimeUnixNano {
			return out[i].StartTimeUnixNano < out[j].StartTimeUnixNano
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func (b *backend) raw() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Join(b.bodies, nil)
}

func (b *backend) named(name string) *tracepb.Span {
	for _, sp := range b.spans() {
		if sp.Name == name {
			return sp
		}
	}
	return nil
}

func (b *backend) route(id string, platform bool, level string) tracedest.Route {
	return tracedest.Route{
		ID: id, Name: id, Platform: platform, Level: level,
		Destination:        export.Destination{Preset: export.Generic, Endpoint: b.URL + "/v1/traces", Secrets: map[string]string{"X-Test": "koinon_test_not-a-real-token"}},
		ResourceAttributes: map[string]string{"deployment.environment": "test"},
	}
}

// attr returns an attribute as text ("" when absent); ok says whether it is there.
func attr(sp *tracepb.Span, k string) (string, bool) {
	for _, kv := range sp.Attributes {
		if kv.Key == k {
			switch v := kv.Value.Value.(type) {
			case *commonpb.AnyValue_StringValue:
				return v.StringValue, true
			case *commonpb.AnyValue_IntValue:
				return strconv.FormatInt(v.IntValue, 10), true
			case *commonpb.AnyValue_BoolValue:
				if v.BoolValue {
					return "true", true
				}
				return "false", true
			case *commonpb.AnyValue_DoubleValue:
				return strconv.FormatFloat(v.DoubleValue, 'f', -1, 64), true
			}
			return "", true
		}
	}
	return "", false
}

func has(sp *tracepb.Span, k string) bool { _, ok := attr(sp, k); return ok }

// fakeStore is the Postgres side: which runs and turns exist, and the ledger rows.
type fakeStore struct {
	mu    sync.Mutex
	turns map[[16]byte]fakeTurn
	runs  map[[32]byte]string // org+run -> "traced" | "untraced"
	calls []router.ModelCall
	reads int
	// failCalls makes ModelCalls fail.
	failCalls bool
}

type fakeTurn struct {
	run  pgtype.UUID
	turn router.Turn
}

func newStore() *fakeStore {
	return &fakeStore{turns: map[[16]byte]fakeTurn{}, runs: map[[32]byte]string{}}
}

func key(org, run pgtype.UUID) (k [32]byte) {
	copy(k[:16], org.Bytes[:])
	copy(k[16:], run.Bytes[:])
	return
}

func (s *fakeStore) addRun(org, run pgtype.UUID, traced bool, trace [16]byte, turn router.Turn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if traced {
		s.runs[key(org, run)] = "traced"
	} else {
		s.runs[key(org, run)] = "untraced"
	}
	s.turns[trace] = fakeTurn{run: run, turn: turn}
}

func (s *fakeStore) setCalls(c []router.ModelCall) {
	s.mu.Lock()
	s.calls = c
	s.mu.Unlock()
}

func (s *fakeStore) Attribute(_ context.Context, org, run pgtype.UUID, trace [16]byte) (router.Turn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.runs[key(org, run)] {
	case "":
		return router.Turn{}, router.ErrUnknownRun
	case "untraced":
		return router.Turn{}, router.ErrUntraced
	}
	t, ok := s.turns[trace]
	if !ok || t.run != run {
		return router.Turn{}, router.ErrUnknownTrace
	}
	return t.turn, nil
}

func (s *fakeStore) ModelCalls(_ context.Context, _, _ pgtype.UUID, _ int32, from, to time.Time) ([]router.ModelCall, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	if s.failCalls {
		return nil, context.DeadlineExceeded
	}
	var out []router.ModelCall
	for _, c := range s.calls {
		if !c.Time.Before(from) && c.Time.Before(to) {
			out = append(out, c)
		}
	}
	return out, nil
}

type fakeRoutes struct {
	mu  sync.Mutex
	m   map[pgtype.UUID][]tracedest.Route
	ver int
	// reads counts Routes calls, to see what the fingerprint saves.
	reads int
}

// ConfigVersion changes whenever set is called, as the real fingerprint changes with the settings.
func (f *fakeRoutes) ConfigVersion(context.Context, pgtype.UUID) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strconv.Itoa(f.ver), nil
}

func (f *fakeRoutes) Routes(_ context.Context, org pgtype.UUID) ([]tracedest.Route, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	return f.m[org], nil
}

func (f *fakeRoutes) set(org pgtype.UUID, r ...tracedest.Route) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.m == nil {
		f.m = map[pgtype.UUID][]tracedest.Route{}
	}
	f.ver++
	f.m[org] = r
}

// fakeSecrets is the org secret store; Version changes with every set or failure change, as the
// real fingerprint changes with the stored envelopes.
type fakeSecrets struct {
	mu        sync.Mutex
	sets      map[pgtype.UUID][]string
	err       error
	ver       int
	versioned bool
}

func (f *fakeSecrets) Secrets(_ context.Context, org pgtype.UUID) (*scrub.Secrets, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return scrub.NewSecrets(f.sets[org]...), nil
}

func (f *fakeSecrets) setErr(err error) {
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
}

func (f *fakeSecrets) set(org pgtype.UUID, values ...string) {
	f.mu.Lock()
	f.sets[org] = values
	f.ver++
	f.mu.Unlock()
}

// versionedSecrets is a fakeSecrets that also fingerprints its content.
type versionedSecrets struct{ *fakeSecrets }

func (v versionedSecrets) Version(context.Context, pgtype.UUID) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.err != nil {
		return "", v.err
	}
	return strconv.Itoa(v.ver), nil
}

type fakeHealth struct {
	mu      sync.Mutex
	ok      int
	fail    int
	dropped int64
	last    string
}

func (h *fakeHealth) Success(context.Context, pgtype.UUID, tracedest.Route) error {
	h.mu.Lock()
	h.ok++
	h.mu.Unlock()
	return nil
}

func (h *fakeHealth) Failure(_ context.Context, _ pgtype.UUID, _ tracedest.Route, msg string, dropped int64) error {
	h.mu.Lock()
	h.fail++
	h.dropped += dropped
	h.last = msg
	h.mu.Unlock()
	return nil
}

func (h *fakeHealth) counts() (ok, fail int, dropped int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ok, h.fail, h.dropped
}

// clock is the router's time: real time plus whatever a test has advanced, so waits and rate limits
// keep running while a test moves the ledger deadline by whole seconds. A frozen clock moves only
// when the test advances it, for a spec about an interval rather than a wait.
type clock struct {
	mu     sync.Mutex
	base   time.Time
	start  time.Time
	skew   time.Duration
	frozen bool
}

func newClock(base time.Time) *clock { return &clock{base: base, start: time.Now()} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.frozen {
		return c.base.Add(c.skew)
	}
	return c.base.Add(c.skew).Add(time.Since(c.start))
}

// Freeze stops the clock where it is; Advance still moves it.
func (c *clock) Freeze() {
	c.mu.Lock()
	c.base, c.skew, c.frozen = c.base.Add(c.skew).Add(time.Since(c.start)), 0, true
	c.mu.Unlock()
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.skew += d
	c.mu.Unlock()
}

// SetNow moves the clock to t.
func (c *clock) SetNow(t time.Time) {
	c.mu.Lock()
	c.base, c.start, c.skew = t, time.Now(), 0
	c.mu.Unlock()
}

// env wires a router to fakes and runs it.
type env struct {
	store   *fakeStore
	routes  *fakeRoutes
	secrets *fakeSecrets
	health  *fakeHealth
	clock   *clock
	r       *router.Router
	srv     *httptest.Server
}

var t0 = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

func newEnv(mod func(*router.Config), secrets *fakeSecrets) *env {
	e := &env{store: newStore(), routes: &fakeRoutes{}, secrets: secrets, health: &fakeHealth{}, clock: newClock(t0.Add(time.Hour))}
	client, err := export.NewClient((&net.Dialer{Timeout: exportTimeout}).DialContext, exportTimeout)
	Expect(err).NotTo(HaveOccurred())
	cfg := router.Config{
		Store: e.store, Routes: e.routes, Secrets: secretSource(e.secrets), Health: e.health, Sender: client,
		Now: e.clock.Now, Metrics: router.NewMetrics(nil),
		Linger: 2 * time.Millisecond, FlushInterval: 5 * time.Millisecond, PollInterval: time.Hour, ConfigCheck: time.Millisecond, RecheckInterval: time.Hour,
		BackoffBase: time.Millisecond, BackoffMax: 5 * time.Millisecond, RouteTTL: time.Millisecond,
	}
	if mod != nil {
		mod(&cfg)
	}
	e.r, err = router.New(cfg)
	Expect(err).NotTo(HaveOccurred())
	ctx, cancel := context.WithCancel(context.Background())
	e.r.Start(ctx)
	e.srv = httptest.NewServer(e.r.Handler())
	DeferCleanup(func() {
		e.srv.Close()
		cancel()
		e.r.Close()
		client.Close()
	})
	return e
}

// post sends spans the way agentd's exporter does.
func (e *env) post(spans ...*tracepb.Span) *coltracepb.ExportTraceServiceResponse {
	req := &coltracepb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{
		Resource:   &resourcepb.Resource{},
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: spans}},
	}}}
	body, err := proto.Marshal(req)
	Expect(err).NotTo(HaveOccurred())
	resp := httpDo(http.MethodPost, e.srv.URL, "application/x-protobuf", bytes.NewReader(body))
	defer func() { _ = resp.Body.Close() }()
	Expect(resp.StatusCode).To(Equal(200))
	out, _ := io.ReadAll(resp.Body)
	var r coltracepb.ExportTraceServiceResponse
	Expect(proto.Unmarshal(out, &r)).To(Succeed())
	return &r
}

// turn is one traced turn of a run, as agentd would have recorded it.
type turn struct {
	org, run pgtype.UUID
	trace    [16]byte
	start    time.Time
	end      time.Time
}

func newTurn(e *env, org, run string, n byte) turn {
	tr := turn{org: uid(org), run: uid(run), start: t0, end: t0.Add(10 * time.Second)}
	tr.trace[0], tr.trace[15] = 0x7a, n
	end := tr.end
	e.store.addRun(tr.org, tr.run, true, tr.trace, router.Turn{Seq: 1, StartedAt: tr.start, EndedAt: &end})
	return tr
}

func str(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

func strs(k string, vs ...string) *commonpb.KeyValue {
	arr := &commonpb.ArrayValue{}
	for _, v := range vs {
		arr.Values = append(arr.Values, &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}})
	}
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: arr}}}
}

// root is agentd's root span of the turn.
func (t turn) root(content bool) *tracepb.Span {
	id := semconv.RootSpanID(t.trace)
	sp := &tracepb.Span{
		TraceId: t.trace[:], SpanId: id[:], Name: "invoke_agent Support Bot", Kind: tracepb.Span_SPAN_KIND_INTERNAL,
		StartTimeUnixNano: uint64(t.start.UnixNano()), EndTimeUnixNano: uint64(t.end.UnixNano()),
		Attributes: []*commonpb.KeyValue{
			str(semconv.GenAIConversationID, t.run.String()), str(semconv.SessionID, t.run.String()),
			str(semconv.KoinonRun, t.run.String()), str(semconv.KoinonOrg, t.org.String()),
			str(semconv.KoinonTask, "0192f5a0-0000-7000-8000-000000000042"),
			str(semconv.GenAIOperationName, semconv.OperationInvokeAgent),
			str(semconv.GenAIAgentID, "0192f5a0-0000-7000-8000-0000000000a1"), str(semconv.GenAIAgentName, "Support Bot"),
			str(semconv.GenAIRequestModel, "claude-sonnet"),
		},
	}
	if content {
		sp.Attributes = append(sp.Attributes,
			str(semconv.GenAIInputMessages, `[{"role":"user","parts":[{"type":"text","content":"Please email `+fakeEmail+` about the invoice"}]}]`),
			strs(semconv.KoinonContentAttributes, semconv.GenAIInputMessages),
		)
	}
	return sp
}

// tool is agentd's execute_tool span, with a gateway key and an org secret in its arguments and an
// email in its result when content is on.
func (t turn) tool(content bool) *tracepb.Span {
	root := semconv.RootSpanID(t.trace)
	id := [8]byte{0xaa, 1, 2, 3, 4, 5, 6, 7}
	sp := &tracepb.Span{
		TraceId: t.trace[:], SpanId: id[:], ParentSpanId: root[:], Name: "execute_tool bash", Kind: tracepb.Span_SPAN_KIND_INTERNAL,
		StartTimeUnixNano: uint64(t.start.Add(time.Second).UnixNano()), EndTimeUnixNano: uint64(t.start.Add(2 * time.Second).UnixNano()),
		Attributes: []*commonpb.KeyValue{
			str(semconv.KoinonRun, t.run.String()), str(semconv.KoinonOrg, t.org.String()),
			str(semconv.GenAIToolName, "bash"), str(semconv.GenAIToolCallID, "call-1"),
			// A credential in a plain attribute must go too, at every level.
			str("koinon.tool.status", "failed with "+fakeOrgSecret),
		},
		Events: []*tracepb.Span_Event{{Name: "note", Attributes: []*commonpb.KeyValue{str("detail", "key "+fakeGatewayKey)}}},
	}
	if content {
		sp.Attributes = append(sp.Attributes,
			str(semconv.GenAIToolCallArguments, `{"command":"curl -H 'Authorization: Bearer `+fakeGatewayKey+`' --data `+fakeOrgSecret+`"}`),
			str(semconv.GenAIToolCallResult, `{"output":"sent to `+fakeEmail+`"}`),
			strs(semconv.KoinonContentAttributes, semconv.GenAIToolCallArguments, semconv.GenAIToolCallResult),
		)
	}
	return sp
}

func call(req string, at time.Duration, cost float64) router.ModelCall {
	c := cost
	return router.ModelCall{
		ModelID:   "0192f5a0-0000-7000-8000-0000000000d1",
		RequestID: req, Model: "claude-sonnet", Provider: "anthropic", Input: 1000, Output: 200, CacheRead: 600, CacheWrite: 100, Reasoning: 50,
		Cost: &c, GatewayCost: &c, Time: t0.Add(at),
	}
}

// av is attr without the presence flag, for Expect.
func av(sp *tracepb.Span, k string) string { v, _ := attr(sp, k); return v }

// httpDo sends one request, with no context-less shortcut.
func httpDo(method, url, contentType string, body io.Reader) *http.Response {
	req, err := http.NewRequestWithContext(context.Background(), method, url, body)
	Expect(err).NotTo(HaveOccurred())
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := (&http.Client{Timeout: hangBound}).Do(req)
	Expect(err).NotTo(HaveOccurred())
	return resp
}

// secretSource lets a test pick whether the secret store can be fingerprinted.
func secretSource(f *fakeSecrets) router.SecretSource {
	if f.versioned {
		return versionedSecrets{f}
	}
	return f
}

// newBlockingBody is a request body that never ends until the writer is closed.
func newBlockingBody() (*io.PipeReader, *io.PipeWriter) { return io.Pipe() }

func strVal(s string) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s}}
}

// timerSleep is the router's own sleep, for a test's Sleep that records or adds a step.
func timerSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
