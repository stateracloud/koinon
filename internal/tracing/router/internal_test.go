// SPDX-License-Identifier: Apache-2.0

package router

// Internal specs, from the review of the first router: they need the router's own types.

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/stateracloud/koinon/internal/tracedest"
	"github.com/stateracloud/koinon/internal/tracing/export"
	"github.com/stateracloud/koinon/internal/tracing/scrub"
	"github.com/stateracloud/koinon/internal/tracing/semconv"
)

var (
	pOrg  = mustUUID("11111111-1111-1111-1111-111111111111")
	pOrg2 = mustUUID("22222222-2222-2222-2222-222222222222")
	pRun  = mustUUID("33333333-3333-3333-3333-333333333333")
)

func mustUUID(s string) pgtype.UUID {
	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		panic(err)
	}
	return u
}

type pStore struct {
	gate  chan struct{}
	enter atomic.Int32
	calls []ModelCall
	// hang makes Attribute wait for its context or for release, as a database that does not answer
	// until it fails.
	hang    bool
	release chan struct{}
	attrs   atomic.Int32
}

func (s *pStore) Attribute(ctx context.Context, _, _ pgtype.UUID, _ [16]byte) (Turn, error) {
	s.attrs.Add(1)
	if s.hang {
		select {
		case <-ctx.Done():
			return Turn{}, ctx.Err()
		case <-s.release:
			return Turn{}, errors.New("database down")
		}
	}
	return Turn{Seq: 1, StartedAt: time.Unix(0, 0)}, nil
}

func (s *pStore) ModelCalls(ctx context.Context, _, _ pgtype.UUID, _ int32, _, _ time.Time) ([]ModelCall, error) {
	s.enter.Add(1)
	if s.gate != nil {
		<-s.gate
	}
	return s.calls, nil
}

type pRoutes struct {
	mu     sync.Mutex
	routes map[pgtype.UUID][]tracedest.Route
	ver    int
}

func (p *pRoutes) Routes(_ context.Context, org pgtype.UUID) ([]tracedest.Route, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.routes[org], nil
}

func (p *pRoutes) ConfigVersion(context.Context, pgtype.UUID) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return strconv.Itoa(p.ver), nil
}

func (p *pRoutes) set(org pgtype.UUID, routes ...tracedest.Route) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.routes == nil {
		p.routes = map[pgtype.UUID][]tracedest.Route{}
	}
	p.ver++
	p.routes[org] = routes
}

type pHealth struct{}

func (pHealth) Success(context.Context, pgtype.UUID, tracedest.Route) error { return nil }
func (pHealth) Failure(context.Context, pgtype.UUID, tracedest.Route, string, int64) error {
	return nil
}

type pSender struct {
	mu    sync.Mutex
	spans map[string]int // dest endpoint -> spans
	fail  map[string]bool
	calls atomic.Int32
	// rejected is the rejected_spans every export answers with.
	rejected int64
}

func (s *pSender) Export(_ context.Context, d export.Destination, req *coltracepb.ExportTraceServiceRequest) (export.Result, error) {
	s.calls.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail[d.Endpoint] {
		return export.Result{}, &export.Error{Kind: export.KindServer, Status: 503, Message: "down"}
	}
	for _, rs := range req.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			s.spans[d.Endpoint] += len(ss.Spans)
		}
	}
	return export.Result{Rejected: s.rejected}, nil
}

func route(id, endpoint string) tracedest.Route {
	return tracedest.Route{ID: id, Name: id, Level: "full", Destination: export.Destination{Preset: export.Generic, Endpoint: endpoint}}
}

func span(org pgtype.UUID, trace [16]byte, root bool, pad int) *tracepb.Span {
	sid := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	var parent []byte
	if root {
		sid = semconv.RootSpanID(trace)
	} else {
		parent = []byte{9, 9, 9, 9, 9, 9, 9, 9}
	}
	sp := &tracepb.Span{TraceId: trace[:], SpanId: sid[:], ParentSpanId: parent, Name: "x", EndTimeUnixNano: 1}
	sp.Attributes = append(sp.Attributes, kvStr(semconv.KoinonOrg, org.String()), kvStr(semconv.KoinonRun, pRun.String()))
	if pad > 0 {
		sp.Attributes = append(sp.Attributes, kvStr("pad", string(bytes.Repeat([]byte("a"), pad))))
	}
	return sp
}

func buildAmplified(n int, emptyResourceSpans bool) []byte {
	if emptyResourceSpans {
		b := make([]byte, 0, 2*n)
		for range n {
			b = protowire.AppendTag(b, 1, protowire.BytesType)
			b = protowire.AppendBytes(b, nil)
		}
		return b
	}
	var sp []byte
	sp = protowire.AppendTag(sp, 1, protowire.BytesType)
	sp = protowire.AppendBytes(sp, bytes.Repeat([]byte{7}, 16))
	sp = protowire.AppendTag(sp, 2, protowire.BytesType)
	sp = protowire.AppendBytes(sp, bytes.Repeat([]byte{7}, 8))
	for range n {
		sp = protowire.AppendTag(sp, 9, protowire.BytesType) // Span.attributes, empty KeyValue
		sp = protowire.AppendBytes(sp, nil)
	}
	ss := protowire.AppendBytes(protowire.AppendTag(nil, 2, protowire.BytesType), sp)
	rs := protowire.AppendBytes(protowire.AppendTag(nil, 2, protowire.BytesType), ss)
	return protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), rs)
}

func gz(raw []byte) []byte {
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	_, _ = zw.Write(raw)
	_ = zw.Close()
	return b.Bytes()
}

func newRouter(t *testing.T, cfg Config) *Router {
	t.Helper()
	if cfg.Store == nil {
		cfg.Store = &pStore{}
	}
	if cfg.Routes == nil {
		cfg.Routes = &pRoutes{}
	}
	if cfg.Health == nil {
		cfg.Health = pHealth{}
	}
	if cfg.Sender == nil {
		cfg.Sender = &pSender{spans: map[string]int{}}
	}
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.Start(context.Background())
	t.Cleanup(r.Close)
	return r
}

// hangBound tells a hang from a slow machine: a test waits on state, never on a time, and this
// deadline only ends a wait that would otherwise never end. A loaded 3-CPU external gate has taken
// more than 2 s for a step that takes milliseconds alone (old issue 533, old issue 612).
const hangBound = time.Minute

// manualClock is a router clock that moves only when a spec advances it, so an interval the spec is
// about (ConfigCheck, a read that hangs past it) does not depend on how fast the machine runs.
type manualClock struct {
	mu  sync.Mutex
	now time.Time
}

func newManualClock() *manualClock { return &manualClock{now: time.Unix(1_000_000, 0)} }

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(hangBound)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A few KB of gzip must not make the router allocate for millions of empty sub-messages.
func TestAmplifiedRequestIsRefusedBeforeDecoding(t *testing.T) {
	r := newRouter(t, Config{})
	nested := func() []byte {
		// A kvlist inside a kvlist ..., 40 deep, as one attribute value.
		v := []byte(nil)
		for range 40 {
			kv := protowire.AppendBytes(protowire.AppendTag(nil, 2, protowire.BytesType), v)                                                                         // KeyValue.value = AnyValue
			v = protowire.AppendBytes(protowire.AppendTag(nil, 6, protowire.BytesType), protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), kv)) // AnyValue.kvlist_value
		}
		sp := protowire.AppendBytes(protowire.AppendTag(nil, 9, protowire.BytesType), protowire.AppendBytes(protowire.AppendTag(nil, 2, protowire.BytesType), v))
		ss := protowire.AppendBytes(protowire.AppendTag(nil, 2, protowire.BytesType), sp)
		rs := protowire.AppendBytes(protowire.AppendTag(nil, 2, protowire.BytesType), ss)
		return protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), rs)
	}
	cases := map[string][]byte{
		"empty attributes":     buildAmplified(7_500_000, false),
		"empty resource spans": buildAmplified(7_500_000, true),
		"deeply nested values": nested(),
	}
	for name, raw := range cases {
		body := gz(raw)
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, TracesPath, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/x-protobuf")
		req.Header.Set("Content-Encoding", "gzip")
		w := httptest.NewRecorder()
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		r.Handler().ServeHTTP(w, req)
		runtime.ReadMemStats(&after)
		if w.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("%s: status %d, want 413 (wire %d B, decompressed %d B)", name, w.Code, len(body), len(raw))
		}
		// The decompressed body (up to 16 MiB) is read once, with the buffer growing as it is read; nothing
		// is decoded from it. Decoding it would have cost about 1 GiB.
		if grown := (after.TotalAlloc - before.TotalAlloc) >> 20; grown > 100 {
			t.Errorf("%s: the request allocated %d MiB", name, grown)
		}
	}
}

func TestScanCountsSpansAndAcceptsAgentdBatches(t *testing.T) {
	req := &coltracepb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{}}}}}
	for i := range 256 {
		sp := span(pOrg, [16]byte{byte(i + 1)}, false, 100)
		sp.Events = []*tracepb.Span_Event{{Name: "e"}}
		req.ResourceSpans[0].ScopeSpans[0].Spans = append(req.ResourceSpans[0].ScopeSpans[0].Spans, sp)
	}
	raw, _ := proto.Marshal(req)
	n, err := scanRequest(raw, 1024)
	if err != nil || n != 256 {
		t.Fatalf("scan = %d, %v; want 256, nil", n, err)
	}
	if _, err := scanRequest(raw, 100); !errors.Is(err, errTooManySpans) {
		t.Fatalf("over the span cap: %v", err)
	}
	if _, err := scanRequest([]byte{0x0a, 0xff}, 10); err == nil || errors.Is(err, errBounds) {
		t.Fatalf("malformed input must be a parse error, got %v", err)
	}
}

// Once the fingerprint says the credentials changed, a failed reload must not serve the old set.
type pSecrets struct {
	ver     string
	verErr  error
	fail    error
	set     *scrub.Secrets
	reloads int
}

func (s *pSecrets) Version(context.Context, pgtype.UUID) (string, error) { return s.ver, s.verErr }
func (s *pSecrets) Secrets(context.Context, pgtype.UUID) (*scrub.Secrets, error) {
	s.reloads++
	if s.fail != nil {
		return nil, s.fail
	}
	return s.set, nil
}

func TestSecretsAreNotServedStaleAfterAKnownChange(t *testing.T) {
	now := time.Unix(1000, 0)
	src := &pSecrets{ver: "v1", set: scrub.NewSecrets("koinon_test_not-a-real-old-secret")}
	r, _ := New(Config{Store: &pStore{}, Routes: &pRoutes{}, Health: pHealth{}, Sender: &pSender{}, Secrets: src, Now: func() time.Time { return now }})
	ctx := context.Background()
	if _, err := r.secretsFor(ctx, pOrg); err != nil {
		t.Fatal(err)
	}

	// The version cannot be read: nothing says the credentials changed, so the old set holds for the grace.
	src.verErr = errors.New("database down")
	now = now.Add(2 * time.Second)
	if _, err := r.secretsFor(ctx, pOrg); err != nil {
		t.Errorf("version unreadable, within the grace: %v", err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := r.secretsFor(ctx, pOrg); err == nil {
		t.Error("version unreadable past the grace: the old set must not be served")
	}

	// The version changed and the new envelope cannot be opened: no grace at all.
	now = now.Add(time.Minute)
	src.verErr, src.ver, src.fail = nil, "v2", ErrUnopenable
	if _, err := r.secretsFor(ctx, pOrg); !errors.Is(err, ErrUnopenable) {
		t.Errorf("changed and unopenable: err = %v, want ErrUnopenable", err)
	}
	// ... and a reload that fails for another reason after a change is not graced either.
	src.fail = errors.New("database down")
	now = now.Add(2 * time.Second)
	if set, err := r.secretsFor(ctx, pOrg); err == nil {
		t.Errorf("changed and the reload failed: the old set was served (%v)", set != nil)
	}
}

// Two roots of one trace arriving together make the model calls once. The spec waits on the router's
// state, not on time (old issue 612): the second root arrives while the first is inside its ledger read (or
// held), the gate opens only once the second has been dropped as a duplicate, and the sends are
// counted when they have happened.
func TestDuplicateRootsArrivingTogetherEmitOnce(t *testing.T) {
	for name, cfg := range map[string]Config{
		"over the byte bound": {MaxHeldBytes: 1},
		"over the count":      {MaxHeld: 1},
		"held":                {},
	} {
		t.Run(name, func(t *testing.T) {
			st := &pStore{gate: make(chan struct{}), calls: []ModelCall{{RequestID: "req-1", Model: "m", Time: time.Unix(1, 0)}}}
			snd := &pSender{spans: map[string]int{}}
			cfg.Store, cfg.Sender, cfg.Linger = st, snd, time.Millisecond
			cfg.Routes = &pRoutes{routes: map[pgtype.UUID][]tracedest.Route{pOrg: {route("d1", "http://d1")}}}
			cfg.PollInterval = time.Hour
			r := newRouter(t, cfg)
			if name == "over the count" {
				// Fill the hold with another trace so both duplicates take the release path.
				other := [16]byte{9}
				r.hold(context.Background(), &heldRoot{span: span(pOrg, other, true, 0), org: pOrg, run: pRun, trace: other, since: time.Now()})
			}
			duplicates := func() float64 { return testutilValue(r.metrics.Discarded.WithLabelValues(discardDuplicate)) }
			trace := [16]byte{1}
			var wg sync.WaitGroup
			for range 2 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					r.hold(context.Background(), &heldRoot{span: span(pOrg, trace, true, 0), org: pOrg, run: pRun, trace: trace, since: time.Now()})
				}()
			}
			// One root claimed the trace: it is held, or released and blocked in its ledger read. The
			// other was dropped while it was so.
			eventually(t, "the second root was dropped as a duplicate", func() bool { return duplicates() == 1 })
			if name == "held" {
				wg.Wait()
				if n := r.HeldRoots(); n != 1 {
					t.Fatalf("held roots = %d, want the one", n)
				}
			} else {
				eventually(t, "the first root is in its ledger read", func() bool { return st.enter.Load() == 1 })
			}
			close(st.gate)
			wg.Wait()
			if name == "held" {
				r.Sweep(context.Background(), time.Now())
				r.Sweep(context.Background(), time.Now().Add(time.Minute))
			}
			sent := func() int {
				snd.mu.Lock()
				defer snd.mu.Unlock()
				return snd.spans["http://d1"]
			}
			// The filler of "over the count" stays held (no sweep runs), so in every case the trace's
			// chat span and its root are all that is sent.
			eventually(t, "the chat span and the root are sent", func() bool { return sent() >= 2 })
			if n := sent(); n != 2 {
				t.Errorf("spans sent = %d; want one chat span and one root", n)
			}
			if d := duplicates(); d != 1 {
				t.Errorf("duplicates = %v, want 1: the second root never reaches the ledger", d)
			}
			if name != "held" {
				if n := st.enter.Load(); n != 1 {
					t.Errorf("ledger reads = %d, want 1", n)
				}
			}
		})
	}
}

// A batch being retried is still in memory: it stays in the byte budget, and a batch is bounded by bytes.
func TestInflightBatchStaysInTheBudgetAndBatchesAreBoundedByBytes(t *testing.T) {
	snd := &pSender{spans: map[string]int{}, fail: map[string]bool{"http://d1": true}}
	rs := &pRoutes{routes: map[pgtype.UUID][]tracedest.Route{pOrg: {route("d1", "http://d1")}}}
	r := newRouter(t, Config{Routes: rs, Sender: snd, Linger: time.Millisecond, QueueBytes: 1 << 20, BatchBytes: 64 << 10, BackoffBase: time.Hour})
	var items []*tracepb.Span
	for i := range 100 {
		items = append(items, span(pOrg, [16]byte{byte(i + 1)}, false, 8<<10))
	}
	r.dispatch(context.Background(), pOrg, items...)
	eventually(t, "the first attempt", func() bool { return snd.calls.Load() >= 1 })
	size := int64(proto.Size(items[0]))
	// The queue keeps its share (a quarter of the budget): the batch in flight and what waits are counted.
	if got := r.queueBytes.Load(); got < 200<<10 || got > 256<<10+16*size {
		t.Errorf("queueBytes = %d; want about the queue's 256 KiB share, counting the batch in flight", got)
	}
	r.dispatch(context.Background(), pOrg, items...)
	if got := r.queueBytes.Load(); got > 1<<20 {
		t.Errorf("queueBytes = %d, over the 1 MiB budget", got)
	}
}

// One destination that is down must not take the memory of the others: each queue drops only its own.
func TestFailingDestinationDoesNotStarveAnother(t *testing.T) {
	for name, batch := range map[string]int{"small batches": 1, "one big in-flight batch": 512} {
		t.Run(name, func(t *testing.T) {
			snd := &pSender{spans: map[string]int{}, fail: map[string]bool{"http://bad": true}}
			rs := &pRoutes{routes: map[pgtype.UUID][]tracedest.Route{
				pOrg:  {route("bad", "http://bad")},
				pOrg2: {route("good", "http://good")},
			}}
			r := newRouter(t, Config{Routes: rs, Sender: snd, Linger: time.Millisecond, QueueBytes: 1 << 20, BatchSize: batch, BackoffBase: time.Hour})
			var bad []*tracepb.Span
			for i := range 200 {
				bad = append(bad, span(pOrg, [16]byte{byte(i + 1)}, false, 8<<10))
			}
			r.dispatch(context.Background(), pOrg, bad...)
			eventually(t, "the failing destination has tried and is backing off", func() bool { return snd.calls.Load() >= 1 })
			// The healthy org is the larger: 100 spans over time, exported as they come.
			for chunk := range 10 {
				var good []*tracepb.Span
				for i := range 10 {
					good = append(good, span(pOrg2, [16]byte{0xe0, byte(chunk), byte(i + 1)}, false, 8<<10))
				}
				r.dispatch(context.Background(), pOrg2, good...)
				want := (chunk + 1) * 10
				eventually(t, "the healthy destination exports its chunk", func() bool {
					snd.mu.Lock()
					defer snd.mu.Unlock()
					return snd.spans["http://good"] == want
				})
			}
		})
	}
}

func testutilValue(c prometheus.Counter) float64 {
	var m dto.Metric
	_ = c.Write(&m)
	return m.GetCounter().GetValue()
}

// A backend's rejected_spans is its own claim: a huge one must not hang the router.
func TestHugeRejectedCountIsClamped(t *testing.T) {
	snd := &pSender{spans: map[string]int{}, rejected: 1 << 62}
	rs := &pRoutes{routes: map[pgtype.UUID][]tracedest.Route{
		pOrg:  {route("d1", "http://d1")},
		pOrg2: {route("d2", "http://d2")},
	}}
	r := newRouter(t, Config{Routes: rs, Sender: snd, Linger: time.Millisecond})
	var spans []*tracepb.Span
	for i := range 3 {
		spans = append(spans, span(pOrg, [16]byte{byte(i + 1)}, false, 0))
	}
	r.dispatch(context.Background(), pOrg, spans...)
	eventually(t, "the batch is done", func() bool { return snd.calls.Load() >= 1 && r.queueBytes.Load() == 0 })
	if got := testutilValue(r.metrics.Rejected.WithLabelValues(KindOrg, "generic", rejectPartial)); got != 3 {
		t.Errorf("rejected = %v, want 3 (clamped to the batch)", got)
	}
	if got := testutilValue(r.metrics.Exported.WithLabelValues(KindOrg, "generic")); got != 0 {
		t.Errorf("exported = %v, want 0", got)
	}
}

// A destination whose credentials cannot be opened is not a deleted one.
func TestUnreadableDestinationKeepsItsQueue(t *testing.T) {
	snd := &pSender{spans: map[string]int{}, fail: map[string]bool{"http://d1": true}}
	rs := &pRoutes{routes: map[pgtype.UUID][]tracedest.Route{pOrg: {route("d1", "http://d1")}}}
	clk := newManualClock()
	r := newRouter(t, Config{Routes: rs, Sender: snd, Linger: time.Millisecond, BackoffBase: time.Hour, ConfigCheck: time.Millisecond, Now: clk.Now})
	r.dispatch(context.Background(), pOrg, span(pOrg, [16]byte{1}, false, 0))
	eventually(t, "the first attempt fails", func() bool { return snd.calls.Load() >= 1 })

	un := route("d1", "http://d1")
	un.Unreadable = true
	rs.set(pOrg, un)
	clk.Advance(time.Millisecond) // past ConfigCheck: the next dispatch reads the settings again
	r.dispatch(context.Background(), pOrg, span(pOrg, [16]byte{2}, false, 0))
	if r.QueueCount() != 1 {
		t.Errorf("queues = %d; an unreadable destination keeps its queue", r.QueueCount())
	}
	if got := testutilValue(r.metrics.Dropped.WithLabelValues(KindOrg, "generic", dropUnreadable)); got < 1 {
		t.Errorf("spans lost to unreadable credentials = %v, want at least the new one", got)
	}
	if got := testutilValue(r.metrics.Dropped.WithLabelValues(KindOrg, "generic", dropRemoved)); got != 0 {
		t.Errorf("removed = %v; the destination was not deleted", got)
	}
}

// waiting is how many callers have joined the call running for key (0 when none runs).
func (f *flight) waiting(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.calls[key]; ok {
		return c.waiters
	}
	return 0
}

// The leader panics while a second caller waits on its call: both get an error, neither hangs. The
// spec waits on the flight's own state, not on time (old issue 533): the leader panics only once the second
// caller has joined, and the deadline only tells a hang from a slow machine.
func TestFlightReleasesWaitersWhenTheLeaderPanics(t *testing.T) {
	var f flight
	started := make(chan struct{})
	release := make(chan struct{})
	errs := make(chan error, 2)
	go func() {
		_, err := f.do("k", func() (any, error) {
			close(started)
			<-release
			panic("boom")
		})
		errs <- err
	}()
	<-started
	go func() {
		_, err := f.do("k", func() (any, error) { return nil, errors.New("the second caller ran its own read instead of joining") })
		errs <- err
	}()
	// One deadline for the whole spec, far beyond any slow machine: the lab once took more than 2 s.
	deadline := time.After(time.Minute)
	for f.waiting("k") != 1 {
		select {
		case <-deadline:
			t.Fatal("the second caller never joined the leader's call")
		case <-time.After(time.Millisecond):
		}
	}
	close(release)
	for range 2 {
		select {
		case err := <-errs:
			if err == nil || !strings.Contains(err.Error(), "a read panicked") {
				t.Errorf("err = %v; a panicking read must give each of its callers the panic's error", err)
			}
		case <-deadline:
			t.Fatal("a caller was never released")
		}
	}
}

func TestRefusalsSayWhyAndAreCounted(t *testing.T) {
	r := newRouter(t, Config{MaxRequestSpans: 2, RequestBytes: 8 << 10})
	post := func(spans ...*tracepb.Span) (int, string) {
		raw, _ := proto.Marshal(&coltracepb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: spans}}}}})
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, TracesPath, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/x-protobuf")
		w := httptest.NewRecorder()
		r.Handler().ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	one := func() *tracepb.Span { return span(pOrg, [16]byte{1}, false, 0) }
	if code, body := post(one(), one(), one()); code != http.StatusRequestEntityTooLarge || !strings.Contains(body, "more than 2 spans") {
		t.Errorf("too many spans: %d %q", code, body)
	}
	if got := testutilValue(r.metrics.RequestsRejected.WithLabelValues(reqTooManySpans)); got != 1 {
		t.Errorf("too_many_spans = %v", got)
	}
	big := span(pOrg, [16]byte{1}, false, 6<<10)
	if code, body := post(big, big); code != http.StatusRequestEntityTooLarge || !strings.Contains(body, "memory budget") {
		t.Errorf("a request that can never fit: %d %q", code, body)
	}
	if got := testutilValue(r.metrics.RequestsRejected.WithLabelValues(reqMemory)); got != 1 {
		t.Errorf("memory = %v", got)
	}
}

func TestBudgetsDeriveFromTheMemoryLimit(t *testing.T) {
	r, _ := New(Config{Store: &pStore{}, Routes: &pRoutes{}, Health: pHealth{}, Sender: &pSender{}, MemoryLimit: 512 << 20})
	lim := int64(512 << 20)
	for name, got := range map[string]struct{ have, want int64 }{
		"queues":   {int64(r.cfg.QueueBytes), lim / 100 * 20},
		"intake":   {int64(r.cfg.IntakeBytes), lim / 100 * 5},
		"held":     {int64(r.cfg.MaxHeldBytes), lim / 100 * 3},
		"requests": {int64(r.cfg.RequestBytes), lim / 100 * 8},
	} {
		if got.have != got.want {
			t.Errorf("%s budget = %d, want %d", name, got.have, got.want)
		}
	}
	explicit, _ := New(Config{Store: &pStore{}, Routes: &pRoutes{}, Health: pHealth{}, Sender: &pSender{}, MemoryLimit: 512 << 20, QueueBytes: 1 << 20})
	if explicit.cfg.QueueBytes != 1<<20 {
		t.Error("an explicit budget must win")
	}
}

func TestEstimateChargesTheObjectsAroundTheBytes(t *testing.T) {
	plain := span(pOrg, [16]byte{1}, false, 0)
	many := span(pOrg, [16]byte{1}, false, 0)
	for range 100 {
		many.Attributes = append(many.Attributes, kvStr("k", "v"))
	}
	if estimateSize(many)-estimateSize(plain) < 100*100 {
		t.Errorf("100 attributes cost %d bytes more; each should cost at least 100", estimateSize(many)-estimateSize(plain))
	}
}

// A database that does not answer must not stop the receiver, and is asked once, not once per span.
// The database hangs until the spec releases it, far longer than any post may take, so a receiver that
// waited for it could not answer; the reads are counted, not timed (old issue 612). The failure is
// remembered from when the read failed, not from when it started: a read that hung past ConfigCheck
// used to be remembered until a time already past, so the next span asked again (#134).
func TestHangingDatabaseDoesNotStopTheReceiver(t *testing.T) {
	st := &pStore{hang: true, release: make(chan struct{})}
	// The failure is remembered for ConfigCheck on the router's clock, which moves only when the spec
	// advances it, so a slow machine cannot outlast it.
	clk := newManualClock()
	const check = time.Second
	r := newRouter(t, Config{Store: st, ConfigTimeout: 2 * hangBound, ConfigCheck: check, Workers: 2, IngestQueue: 8, NegativeTTL: time.Hour, Now: clk.Now})
	var once sync.Once
	release := func() { once.Do(func() { close(st.release) }) }
	t.Cleanup(release) // before r.Close, which waits for the workers
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	client := &http.Client{Timeout: hangBound} // a receiver that hangs fails the post, not the package
	post := func() (time.Duration, int) {
		sp := span(pOrg, [16]byte{1}, false, 0)
		raw, _ := proto.Marshal(&coltracepb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{sp}}}}}})
		start := time.Now()
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/x-protobuf")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return time.Since(start), resp.StatusCode
	}
	const posts = 50
	for range posts {
		if d, code := post(); d > hangBound || code != 200 {
			t.Fatalf("the receiver took %v and answered %d while the database hung", d, code)
		}
	}
	eventually(t, "a worker asked the database", func() bool { return st.attrs.Load() >= 1 })
	if n := st.attrs.Load(); n != 1 {
		t.Errorf("the database was asked %d times while it hung; the workers share one read", n)
	}
	// The second worker joins the read in flight before it fails, so the count below sees the shared
	// read rather than a worker that had not got there yet.
	trace := [16]byte{1}
	fkey := "attr/" + pOrg.String() + "/" + pRun.String() + "/" + string(trace[:]) // as Router.attribute keys it
	eventually(t, "the second worker joined the read", func() bool { return r.fl.waiting(fkey) >= 1 })
	// The read hangs past ConfigCheck, as one that runs into the default 3 s ConfigTimeout does, then
	// fails; the spans still in the intake are dropped on the failure, remembered from when it failed.
	clk.Advance(3 * check)
	release()
	discarded := func(reason string) float64 { return testutilValue(r.metrics.Discarded.WithLabelValues(reason)) }
	eventually(t, "every span was refused at the intake or dropped", func() bool {
		return discarded(discardLookup)+discarded(discardIngestFull) == posts
	})
	if n := st.attrs.Load(); n != 1 {
		t.Errorf("the database was asked %d times about one trace; a failure is remembered", n)
	}
}

type hangRoutes struct {
	pRoutes
	versions atomic.Int32
}

func (h *hangRoutes) ConfigVersion(ctx context.Context, _ pgtype.UUID) (string, error) {
	h.versions.Add(1)
	<-ctx.Done()
	return "", ctx.Err()
}

func TestFailedSettingsReadIsRememberedAndAskedOnce(t *testing.T) {
	h := &hangRoutes{}
	r, _ := New(Config{Store: &pStore{}, Routes: h, Health: pHealth{}, Sender: &pSender{}, ConfigTimeout: 50 * time.Millisecond, ConfigCheck: time.Hour})
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.routesFor(context.Background(), pOrg); err == nil {
				t.Error("a settings read that failed must fail closed")
			}
		}()
	}
	wg.Wait()
	if _, err := r.routesFor(context.Background(), pOrg); err == nil {
		t.Error("the remembered failure must still fail closed")
	}
	if n := h.versions.Load(); n != 1 {
		t.Errorf("the database was asked %d times; once for all callers", n)
	}
}

func TestRetiredQueueIsRemovedAtOnce(t *testing.T) {
	rs := &pRoutes{routes: map[pgtype.UUID][]tracedest.Route{pOrg: {route("d1", "http://d1")}}}
	r := newRouter(t, Config{Routes: rs, Sender: &pSender{spans: map[string]int{}}})
	r.dispatch(context.Background(), pOrg, span(pOrg, [16]byte{1}, false, 0))
	if r.QueueCount() != 1 {
		t.Fatalf("queues = %d", r.QueueCount())
	}
	r.syncQueues(pOrg, nil)
	if r.QueueCount() != 0 {
		t.Errorf("a retired queue stays registered: %d", r.QueueCount())
	}
}

func TestRejectedSpansAreChargedToTheirOrgs(t *testing.T) {
	q := &queue{droppedByOrg: map[pgtype.UUID]int64{}}
	var batch []item
	for range 6 {
		batch = append(batch, item{org: pOrg})
	}
	for range 3 {
		batch = append(batch, item{org: pOrg2})
	}
	q.chargeRejected(batch, 4)
	if q.droppedByOrg[pOrg]+q.droppedByOrg[pOrg2] != 4 || q.droppedByOrg[pOrg] != 3 || q.droppedByOrg[pOrg2] != 1 {
		t.Errorf("charges = %v; want 3 and 1 by share (6 and 3 spans)", q.droppedByOrg)
	}
}

func TestDropCountsSurviveALostBatch(t *testing.T) {
	snd := &pSender{spans: map[string]int{}, fail: map[string]bool{"http://d1": true}}
	rs := &pRoutes{routes: map[pgtype.UUID][]tracedest.Route{pOrg: {route("d1", "http://d1")}}}
	r := newRouter(t, Config{Routes: rs, Sender: snd, Linger: time.Millisecond, MaxAttempts: 1, BackoffBase: time.Millisecond, BreakerBase: time.Millisecond})
	r.dispatch(context.Background(), pOrg, span(pOrg, [16]byte{1}, false, 0)) // creates the queue
	eventually(t, "the first batch is lost", func() bool { return snd.calls.Load() >= 1 && r.queueBytes.Load() == 0 })
	r.qmu.Lock()
	var q *queue
	for _, v := range r.queues {
		q = v
	}
	r.qmu.Unlock()
	q.dmu.Lock()
	q.droppedByOrg[pOrg] = 7 // spans lost earlier, to be reported by the next root
	q.dmu.Unlock()
	trace := [16]byte{2}
	r.dispatch(context.Background(), pOrg, span(pOrg, trace, true, 0))
	eventually(t, "the batch with the root was tried and lost", func() bool { return snd.calls.Load() >= 2 })
	// The batch's bytes are released before its roots' counts are put back (send's defers), so the
	// spec waits for the count itself: a router that lost it never gets there.
	eventually(t, "the count to report survives the lost batch", func() bool {
		q.dmu.Lock()
		defer q.dmu.Unlock()
		return q.droppedByOrg[pOrg] >= 7
	})
}

func TestPollIntervalIsNeverShorterThanTheTurnGrace(t *testing.T) {
	r, _ := New(Config{Store: &pStore{}, Routes: &pRoutes{}, Health: pHealth{}, Sender: &pSender{}, PollInterval: 100 * time.Millisecond, TurnGrace: 3 * time.Second})
	if r.cfg.PollInterval < r.cfg.TurnGrace {
		t.Errorf("poll interval %v < turn grace %v", r.cfg.PollInterval, r.cfg.TurnGrace)
	}
}

type cHealth struct{ ok, fail atomic.Int32 }

func (h *cHealth) Success(context.Context, pgtype.UUID, tracedest.Route) error {
	h.ok.Add(1)
	return nil
}

func (h *cHealth) Failure(context.Context, pgtype.UUID, tracedest.Route, string, int64) error {
	h.fail.Add(1)
	return nil
}

type flakyRoutes struct {
	pRoutes
	broken atomic.Bool
	// failed counts the reads refused while broken.
	failed atomic.Int32
}

func (f *flakyRoutes) ConfigVersion(ctx context.Context, org pgtype.UUID) (string, error) {
	if f.broken.Load() {
		f.failed.Add(1)
		return "", errors.New("database down")
	}
	return f.pRoutes.ConfigVersion(ctx, org)
}

// timerSleep is the router's own sleep, for a test's Sleep that adds a step before it.
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

// The database being down is not the destination's failure.
func TestSettingsReadFailureIsNotTheDestinationsFailure(t *testing.T) {
	rs := &flakyRoutes{pRoutes: pRoutes{routes: map[pgtype.UUID][]tracedest.Route{pOrg: {route("d1", "http://d1")}}}}
	snd := &pSender{spans: map[string]int{}}
	h := &cHealth{}
	// The worker's first sleep is the linger before it makes the copy: it waits until the settings
	// are broken, then lingers (longer than ConfigCheck, so the copy reads them again) (old issue 612).
	broken := make(chan struct{})
	var lingered atomic.Bool
	r := newRouter(t, Config{
		Routes: rs, Sender: snd, Health: h, Linger: 10 * time.Millisecond, ConfigCheck: time.Millisecond,
		BackoffBase: 5 * time.Millisecond, MaxAttempts: 2, BreakerThreshold: 1, BreakerBase: time.Hour,
		Sleep: func(ctx context.Context, d time.Duration) error {
			if lingered.CompareAndSwap(false, true) {
				select {
				case <-broken:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return timerSleep(ctx, d)
		},
	})
	r.dispatch(context.Background(), pOrg, span(pOrg, [16]byte{1}, false, 0))
	rs.broken.Store(true) // during the linger, before the worker makes the copy
	close(broken)
	eventually(t, "the worker tried several times to read the settings", func() bool { return rs.failed.Load() >= 3 })
	if snd.calls.Load() != 0 {
		t.Fatal("nothing can be sent while the settings cannot be read")
	}
	if h.fail.Load() != 0 {
		t.Errorf("the destination's health recorded %d failures for the database's", h.fail.Load())
	}
	rs.broken.Store(false)
	eventually(t, "the kept batch is sent once the settings can be read", func() bool {
		snd.mu.Lock()
		defer snd.mu.Unlock()
		return snd.spans["http://d1"] == 1
	})
}

// A failed batch is retried as it was; attempts start again when its head is no longer first.
func TestRetryTakesTheFailedBatchAndResetsWhenItsHeadChanged(t *testing.T) {
	r, _ := New(Config{Store: &pStore{}, Routes: &pRoutes{}, Health: pHealth{}, Sender: &pSender{}, BatchSize: 5})
	mk := func(n int) []item {
		var items []item
		for i := range n {
			items = append(items, item{sp: span(pOrg, [16]byte{byte(i + 1)}, false, 0), org: pOrg, size: 10})
		}
		return items
	}
	newQ := func(items []item) *queue {
		q := &queue{r: r, kind: KindOrg, backend: "generic", items: items, droppedByOrg: map[pgtype.UUID]int64{}}
		for _, it := range items {
			q.queued += it.size
			r.queueBytes.Add(int64(it.size))
		}
		return q
	}

	items := mk(8)
	q := newQ(items)
	q.attempts, q.retryN, q.retrySp = 2, 3, items[0].sp
	if got := q.takeBatch(); len(got) != 3 || q.attempts != 2 {
		t.Errorf("retry took %d items with attempts %d; want exactly the 3 that failed, attempts kept", len(got), q.attempts)
	}

	items = mk(8)
	q = newQ(items[2:]) // the head was trimmed away
	q.attempts, q.retryN, q.retrySp = 2, 3, items[0].sp
	if got := q.takeBatch(); len(got) != 5 || q.attempts != 0 {
		t.Errorf("after a trim: took %d items with attempts %d; want a normal batch of 5 and attempts reset", len(got), q.attempts)
	}
}
