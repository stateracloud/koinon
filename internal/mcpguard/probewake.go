// SPDX-License-Identifier: Apache-2.0

package mcpguard

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/mcpservers"
)

// ProbeWakeEvery is the shortest time between two wakes the gateway sends for one server (#1173). A
// list that keeps showing the same unrecorded change (every agent lists, the prober is slow) sends one
// wake per server in this time, not one per list.
const ProbeWakeEvery = 10 * time.Second

// probeWaker sends the prober a wake for one server, at most once per ProbeWakeEvery per server. It
// keeps no state beyond the time of the last wake of each server it woke.
type probeWaker struct {
	mu   sync.Mutex
	last map[pgtype.UUID]time.Time
	now  func() time.Time
}

// due reports whether a wake for server may go now, and records it.
func (w *probeWaker) due(server pgtype.UUID) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	if t, ok := w.last[server]; ok && now.Sub(t) < ProbeWakeEvery {
		return false
	}
	if w.last == nil {
		w.last = map[pgtype.UUID]time.Time{}
	}
	// Drop entries that can no longer hold a wake back, so the map stays as small as the set of
	// servers that changed lately.
	for k, t := range w.last {
		if now.Sub(t) >= ProbeWakeEvery {
			delete(w.last, k)
		}
	}
	w.last[server] = now
	return true
}

// forget gives back the slot due took for a wake that was not sent, so the next call can try again.
func (w *probeWaker) forget(server pgtype.UUID) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.last, server)
}

// wakeProber asks the reconciler's prober to sync the server now. Best effort: the list decision does
// not depend on it, and the next probe interval syncs the server anyway.
func (g *Guard) wakeProber(ctx context.Context, org, server pgtype.UUID) {
	if !g.wake.due(server) {
		return
	}
	err := g.q.NotifyMCPProbe(ctx, store.NotifyMCPProbeParams{
		Channel: mcpservers.ProbeWakeChannel, Payload: mcpservers.ProbeWakePayload(org, server),
	})
	if err != nil {
		g.wake.forget(server)
		g.log.Warn("mcp guard: waking the prober failed; the next probe syncs the server", "err", err)
	}
}
