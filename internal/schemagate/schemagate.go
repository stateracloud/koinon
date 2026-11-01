// SPDX-License-Identifier: Apache-2.0

// Package schemagate keeps a binary not Ready while the database schema is older than the binary
// needs or declares (schema_compat.min_reader_version) that only a newer release can read it
// (ADR 0085, item 8).
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
	"github.com/jackc/pgx/v5/pgconn"
)

// undefinedTable is the SQLSTATE of a query on a table that does not exist.
const undefinedTable = "42P01"

// DefaultInterval is how often the gate re-reads the schema version.
const DefaultInterval = 15 * time.Second

// Querier is what the gate needs of a pgx pool.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Range is what a build requires: Min is the oldest goose version its code works against and
// Latest the newest migration it embeds, the highest min_reader_version it can read.
type Range struct{ Min, Latest int64 }

// Schema is what the database says about itself: the highest applied goose version and the oldest
// release that can still read it (table schema_compat, written by migrations).
type Schema struct{ Version, MinReader int64 }

// Read reads the schema. A database that goose never touched reads as version 0: the migration
// Job has not run. A database from before schema_compat existed reads as MinReader 0.
func Read(ctx context.Context, q Querier) (Schema, error) {
	var s Schema
	if err := q.QueryRow(ctx, `SELECT COALESCE(MAX(version_id), 0) FROM goose_db_version WHERE is_applied`).Scan(&s.Version); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == undefinedTable {
			return Schema{}, nil // goose never ran: no goose_db_version table yet
		}
		return Schema{}, fmt.Errorf("read the schema version: %w", err)
	}
	var has bool
	if err := q.QueryRow(ctx, `SELECT to_regclass('schema_compat') IS NOT NULL`).Scan(&has); err != nil {
		return Schema{}, fmt.Errorf("read the schema compatibility: %w", err)
	}
	if !has {
		return s, nil
	}
	if err := q.QueryRow(ctx, `SELECT COALESCE(MAX(min_reader_version), 0) FROM schema_compat`).Scan(&s.MinReader); err != nil {
		return Schema{}, fmt.Errorf("read the schema compatibility: %w", err)
	}
	return s, nil
}

// Judge says whether this build can run against s; the error is the sentence the log line and the
// probe carry.
func Judge(s Schema, r Range) error {
	switch {
	case s.Version < r.Min:
		return fmt.Errorf("the database schema is at version %d, older than the %d this build needs: the migration Job has not run or the database was restored from an older backup; run koinon-api migrate", s.Version, r.Min)
	case s.MinReader > r.Latest:
		return fmt.Errorf("the database schema is at version %d and needs a release that knows migration %d or newer, but this build knows up to %d: this is an older release than the one that migrated it; upgrade the app, or restore a backup taken before the upgrade", s.Version, s.MinReader, r.Latest)
	}
	return nil
}

// Gate holds the latest verdict.
type Gate struct {
	read     func(context.Context) (Schema, error)
	rng      Range
	log      *slog.Logger
	interval time.Duration

	mu      sync.Mutex
	checked bool
	err     error
}

// New returns a gate over a database connection, not Ready until its first check passes.
func New(q Querier, r Range, log *slog.Logger) *Gate {
	return NewFunc(func(ctx context.Context) (Schema, error) { return Read(ctx, q) }, r, log)
}

// NewFunc is New over any reader of the schema version.
func NewFunc(read func(context.Context) (Schema, error), r Range, log *slog.Logger) *Gate {
	if log == nil {
		log = slog.Default()
	}
	return &Gate{read: read, rng: r, log: log, interval: DefaultInterval}
}

// Check reads the version once, records the verdict and logs it when it changes.
func (g *Gate) Check(ctx context.Context) error {
	sc, err := g.read(ctx)
	if err == nil {
		err = Judge(sc, g.rng)
	}
	g.mu.Lock()
	changed := !g.checked || (g.err == nil) != (err == nil) || (err != nil && g.err.Error() != err.Error())
	g.checked, g.err = true, err
	g.mu.Unlock()
	switch {
	case err != nil && changed:
		g.log.Error("not ready: the schema is not supported by this build", "supported_min", g.rng.Min, "latest", g.rng.Latest, "err", err)
	case err == nil && changed:
		g.log.Info("schema supported", "version", sc.Version, "min_reader", sc.MinReader, "supported_min", g.rng.Min, "latest", g.rng.Latest)
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
