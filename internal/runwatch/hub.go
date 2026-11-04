// SPDX-License-Identifier: Apache-2.0

// Package runwatch wakes WatchRun streams when a run changes (ADR 0022).
//
// Each API replica holds one Postgres connection that LISTENs on
// koinon_run; triggers on runs and run_events send NOTIFY with
// "<org_id>/<run_id>" on commit, whichever process wrote (API, agentd). The
// Hub fans each notification out in process to the streams watching that
// run. A wake-up carries no data: streams re-read run_events after the last
// seq they sent, so lost or coalesced notifications never lose or duplicate
// events. (Live text is the exception: a notification may carry a batch of chunks
// of the text the agent is typing, chunks.go.) To bound the delay after a missed notification (for example while
// the listener reconnects), the Hub wakes every stream when it (re)connects,
// and streams also poll on their own at a slow interval.
package runwatch

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Channel is the NOTIFY channel (see migration 00005).
const Channel = "koinon_run"

// Hub fans run change notifications out to subscribers.
type Hub struct {
	connConfig *pgx.ConnConfig
	log        *slog.Logger

	mu   sync.Mutex
	subs map[string]map[*Subscription]struct{}

	listening chan struct{}
	once      sync.Once
}

// New returns a Hub that listens over a dedicated connection built from cfg.
// Call Run to start listening.
func New(cfg *pgx.ConnConfig, log *slog.Logger) *Hub {
	if log == nil {
		log = slog.Default()
	}
	return &Hub{
		connConfig: cfg.Copy(),
		log:        log,
		subs:       map[string]map[*Subscription]struct{}{},
		listening:  make(chan struct{}),
	}
}

// Subscription receives a wake-up on C whenever its run may have changed.
// Wake-ups coalesce: C has room for one. Chunks carries the run's live text
// (chunks.go); a subscriber that reads too slowly loses batches, never blocks
// the Hub.
type Subscription struct {
	C      <-chan struct{}
	Chunks <-chan []Chunk
	c      chan struct{}
	chunks chan []Chunk
	hub    *Hub
	key    string
}

// Subscribe starts waking the returned subscription for the run. Subscribe
// before reading the run's events, so no change between the read and the
// first wait is missed.
func (h *Hub) Subscribe(org, run pgtype.UUID) *Subscription {
	c := make(chan struct{}, 1)
	ch := make(chan []Chunk, chunkBacklog)
	s := &Subscription{C: c, c: c, Chunks: ch, chunks: ch, hub: h, key: Key(org, run)}
	h.mu.Lock()
	defer h.mu.Unlock()
	m := h.subs[s.key]
	if m == nil {
		m = map[*Subscription]struct{}{}
		h.subs[s.key] = m
	}
	m[s] = struct{}{}
	return s
}

// Close stops the subscription.
func (s *Subscription) Close() {
	h := s.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	if m := h.subs[s.key]; m != nil {
		delete(m, s)
		if len(m) == 0 {
			delete(h.subs, s.key)
		}
	}
}

func (s *Subscription) wake() {
	select {
	case s.c <- struct{}{}:
	default:
	}
}

// Key is the notification payload for a run.
func Key(org, run pgtype.UUID) string { return org.String() + "/" + run.String() }

// Notify wakes the local subscribers of a run, as a NOTIFY for it would.
func (h *Hub) Notify(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs[key] {
		s.wake()
	}
}

// chunkBacklog is how many batches of live chunks a subscription holds.
const chunkBacklog = 256

// NotifyChunks hands a batch of live chunks to the local subscribers of a run.
func (h *Hub) NotifyChunks(key string, cs []Chunk) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs[key] {
		select {
		case s.chunks <- cs:
		default:
		}
	}
}

func (h *Hub) wakeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range h.subs {
		for s := range m {
			s.wake()
		}
	}
}

// Subscribers returns the number of open subscriptions, for tests and
// metrics.
func (h *Hub) Subscribers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, m := range h.subs {
		n += len(m)
	}
	return n
}

// Listening is closed once the Hub has LISTENed for the first time.
func (h *Hub) Listening() <-chan struct{} { return h.listening }

// Run listens until ctx ends, reconnecting with backoff after errors.
func (h *Hub) Run(ctx context.Context) {
	backoff := 100 * time.Millisecond
	for ctx.Err() == nil {
		err := h.listen(ctx)
		if ctx.Err() != nil {
			return
		}
		h.log.Warn("run notification listener lost its connection; reconnecting", "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 5*time.Second)
	}
}

func (h *Hub) listen(ctx context.Context) error {
	conn, err := pgx.ConnectConfig(ctx, h.connConfig)
	if err != nil {
		return err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cancel()
		_ = conn.Close(closeCtx)
	}()
	if _, err := conn.Exec(ctx, "LISTEN "+Channel); err != nil {
		return err
	}
	h.once.Do(func() { close(h.listening) })
	// Changes made while no connection was listening produced no wake-ups.
	h.wakeAll()
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		if n.Channel != Channel {
			continue
		}
		key, body, ok := splitPayload(n.Payload)
		if !ok {
			continue
		}
		if body == "" {
			h.Notify(key)
			continue
		}
		if cs := parseChunks(body); len(cs) > 0 {
			h.NotifyChunks(key, cs)
		}
	}
}
