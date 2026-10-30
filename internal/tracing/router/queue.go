// SPDX-License-Identifier: Apache-2.0

package router

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/stateracloud/koinon/internal/tracedest"
	"github.com/stateracloud/koinon/internal/tracing/export"
	"github.com/stateracloud/koinon/internal/tracing/semconv"
)

// queue is one destination's bounded queue and the worker that drains it (ADR 0074, decision 7).
// Nothing in it waits for another destination, for a run, for the ledger or for the gateway: a full
// queue drops its oldest span, and a destination that keeps failing is paused by its own breaker.
type queue struct {
	r        *Router
	key      string
	org      pgtype.UUID
	platform bool
	kind     string
	backend  string

	mu      sync.Mutex
	items   []*tracepb.Span
	head    int
	route   tracedest.Route
	retired bool
	closed  bool
	wake    chan struct{}

	// dropped counts spans lost since the last root span reported them (koinon.spans_dropped).
	dropped atomic.Int64
	// lostForHealth counts spans lost since the last health write.
	lostForHealth atomic.Int64

	// Worker state; only the worker goroutine touches it.
	failures   int
	openUntil  time.Time
	okDirty    bool
	errMsg     string
	lastHealth time.Time
}

func (q *queue) depth() int { return len(q.items) - q.head }

// enqueue adds spans, dropping the oldest when the queue is full. It reports false when the queue
// has been closed (idle, retired), and the caller then asks for a new one.
func (q *queue) enqueue(route tracedest.Route, spans []*tracepb.Span) bool {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return false
	}
	q.route = route
	var lost int64
	for _, sp := range spans {
		if q.depth() >= q.r.cfg.QueueSize {
			q.items[q.head] = nil
			q.head++
			lost++
		}
		q.items = append(q.items, sp)
	}
	if q.head > 1024 && q.head > len(q.items)/2 {
		q.items = append([]*tracepb.Span(nil), q.items[q.head:]...)
		q.head = 0
	}
	q.mu.Unlock()
	q.r.metrics.QueueDepth.WithLabelValues(q.kind, q.backend).Add(float64(int64(len(spans)) - lost))
	if lost > 0 {
		q.lose(lost, dropQueueFull)
	}
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return true
}

// take removes up to n spans from the front.
func (q *queue) take(n int) []*tracepb.Span {
	q.mu.Lock()
	defer q.mu.Unlock()
	k := min(n, q.depth())
	out := make([]*tracepb.Span, k)
	copy(out, q.items[q.head:q.head+k])
	for i := range k {
		q.items[q.head+i] = nil
	}
	q.head += k
	if q.head == len(q.items) {
		q.items, q.head = q.items[:0], 0
	}
	q.r.metrics.QueueDepth.WithLabelValues(q.kind, q.backend).Sub(float64(k))
	return out
}

func (q *queue) snapshot() (tracedest.Route, bool, int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.route, q.retired, q.depth()
}

func (q *queue) retire() {
	q.mu.Lock()
	q.retired = true
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// lose counts n spans that will never be exported.
func (q *queue) lose(n int64, reason string) {
	q.r.metrics.Dropped.WithLabelValues(q.kind, q.backend, reason).Add(float64(n))
	q.dropped.Add(n)
	q.lostForHealth.Add(n)
}

// reportDropped returns, and resets, the spans lost since the last report.
func (q *queue) reportDropped() int64 { return q.dropped.Swap(0) }

// run is the worker. It exits when ctx ends, when the destination is retired, or when the queue has
// been idle for IdleTTL.
func (q *queue) run(ctx context.Context) {
	cfg := q.r.cfg
	tick := time.NewTicker(cfg.FlushInterval)
	defer tick.Stop()
	idleSince := q.r.now()
	for {
		if ctx.Err() != nil {
			q.drain(dropShutdown)
			q.flushHealth(true)
			q.finish()
			return
		}
		route, retired, depth := q.snapshot()
		if retired {
			q.drain(dropRemoved)
			q.finish()
			return
		}
		if depth == 0 {
			q.flushHealth(false)
			if q.r.now().Sub(idleSince) >= cfg.IdleTTL && !q.okDirty && q.errMsg == "" && q.tryClose() {
				return
			}
			select {
			case <-q.wake:
			case <-tick.C:
			case <-ctx.Done():
			}
			continue
		}
		idleSince = q.r.now()
		if depth < cfg.BatchSize && cfg.Linger > 0 {
			// Let a few more spans arrive, so a trace goes out in one request.
			_ = q.r.sleep(ctx, cfg.Linger)
		}
		batch := q.take(cfg.BatchSize)
		if len(batch) == 0 {
			continue
		}
		q.send(ctx, route, batch)
	}
}

// tryClose unregisters an idle queue. It fails if a span arrived meanwhile.
func (q *queue) tryClose() bool {
	q.r.qmu.Lock()
	defer q.r.qmu.Unlock()
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.depth() > 0 {
		return false
	}
	q.closed = true
	if q.r.queues[q.key] == q {
		delete(q.r.queues, q.key)
	}
	return true
}

// finish closes and unregisters the queue after a drain.
func (q *queue) finish() {
	q.r.qmu.Lock()
	defer q.r.qmu.Unlock()
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	if q.r.queues[q.key] == q {
		delete(q.r.queues, q.key)
	}
}

// drain drops everything queued.
func (q *queue) drain(reason string) {
	for {
		batch := q.take(1024)
		if len(batch) == 0 {
			return
		}
		if reason == dropRemoved {
			// A removed destination is no loss to report on a health record that is gone.
			q.r.metrics.Dropped.WithLabelValues(q.kind, q.backend, reason).Add(float64(len(batch)))
			continue
		}
		q.lose(int64(len(batch)), reason)
	}
}

// send exports one batch, retrying a retryable failure with exponential backoff, and dropping the
// batch after MaxAttempts or at once for a refusal that retrying cannot fix.
func (q *queue) send(ctx context.Context, route tracedest.Route, batch []*tracepb.Span) {
	cfg := q.r.cfg
	req := buildRequest(route, batch)
	for attempt := 1; ; attempt++ {
		if wait := q.openUntil.Sub(q.r.now()); wait > 0 {
			// The breaker is open: this destination is paused. Its queue keeps filling and dropping
			// its oldest; nothing else waits.
			q.flushHealth(false)
			if q.r.sleep(ctx, wait) != nil {
				q.lose(int64(len(batch)), dropShutdown)
				return
			}
		}
		start := q.r.now()
		cctx, cancel := context.WithTimeout(ctx, cfg.ExportTimeout)
		res, err := q.r.sender.Export(cctx, route.Destination, req)
		cancel()
		took := q.r.now().Sub(start)

		if err == nil {
			q.r.metrics.ExportTime.WithLabelValues(q.kind, q.backend, outcomeOK).Observe(took.Seconds())
			q.failures, q.openUntil, q.errMsg = 0, time.Time{}, ""
			ok := int64(len(batch)) - min(res.Rejected, int64(len(batch)))
			q.r.metrics.Exported.WithLabelValues(q.kind, q.backend).Add(float64(ok))
			if res.Rejected > 0 {
				q.r.metrics.Rejected.WithLabelValues(q.kind, q.backend, rejectPartial).Add(float64(res.Rejected))
				q.dropped.Add(res.Rejected)
				q.lostForHealth.Add(res.Rejected)
			}
			q.okDirty = true
			q.flushHealth(false)
			return
		}

		var ee *export.Error
		retryable, retryAfter := false, time.Duration(0)
		msg := "the batch could not be sent"
		if errors.As(err, &ee) {
			retryable, retryAfter, msg = ee.Retryable(), ee.RetryAfter, ee.Message
		}
		if ctx.Err() != nil {
			q.lose(int64(len(batch)), dropShutdown)
			return
		}
		outcome := outcomeRefused
		if retryable {
			outcome = outcomeRetryable
		}
		q.r.metrics.ExportTime.WithLabelValues(q.kind, q.backend, outcome).Observe(took.Seconds())
		q.failures++
		q.errMsg = msg
		if q.failures >= cfg.BreakerThreshold {
			pause := cfg.BreakerBase << min(q.failures-cfg.BreakerThreshold, 16)
			q.openUntil = q.r.now().Add(min(pause, cfg.BreakerMax))
		}
		if !retryable {
			q.r.metrics.Rejected.WithLabelValues(q.kind, q.backend, rejectRefused).Add(float64(len(batch)))
			q.dropped.Add(int64(len(batch)))
			q.lostForHealth.Add(int64(len(batch)))
			q.flushHealth(false)
			return
		}
		if attempt >= cfg.MaxAttempts {
			q.lose(int64(len(batch)), dropExportFailed)
			q.flushHealth(false)
			return
		}
		delay := min(cfg.BackoffBase<<min(attempt-1, 16), cfg.BackoffMax)
		delay = max(delay, retryAfter)
		q.flushHealth(false)
		if q.r.sleep(ctx, delay) != nil {
			q.lose(int64(len(batch)), dropShutdown)
			return
		}
	}
}

// flushHealth writes the destination's health to Postgres: at most one write per HealthInterval, so a
// busy or failing destination never turns into a stream of updates. A slow database delays only this
// destination's worker.
func (q *queue) flushHealth(force bool) {
	cfg := q.r.cfg
	lost := q.lostForHealth.Load()
	if !q.okDirty && q.errMsg == "" && lost == 0 {
		return
	}
	now := q.r.now()
	if !force && now.Sub(q.lastHealth) < cfg.HealthInterval {
		return
	}
	route, _, _ := q.snapshot()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(q.r.ctx), 3*time.Second)
	defer cancel()
	if q.okDirty {
		if err := q.r.health.Success(ctx, q.org, route); err != nil {
			q.r.log.Warn("recording a trace destination's health failed", "err", err)
		}
		q.okDirty = false
	}
	if q.errMsg != "" || lost > 0 {
		msg := q.errMsg
		if msg == "" {
			msg = "spans were dropped"
		}
		if err := q.r.health.Failure(ctx, q.org, route, msg, lost); err != nil {
			q.r.log.Warn("recording a trace destination's health failed", "err", err)
		}
		q.lostForHealth.Add(-lost)
	}
	q.lastHealth = now
}

// buildRequest wraps spans in one OTLP request with the destination's resource attributes.
func buildRequest(route tracedest.Route, spans []*tracepb.Span) *coltracepb.ExportTraceServiceRequest {
	res := &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kvStr("service.name", semconv.ServiceName)}}
	for _, k := range slices.Sorted(maps.Keys(route.ResourceAttributes)) {
		if k != "service.name" {
			res.Attributes = append(res.Attributes, kvStr(k, route.ResourceAttributes[k]))
		}
	}
	return &coltracepb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{
		Resource:  res,
		SchemaUrl: semconv.SchemaURL,
		ScopeSpans: []*tracepb.ScopeSpans{{
			Scope:     &commonpb.InstrumentationScope{Name: semconv.ScopeName},
			SchemaUrl: semconv.SchemaURL,
			Spans:     spans,
		}},
	}}}
}
