// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"sync"
)

// keyedQueue runs work items with a bounded set of workers and these rules (issue 573, ADR 0066
// "Per-agent work queue"):
//
//   - One key is never worked on twice at once.
//   - An item added for a key that is queued replaces the queued one (the newer item holds the
//     newer state); the replaced item is dropped.
//   - An item added for a key whose worker is running is kept and runs after that worker ends
//     (requeued, never in parallel).
//   - At most `workers` keys run at the same time; a slow item holds up its own key and one worker,
//     never the other keys.
//
// It is the per-key dedup of the Kubernetes work queue (dirty set, processing set) with the item
// kept in place of a bare key, no rate limit, and a replaced item reported through drop. Workers
// start on demand and end when the queue is empty.
type keyedQueue[K comparable, T any] struct {
	run  func(ctx context.Context, key K, item T)
	drop func(item T) // an item that will never run: replaced, or left when the queue stops

	ctx     context.Context
	cancel  context.CancelFunc
	workers int

	mu         sync.Mutex
	pending    map[K]T // the newest item per key not yet started
	order      []K     // keys with a pending item that no worker holds, oldest first
	processing map[K]bool
	active     int // workers started
	stopped    bool
	wg         sync.WaitGroup
}

// newKeyedQueue returns a queue whose workers stop with parent. workers < 1 is 1.
func newKeyedQueue[K comparable, T any](parent context.Context, workers int, run func(context.Context, K, T), drop func(T)) *keyedQueue[K, T] {
	ctx, cancel := context.WithCancel(parent) //nolint:gosec // cancel is kept in the queue and called by Stop
	return &keyedQueue[K, T]{
		run: run, drop: drop, ctx: ctx, cancel: cancel, workers: max(workers, 1),
		pending: map[K]T{}, processing: map[K]bool{},
	}
}

// Add queues item for key. After Stop, or when the queue's context has ended, the item is dropped.
func (q *keyedQueue[K, T]) Add(key K, item T) {
	q.mu.Lock()
	if q.stopped || q.ctx.Err() != nil {
		q.mu.Unlock()
		q.drop(item)
		return
	}
	old, had := q.pending[key]
	q.pending[key] = item
	if !had && !q.processing[key] {
		q.order = append(q.order, key)
	}
	start := q.active < q.workers && len(q.order) > 0
	if start {
		q.active++
		q.wg.Add(1)
	}
	q.mu.Unlock()
	if had {
		q.drop(old)
	}
	if start {
		go q.worker()
	}
}

// Busy says whether an item for key is running now.
func (q *keyedQueue[K, T]) Busy(key K) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.processing[key]
}

// worker takes keys until none is ready or the queue stops.
func (q *keyedQueue[K, T]) worker() {
	defer q.wg.Done()
	for {
		q.mu.Lock()
		if q.stopped || q.ctx.Err() != nil {
			// The queue ended: nothing queued will run, so drop it now. Waiting for Stop would hang
			// a caller that waits for the items before it stops the queue (ReconcileAll).
			left := q.pending
			q.pending, q.order = map[K]T{}, nil
			q.active--
			q.mu.Unlock()
			for _, it := range left {
				q.drop(it)
			}
			return
		}
		if len(q.order) == 0 {
			q.active--
			q.mu.Unlock()
			return
		}
		key := q.order[0]
		q.order = q.order[1:]
		item := q.pending[key]
		delete(q.pending, key)
		q.processing[key] = true
		q.mu.Unlock()

		q.run(q.ctx, key, item)

		q.mu.Lock()
		delete(q.processing, key)
		if _, again := q.pending[key]; again {
			q.order = append(q.order, key) // changed while it ran
		}
		q.mu.Unlock()
	}
}

// Stop cancels the workers' context, waits for the running items and drops the queued ones.
func (q *keyedQueue[K, T]) Stop() {
	q.mu.Lock()
	q.stopped = true
	q.mu.Unlock()
	q.cancel()
	q.wg.Wait()
	q.mu.Lock()
	left := q.pending
	q.pending, q.order = map[K]T{}, nil
	q.mu.Unlock()
	for _, it := range left {
		q.drop(it)
	}
}
