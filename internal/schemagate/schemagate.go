// SPDX-License-Identifier: Apache-2.0

// Package schemagate keeps a binary not Ready while the database schema is outside the goose
// version range the binary supports (ADR 0085, item 8).
//
// The migration Job runs before the new pods start, so a rolling upgrade always finds the schema
// at the new version; the gate matters when an app-only rollback leaves older pods on a newer
// schema, when a pod starts before the Job has finished, and when a database is restored from an
// older backup. Without it the pod passes its probes and fails at its first query.
package schemagate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// DefaultInterval is how often the gate re-reads the schema version.
const DefaultInterval = 15 * time.Second

// Querier is what the gate needs of a pgx pool.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Range is the inclusive range of supported goose versions.
type Range struct{ Min, Max int64 }

// Version reads the highest applied goose version. A database that goose never touched is an
// error: the migration Job has not run.
func Version(ctx context.Context, q Querier) (int64, error) {
	var v int64
	if err := q.QueryRow(ctx, `SELECT COALESCE(MAX(version_id), 0) FROM goose_db_version WHERE is_applied`).Scan(&v); err != nil {
		return 0, fmt.Errorf("read the schema version: %w", err)
	}
	return v, nil
}

// Judge says whether schema version v is inside r; the error is the sentence the log line and the
// probe carry.
func Judge(v int64, r Range) error {
	switch {
	case v < r.Min:
		return fmt.Errorf("the database schema is at version %d, older than the %d this build needs (supported %d to %d): the migration Job has not run or the database was restored from an older backup; run koinon-api migrate", v, r.Min, r.Min, r.Max)
	case v > r.Max:
		return fmt.Errorf("the database schema is at version %d, newer than the %d this build supports (supported %d to %d): this is an older release than the one that migrated it; upgrade the app, or restore a backup taken before the upgrade", v, r.Max, r.Min, r.Max)
	}
	return nil
}

// Gate holds the latest verdict.
type Gate struct {
	read     func(context.Context) (int64, error)
	rng      Range
	log      *slog.Logger
	interval time.Duration

	mu      sync.Mutex
	checked bool
	err     error
}

// New returns a gate over a database connection, not Ready until its first check passes.
func New(q Querier, r Range, log *slog.Logger) *Gate {
	return NewFunc(func(ctx context.Context) (int64, error) { return Version(ctx, q) }, r, log)
}

// NewFunc is New over any reader of the schema version.
func NewFunc(read func(context.Context) (int64, error), r Range, log *slog.Logger) *Gate {
	if log == nil {
		log = slog.Default()
	}
	return &Gate{read: read, rng: r, log: log, interval: DefaultInterval}
}

// Check reads the version once, records the verdict and logs it when it changes.
func (g *Gate) Check(ctx context.Context) error {
	v, err := g.read(ctx)
	if err == nil {
		err = Judge(v, g.rng)
	}
	g.mu.Lock()
	changed := !g.checked || (g.err == nil) != (err == nil) || (err != nil && g.err.Error() != err.Error())
	g.checked, g.err = true, err
	g.mu.Unlock()
	switch {
	case err != nil && changed:
		g.log.Error("not ready: schema version outside the supported range", "supported_min", g.rng.Min, "supported_max", g.rng.Max, "err", err)
	case err == nil && changed:
		g.log.Info("schema version supported", "version", v, "supported_min", g.rng.Min, "supported_max", g.rng.Max)
	}
	return err
}

// Run checks now and then every interval until ctx ends.
func (g *Gate) Run(ctx context.Context) {
	for {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_ = g.Check(cctx)
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-time.After(g.interval):
		}
	}
}

// Ready is nil once the latest check passed.
func (g *Gate) Ready() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.checked {
		return errors.New("the schema version has not been checked yet")
	}
	return g.err
}

// ReadyHandler answers 200 while Ready and 503 with the reason otherwise.
func (g *Gate) ReadyHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if err := g.Ready(); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}
