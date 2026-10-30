// SPDX-License-Identifier: Apache-2.0

// Package router is Koinon's trace router (ADR 0074, issue #213). It lives in koinon-api and takes
// the spans koinon-agentd sends to the internal OTLP listener's /v1/traces, and for each org:
//
//   - attributes a span to a run it can prove (the run is in that org, the trace ID is one of the
//     run's turns), and drops spans of runs that are not traced;
//   - makes the model-call spans of a turn from the spend ledger and puts the turn's totals on its
//     root span;
//   - makes one copy of every span per destination, at that destination's content level: content
//     removed at none, scrubbed at redacted, credentials removed at full, names replaced by IDs on
//     the platform's copy;
//   - queues the copies per destination (bounded, drop-oldest, retry with backoff, circuit breaker)
//     and exports them as OTLP/HTTP protobuf.
//
// Nothing here waits for a destination: export never blocks a run, the ledger or the gateway.
package router

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/stateracloud/koinon/internal/tracedest"
	"github.com/stateracloud/koinon/internal/tracing/export"
	"github.com/stateracloud/koinon/internal/tracing/scrub"
	"github.com/stateracloud/koinon/internal/tracing/semconv"
)

// Why a span cannot be attributed (returned by Store.Attribute).
var (
	ErrUnknownRun   = errors.New("router: no such run in that org")
	ErrUnknownTrace = errors.New("router: the trace is not one of the run's turns")
	ErrUntraced     = errors.New("router: the run is not traced")
)

// Turn is what the router needs of a run_turns row.
type Turn struct {
	Seq       int32
	StartedAt time.Time
	// EndedAt is nil while the turn is open.
	EndedAt *time.Time
}

// Store reads what attribution and the model-call spans need from Postgres.
type Store interface {
	// Attribute checks that the run exists in org, is traced and that trace is one of its turns. It
	// returns ErrUnknownRun, ErrUnknownTrace or ErrUntraced.
	Attribute(ctx context.Context, org, run pgtype.UUID, trace [16]byte) (Turn, error)
	// ModelCalls returns the run's ledger entries with from <= time <= to, in time order.
	ModelCalls(ctx context.Context, org, run pgtype.UUID, from, to time.Time) ([]ModelCall, error)
}

// RouteSource gives the enabled destinations of an org with their credentials opened.
// *tracedest.Service is one.
type RouteSource interface {
	Routes(ctx context.Context, org pgtype.UUID) ([]tracedest.Route, error)
}

// SecretSource gives the set of an org's known secret values for the scrubber. The values are never
// logged.
type SecretSource interface {
	Secrets(ctx context.Context, org pgtype.UUID) (*scrub.Secrets, error)
}

// Health records a destination's state for its page. org is the org of an org destination and the
// org whose span last used the platform destination for the platform's (unused there).
type Health interface {
	Success(ctx context.Context, org pgtype.UUID, route tracedest.Route) error
	Failure(ctx context.Context, org pgtype.UUID, route tracedest.Route, message string, dropped int64) error
}

// Sender sends one OTLP request to one destination. *export.Client is one.
type Sender interface {
	Export(ctx context.Context, d export.Destination, req *coltracepb.ExportTraceServiceRequest) (export.Result, error)
}

// Config configures a Router. Zero values take the defaults named in the comments.
type Config struct {
	Store   Store
	Routes  RouteSource
	Secrets SecretSource
	Health  Health
	Sender  Sender
	Metrics *Metrics
	Log     *slog.Logger

	// Now and Sleep are replaced by tests.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error

	// IngestQueue bounds the spans waiting for attribution; a full intake rejects (default 8192).
	IngestQueue int
	// Workers attribute and route spans (default 4).
	Workers int
	// QueueSize bounds each destination's queue (default 10,000, ADR 0074 decision 7).
	QueueSize int
	// BatchSize is the most spans in one export (default 512).
	BatchSize int
	// Linger is how long a worker lets a small batch fill (default 500ms).
	Linger time.Duration
	// FlushInterval is how often an idle worker looks again (default 1s).
	FlushInterval time.Duration
	// ExportTimeout bounds one request (default 10s).
	ExportTimeout time.Duration
	// IdleTTL stops the worker of a queue that stayed empty (default 5m).
	IdleTTL time.Duration

	// LedgerWait is how long a turn's root waits for late ledger rows (default 30s, decision 5), and
	// PollInterval how often the ledger is read meanwhile (default 2s).
	LedgerWait   time.Duration
	PollInterval time.Duration
	// MaxHeld and MaxHeldBytes bound the roots waiting for the ledger (defaults 2048 and 32 MiB); a
	// root over the bound is released at once with the ledger as it stands.
	MaxHeld      int
	MaxHeldBytes int

	// ContentLimit is the most bytes one content attribute keeps (default 16 KiB).
	ContentLimit int

	// Cache lifetimes: attribution (default 10m; a refusal 30s), routes (15s), secrets (5m).
	AttributionTTL, NegativeTTL, RouteTTL, SecretsTTL time.Duration

	// HealthInterval is the most often one destination writes its health (default 5s).
	HealthInterval time.Duration

	// Retry: MaxAttempts per batch (default 6), exponential backoff from BackoffBase (1s) to
	// BackoffMax (60s). The breaker opens after BreakerThreshold consecutive failures (3) and pauses
	// BreakerBase (30s), doubling with every further failure, up to BreakerMax (10m).
	MaxAttempts      int
	BackoffBase      time.Duration
	BackoffMax       time.Duration
	BreakerThreshold int
	BreakerBase      time.Duration
	BreakerMax       time.Duration
}

func (c *Config) defaults() error {
	if c.Store == nil || c.Routes == nil || c.Health == nil || c.Sender == nil {
		return errors.New("router: Store, Routes, Health and Sender are required")
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
	if c.Metrics == nil {
		c.Metrics = NewMetrics(nil)
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Sleep == nil {
		c.Sleep = sleep
	}
	setInt := func(p *int, d int) {
		if *p <= 0 {
			*p = d
		}
	}
	setDur := func(p *time.Duration, d time.Duration) {
		if *p <= 0 {
			*p = d
		}
	}
	setInt(&c.IngestQueue, 8192)
	setInt(&c.Workers, 4)
	setInt(&c.QueueSize, 10_000)
	setInt(&c.BatchSize, 512)
	setDur(&c.Linger, 500*time.Millisecond)
	setDur(&c.FlushInterval, time.Second)
	setDur(&c.ExportTimeout, export.DefaultTimeout)
	setDur(&c.IdleTTL, 5*time.Minute)
	setDur(&c.LedgerWait, 30*time.Second)
	setDur(&c.PollInterval, 2*time.Second)
	setInt(&c.MaxHeld, 2048)
	setInt(&c.MaxHeldBytes, 32<<20)
	setInt(&c.ContentLimit, DefaultContentLimit)
	setDur(&c.AttributionTTL, 10*time.Minute)
	setDur(&c.NegativeTTL, 30*time.Second)
	setDur(&c.RouteTTL, 15*time.Second)
	setDur(&c.SecretsTTL, 5*time.Minute)
	setDur(&c.HealthInterval, 5*time.Second)
	setInt(&c.MaxAttempts, 6)
	setDur(&c.BackoffBase, time.Second)
	setDur(&c.BackoffMax, time.Minute)
	setInt(&c.BreakerThreshold, 3)
	setDur(&c.BreakerBase, 30*time.Second)
	setDur(&c.BreakerMax, 10*time.Minute)
	return nil
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Router is the trace router. Build it with New, Start it once and Close it at shutdown.
type Router struct {
	cfg     Config
	log     *slog.Logger
	metrics *Metrics
	store   Store
	routes  RouteSource
	secrets SecretSource
	health  Health
	sender  Sender
	now     func() time.Time
	sleep   func(context.Context, time.Duration) error

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	intake chan *tracepb.Span

	cmu   sync.Mutex
	attrs map[attrKey]attrEntry
	rts   map[pgtype.UUID]routeEntry
	scr   map[pgtype.UUID]secretEntry

	qmu    sync.Mutex
	queues map[string]*queue

	hmu       sync.Mutex
	held      map[[16]byte]*heldRoot
	heldBytes int
}

// New returns a Router. It does nothing until Start.
func New(cfg Config) (*Router, error) {
	if err := cfg.defaults(); err != nil {
		return nil, err
	}
	return &Router{
		cfg: cfg, log: cfg.Log, metrics: cfg.Metrics,
		store: cfg.Store, routes: cfg.Routes, secrets: cfg.Secrets, health: cfg.Health, sender: cfg.Sender,
		now: cfg.Now, sleep: cfg.Sleep,
		intake: make(chan *tracepb.Span, cfg.IngestQueue),
		attrs:  map[attrKey]attrEntry{}, rts: map[pgtype.UUID]routeEntry{}, scr: map[pgtype.UUID]secretEntry{},
		queues: map[string]*queue{}, held: map[[16]byte]*heldRoot{},
	}, nil
}

// HeldRoots is the number of turn roots waiting for the ledger.
func (r *Router) HeldRoots() int {
	r.hmu.Lock()
	defer r.hmu.Unlock()
	return len(r.held)
}

// Metrics returns the router's metrics.
func (r *Router) Metrics() *Metrics { return r.metrics }

// Start runs the workers and the ledger poller until ctx ends or Close is called.
func (r *Router) Start(ctx context.Context) {
	r.ctx, r.cancel = context.WithCancel(ctx)
	for range r.cfg.Workers {
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			for {
				select {
				case sp := <-r.intake:
					r.process(r.ctx, sp)
				case <-r.ctx.Done():
					return
				}
			}
		}()
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		t := time.NewTicker(r.cfg.PollInterval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				r.Sweep(r.ctx, r.now())
			case <-r.ctx.Done():
				return
			}
		}
	}()
}

// Close stops the router. Spans still waiting are dropped and counted.
func (r *Router) Close() {
	if r.cancel == nil {
		return
	}
	r.cancel()
	r.wg.Wait() // each destination worker drains its queue as it stops
	r.hmu.Lock()
	for id := range r.held {
		r.metrics.Discarded.WithLabelValues(discardShutdown).Inc()
		delete(r.held, id)
	}
	r.heldBytes = 0
	r.hmu.Unlock()
	r.metrics.HeldRoots.Set(0)
}

// Submit hands spans of source to the router without blocking. It returns how many it could not take
// because the intake was full; the receiver reports them as rejected.
func (r *Router) Submit(source string, spans []*tracepb.Span) (rejected int64) {
	for _, sp := range spans {
		r.metrics.Received.WithLabelValues(source).Inc()
		select {
		case r.intake <- sp:
		default:
			rejected++
			r.metrics.Discarded.WithLabelValues(discardIngestFull).Inc()
		}
	}
	return rejected
}

// process attributes one span from agentd and routes it.
func (r *Router) process(ctx context.Context, sp *tracepb.Span) {
	org, run, trace, ok := identify(sp)
	if !ok {
		r.metrics.Discarded.WithLabelValues(discardUnattributed).Inc()
		return
	}
	turn, err := r.attribute(ctx, org, run, trace)
	if err != nil {
		r.metrics.Discarded.WithLabelValues(discardReason(err)).Inc()
		if discardReason(err) == discardLookup {
			r.log.Warn("trace router: attribution lookup failed; dropping a span", "err", err)
		}
		return
	}
	// Canonical IDs: whatever form the sender used, the copy carries what was proven.
	sp.Attributes = setAttr(sp.Attributes, kvStr(semconv.KoinonOrg, org.String()))
	sp.Attributes = setAttr(sp.Attributes, kvStr(semconv.KoinonRun, run.String()))

	rootID := semconv.RootSpanID(trace)
	if len(sp.ParentSpanId) == 0 && bytes.Equal(sp.SpanId, rootID[:]) {
		r.hold(ctx, &heldRoot{span: sp, org: org, run: run, trace: trace, turn: turn, since: r.now()})
		return
	}
	r.dispatch(ctx, org, sp)
}

func discardReason(err error) string {
	switch {
	case errors.Is(err, ErrUnknownRun):
		return discardUnknownRun
	case errors.Is(err, ErrUnknownTrace):
		return discardUnknownTrace
	case errors.Is(err, ErrUntraced):
		return discardUntraced
	}
	return discardLookup
}

// identify reads the org, the run and the trace ID a span claims.
func identify(sp *tracepb.Span) (org, run pgtype.UUID, trace [16]byte, ok bool) {
	if len(sp.TraceId) != 16 || len(sp.SpanId) != 8 {
		return
	}
	copy(trace[:], sp.TraceId)
	if trace == ([16]byte{}) {
		return
	}
	o, _ := attrString(sp.Attributes, semconv.KoinonOrg)
	u, _ := attrString(sp.Attributes, semconv.KoinonRun)
	if org.Scan(o) != nil || run.Scan(u) != nil || !org.Valid || !run.Valid {
		return
	}
	return org, run, trace, true
}

// ---- attribution cache ----

type attrKey struct {
	org, run pgtype.UUID
	trace    [16]byte
}

type attrEntry struct {
	turn    Turn
	err     error
	expires time.Time
}

const maxCache = 50_000

// attribute checks (org, run, trace) against Postgres, once per TTL. A refusal is cached briefly so
// a flood of forged spans costs one query, not one each. A lookup failure is not cached.
func (r *Router) attribute(ctx context.Context, org, run pgtype.UUID, trace [16]byte) (Turn, error) {
	k := attrKey{org, run, trace}
	now := r.now()
	r.cmu.Lock()
	if e, ok := r.attrs[k]; ok && now.Before(e.expires) {
		r.cmu.Unlock()
		return e.turn, e.err
	}
	r.cmu.Unlock()

	turn, err := r.store.Attribute(ctx, org, run, trace)
	ttl := r.cfg.AttributionTTL
	switch {
	case err == nil:
	case errors.Is(err, ErrUnknownRun), errors.Is(err, ErrUnknownTrace), errors.Is(err, ErrUntraced):
		ttl = r.cfg.NegativeTTL
	default:
		return Turn{}, err
	}
	r.cmu.Lock()
	if len(r.attrs) >= maxCache {
		for key, e := range r.attrs {
			if !now.Before(e.expires) {
				delete(r.attrs, key)
			}
		}
		if len(r.attrs) >= maxCache {
			clear(r.attrs)
		}
	}
	r.attrs[k] = attrEntry{turn: turn, err: err, expires: now.Add(ttl)}
	r.cmu.Unlock()
	return turn, err
}

// ---- routes and secrets ----

type routeEntry struct {
	routes  []tracedest.Route
	expires time.Time
}

type secretEntry struct {
	set     *scrub.Secrets
	expires time.Time
}

func (r *Router) routesFor(ctx context.Context, org pgtype.UUID) ([]tracedest.Route, error) {
	now := r.now()
	r.cmu.Lock()
	if e, ok := r.rts[org]; ok && now.Before(e.expires) {
		r.cmu.Unlock()
		return e.routes, nil
	}
	r.cmu.Unlock()
	routes, err := r.routes.Routes(ctx, org)
	if err != nil {
		return nil, err
	}
	r.cmu.Lock()
	r.rts[org] = routeEntry{routes: routes, expires: now.Add(r.cfg.RouteTTL)}
	r.cmu.Unlock()
	r.syncQueues(org, routes)
	return routes, nil
}

// secretsFor returns the org's secret set. When it cannot be loaded a stale set is used; with none at
// all the error is returned and the caller fails closed (no content).
func (r *Router) secretsFor(ctx context.Context, org pgtype.UUID) (*scrub.Secrets, error) {
	if r.secrets == nil {
		return nil, nil
	}
	now := r.now()
	r.cmu.Lock()
	e, ok := r.scr[org]
	r.cmu.Unlock()
	if ok && now.Before(e.expires) {
		return e.set, nil
	}
	set, err := r.secrets.Secrets(ctx, org)
	if err != nil {
		if ok {
			return e.set, nil
		}
		return nil, err
	}
	r.cmu.Lock()
	r.scr[org] = secretEntry{set: set, expires: now.Add(r.cfg.SecretsTTL)}
	r.cmu.Unlock()
	return set, nil
}

// ---- fan-out ----

// dispatch makes each destination's copy of spans and queues it. spans must belong to org.
func (r *Router) dispatch(ctx context.Context, org pgtype.UUID, spans ...*tracepb.Span) {
	routes, err := r.routesFor(ctx, org)
	if err != nil {
		r.log.Warn("trace router: reading destinations failed; dropping spans", "err", err)
		r.metrics.Discarded.WithLabelValues(discardLookup).Add(float64(len(spans)))
		return
	}
	if len(routes) == 0 {
		r.metrics.Discarded.WithLabelValues(discardNoRoute).Add(float64(len(spans)))
		return
	}
	secrets, serr := r.secretsFor(ctx, org)
	if serr != nil {
		r.log.Warn("trace router: the org's secrets cannot be loaded; sending no content", "err", serr)
	}
	for _, route := range routes {
		level, _ := scrub.ParseLevel(route.Level)
		if serr != nil {
			level = scrub.LevelNone // fail closed
		}
		q := r.queueFor(org, route)
		out := make([]*tracepb.Span, 0, len(spans))
		for _, sp := range spans {
			p := newCopyParams(level, secrets, r.cfg.ContentLimit, route.Platform, func(k scrub.Kind) {
				r.metrics.Redactions.WithLabelValues(string(k)).Inc()
			})
			if isRoot(sp) {
				p.Dropped = q.reportDropped()
			}
			out = append(out, copySpan(sp, p))
		}
		for tries := 0; tries < 3; tries++ {
			if q.enqueue(route, out) {
				break
			}
			q = r.queueFor(org, route) // the queue closed meanwhile (idle); take a fresh one
		}
	}
}

func isRoot(sp *tracepb.Span) bool {
	if len(sp.ParentSpanId) != 0 || len(sp.TraceId) != 16 || len(sp.SpanId) != 8 {
		return false
	}
	var t [16]byte
	copy(t[:], sp.TraceId)
	id := semconv.RootSpanID(t)
	return bytes.Equal(sp.SpanId, id[:])
}

// queueFor returns the queue of a destination, starting its worker on first use. The platform has one
// queue for every org; an org destination has its own.
func (r *Router) queueFor(org pgtype.UUID, route tracedest.Route) *queue {
	key := "platform"
	kind := KindPlatform
	if !route.Platform {
		key = org.String() + "/" + route.ID
		kind = KindOrg
	}
	r.qmu.Lock()
	defer r.qmu.Unlock()
	if q, ok := r.queues[key]; ok {
		return q
	}
	q := &queue{r: r, key: key, org: org, platform: route.Platform, kind: kind, backend: string(route.Destination.Preset), route: route, wake: make(chan struct{}, 1)}
	r.queues[key] = q
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		q.run(r.ctx)
	}()
	return q
}

// syncQueues retires the queues of destinations that are gone or disabled, so spans queued for them
// are not sent.
func (r *Router) syncQueues(org pgtype.UUID, routes []tracedest.Route) {
	have := map[string]bool{}
	platform := false
	for _, rt := range routes {
		if rt.Platform {
			platform = true
		} else {
			have[org.String()+"/"+rt.ID] = true
		}
	}
	r.qmu.Lock()
	defer r.qmu.Unlock()
	for key, q := range r.queues {
		switch {
		case q.platform && !platform:
			q.retire()
		case !q.platform && q.org == org && !have[key]:
			q.retire()
		}
	}
}
