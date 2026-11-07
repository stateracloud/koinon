// SPDX-License-Identifier: Apache-2.0

// Package router is Koinon's trace router (ADR 0074, old issue 213). It lives in koinon-api and takes
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
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

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
	// ModelCalls returns the run's ledger entries of turn seq: those with from <= time < to that are
	// not at or after the next turn's start, in time order, so a row on a boundary counts once.
	ModelCalls(ctx context.Context, org, run pgtype.UUID, seq int32, from, to time.Time) ([]ModelCall, error)
}

// RouteSource gives the enabled destinations of an org with their credentials opened.
// *tracedest.Service is one.
type RouteSource interface {
	Routes(ctx context.Context, org pgtype.UUID) ([]tracedest.Route, error)
}

// RouteVersioner is implemented by a RouteSource that can fingerprint an org's destination settings
// cheaply (*tracedest.Service does). The router checks it before every batch, so a change applies to
// spans already queued. Without it routes are reloaded after RouteTTL.
type RouteVersioner interface {
	ConfigVersion(ctx context.Context, org pgtype.UUID) (string, error)
}

// SecretVersioner is implemented by a SecretSource that can fingerprint an org's stored credentials
// cheaply (*PGSecrets does), so a changed credential is scrubbed from the next batch on.
type SecretVersioner interface {
	Version(ctx context.Context, org pgtype.UUID) (string, error)
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
	// MaxHeld and MaxHeldBytes bound the roots waiting for the ledger (defaults 2048 and 3% of the memory limit); a
	// root over the bound is released at once with the ledger as it stands.
	MaxHeld      int
	MaxHeldBytes int

	// ContentLimit is the most bytes one content attribute keeps (default 16 KiB).
	ContentLimit int

	// Cache lifetimes: attribution (default 10m; a refusal 30s). A source that can be fingerprinted
	// (RouteVersioner, SecretVersioner) is checked every ConfigCheck (default 1s) and reloaded when
	// the fingerprint changes; one that cannot is reloaded after RouteTTL (15s) or SecretsTTL (5m).
	AttributionTTL, NegativeTTL, RouteTTL, SecretsTTL, ConfigCheck time.Duration
	// SecretsGrace is how long a secret set may be used after it could last be confirmed current
	// when reloading it fails (default 1 minute). After that the org gets no content.
	SecretsGrace time.Duration
	// RecheckInterval is how often live queues re-read their destination's settings, so a quiet
	// org's deleted destination is retired (default 15s), and expired cache entries are evicted.
	RecheckInterval time.Duration
	// TurnGrace is how long after a turn ends its ledger rows may still carry a time in it (default
	// 2s); a row belongs to the turn whose window holds it, up to the next turn's start.
	TurnGrace time.Duration
	// FinishedTTL is how long a finished turn is remembered, so a duplicate root does not make its
	// model calls twice (default 10m).
	FinishedTTL time.Duration
	// MemoryLimit is the process's memory limit in bytes (internal/memlimit.Limit; 0 when unknown). The
	// byte budgets below default to shares of it: queues 20%, intake 5%, held roots 3%, requests in
	// progress 8% (without a limit: 128, 32, 16 and 64 MiB). Bytes are counted as the size of the
	// encoded span plus an estimate of the Go objects it decodes to (see estimateSize).
	MemoryLimit int64
	// IntakeBytes bounds the spans waiting for attribution. QueueBytes bounds all destination queues
	// together; each queue gets an equal share of it (at least queueFloor) and drops only its own oldest
	// spans when it is over, so one destination that is down cannot take what another needs.
	// RequestBytes bounds the bytes of the requests being read and decoded at once.
	IntakeBytes, QueueBytes, RequestBytes int
	// ConfigTimeout bounds every database read on the path of a span (attribution, destinations,
	// secrets); a failed read is remembered for ConfigCheck so a slow database is asked once, not once
	// per span (default 3s).
	ConfigTimeout time.Duration
	// BatchBytes bounds the bytes of one export batch, besides BatchSize (default 4 MiB).
	BatchBytes int
	// SweepParallel and SweepTimeout bound the ledger reads for held roots (defaults 8 and 5s), so
	// one slow read does not hold the others past their 30 seconds.
	SweepParallel int
	SweepTimeout  time.Duration
	// MaxRequestSpans bounds the spans in one request (default 1024; agentd sends 256), and
	// MaxRequestBytes its size, compressed and decompressed (4 MiB and 16 MiB).
	// MaxConcurrentRequests bounds requests in progress (default 8).
	MaxRequestSpans       int
	MaxRequestBytes       int
	MaxConcurrentRequests int

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
	setInt(&c.ContentLimit, DefaultContentLimit)
	setDur(&c.AttributionTTL, 10*time.Minute)
	setDur(&c.NegativeTTL, 30*time.Second)
	setDur(&c.RouteTTL, 15*time.Second)
	setDur(&c.SecretsTTL, 5*time.Minute)
	setDur(&c.ConfigCheck, time.Second)
	setDur(&c.SecretsGrace, time.Minute)
	setDur(&c.RecheckInterval, 15*time.Second)
	setDur(&c.TurnGrace, 2*time.Second)
	setDur(&c.FinishedTTL, 10*time.Minute)
	share := func(p int64, def int) int {
		if c.MemoryLimit > 0 {
			return int(c.MemoryLimit / 100 * p)
		}
		return def
	}
	setInt(&c.IntakeBytes, share(5, 32<<20))
	setInt(&c.QueueBytes, share(20, 128<<20))
	setInt(&c.RequestBytes, share(8, 64<<20))
	setInt(&c.MaxHeldBytes, share(3, 16<<20))
	setInt(&c.SweepParallel, 8)
	setDur(&c.SweepTimeout, 5*time.Second)
	setDur(&c.ConfigTimeout, 3*time.Second)
	setInt(&c.BatchBytes, 4<<20)
	// A held root settles after two polls that agree. Rows of a turn can still be logged up to
	// TurnGrace after it ended, so polls closer together than that would settle too early.
	c.PollInterval = max(c.PollInterval, c.TurnGrace)
	setInt(&c.MaxRequestSpans, 1024)
	setInt(&c.MaxRequestBytes, 4<<20)
	setInt(&c.MaxConcurrentRequests, 8)
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
	intake chan intakeItem
	// reqs limits the receiver's requests in progress.
	reqs chan struct{}

	intakeBytes atomic.Int64
	queueBytes  atomic.Int64
	reqBytes    atomic.Int64

	cmu      sync.Mutex
	attrs    map[attrKey]attrEntry
	rts      map[pgtype.UUID]routeEntry
	scr      map[pgtype.UUID]secretEntry
	fails    map[string]failEntry
	routeSeq uint64
	fl       flight

	qmu    sync.Mutex
	queues map[string]*queue

	hmu       sync.Mutex
	held      map[[16]byte]*heldRoot
	heldBytes int
	// claimed holds the traces whose root was accepted, held or finished, until FinishedTTL: a second
	// root of the same trace is a duplicate. It is claimed in the same critical section as the check.
	claimed map[[16]byte]time.Time
}

// failEntry remembers a failed read for a moment.
type failEntry struct {
	err   error
	until time.Time
}

type intakeItem struct {
	sp   *tracepb.Span
	size int
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
		intake: make(chan intakeItem, cfg.IngestQueue), reqs: make(chan struct{}, cfg.MaxConcurrentRequests),
		attrs: map[attrKey]attrEntry{}, rts: map[pgtype.UUID]routeEntry{}, scr: map[pgtype.UUID]secretEntry{},
		fails:   map[string]failEntry{},
		claimed: map[[16]byte]time.Time{},
		queues:  map[string]*queue{}, held: map[[16]byte]*heldRoot{},
	}, nil
}

// RequestsInProgress is the number of export requests the receiver is handling.
func (r *Router) RequestsInProgress() int { return len(r.reqs) }

// HeldRoots is the number of turn roots waiting for the ledger.
func (r *Router) HeldRoots() int {
	r.hmu.Lock()
	defer r.hmu.Unlock()
	return len(r.held)
}

// Metrics returns the router's metrics.
func (r *Router) Metrics() *Metrics { return r.metrics }

// Start runs the workers, the ledger poller and the settings re-check until ctx ends or Close is
// called.
func (r *Router) Start(ctx context.Context) {
	r.ctx, r.cancel = context.WithCancel(ctx)
	for range r.cfg.Workers {
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			for {
				select {
				case it := <-r.intake:
					r.intakeBytes.Add(-int64(it.size))
					r.process(r.ctx, it)
				case <-r.ctx.Done():
					return
				}
			}
		}()
	}
	r.every(r.cfg.PollInterval, func() { r.Sweep(r.ctx, r.now()) })
	r.every(r.cfg.RecheckInterval, func() { r.Recheck(r.ctx) })
}

func (r *Router) every(d time.Duration, f func()) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		t := time.NewTicker(d)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				f()
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

// Submit hands spans of source to the router without blocking. It returns how many it could not take:
// spans that are not structurally valid, and spans the intake had no room for (by count or bytes).
// The receiver reports them as rejected.
func (r *Router) Submit(source string, spans []*tracepb.Span) (rejected int64) {
	for _, sp := range spans {
		r.metrics.Received.WithLabelValues(source).Inc()
		if !validSpan(sp) {
			rejected++
			r.metrics.Discarded.WithLabelValues(discardInvalid).Inc()
			continue
		}
		size := estimateSize(sp)
		if r.intakeBytes.Add(int64(size)) > int64(r.cfg.IntakeBytes) {
			r.intakeBytes.Add(-int64(size))
			rejected++
			r.metrics.Discarded.WithLabelValues(discardIngestFull).Inc()
			continue
		}
		select {
		case r.intake <- intakeItem{sp: sp, size: size}:
		default:
			r.intakeBytes.Add(-int64(size))
			rejected++
			r.metrics.Discarded.WithLabelValues(discardIngestFull).Inc()
		}
	}
	return rejected
}

// estimateSize is what a span costs in memory: its encoded size plus about 100 bytes for each
// attribute, event and link (the Go objects around the strings), which is what makes a span of many
// small attributes weigh what it does once decoded.
func estimateSize(sp *tracepb.Span) int {
	n := len(sp.Attributes) + len(sp.Events) + len(sp.Links)
	for _, e := range sp.Events {
		n += len(e.Attributes)
	}
	for _, l := range sp.Links {
		n += len(l.Attributes)
	}
	return proto.Size(sp) + 100*n
}

// Structural limits on one span: far above what agentd makes (content is pre-capped at 64 KiB), far
// below what would cost real memory.
const (
	maxSpanBytes      = 1 << 20
	maxSpanAttributes = 256
	maxSpanEvents     = 256
	maxSpanLinks      = 64
)

// validSpan checks what the router relies on before a span enters the intake: IDs of the right
// length, a trace ID that is not all zero, and bounded size.
func validSpan(sp *tracepb.Span) bool {
	if sp == nil || len(sp.TraceId) != 16 || len(sp.SpanId) != 8 || len(sp.ParentSpanId) != 0 && len(sp.ParentSpanId) != 8 {
		return false
	}
	if bytes.Equal(sp.TraceId, make([]byte, 16)) || bytes.Equal(sp.SpanId, make([]byte, 8)) {
		return false
	}
	return len(sp.Attributes) <= maxSpanAttributes && len(sp.Events) <= maxSpanEvents && len(sp.Links) <= maxSpanLinks && proto.Size(sp) <= maxSpanBytes
}

// process attributes one span from agentd and routes it.
func (r *Router) process(ctx context.Context, in intakeItem) {
	sp := in.sp
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

	if isRoot(sp) {
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

	fkey := "attr/" + org.String() + "/" + run.String() + "/" + string(trace[:])
	if err := r.failed(fkey, now); err != nil {
		return Turn{}, err
	}
	type result struct {
		turn Turn
		err  error
	}
	refusal := func(err error) bool {
		return errors.Is(err, ErrUnknownRun) || errors.Is(err, ErrUnknownTrace) || errors.Is(err, ErrUntraced)
	}
	v, ferr := r.fl.do(fkey, func() (any, error) {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.cfg.ConfigTimeout)
		defer cancel()
		t, err := r.store.Attribute(cctx, org, run, trace)
		if err != nil && !refusal(err) {
			// Remembered by the leader, before the callers that joined it are released (see read).
			r.fail(fkey, err)
		}
		return result{t, err}, nil
	})
	if ferr != nil {
		return Turn{}, ferr
	}
	turn, err := v.(result).turn, v.(result).err
	ttl := r.cfg.AttributionTTL
	switch {
	case err == nil:
	case refusal(err):
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
	ver     string
	seq     uint64    // order of the load, so an older load never replaces a newer one
	checked time.Time // last confirmed current
	expires time.Time // for a source without versions
}

type secretEntry struct {
	set     *scrub.Secrets
	ver     string
	checked time.Time // last confirmed current
	expires time.Time // for a source without versions
}

// failed returns a recent failure of the same read, if any: a read that just failed is not repeated by
// every span behind it. The caller still fails closed.
func (r *Router) failed(key string, now time.Time) error {
	r.cmu.Lock()
	defer r.cmu.Unlock()
	if f, ok := r.fails[key]; ok {
		if now.Before(f.until) {
			return f.err
		}
		delete(r.fails, key)
	}
	return nil
}

// fail remembers a failed read for ConfigCheck from the moment it failed, not from when it started: a
// read that hung until ConfigTimeout (3 s by default, longer than the 1 s ConfigCheck) would otherwise
// be remembered until a time already past, and every span behind it would ask the database again.
func (r *Router) fail(key string, err error) {
	until := r.now().Add(r.cfg.ConfigCheck)
	r.cmu.Lock()
	r.fails[key] = failEntry{err: err, until: until}
	r.cmu.Unlock()
}

// read runs fn once per key at a time (see flight). The leader remembers a failure before the callers
// that joined it are released, so no caller can start the same read between the failure and its
// record. Each caller records the error again once released, which also covers a read that panicked
// (flight turns the panic into the error after fn has gone).
func (r *Router) read(key string, fn func() (any, error)) (any, error) {
	v, err := r.fl.do(key, func() (any, error) {
		v, err := fn()
		if err != nil {
			r.fail(key, err)
		}
		return v, err
	})
	if err != nil {
		r.fail(key, err)
	}
	return v, err
}

// routesFor returns the org's destinations. With a versioned source the cache is confirmed against
// the settings' fingerprint every ConfigCheck and reloaded the moment it changes; a failed check or
// load is an error, never a stale answer, because a copy made from stale settings could carry more
// than the org now allows. Reads have a deadline, run once per org at a time, and a failure is
// remembered for ConfigCheck.
func (r *Router) routesFor(ctx context.Context, org pgtype.UUID) ([]tracedest.Route, error) {
	now := r.now()
	r.cmu.Lock()
	e, ok := r.rts[org]
	r.cmu.Unlock()
	rv, versioned := r.routes.(RouteVersioner)
	if ok {
		if !versioned && now.Before(e.expires) {
			return e.routes, nil
		}
		if versioned && now.Sub(e.checked) < r.cfg.ConfigCheck {
			return e.routes, nil
		}
	}
	key := "routes/" + org.String()
	if err := r.failed(key, now); err != nil {
		return nil, err
	}
	v, err := r.read(key, func() (any, error) {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.cfg.ConfigTimeout)
		defer cancel()
		var ver string
		if versioned {
			var err error
			if ver, err = rv.ConfigVersion(cctx, org); err != nil {
				return nil, err
			}
			if ok && ver == e.ver {
				e.checked = now
				r.cmu.Lock()
				if cur, still := r.rts[org]; still && cur.seq == e.seq {
					r.rts[org] = e
				}
				r.cmu.Unlock()
				return e.routes, nil
			}
		}
		r.cmu.Lock()
		r.routeSeq++
		seq := r.routeSeq
		r.cmu.Unlock()
		routes, err := r.routes.Routes(cctx, org)
		if err != nil {
			return nil, err
		}
		r.cmu.Lock()
		if cur, still := r.rts[org]; !still || cur.seq < seq {
			r.rts[org] = routeEntry{routes: routes, ver: ver, seq: seq, checked: now, expires: now.Add(r.cfg.RouteTTL)}
		}
		r.cmu.Unlock()
		r.syncQueues(org, routes)
		return routes, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]tracedest.Route), nil
}

// secretsFor returns the org's secret set. It is confirmed against the credentials' fingerprint like
// the routes. The last set is used for SecretsGrace only when the fingerprint itself cannot be read (or,
// for a source without one, after its TTL): once the fingerprint says the credentials changed, or an
// envelope cannot be opened, a failed reload gives an error and the caller sends no content (fail
// closed).
func (r *Router) secretsFor(ctx context.Context, org pgtype.UUID) (*scrub.Secrets, error) {
	if r.secrets == nil {
		return nil, nil
	}
	now := r.now()
	r.cmu.Lock()
	e, ok := r.scr[org]
	r.cmu.Unlock()
	sv, versioned := r.secrets.(SecretVersioner)
	if ok {
		if !versioned && now.Before(e.expires) {
			return e.set, nil
		}
		if versioned && now.Sub(e.checked) < r.cfg.ConfigCheck {
			return e.set, nil
		}
	}
	key := "secrets/" + org.String()
	if err := r.failed(key, now); err != nil {
		return r.graceOrFail(e, ok, versioned, err, now)
	}
	v, err := r.read(key, func() (any, error) {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.cfg.ConfigTimeout)
		defer cancel()
		var ver string
		if versioned {
			var err error
			if ver, err = sv.Version(cctx, org); err != nil {
				return nil, &graceable{err}
			}
			if ok && ver == e.ver {
				e.checked = now
				r.cmu.Lock()
				r.scr[org] = e
				r.cmu.Unlock()
				return e.set, nil
			}
		}
		set, err := r.secrets.Secrets(cctx, org)
		if err != nil {
			if versioned || errors.Is(err, ErrUnopenable) {
				return nil, err // the credentials changed, or cannot be read: no grace
			}
			return nil, &graceable{err}
		}
		r.cmu.Lock()
		r.scr[org] = secretEntry{set: set, ver: ver, checked: now, expires: now.Add(r.cfg.SecretsTTL)}
		r.cmu.Unlock()
		return set, nil
	})
	if err != nil {
		return r.graceOrFail(e, ok, versioned, err, now)
	}
	return v.(*scrub.Secrets), nil
}

// graceable marks a failure that leaves the last set usable for SecretsGrace: the fingerprint could not
// be read, so nothing says the credentials changed.
type graceable struct{ error }

func (g *graceable) Unwrap() error { return g.error }

func (r *Router) graceOrFail(e secretEntry, ok, versioned bool, err error, now time.Time) (*scrub.Secrets, error) {
	var g *graceable
	if ok && errors.As(err, &g) && !errors.Is(err, ErrUnopenable) {
		valid := e.expires
		if versioned {
			valid = e.checked.Add(r.cfg.ConfigCheck)
		}
		if now.Before(valid.Add(r.cfg.SecretsGrace)) {
			return e.set, nil
		}
	}
	return nil, err
}

// ---- fan-out ----

// dispatch queues spans for every destination the org has. spans must belong to org. The copy for a
// destination is made when its batch is sent (see queue.prepare).
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
	items := make([]item, len(spans))
	for i, sp := range spans {
		items[i] = item{sp: sp, org: org, size: estimateSize(sp)}
	}
	for _, route := range routes {
		if route.Unreadable {
			// The destination exists but its credentials cannot be opened: its queue is kept (it is not
			// a deleted destination), and what would go to it is counted as lost.
			kind := KindOrg
			if route.Platform {
				kind = KindPlatform
			}
			r.metrics.Dropped.WithLabelValues(kind, string(route.Destination.Preset), dropUnreadable).Add(float64(len(items)))
			continue
		}
		queued := false
		for range 3 {
			if r.queueFor(org, route).enqueue(route, items) {
				queued = true
				break
			}
			// The queue closed or was retired meanwhile; take a fresh one.
		}
		if !queued {
			r.metrics.Discarded.WithLabelValues(discardEnqueueFailed).Add(float64(len(items)))
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
	key, kind, destID := "platform", KindPlatform, ""
	if !route.Platform {
		key, kind, destID = org.String()+"/"+route.ID, KindOrg, route.ID
	}
	r.qmu.Lock()
	defer r.qmu.Unlock()
	if q, ok := r.queues[key]; ok {
		return q
	}
	ctx, cancel := context.WithCancel(r.ctx)
	q := &queue{
		r: r, key: key, destID: destID, org: org, platform: route.Platform, kind: kind, backend: string(route.Destination.Preset),
		route: route, wake: make(chan struct{}, 1), ctx: ctx, cancel: cancel, droppedByOrg: map[pgtype.UUID]int64{},
	}
	r.queues[key] = q
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		q.run()
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
	for _, q := range r.queues {
		switch {
		case q.platform && !platform, !q.platform && q.org == org && !have[q.key]:
			q.retire()
			delete(r.queues, q.key) // a new destination under the same key gets a new queue
		}
	}
}

// Recheck re-reads the settings of every live queue's org, which retires the queues of destinations
// that were deleted or disabled even when their org sends no more spans, and evicts cache entries
// that expired (deleted orgs, finished runs). The router calls it every RecheckInterval.
func (r *Router) Recheck(ctx context.Context) {
	r.qmu.Lock()
	orgs := map[pgtype.UUID]bool{}
	for _, q := range r.queues {
		orgs[q.org] = true
	}
	r.qmu.Unlock()
	for org := range orgs {
		if ctx.Err() != nil {
			return
		}
		cctx, cancel := context.WithTimeout(ctx, r.cfg.SweepTimeout)
		if _, err := r.routesFor(cctx, org); err != nil {
			r.log.Debug("trace router: re-checking a destination's settings failed", "err", err)
		}
		cancel()
	}
	r.evict()
}

// evict drops cache entries that are past their lifetime.
func (r *Router) evict() {
	now := r.now()
	r.cmu.Lock()
	defer r.cmu.Unlock()
	for k, e := range r.attrs {
		if !now.Before(e.expires) {
			delete(r.attrs, k)
		}
	}
	for org, e := range r.rts {
		if now.Sub(e.checked) > 10*time.Minute {
			delete(r.rts, org)
		}
	}
	for org, e := range r.scr {
		if now.Sub(e.checked) > 10*time.Minute {
			delete(r.scr, org)
		}
	}
	for k, f := range r.fails {
		if !now.Before(f.until) {
			delete(r.fails, k)
		}
	}
	r.cmu.Unlock()
	r.hmu.Lock()
	for t, exp := range r.claimed {
		if _, held := r.held[t]; !held && !now.Before(exp) {
			delete(r.claimed, t)
		}
	}
	r.hmu.Unlock()
	r.cmu.Lock()
}

// reserve takes size bytes of the global queue budget, or reports false when the budget is full. The
// per-queue shares keep the sum of the queues within it, so this is a backstop; it never evicts another
// queue's spans.
func (r *Router) reserve(size int) bool {
	for try := 0; try < 2; try++ {
		if r.queueBytes.Add(int64(size)) <= int64(r.cfg.QueueBytes) {
			return true
		}
		r.queueBytes.Add(-int64(size))
		if try == 0 {
			// The budget is full because a queue holds more than its share (the shares shrank when
			// another queue appeared): make each such queue drop its own excess, then try again.
			r.rebalance()
		}
	}
	return false
}

// rebalance makes every queue that holds more than its share drop its own oldest spans down to it.
func (r *Router) rebalance() {
	capBytes := r.queueCap()
	r.qmu.Lock()
	qs := make([]*queue, 0, len(r.queues))
	for _, q := range r.queues {
		qs = append(qs, q)
	}
	r.qmu.Unlock()
	for _, q := range qs {
		q.trim(capBytes)
	}
}

// queueFloor is the least a queue may hold, however many queues share the budget.
const queueFloor = 256 << 10

// minShares is how many queues the budget is divided among at least: a lone queue never takes more than
// a quarter of it, so the room for the next destination to appear is there before it does (a queue that
// is down could otherwise have its whole batch in flight when the first healthy one arrives).
const minShares = 4

// queueCap is one queue's share of the queue budget: equal shares among the live queues (at least
// minShares of them), and at least queueFloor.
func (r *Router) queueCap() int {
	r.qmu.Lock()
	n := max(len(r.queues), minShares)
	r.qmu.Unlock()
	return max(r.cfg.QueueBytes/n, queueFloor)
}

// unregister removes a retired queue from the router, so a new destination under the same key gets a
// queue of its own.
func (r *Router) unregister(q *queue) {
	r.qmu.Lock()
	defer r.qmu.Unlock()
	if r.queues[q.key] == q {
		delete(r.queues, q.key)
	}
}
