// SPDX-License-Identifier: Apache-2.0

package durable

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Executor identity and dead-executor recovery (ADR 0085, decision 11).
//
// DBOS recovers a pending workflow at Launch, and only the workflows of its own executor ID. The ID
// of a koinon-api process is its pod name (the chart passes it from the downward API), and a pod
// name is never reused: the workflows a crashed or evicted pod left pending would never be recovered
// by the pod that replaces it. So every executor heartbeats into ExecutorsTable, and one executor at
// a time (the holder of a Postgres advisory lock) recovers the workflows of the executors whose
// heartbeat is stale.

// ExecutorsTable is the table of executor heartbeats, in Schema. Migrate creates it.
const ExecutorsTable = "koinon_executors"

// Defaults for the heartbeat and the recovery job. StaleAfter is long against HeartbeatInterval so
// that a slow heartbeat (a busy database, a GC pause) is not mistaken for a dead executor: a live
// executor that is recovered by mistake would have its workflows run twice.
const (
	DefaultHeartbeatInterval = 10 * time.Second
	DefaultStaleAfter        = 90 * time.Second
	DefaultRecoveryInterval  = 30 * time.Second
)

// recoveryLockKey is the Postgres advisory lock key of the recovery job (the bytes of "kndurrec").
const recoveryLockKey int64 = 0x6b6e647572726563

func executorsDDL() string {
	return fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.%s (
    executor_id         text PRIMARY KEY,
    application_version text NOT NULL,
    started_at          timestamptz NOT NULL DEFAULT now(),
    last_heartbeat      timestamptz NOT NULL DEFAULT now()
)`, pgx.Identifier{Schema}.Sanitize(), pgx.Identifier{ExecutorsTable}.Sanitize())
}

func executorsTable() string {
	return pgx.Identifier{Schema, ExecutorsTable}.Sanitize()
}

// heartbeat records that executor id, running version, is alive. The database clock is the only
// clock, so a skewed pod clock cannot make an executor look dead.
func heartbeat(ctx context.Context, pool *pgxpool.Pool, id, version string) error {
	_, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (executor_id, application_version) VALUES ($1, $2)
ON CONFLICT (executor_id) DO UPDATE SET application_version = EXCLUDED.application_version, last_heartbeat = now()`,
		executorsTable()), id, version)
	if err != nil {
		return fmt.Errorf("durable: heartbeat of executor %s: %w", id, err)
	}
	return nil
}

func (r *Runtime) heartbeatInterval() time.Duration {
	if r.cfg.HeartbeatInterval == 0 {
		return DefaultHeartbeatInterval
	}
	return r.cfg.HeartbeatInterval
}

func (r *Runtime) staleAfter() time.Duration {
	if r.cfg.ExecutorStaleAfter <= 0 {
		return DefaultStaleAfter
	}
	return r.cfg.ExecutorStaleAfter
}

func (r *Runtime) recoveryInterval() time.Duration {
	if r.cfg.RecoveryInterval == 0 {
		return DefaultRecoveryInterval
	}
	return r.cfg.RecoveryInterval
}

// ExecutorID is the ID this runtime runs under (DBOS's choice when Config.ExecutorID is empty), or
// the empty string before Launch.
func (r *Runtime) ExecutorID() string {
	dctx := r.Context()
	if dctx == nil {
		return ""
	}
	return dbos.GetExecutorID(dctx)
}

// heartbeatLoop beats every interval until ctx ends. A failed beat is logged; the next one retries.
func (r *Runtime) heartbeatLoop(ctx context.Context, pool *pgxpool.Pool, id string, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := heartbeat(ctx, pool, id, r.version()); err != nil && ctx.Err() == nil {
				r.cfg.Logger.Error("durable: executor heartbeat failed, retrying", "executor", id, "err", err)
			}
		}
	}
}

// recoveryLoop runs RecoverDeadExecutors every interval until ctx ends.
func (r *Runtime) recoveryLoop(ctx context.Context, pool *pgxpool.Pool, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := r.recoverDead(ctx, pool)
			if err != nil && ctx.Err() == nil {
				r.cfg.Logger.Error("durable: recovering the workflows of dead executors failed, retrying", "err", err)
			} else if n > 0 {
				r.cfg.Logger.Info("durable: recovered the pending workflows of dead executors", "executors", n)
			}
		}
	}
}

// RecoverDeadExecutors runs one pass of the recovery job: when this process holds the job's lock
// (one executor at a time does; the others skip the pass), it recovers the pending workflows of
// every other executor of this version whose heartbeat is older than the stale threshold, and
// forgets it. It returns how many executors it recovered. An executor of an older version is only
// forgotten: CarryOver forks its workflows. An executor of a newer version is left to a process of
// that version, which recovers it after a rollback forward.
func (r *Runtime) RecoverDeadExecutors(ctx context.Context) (int, error) {
	r.mu.Lock()
	pool := r.pool
	r.mu.Unlock()
	if pool == nil {
		return 0, errors.New("durable: RecoverDeadExecutors before Launch")
	}
	return r.recoverDead(ctx, pool)
}

func (r *Runtime) recoverDead(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	dctx := r.Context()
	if dctx == nil {
		return 0, errors.New("durable: RecoverDeadExecutors before Launch")
	}
	self := dbos.GetExecutorID(dctx)
	version := r.version()

	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("durable: begin recovery: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// The lock lives as long as the transaction: a leader that dies releases it with its connection.
	var leader bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, recoveryLockKey).Scan(&leader); err != nil {
		return 0, fmt.Errorf("durable: take the recovery lock: %w", err)
	}
	if !leader {
		return 0, nil
	}
	rows, err := tx.Query(ctx, fmt.Sprintf(`SELECT executor_id, application_version FROM %s
WHERE executor_id <> $1 AND last_heartbeat < now() - make_interval(secs => $2)
ORDER BY last_heartbeat FOR UPDATE`, executorsTable()), self, r.staleAfter().Seconds())
	if err != nil {
		return 0, fmt.Errorf("durable: list stale executors: %w", err)
	}
	var recover, forget []string
	for rows.Next() {
		var id, v string
		if err := rows.Scan(&id, &v); err != nil {
			rows.Close()
			return 0, err
		}
		switch {
		case v == version:
			recover = append(recover, id)
			forget = append(forget, id)
		case olderVersion(v, version):
			forget = append(forget, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("durable: list stale executors: %w", err)
	}
	// Workflows of an executor that has no row at all: the ones earlier builds left pending under
	// DBOS's default ID "local", or any ID that never beat. No heartbeat can ever mark them stale, so
	// they are recovered by their own age (a pod that has not yet written its first beat only has
	// young workflows). This runs only when the deployment uses per-pod IDs.
	if r.cfg.ExecutorID != "" {
		orphans, err := orphanExecutors(ctx, tx, self, version, r.staleAfter())
		if err != nil {
			return 0, err
		}
		recover = append(recover, orphans...)
	}
	if len(forget) == 0 && len(recover) == 0 {
		return 0, nil
	}
	if len(recover) > 0 {
		// Re-enqueues them on DBOS's internal queue, where this process runs them.
		rec, ok := dctx.(interface {
			RecoverPendingWorkflows([]string) ([]dbos.WorkflowHandle[any], error)
		})
		if !ok {
			return 0, errors.New("durable: this DBOS context cannot recover the workflows of other executors")
		}
		if _, err := rec.RecoverPendingWorkflows(recover); err != nil {
			return 0, fmt.Errorf("durable: recover the workflows of executors %v: %w", recover, err)
		}
	}
	if len(forget) > 0 {
		if _, err := tx.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE executor_id = ANY($1)`, executorsTable()), forget); err != nil {
			return 0, fmt.Errorf("durable: forget stale executors: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("durable: commit recovery: %w", err)
	}
	return len(recover), nil
}

// orphanExecutors lists the executor IDs that own pending workflows of version, have no row in
// ExecutorsTable, are not self, and whose workflows have not changed for staleAfter.
func orphanExecutors(ctx context.Context, tx pgx.Tx, self, version string, staleAfter time.Duration) ([]string, error) {
	rows, err := tx.Query(ctx, fmt.Sprintf(`SELECT DISTINCT w.executor_id FROM %s w
WHERE w.status = 'PENDING' AND w.application_version = $1 AND w.executor_id IS NOT NULL AND w.executor_id <> $2
  AND w.updated_at < (extract(epoch FROM now()) * 1000)::bigint - $3::bigint
  AND NOT EXISTS (SELECT 1 FROM %s e WHERE e.executor_id = w.executor_id)`,
		pgx.Identifier{Schema, "workflow_status"}.Sanitize(), executorsTable()), version, self, staleAfter.Milliseconds())
	if err != nil {
		return nil, fmt.Errorf("durable: list executors without a heartbeat row: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("durable: list executors without a heartbeat row: %w", err)
	}
	return ids, nil
}

// startExecutorLoops writes the first heartbeat and starts the heartbeat and recovery loops.
// It runs after DBOS launched, so the executor ID is final.
func (r *Runtime) startExecutorLoops(ctx context.Context) error {
	pool, err := pgxpool.New(ctx, r.cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("durable: open the executor pool: %w", err)
	}
	id := r.ExecutorID()
	if r.cfg.Migrate {
		// Local and test runs let DBOS create its own tables; the heartbeat table is ours.
		if _, err := pool.Exec(ctx, executorsDDL()); err != nil {
			pool.Close()
			return fmt.Errorf("durable: create the executor heartbeat table: %w", err)
		}
	}
	if err := heartbeat(ctx, pool, id, r.version()); err != nil {
		pool.Close()
		return err
	}
	r.mu.Lock()
	r.pool = pool
	r.mu.Unlock()
	if iv := r.heartbeatInterval(); iv > 0 {
		r.loop.Add(1)
		go func() { defer r.loop.Done(); r.heartbeatLoop(ctx, pool, id, iv) }()
	}
	if iv := r.recoveryInterval(); iv > 0 {
		r.loop.Add(1)
		go func() { defer r.loop.Done(); r.recoveryLoop(ctx, pool, iv) }()
	}
	return nil
}
