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
	"github.com/stateracloud/koinon/internal/tracing/scrub"
	"github.com/stateracloud/koinon/internal/tracing/semconv"
)

// item is a span waiting in a destination's queue. It is the span as the router received it, not the
// destination's copy: the copy is made when the batch is sent, from the destination's settings as
// they are then, so a level that was lowered, a destination that was deleted or a secret that was
// added applies to spans queued before the change.
type item struct {
	sp   *tracepb.Span
	org  pgtype.UUID
	size int
}

// queue is one destination's bounded queue and the worker that drains it (ADR 0074, decision 7).
// Nothing in it waits for another destination, for a run, for the ledger or for the gateway: a full
// queue drops its oldest span, and a destination that keeps failing is paused by its own breaker.
type queue struct {
	r        *Router
	key      string
	destID   string // empty for the platform's
	org      pgtype.UUID
	platform bool
	kind     string
	backend  string

	// ctx ends when the router stops or the destination is retired, so a request or a sleep in
	// progress ends with it.
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	items   []item
	head    int
	queued  int // bytes of the items in the queue (a batch being sent is counted in the router's total, not here)
	route   tracedest.Route
	retired bool
	closed  bool
	wake    chan struct{}

	// droppedByOrg counts spans lost since the org's last root span reported them
	// (koinon.spans_dropped). The platform queue serves every org, so the count is per org.
	dmu          sync.Mutex
	droppedByOrg map[pgtype.UUID]int64
	// lostForHealth counts spans lost since the last health write.
	lostForHealth atomic.Int64

	// Worker state; only the worker goroutine touches it.
	failures   int
	openUntil  time.Time
	okDirty    bool
	errDirty   bool
	errMsg     string
	errWritten string
	lastHealth time.Time
}

func (q *queue) depth() int { return len(q.items) - q.head }

// enqueue adds items. Each item first reserves its bytes in the router's global budget; when the budget
// is full the oldest spans of the queue holding the most bytes are dropped to make room, so one
// destination that is down cannot take the memory of the others. A queue over its count drops its own
// oldest. It reports false when the queue has been closed (idle, retired), and the caller then asks for
// a new one.
func (q *queue) enqueue(route tracedest.Route, in []item) bool {
	var lost []item
	for _, it := range in {
		if !q.r.reserve(it.size) {
			lost = append(lost, it) // nothing is queued anywhere that could make room
			continue
		}
		q.mu.Lock()
		if q.closed || q.retired {
			q.mu.Unlock()
			q.r.queueBytes.Add(-int64(it.size))
			q.lose(lost, dropQueueFull)
			return false
		}
		q.route = route
		for q.depth() >= q.r.cfg.QueueSize {
			lost = append(lost, q.popLocked())
		}
		q.items = append(q.items, it)
		q.queued += it.size
		q.r.metrics.QueueDepth.WithLabelValues(q.kind, q.backend).Inc()
		q.mu.Unlock()
	}
	q.mu.Lock()
	if q.head > 1024 && q.head > len(q.items)/2 {
		q.items = append([]item(nil), q.items[q.head:]...)
		q.head = 0
	}
	q.mu.Unlock()
	q.lose(lost, dropQueueFull)
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return true
}

// popLocked removes the oldest item and frees its bytes. q.mu must be held and the queue not empty.
func (q *queue) popLocked() item {
	old := q.items[q.head]
	q.items[q.head] = item{}
	q.head++
	q.queued -= old.size
	q.r.queueBytes.Add(-int64(old.size))
	q.r.metrics.QueueDepth.WithLabelValues(q.kind, q.backend).Dec()
	return old
}

// dropOldest drops the oldest queued item to make room for another queue's span.
func (q *queue) dropOldest() bool {
	q.mu.Lock()
	if q.depth() == 0 {
		q.mu.Unlock()
		return false
	}
	old := q.popLocked()
	q.mu.Unlock()
	q.lose([]item{old}, dropQueueFull)
	return true
}

func (q *queue) queuedBytes() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.queued
}

// take removes up to n items, and up to maxBytes (at least one), from the front. Their bytes stay in
// the router's budget until the caller has finished with them: a batch being sent or retried is still
// in memory.
func (q *queue) take(n, maxBytes int) []item {
	q.mu.Lock()
	defer q.mu.Unlock()
	k, sum := 0, 0
	for k < min(n, q.depth()) {
		size := q.items[q.head+k].size
		if k > 0 && sum+size > maxBytes {
			break
		}
		sum += size
		k++
	}
	out := make([]item, k)
	copy(out, q.items[q.head:q.head+k])
	for i := range k {
		q.items[q.head+i] = item{}
	}
	q.head += k
	q.queued -= sum
	if q.head == len(q.items) {
		q.items, q.head = q.items[:0], 0
	}
	q.r.metrics.QueueDepth.WithLabelValues(q.kind, q.backend).Sub(float64(k))
	return out
}

// release frees the budget of items that are finished with, however they ended.
func (q *queue) release(items []item) {
	sum := 0
	for _, it := range items {
		sum += it.size
	}
	q.r.queueBytes.Add(-int64(sum))
}

func (q *queue) snapshot() (tracedest.Route, int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.route, q.depth()
}

func (q *queue) isRetired() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.retired
}

// retire stops the queue: nothing more is accepted, a request or sleep in progress is canceled and
// what is queued is dropped. It is called when the destination was deleted or disabled.
func (q *queue) retire() {
	q.mu.Lock()
	q.retired = true
	q.mu.Unlock()
	q.cancel()
}

// lose counts items that will never be exported, per org for koinon.spans_dropped. An empty reason
// leaves the dropped metric alone (a refusal is counted as rejected).
func (q *queue) lose(items []item, reason string) {
	if len(items) == 0 {
		return
	}
	if reason != "" {
		q.r.metrics.Dropped.WithLabelValues(q.kind, q.backend, reason).Add(float64(len(items)))
	}
	q.dmu.Lock()
	for _, it := range items {
		q.droppedByOrg[it.org]++
	}
	q.dmu.Unlock()
	q.lostForHealth.Add(int64(len(items)))
}

// removed counts items dropped because the destination is gone. That is no loss to report: there is
// no destination page to show it on, and no next root to carry it.
func (q *queue) removed(items []item) {
	if len(items) > 0 {
		q.r.metrics.Dropped.WithLabelValues(q.kind, q.backend, dropRemoved).Add(float64(len(items)))
	}
}

// reportDropped returns, and resets, the spans lost for org since the last report.
func (q *queue) reportDropped(org pgtype.UUID) int64 {
	q.dmu.Lock()
	defer q.dmu.Unlock()
	n := q.droppedByOrg[org]
	delete(q.droppedByOrg, org)
	return n
}

// run is the worker. It exits when the router stops, when the destination is retired, or when the
// queue has been idle for IdleTTL with nothing left to report.
func (q *queue) run() {
	cfg := q.r.cfg
	tick := time.NewTicker(cfg.FlushInterval)
	defer tick.Stop()
	idleSince := q.r.now()
	for {
		if q.ctx.Err() != nil {
			rest := q.take(1<<30, 1<<62)
			if q.isRetired() {
				q.removed(rest)
			} else {
				q.lose(rest, dropShutdown)
				q.flushHealth(true)
			}
			q.release(rest)
			q.finish()
			return
		}
		_, depth := q.snapshot()
		if depth == 0 {
			q.flushHealth(false)
			if q.r.now().Sub(idleSince) >= cfg.IdleTTL && !q.okDirty && !q.errDirty && q.lostForHealth.Load() == 0 && q.tryClose() {
				return
			}
			select {
			case <-q.wake:
			case <-tick.C:
			case <-q.ctx.Done():
			}
			continue
		}
		idleSince = q.r.now()
		if depth < cfg.BatchSize && cfg.Linger > 0 {
			// Let a few more spans arrive, so a trace goes out in one request.
			_ = q.r.sleep(q.ctx, cfg.Linger)
		}
		batch := q.take(cfg.BatchSize, cfg.BatchBytes)
		if len(batch) == 0 {
			continue
		}
		q.send(batch)
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
	q.cancel()
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
	q.cancel()
	if q.r.queues[q.key] == q {
		delete(q.r.queues, q.key)
	}
}

// prepare makes the destination's copy of a batch from the destination's settings as they are now.
// Items for an org that no longer has this destination are returned as gone (and an org queue is
// retired). An error means the settings could not be read; nothing is sent then, because a copy made
// from stale settings could carry more than the org now allows.
func (q *queue) prepare(ctx context.Context, batch []item, drops map[int]int64) (spans []*tracepb.Span, route tracedest.Route, gone []item, err error) {
	type orgState struct {
		route   tracedest.Route
		found   bool
		secrets *scrub.Secrets
		serr    error
	}
	states := map[pgtype.UUID]*orgState{}
	for i, it := range batch {
		st, ok := states[it.org]
		if !ok {
			rts, err := q.r.routesFor(ctx, it.org)
			if err != nil {
				return nil, route, nil, err
			}
			st = &orgState{}
			for _, rt := range rts {
				if rt.Platform == q.platform && (q.platform || rt.ID == q.destID) {
					st.route, st.found = rt, true
				}
			}
			if st.found {
				st.secrets, st.serr = q.r.secretsFor(ctx, it.org)
				if st.serr != nil {
					q.r.log.Warn("trace router: the org's secrets cannot be loaded; sending no content", "err", st.serr)
				}
			} else if !q.platform {
				q.retire()
				q.r.unregister(q)
			}
			states[it.org] = st
		}
		if !st.found {
			gone = append(gone, it)
			continue
		}
		route = st.route
		level, _ := scrub.ParseLevel(st.route.Level)
		if st.serr != nil {
			level = scrub.LevelNone // fail closed
		}
		p := newCopyParams(level, st.secrets, q.r.cfg.ContentLimit, st.route.Platform, func(k scrub.Kind) {
			q.r.metrics.Redactions.WithLabelValues(string(k)).Inc()
		})
		p.Dropped = drops[i]
		spans = append(spans, copySpan(it.sp, p))
	}
	if len(spans) > 0 {
		q.mu.Lock()
		q.route = route
		q.mu.Unlock()
	}
	return spans, route, gone, nil
}

// send exports one batch, retrying a retryable failure with exponential backoff, and dropping the
// batch after MaxAttempts or at once for a refusal that retrying cannot fix. Every attempt makes the
// copy anew and first checks that the destination still exists.
func (q *queue) send(batch []item) {
	cfg := q.r.cfg
	defer q.release(batch) // the batch is in memory until its final outcome
	drops := map[int]int64{}
	for i, it := range batch {
		if isRoot(it.sp) {
			drops[i] = q.reportDropped(it.org)
		}
	}
	delivered := false
	defer func() {
		if delivered {
			return
		}
		// The batch was not delivered: the roots in it did not tell their orgs what was lost, so the
		// next roots must.
		q.dmu.Lock()
		for i, n := range drops {
			q.droppedByOrg[batch[i].org] += n
		}
		q.dmu.Unlock()
	}()
	stop := func() {
		if q.isRetired() {
			q.removed(batch)
		} else {
			q.lose(batch, dropShutdown)
		}
	}
	for attempt := 1; ; attempt++ {
		if q.ctx.Err() != nil {
			stop()
			return
		}
		if wait := q.openUntil.Sub(q.r.now()); wait > 0 {
			// The breaker is open: this destination is paused. Its queue keeps filling and dropping
			// its oldest; nothing else waits.
			q.flushHealth(false)
			if q.r.sleep(q.ctx, wait) != nil {
				stop()
				return
			}
		}
		spans, route, gone, err := q.prepare(q.ctx, batch, drops)
		q.removed(gone)
		var (
			retryable  = true
			retryAfter time.Duration
			msg        string
			took       time.Duration
		)
		if err != nil {
			if q.ctx.Err() != nil {
				stop()
				return
			}
			msg = "the destination's settings could not be read"
		} else {
			if len(spans) == 0 {
				return
			}
			start := q.r.now()
			cctx, cancel := context.WithTimeout(q.ctx, cfg.ExportTimeout)
			res, xerr := q.r.sender.Export(cctx, route.Destination, buildRequest(route, spans))
			cancel()
			took = q.r.now().Sub(start)
			if xerr == nil {
				q.r.metrics.ExportTime.WithLabelValues(q.kind, q.backend, outcomeOK).Observe(took.Seconds())
				q.failures, q.openUntil = 0, time.Time{}
				q.errDirty, q.errWritten = false, ""
				ok := int64(len(spans)) - min(res.Rejected, int64(len(spans)))
				q.r.metrics.Exported.WithLabelValues(q.kind, q.backend).Add(float64(ok))
				if res.Rejected > 0 {
					q.r.metrics.Rejected.WithLabelValues(q.kind, q.backend, rejectPartial).Add(float64(res.Rejected))
					q.lostForHealth.Add(res.Rejected)
					q.chargeRejected(batch, res.Rejected)
				}
				delivered = true
				q.okDirty = true
				q.flushHealth(false)
				return
			}
			if q.ctx.Err() != nil {
				stop()
				return
			}
			var ee *export.Error
			msg = "the batch could not be sent"
			if errors.As(xerr, &ee) {
				retryable, retryAfter, msg = ee.Retryable(), ee.RetryAfter, ee.Message
			} else {
				retryable = false
			}
			outcome := outcomeRefused
			if retryable {
				outcome = outcomeRetryable
			}
			q.r.metrics.ExportTime.WithLabelValues(q.kind, q.backend, outcome).Observe(took.Seconds())
		}

		// A failure. The error is written to the destination's record once per episode, not on
		// every attempt: the same message again changes nothing the admin can see.
		q.failures++
		if msg != q.errWritten {
			q.errMsg, q.errDirty, q.errWritten = msg, true, msg
		}
		if q.failures >= cfg.BreakerThreshold {
			pause := cfg.BreakerBase << min(q.failures-cfg.BreakerThreshold, 16)
			q.openUntil = q.r.now().Add(min(pause, cfg.BreakerMax))
		}
		if !retryable {
			q.r.metrics.Rejected.WithLabelValues(q.kind, q.backend, rejectRefused).Add(float64(len(spans)))
			q.lose(nonGone(batch, gone), "")
			q.flushHealth(false)
			return
		}
		if attempt >= cfg.MaxAttempts {
			q.lose(nonGone(batch, gone), dropExportFailed)
			q.flushHealth(false)
			return
		}
		delay := max(min(cfg.BackoffBase<<min(attempt-1, 16), cfg.BackoffMax), retryAfter)
		q.flushHealth(false)
		if q.r.sleep(q.ctx, delay) != nil {
			stop()
			return
		}
	}
}

// chargeRejected shares n rejected spans among the orgs of a batch in proportion to their spans (the
// backend does not say which it refused), the remainder going to the first orgs, so koinon.spans_dropped
// tells each org about its own share.
func (q *queue) chargeRejected(batch []item, n int64) {
	var order []pgtype.UUID
	count := map[pgtype.UUID]int64{}
	for _, it := range batch {
		if count[it.org] == 0 {
			order = append(order, it.org)
		}
		count[it.org]++
	}
	total, given := int64(len(batch)), int64(0)
	share := map[pgtype.UUID]int64{}
	for _, o := range order {
		share[o] = n * count[o] / total
		given += share[o]
	}
	for i := 0; given < n; i++ {
		share[order[i%len(order)]]++
		given++
	}
	q.dmu.Lock()
	for o, v := range share {
		q.droppedByOrg[o] += v
	}
	q.dmu.Unlock()
}

// nonGone returns the items of batch that are not in gone.
func nonGone(batch, gone []item) []item {
	if len(gone) == 0 {
		return batch
	}
	skip := map[*tracepb.Span]bool{}
	for _, g := range gone {
		skip[g.sp] = true
	}
	out := make([]item, 0, len(batch))
	for _, it := range batch {
		if !skip[it.sp] {
			out = append(out, it)
		}
	}
	return out
}

// flushHealth writes the destination's health to Postgres: at most one write per HealthInterval, and
// an error only once per episode, so a busy or failing destination never turns into a stream of
// updates. A slow database delays only this destination's worker.
func (q *queue) flushHealth(force bool) {
	cfg := q.r.cfg
	lost := q.lostForHealth.Load()
	if !q.okDirty && !q.errDirty && lost == 0 {
		return
	}
	now := q.r.now()
	if !force && now.Sub(q.lastHealth) < cfg.HealthInterval {
		return
	}
	route, _ := q.snapshot()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(q.r.ctx), 3*time.Second)
	defer cancel()
	if q.okDirty {
		if err := q.r.health.Success(ctx, q.org, route); err != nil {
			q.r.log.Warn("recording a trace destination's health failed", "err", err)
		}
		q.okDirty = false
	}
	if q.errDirty || lost > 0 {
		msg := q.errMsg
		if msg == "" {
			msg = "spans were dropped"
		}
		if err := q.r.health.Failure(ctx, q.org, route, msg, lost); err != nil {
			q.r.log.Warn("recording a trace destination's health failed", "err", err)
		}
		q.lostForHealth.Add(-lost)
		q.errDirty = false
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
