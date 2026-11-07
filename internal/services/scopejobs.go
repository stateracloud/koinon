// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"log/slog"
	"sync"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/safego"
)

// scopeKey names one connection.
type scopeKey struct{ org, id [16]byte }

// scopeJobs runs the background code-host scope readings (syncScope): at most one reading runs per
// connection, and Drain waits for the running ones at shutdown.
//
// A request that arrives while its connection is being read does not start a second reading: it
// leaves its row and actor as the one pending request, replacing an older pending one, and the
// running goroutine reads once more when it finishes. So a burst of Tests lists the host at most
// twice, and a re-key that lands mid-reading is still read with the new secret afterwards.
type scopeJobs struct {
	log *slog.Logger
	// run reads one connection into the scope; ctx is canceled by Drain once its deadline passes.
	run func(ctx context.Context, row store.Connection, actor audit.Actor)

	// ctx is the parent of every reading. It is detached from any request, canceled by Drain.
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	closed  bool
	wg      sync.WaitGroup
	running map[scopeKey]*scopeRequest // a key is present while its goroutine runs; the value is the pending request, if any
}

// scopeRequest is one reading to do.
type scopeRequest struct {
	row   store.Connection
	actor audit.Actor
}

func newScopeJobs(log *slog.Logger, run func(ctx context.Context, row store.Connection, actor audit.Actor)) *scopeJobs {
	ctx, cancel := context.WithCancel(context.Background())
	return &scopeJobs{log: log, run: run, ctx: ctx, cancel: cancel, running: map[scopeKey]*scopeRequest{}}
}

// start asks for a reading of row. It never blocks. After Drain it does nothing: the process is
// stopping, and the periodic refresh reads the connection on the next start.
func (j *scopeJobs) start(row store.Connection, actor audit.Actor) {
	key := scopeKey{org: row.OrgID.Bytes, id: row.ID.Bytes}
	req := &scopeRequest{row: row, actor: actor}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return
	}
	if _, busy := j.running[key]; busy {
		j.running[key] = req
		return
	}
	j.running[key] = nil
	j.wg.Add(1)
	safego.Go(j.ctx, j.log, "code-host-scope-sync", func(ctx context.Context) {
		defer j.wg.Done()
		j.loop(ctx, key, req)
	})
}

// loop reads first, then whatever request arrived meanwhile, until none is pending. The key is
// released even when a reading panics, so the connection is not stuck.
func (j *scopeJobs) loop(ctx context.Context, key scopeKey, req *scopeRequest) {
	for req != nil {
		safego.Do(j.log, "code-host-scope-sync", func() { j.run(ctx, req.row, req.actor) })
		j.mu.Lock()
		req = j.running[key]
		if req == nil || ctx.Err() != nil {
			delete(j.running, key)
			req = nil
		} else {
			j.running[key] = nil
		}
		j.mu.Unlock()
	}
}

// drain stops new readings and waits for the running ones. A reading still running when ctx ends is
// canceled and waited for, so no reading outlives drain.
func (j *scopeJobs) drain(ctx context.Context) {
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
