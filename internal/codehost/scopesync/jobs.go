// SPDX-License-Identifier: Apache-2.0

package scopesync

import (
	"context"
	"log/slog"
	"sync"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/safego"
)

// jobKey names one connection.
type jobKey struct{ org, id [16]byte }

// jobs is the queue behind Syncer.Request: it runs the code-host scope readings, at most one per
// connection, and drain waits for the running ones at shutdown. Readings asked for by a request (a
// Test, a create, a re-key) and by the periodic refresh (Syncer.Run) both go through it, so an
// on-change reading and a scheduled one never list one connection at the same time.
//
// A request that arrives while its connection is being read does not start a second reading: it
// leaves its row and actor as the one pending request, replacing an older pending one (except that a
// person's pending request is never replaced by the periodic refresh's), and the
// running goroutine reads once more when it finishes. So a burst of Tests lists the host at most
// twice, and a re-key that lands mid-reading is still read with the new secret afterwards.
type jobs struct {
	log *slog.Logger
	// run reads one connection into the scope; ctx is canceled by Drain once its deadline passes.
	run func(ctx context.Context, row store.Connection, actor audit.Actor)

	// ctx is the parent of every reading. It is detached from any request, canceled by Drain.
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	closed  bool
	wg      sync.WaitGroup
	running map[jobKey]*jobRequest // a key is present while its goroutine runs; the value is the pending request, if any
}

// jobRequest is one reading to do.
type jobRequest struct {
	row   store.Connection
	actor audit.Actor
	// requestID is the ID of the request that queued the reading ("" for the periodic refresh). The
	// reading runs on jobs' own context, so the ID that audit stamps on its event is carried here.
	requestID string
	// done are closed when a reading that covers this request has finished (or the request was dropped).
	done []chan struct{}
}

func newJobs(log *slog.Logger, run func(ctx context.Context, row store.Connection, actor audit.Actor)) *jobs {
	ctx, cancel := context.WithCancel(context.Background())
	return &jobs{log: log, run: run, ctx: ctx, cancel: cancel, running: map[jobKey]*jobRequest{}}
}

// start asks for a reading of row. It never blocks. The reading is detached from ctx's cancellation
// but carries its request ID, so the audit event has it; a request that replaces a pending one
// brings its own. The returned channel is closed once a reading that covers this request has
// finished. After Drain nothing starts and the channel is already closed: the process is stopping,
// and the periodic refresh reads the connection on the next start.
func (j *jobs) start(ctx context.Context, row store.Connection, actor audit.Actor) <-chan struct{} {
	key := jobKey{org: row.OrgID.Bytes, id: row.ID.Bytes}
	done := make(chan struct{})
	req := &jobRequest{row: row, actor: actor, requestID: audit.RequestID(ctx), done: []chan struct{}{done}}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		close(done)
		return done
	}
	if old, busy := j.running[key]; busy {
		if old != nil {
			if old.actor.Kind != audit.KindSystem && actor.Kind == audit.KindSystem {
				// A person's pending request wins over the periodic refresh's: its row may be newer
				// (a re-key stores a new secret and fingerprint), so keep it whole and only let the
				// refresh wait for the same reading.
				old.done = append(old.done, done)
				return done
			}
			req.done = append(old.done, req.done...)
		}
		j.running[key] = req
		return done
	}
	j.running[key] = nil
	j.wg.Add(1)
	safego.Go(j.ctx, j.log, "code-host-scope-sync", func(ctx context.Context) {
		defer j.wg.Done()
		j.loop(ctx, key, req)
	})
	return done
}

// loop reads first, then whatever request arrived meanwhile, until none is pending. The key is
// released even when a reading panics, so the connection is not stuck.
func (j *jobs) loop(ctx context.Context, key jobKey, req *jobRequest) {
	for req != nil {
		rctx := ctx
		if req.requestID != "" {
			rctx = audit.WithRequestID(ctx, req.requestID)
		}
		safego.Do(j.log, "code-host-scope-sync", func() { j.run(rctx, req.row, req.actor) })
		closeAll(req.done)
		j.mu.Lock()
		req = j.running[key]
		if req == nil || ctx.Err() != nil {
			delete(j.running, key)
			j.mu.Unlock()
			if req != nil {
				closeAll(req.done)
			}
			return
		}
		j.running[key] = nil
		j.mu.Unlock()
	}
}

func closeAll(done []chan struct{}) {
	for _, d := range done {
		close(d)
	}
}

// drain stops new readings and waits for the running ones. A reading still running when ctx ends is
// canceled and waited for, so no reading outlives drain.
func (j *jobs) drain(ctx context.Context) {
	j.mu.Lock()
	j.closed = true
	j.mu.Unlock()
	done := make(chan struct{})
	go func() {
		j.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		j.cancel()
		<-done
	}
	j.cancel()
}
