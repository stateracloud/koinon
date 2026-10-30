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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
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
	// hang makes Attribute wait for its context, as a database that does not answer.
	hang  bool
	attrs atomic.Int32
}

func (s *pStore) Attribute(ctx context.Context, _, _ pgtype.UUID, _ [16]byte) (Turn, error) {
	s.attrs.Add(1)
	if s.hang {
		<-ctx.Done()
		return Turn{}, ctx.Err()
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
}

func (p *pRoutes) Routes(_ context.Context, org pgtype.UUID) ([]tracedest.Route, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.routes[org], nil
}
func (p *pRoutes) ConfigVersion(context.Context, pgtype.UUID) (string, error) { return "v", nil }

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
	return export.Result{}, nil
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

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
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
	if _, err := scanRequest(raw, 100); !errors.Is(err, errBounds) {
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

// Two roots of one trace arriving together make the model calls once.
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
			trace := [16]byte{1}
			var wg sync.WaitGroup
			for range 2 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					r.hold(context.Background(), &heldRoot{span: span(pOrg, trace, true, 0), org: pOrg, run: pRun, trace: trace, since: time.Now()})
				}()
			}
			time.Sleep(50 * time.Millisecond)
			close(st.gate)
			wg.Wait()
			if name == "held" {
				r.Sweep(context.Background(), time.Now())
				r.Sweep(context.Background(), time.Now().Add(time.Minute))
			}
			time.Sleep(300 * time.Millisecond)
			snd.mu.Lock()
			n := snd.spans["http://d1"]
			snd.mu.Unlock()
			if n < 2 || n > 2+boolToInt(name == "over the count") {
				t.Errorf("spans sent = %d; want one chat span and one root (plus the filler's root)", n)
			}
		})
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
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
	if got := r.queueBytes.Load(); got < 100*size-8*size { // the batch is in flight, not released
		t.Errorf("queueBytes = %d with a batch in flight; the whole 100 spans (~%d) should still be counted", got, 100*size)
	}
	r.dispatch(context.Background(), pOrg, items...)
	if got := r.queueBytes.Load(); got > 1<<20 {
		t.Errorf("queueBytes = %d, over the 1 MiB budget", got)
	}
}

// One destination that is down must not take the memory of the others.
func TestFailingDestinationDoesNotStarveAnother(t *testing.T) {
	snd := &pSender{spans: map[string]int{}, fail: map[string]bool{"http://bad": true}}
	rs := &pRoutes{routes: map[pgtype.UUID][]tracedest.Route{
		pOrg:  {route("bad", "http://bad")},
		pOrg2: {route("good", "http://good")},
	}}
	r := newRouter(t, Config{Routes: rs, Sender: snd, Linger: time.Millisecond, QueueBytes: 1 << 20, BatchSize: 1, BackoffBase: time.Hour})
	var bad []*tracepb.Span
	for i := range 200 {
		bad = append(bad, span(pOrg, [16]byte{byte(i + 1)}, false, 8<<10))
	}
	r.dispatch(context.Background(), pOrg, bad...)
	time.Sleep(20 * time.Millisecond)
	var good []*tracepb.Span
	for i := range 5 {
		good = append(good, span(pOrg2, [16]byte{0xe0 + byte(i)}, false, 8<<10))
	}
	r.dispatch(context.Background(), pOrg2, good...)
	eventually(t, "the healthy destination exports all five", func() bool {
		snd.mu.Lock()
		defer snd.mu.Unlock()
		return snd.spans["http://good"] == 5
	})
}

// A database that does not answer must not stop the receiver, and is asked once, not once per span.
func TestHangingDatabaseDoesNotStopTheReceiver(t *testing.T) {
	st := &pStore{hang: true}
	r := newRouter(t, Config{Store: st, ConfigTimeout: 100 * time.Millisecond, Workers: 2, IngestQueue: 8})
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	post := func() (time.Duration, int) {
		sp := span(pOrg, [16]byte{1}, false, 0)
		raw, _ := proto.Marshal(&coltracepb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{sp}}}}}})
		start := time.Now()
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/x-protobuf")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return time.Since(start), resp.StatusCode
	}
	for range 50 {
		if d, code := post(); d > 50*time.Millisecond || code != 200 {
			t.Fatalf("the receiver took %v and answered %d while the database hung", d, code)
		}
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
	time.Sleep(50 * time.Millisecond)
	q.dmu.Lock()
	got := q.droppedByOrg[pOrg]
	q.dmu.Unlock()
	if got < 7 {
		t.Errorf("the count to report fell to %d when the batch with its root was lost", got)
	}
}

func TestPollIntervalIsNeverShorterThanTheTurnGrace(t *testing.T) {
	r, _ := New(Config{Store: &pStore{}, Routes: &pRoutes{}, Health: pHealth{}, Sender: &pSender{}, PollInterval: 100 * time.Millisecond, TurnGrace: 3 * time.Second})
	if r.cfg.PollInterval < r.cfg.TurnGrace {
		t.Errorf("poll interval %v < turn grace %v", r.cfg.PollInterval, r.cfg.TurnGrace)
	}
}
