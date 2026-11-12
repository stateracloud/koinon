// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// waitFor polls cond for up to five seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestKeyedQueueNeverRunsOneKeyTwiceAtOnce(t *testing.T) {
	var (
		mu      sync.Mutex
		running = map[string]int{}
		overlap atomic.Bool
		ran     = map[string][]int{}
	)
	release := make(chan struct{})
	q := newKeyedQueue(t.Context(), 4, func(_ context.Context, k string, n int) {
		mu.Lock()
		running[k]++
		if running[k] > 1 {
			overlap.Store(true)
		}
		mu.Unlock()
		<-release
		mu.Lock()
		running[k]--
		ran[k] = append(ran[k], n)
		mu.Unlock()
	}, func(int) {})
	defer q.Stop()

	q.Add("a", 1)
	waitFor(t, "a to run", func() bool { mu.Lock(); defer mu.Unlock(); return running["a"] == 1 })
	// Changed while its worker runs: kept, requeued, not run in parallel. The newest wins.
	q.Add("a", 2)
	q.Add("a", 3)
	close(release)
	waitFor(t, "a to run again", func() bool { mu.Lock(); defer mu.Unlock(); return len(ran["a"]) == 2 })
	mu.Lock()
	defer mu.Unlock()
	if overlap.Load() {
		t.Error("one key ran twice at the same time")
	}
	if got := ran["a"]; got[0] != 1 || got[1] != 3 {
		t.Errorf("runs of a = %v, want 1 then 3 (2 was replaced)", got)
	}
}

func TestKeyedQueueSlowKeyDoesNotHoldBackOthers(t *testing.T) {
	block := make(chan struct{})
	var bDone atomic.Bool
	q := newKeyedQueue(t.Context(), 2, func(_ context.Context, k string, _ int) {
		if k == "slow" {
			<-block
			return
		}
		bDone.Store(true)
	}, func(int) {})
	defer q.Stop()
	defer close(block)

	q.Add("slow", 1)
	q.Add("b", 1)
	waitFor(t, "b to run while slow is open", bDone.Load)
}

func TestKeyedQueueBoundsWorkers(t *testing.T) {
	var cur, peak, started atomic.Int32
	release := make(chan struct{})
	q := newKeyedQueue(t.Context(), 3, func(_ context.Context, _ int, _ int) {
		n := cur.Add(1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		started.Add(1)
		<-release
		cur.Add(-1)
	}, func(int) {})
	defer q.Stop()

	for i := range 10 {
		q.Add(i, i)
	}
	waitFor(t, "three workers", func() bool { return started.Load() == 3 })
	time.Sleep(20 * time.Millisecond) // a fourth would have started by now
	if got := started.Load(); got != 3 {
		t.Errorf("started = %d, want 3 at a time", got)
	}
	close(release)
	waitFor(t, "all ten", func() bool { return started.Load() == 10 })
	if p := peak.Load(); p > 3 {
		t.Errorf("peak = %d, want at most 3", p)
	}
}

func TestKeyedQueueStopsWithItsContextAndDropsWhatIsLeft(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	var dropped, ran atomic.Int32
	entered := make(chan struct{})
	q := newKeyedQueue(ctx, 1, func(ctx context.Context, _ string, _ int) {
		ran.Add(1)
		close(entered)
		<-ctx.Done() // a running item sees the stop
	}, func(int) { dropped.Add(1) })

	q.Add("a", 1)
	<-entered
	q.Add("b", 2) // waits for the only worker
	q.Add("b", 3) // replaces it: one drop
	cancel()
	q.Stop()
	if ran.Load() != 1 {
		t.Errorf("ran = %d, want only the item that had started", ran.Load())
	}
	if dropped.Load() != 2 {
		t.Errorf("dropped = %d, want 2 (the replaced b and the b left at the stop)", dropped.Load())
	}
	q.Add("c", 4)
	if dropped.Load() != 3 {
		t.Error("an item added after the stop is dropped")
	}
}

func TestKeyedQueueDropsQueuedItemsOnCancelWithoutStop(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup // like a pass's batch: Done by run or by drop
	entered := make(chan struct{})
	q := newKeyedQueue(ctx, 1, func(ctx context.Context, _ string, _ int) {
		defer wg.Done()
		close(entered)
		<-ctx.Done()
	}, func(int) { wg.Done() })
	defer q.Stop()

	wg.Add(3)
	q.Add("a", 1)
	<-entered
	q.Add("b", 2)
	q.Add("c", 3)
	cancel()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }() // what ReconcileAll does before its deferred Stop
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("waiting for the items hangs after the queue's context ended")
	}
}
