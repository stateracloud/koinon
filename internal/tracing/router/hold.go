// SPDX-License-Identifier: Apache-2.0

package router

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// heldRoot is a turn's root span waiting for the ledger.
//
// The totals belong on the root (ADR 0074, decision 3), and the ledger's rows for a turn arrive up
// to LedgerWait after it ends, so the router holds the root, not the other spans: tool spans and the
// like go out at once, and only one small span per open turn waits. Memory is bounded twice, by a
// count and by bytes (a root can carry content up to agentd's pre-cap); a root that does not fit is
// released at once with the ledger as it stands. A child summary span would have avoided holding
// anything, but then no backend would show the cost on the turn itself.
type heldRoot struct {
	span  *tracepb.Span
	org   pgtype.UUID
	run   pgtype.UUID
	trace [16]byte
	turn  Turn
	since time.Time
	size  int
	// seen is the number of ledger rows the last poll found; the root is released when two polls in
	// a row agree and none of the rows is pending.
	seen   int
	polled bool
	reread bool
}

// hold queues a root for its ledger rows, or releases it at once when the bounds are reached. The
// turn row is read again when the root is first polled (rereadTurn), not taken from the attribution
// cache, which may have seen the turn open; that read is off the intake worker. A root whose trace is already held or finished is a duplicate (agentd retried an export) and
// is dropped: the chat spans were or will be made once.
func (r *Router) hold(ctx context.Context, h *heldRoot) {
	h.size = estimateSize(h.span)
	r.hmu.Lock()
	if exp, claimed := r.claimed[h.trace]; claimed && r.now().Before(exp) {
		r.hmu.Unlock()
		r.metrics.Discarded.WithLabelValues(discardDuplicate).Inc()
		return
	}
	// Claimed in the same critical section as the check, before the root is held or released, so two
	// roots of one trace arriving together cannot both make the model calls.
	r.claimed[h.trace] = r.now().Add(r.cfg.LedgerWait + r.cfg.FinishedTTL)
	if len(r.held) < r.cfg.MaxHeld && r.heldBytes+h.size <= r.cfg.MaxHeldBytes {
		r.held[h.trace] = h
		r.heldBytes += h.size
		r.metrics.HeldRoots.Set(float64(len(r.held)))
		r.hmu.Unlock()
		return
	}
	r.hmu.Unlock()
	// Release now: one read of the ledger, no waiting.
	r.release(ctx, h)
}

// Sweep reads the ledger for every held root and releases those that are settled or have waited
// LedgerWait. Reads run in parallel (bounded) with a timeout each, so one slow read cannot hold the
// others past their 30 seconds. The router calls it every PollInterval; tests call it with a chosen
// time.
func (r *Router) Sweep(ctx context.Context, now time.Time) {
	r.hmu.Lock()
	hs := make([]*heldRoot, 0, len(r.held))
	for _, h := range r.held {
		hs = append(hs, h)
	}
	r.hmu.Unlock()
	sem := make(chan struct{}, r.cfg.SweepParallel)
	var wg sync.WaitGroup
	for _, h := range hs {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			r.sweepOne(ctx, h, now)
		}()
	}
	wg.Wait()
}

func (r *Router) sweepOne(ctx context.Context, h *heldRoot, now time.Time) {
	r.rereadTurn(ctx, h)
	cctx, cancel := context.WithTimeout(ctx, r.cfg.SweepTimeout)
	calls, err := r.modelCalls(cctx, h)
	cancel()
	overdue := now.Sub(h.since) >= r.cfg.LedgerWait
	// Settled: two polls agree, none is pending, and the window has closed (rows are logged when the
	// request completes, up to TurnGrace after the turn ended).
	settled := err == nil && h.polled && len(calls) == h.seen && !anyPending(calls) && !now.Before(r.windowEnd(h))
	if err == nil {
		h.seen, h.polled = len(calls), true
	}
	if !settled && !overdue {
		return
	}
	r.hmu.Lock()
	cur, ok := r.held[h.trace]
	if ok && cur == h {
		delete(r.held, h.trace)
		r.heldBytes -= h.size
		r.metrics.HeldRoots.Set(float64(len(r.held)))
	}
	r.hmu.Unlock()
	if !ok || cur != h {
		return
	}
	if err != nil {
		// The ledger could not be read: send the root as it is, without totals, rather than lose it.
		r.log.Warn("trace router: reading the ledger failed; sending the turn without model calls", "err", err)
		calls = nil
	}
	r.finish(ctx, h, calls)
}

func anyPending(calls []ModelCall) bool {
	for _, c := range calls {
		if c.pending() {
			return true
		}
	}
	return false
}

// release finishes a root that was not held.
func (r *Router) release(ctx context.Context, h *heldRoot) {
	r.rereadTurn(ctx, h)
	cctx, cancel := context.WithTimeout(ctx, r.cfg.SweepTimeout)
	defer cancel()
	calls, err := r.modelCalls(cctx, h)
	if err != nil {
		r.log.Warn("trace router: reading the ledger failed; sending the turn without model calls", "err", err)
		calls = nil
	}
	r.finish(ctx, h, calls)
}

// rereadTurn reads the turn row once more, with a deadline; if that fails the cached row stands.
func (r *Router) rereadTurn(ctx context.Context, h *heldRoot) {
	if h.reread {
		return
	}
	h.reread = true
	cctx, cancel := context.WithTimeout(ctx, r.cfg.SweepTimeout)
	defer cancel()
	if turn, err := r.store.Attribute(cctx, h.org, h.run, h.trace); err == nil {
		h.turn = turn
	}
}

// modelCalls reads the turn's ledger rows: from the turn's start to its end plus TurnGrace (a request
// is logged when it completes, which can be just after the turn ended), half-open, and never into the
// next turn. While the row is still open the window ends with the root span.
func (r *Router) modelCalls(ctx context.Context, h *heldRoot) ([]ModelCall, error) {
	return r.store.ModelCalls(ctx, h.org, h.run, h.turn.Seq, h.turn.StartedAt, r.windowEnd(h))
}

// windowEnd is where the turn's ledger window ends: the turn's end, or while its row is still open the
// end of the root span, plus TurnGrace.
func (r *Router) windowEnd(h *heldRoot) time.Time {
	end := time.Unix(0, int64(h.span.EndTimeUnixNano)) //nolint:gosec // a span end time, far below the int64 limit
	if h.turn.EndedAt != nil {
		end = *h.turn.EndedAt
	}
	return end.Add(r.cfg.TurnGrace)
}

// finish builds the model-call spans, puts the totals on the root and queues them all.
func (r *Router) finish(ctx context.Context, h *heldRoot, calls []ModelCall) {
	spans, use := modelCallSpans(h.span, h.trace, h.turn.Seq, calls)
	r.metrics.Received.WithLabelValues(SourceLedger).Add(float64(len(spans)))
	putTotals(h.span, use)
	r.hmu.Lock()
	r.claimed[h.trace] = r.now().Add(r.cfg.FinishedTTL)
	r.hmu.Unlock()
	r.dispatch(ctx, h.org, append(spans, h.span)...)
}
