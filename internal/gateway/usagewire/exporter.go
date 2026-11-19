// SPDX-License-Identifier: Apache-2.0

package usagewire

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/agentgateway"
)

// ExporterOptions configures an [Exporter].
type ExporterOptions struct {
	// URL is the base URL of koinon-api's internal listener, the one that serves the OTLP receiver and
	// the State stream. The exporter posts to URL + agentgateway.LogsPath.
	URL string
	// Token returns the bearer token of that listener, read afresh for every export so a rotated
	// token takes effect without a restart.
	Token func() (string, error)
	// HTTPClient sends the exports. Nil means a client with a 10 s timeout.
	HTTPClient *http.Client
	// Logger gets the exporter's own log lines. They carry counts, never records.
	Logger *slog.Logger
	// MaxQueue bounds the records kept while the receiver is away. Zero means 10000. When it is full the
	// oldest record is dropped and counted: the gateway serves on, and a lost record is a lost charge,
	// so Dropped is a metric to alert on.
	MaxQueue int
	// BatchSize is the most records in one export. Zero means 256.
	BatchSize int
	// Interval is how often a partial batch is sent. Zero means 1 second.
	Interval time.Duration
}

// Exporter queues usage records and sends them to koinon-api as OTLP logs exports. Record never
// blocks the request path: it only appends to a bounded queue.
type Exporter struct {
	o ExporterOptions

	mu      sync.Mutex
	queue   []gateway.UsageRecord
	dropped int64
	sent    int64
	wake    chan struct{}
	done    chan struct{}
}

// NewExporter returns an Exporter; Run sends.
func NewExporter(o ExporterOptions) *Exporter {
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.MaxQueue <= 0 {
		o.MaxQueue = 10000
	}
	if o.BatchSize <= 0 {
		o.BatchSize = 256
	}
	if o.Interval <= 0 {
		o.Interval = time.Second
	}
	return &Exporter{o: o, wake: make(chan struct{}, 1), done: make(chan struct{})}
}

// Record queues r for export.
func (e *Exporter) Record(r gateway.UsageRecord) {
	e.mu.Lock()
	if len(e.queue) >= e.o.MaxQueue {
		e.queue = e.queue[1:]
		e.dropped++
	}
	e.queue = append(e.queue, r)
	full := len(e.queue) >= e.o.BatchSize
	e.mu.Unlock()
	if full {
		select {
		case e.wake <- struct{}{}:
		default:
		}
	}
}

// Dropped is how many records the full queue has dropped.
func (e *Exporter) Dropped() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.dropped
}

// Queued is how many records wait for export.
func (e *Exporter) Queued() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.queue)
}

// Run sends queued records until ctx ends, then makes a last attempt to send what is left (within
// flushTimeout) and returns. It backs off while the receiver fails and keeps the records.
func (e *Exporter) Run(ctx context.Context, flushTimeout time.Duration) {
	defer close(e.done)
	tick := time.NewTicker(e.o.Interval)
	defer tick.Stop()
	backoff := time.Duration(0)
	for {
		select {
		case <-ctx.Done():
			fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), flushTimeout)
			defer cancel()
			for e.Queued() > 0 {
				if err := e.flushOnce(fctx); err != nil {
					e.o.Logger.Warn("usage export: records lost at shutdown", "queued", e.Queued(), "err", err)
					return
				}
			}
			return
		case <-tick.C:
		case <-e.wake:
		}
		if backoff > 0 {
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				continue
			}
		}
		failed := false
		for e.Queued() > 0 && ctx.Err() == nil {
			if err := e.flushOnce(ctx); err != nil {
				e.o.Logger.Warn("usage export failed; records stay queued", "queued", e.Queued(), "err", err)
				failed = true
				break
			}
		}
		switch {
		case !failed:
			backoff = 0
		case backoff == 0:
			backoff = time.Second
		default:
			backoff = min(backoff*2, 30*time.Second)
		}
	}
}

// Done is closed once Run has returned.
func (e *Exporter) Done() <-chan struct{} { return e.done }

// flushOnce sends one batch and removes it from the queue when the receiver took it.
func (e *Exporter) flushOnce(ctx context.Context) error {
	e.mu.Lock()
	n := min(len(e.queue), e.o.BatchSize)
	batch := append([]gateway.UsageRecord(nil), e.queue[:n]...)
	e.mu.Unlock()
	if n == 0 {
		return nil
	}
	if err := e.post(ctx, batch); err != nil {
		return err
	}
	e.mu.Lock()
	// The queue only loses its head to the drop of a full queue, so the first n entries are the batch
	// unless that happened meanwhile; then fewer of them are still queued.
	e.queue = e.queue[min(n, len(e.queue)):]
	e.sent += int64(n)
	e.mu.Unlock()
	return nil
}

func (e *Exporter) post(ctx context.Context, batch []gateway.UsageRecord) error {
	token, err := e.o.Token()
	if err != nil {
		return fmt.Errorf("token: %w", err)
	}
	body, err := proto.Marshal(Encode(batch))
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.o.URL+agentgateway.LogsPath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := e.o.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	// 200 is taken, partial success included: a record the receiver rejected (an unknown agent) is
	// rejected again on a replay. 4xx but 429 is final for the batch too; the rest is retried.
	switch {
	case resp.StatusCode == http.StatusOK:
		return nil
	case resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusUnauthorized:
		e.o.Logger.Error("usage export refused; batch dropped", "status", resp.StatusCode, "records", len(batch))
		e.mu.Lock()
		e.dropped += int64(len(batch))
		e.mu.Unlock()
		return nil
	}
	return fmt.Errorf("receiver answered %d", resp.StatusCode)
}
