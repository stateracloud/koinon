// SPDX-License-Identifier: Apache-2.0

// Package durable runs DBOS Transact for Go inside koinon-api (ADR 0066): schedules, durable timers,
// event processing with dedupe and retries, and per-org queues. DBOS keeps its state in its own
// schema ("dbos") of Koinon's Postgres. It acts on Koinon only by calling Koinon's own operations;
// the run queue, claims and leases stay in Koinon's tables (ADR 0030).
//
// The DBOS system tables are created by the migration Job, never on start (ADR 0060): Migrate runs
// from "koinon-api migrate" with the migration role, and Runtime launches with SkipMigrations, so
// it only verifies the schema and needs DML, not DDL.
package durable

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Schema is the Postgres schema that holds the DBOS system tables.
const Schema = "dbos"

// AppRole is the NOLOGIN role that holds the components' DML (ADR 0003, the chart's CNPG bootstrap).
const AppRole = "koinon_app"

// AppName names the DBOS application. Workflows, schedules and queues are owned by this name.
const AppName = "koinon"

// Migrate creates or upgrades the DBOS system tables in Schema, with the migration role's
// credentials in dsn, and grants the component role AppRole access when that role exists (it does
// in the chart's CNPG mode; in external mode the operator's own role setup applies, as for
// Koinon's goose tables). It is idempotent.
func Migrate(ctx context.Context, dsn string, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	if err := dbos.Migrate(ctx, dsn, dbos.WithMigrateSchema(Schema), dbos.WithMigrateLogger(logger)); err != nil {
		return fmt.Errorf("migrate the DBOS system tables: %w", err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect to grant the DBOS schema: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	// Koinon's own table of executor heartbeats lives beside DBOS's (ADR 0085, decision 11).
	if _, err := conn.Exec(ctx, executorsDDL()); err != nil {
		return fmt.Errorf("create the executor heartbeat table: %w", err)
	}
	var exists bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, AppRole).Scan(&exists); err != nil {
		return fmt.Errorf("look up role %s: %w", AppRole, err)
	}
	if !exists {
		return nil
	}
	for _, stmt := range appGrants() {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("grant the DBOS schema to %s: %w", AppRole, err)
		}
	}
	return nil
}

// appGrants is the component role's access to the DBOS schema: DML only, the same shape as the
// chart's CNPG bootstrap (ADR 0060). dbos.PermissionStatements grants ALL, which adds TRUNCATE,
// TRIGGER and REFERENCES, so it is not used.
func appGrants() []string {
	schema := pgx.Identifier{Schema}.Sanitize()
	role := pgx.Identifier{AppRole}.Sanitize()
	return []string{
		fmt.Sprintf("GRANT USAGE ON SCHEMA %s TO %s", schema, role),
		fmt.Sprintf("GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA %s TO %s", schema, role),
		fmt.Sprintf("GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA %s TO %s", schema, role),
		fmt.Sprintf("GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA %s TO %s", schema, role),
		fmt.Sprintf("ALTER DEFAULT PRIVILEGES IN SCHEMA %s GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO %s", schema, role),
		fmt.Sprintf("ALTER DEFAULT PRIVILEGES IN SCHEMA %s GRANT USAGE, SELECT ON SEQUENCES TO %s", schema, role),
		fmt.Sprintf("ALTER DEFAULT PRIVILEGES IN SCHEMA %s GRANT EXECUTE ON FUNCTIONS TO %s", schema, role),
	}
}

// Config configures a Runtime.
type Config struct {
	// DatabaseURL is Koinon's Postgres. DBOS opens its own small pool from it.
	DatabaseURL string
	// Migrate makes Launch create and migrate the DBOS tables itself. The chart never sets it: the
	// migration Job does that (ADR 0060). It exists for tests and local runs without a Job.
	Migrate bool
	// ApplicationVersion tags workflows, and only a process with the same version recovers them.
	// Empty means WorkflowVersion, which koinon-api uses (ADR 0085): never the release version, which
	// would orphan the pending workflows at every upgrade. Tests set their own.
	ApplicationVersion string
	// CarryOverInterval is the time between sweeps for pending workflows of another version; zero is
	// DefaultCarryOverInterval and a negative value turns the periodic sweep off (the one at Launch
	// still runs).
	CarryOverInterval time.Duration
	// ExecutorID names this process to DBOS and in the heartbeat table: koinon-api passes its pod
	// name (ADR 0085). Empty lets DBOS choose (DBOS__VMID, else "local"), which is only right for
	// a single process.
	ExecutorID string
	// HeartbeatInterval is the time between executor heartbeats; zero is DefaultHeartbeatInterval
	// and a negative value stops the beat after the first one (tests).
	HeartbeatInterval time.Duration
	// ExecutorStaleAfter is how old a heartbeat must be for the executor to count as dead; zero is
	// DefaultStaleAfter.
	ExecutorStaleAfter time.Duration
	// RecoveryInterval is the time between passes of the dead-executor recovery job; zero is
	// DefaultRecoveryInterval and a negative value turns the periodic pass off.
	RecoveryInterval time.Duration
	// SchedulerPollingInterval is how often DBOS picks up created or changed schedules; zero is
	// DBOS's default of 30 seconds.
	SchedulerPollingInterval time.Duration
	Logger                   *slog.Logger
}

// A Registrar registers workflows and queues on a DBOS context before it launches. Later plan items
// (schedule and event triggers, notifications) each provide one.
type Registrar func(dbos.Context) error

// Runtime is a DBOS runtime that is not yet launched, then launched, then stopped.
type Runtime struct {
	cfg        Config
	registrars []Registrar

	mu      sync.Mutex
	dctx    dbos.Context
	stopped bool
	cancel  context.CancelFunc // ends the carry-over, heartbeat and recovery loops
	pool    *pgxpool.Pool      // the heartbeat and recovery job's own connections
	loop    sync.WaitGroup
}

// New returns a Runtime. Add registrars with Register, then call Launch.
func New(cfg Config) *Runtime {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Runtime{cfg: cfg}
}

// Register adds r, to be called in order by Launch. It returns an error after Launch.
func (r *Runtime) Register(reg Registrar) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dctx != nil {
		return errors.New("durable: Register after Launch")
	}
	r.registrars = append(r.registrars, reg)
	return nil
}

// Launch creates the DBOS context, runs the registrars, and starts DBOS: queue runners, the
// scheduler and recovery of workflows left pending by a previous process with the same version.
// ctx bounds the lifetime of the DBOS context; Shutdown stops it sooner.
func (r *Runtime) Launch(ctx context.Context) error {
	if err := r.launch(ctx); err != nil {
		return err
	}
	// Workflows left by a process of another version (an earlier WorkflowVersion, or the release
	// version older koinon-api builds used) are carried over from step 0. A failed sweep is logged
	// and retried; it does not stop the start.
	loopCtx, cancel := context.WithCancel(ctx)
	r.mu.Lock()
	r.cancel = cancel
	r.mu.Unlock()
	if err := r.startExecutorLoops(loopCtx); err != nil {
		_ = r.Shutdown(time.Second)
		return err
	}
	r.sweep(loopCtx)
	if iv := r.cfg.CarryOverInterval; iv >= 0 {
		if iv == 0 {
			iv = DefaultCarryOverInterval
		}
		r.loop.Add(1)
		go func() {
			defer r.loop.Done()
			r.carryOverLoop(loopCtx, iv)
		}()
	}
	return nil
}

func (r *Runtime) launch(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dctx != nil || r.stopped {
		return errors.New("durable: already launched")
	}
	dctx, err := dbos.NewContext(ctx, dbos.Config{
		AppName:            AppName,
		DatabaseURL:        r.cfg.DatabaseURL,
		DatabaseSchema:     Schema,
		SkipMigrations:     !r.cfg.Migrate,
		ApplicationVersion: r.version(),
		ExecutorID:         r.cfg.ExecutorID,

		SchedulerPollingInterval: r.cfg.SchedulerPollingInterval,
		Logger:                   r.cfg.Logger,
	})
	if err != nil {
		return fmt.Errorf("durable: create the DBOS context: %w", err)
	}
	for _, reg := range r.registrars {
		if err := reg(dctx); err != nil {
			_ = dbos.Shutdown(dctx, time.Second)
			return fmt.Errorf("durable: register: %w", err)
		}
	}
	if err := dbos.Launch(dctx); err != nil {
		_ = dbos.Shutdown(dctx, time.Second)
		return fmt.Errorf("durable: launch: %w", err)
	}
	r.dctx = dctx
	return nil
}

// Context returns the launched DBOS context, or nil before Launch.
func (r *Runtime) Context() dbos.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dctx
}

// ApplySchedules creates or replaces the named schedules (dbos.ApplySchedules) on the launched
// runtime. The workflows they name must have been registered by a Registrar.
func (r *Runtime) ApplySchedules(specs ...dbos.ScheduleSpec) error {
	dctx := r.Context()
	if dctx == nil {
		return errors.New("durable: ApplySchedules before Launch")
	}
	return dbos.ApplySchedules(dctx, specs)
}

// Shutdown stops DBOS, waiting up to timeout for running workflows to reach a stopping point.
// Workflows still pending are recovered by the next process. It is safe to call twice or before Launch.
func (r *Runtime) Shutdown(timeout time.Duration) error {
	r.mu.Lock()
	r.stopped = true
	dctx, cancel, pool := r.dctx, r.cancel, r.pool
	r.dctx, r.cancel, r.pool = nil, nil, nil
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	r.loop.Wait() // a sweep in flight reads Context, so the lock is not held here
	if pool != nil {
		pool.Close()
	}
	if dctx == nil {
		return nil
	}
	return dbos.Shutdown(dctx, timeout)
}
