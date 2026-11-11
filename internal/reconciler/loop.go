// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/safego"
)

const (
	// NotifyChannel is the channel the agents and runs triggers notify (migration 00006).
	NotifyChannel = "koinon_reconcile"
	// leaderLock is the advisory lock key held by the reconciling replica ("koinonrc").
	leaderLock int64 = 0x6b6f696e6f6e7263
	// debounce collects a burst of notifications (a claim touches agents and runs) into one
	// pass.
	debounce = 200 * time.Millisecond
	// passTimeout bounds one pass; Apply may wait for agentgateway's hot reload, which
	// follows the kubelet's ConfigMap sync (about a minute by default).
	passTimeout = 5 * time.Minute
	// unmarkedTimeout bounds the gateway count of unmarked objects, which runs after a pass and not
	// inside its timeout.
	unmarkedTimeout = time.Minute
)

// Run reconciles until ctx ends. Only the replica holding the leader lock reconciles; the
// others wait on the lock and take over as soon as the leader releases it (#573). A failed pass
// is logged and retried on the next wake-up; a failed lead (lost connection) is retried after
// ResyncInterval.
func (r *Reconciler) Run(ctx context.Context) error {
	defer r.plan.Close()
	safego.Go(ctx, r.log, "mcp-prober", func(ctx context.Context) { r.RunProber(ctx) })
	for {
		err := r.lead(ctx)
		if ctx.Err() != nil {
			return nil
		}
		r.log.Warn("reconcile loop restarting", "err", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(r.cfg.ResyncInterval):
		}
	}
}

// lead takes the leader lock and LISTENs on one dedicated connection, so that losing the
// connection releases the lock and stops the loop together. A replica that is not the leader
// blocks in pg_advisory_lock until the holder's session ends, so a rollout hands over at once
// instead of after a poll interval. The lock is a session lock: it needs the session-level
// connection that LISTEN needs anyway (a transaction-mode pooler cannot serve this process).
func (r *Reconciler) lead(ctx context.Context) error {
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection: %w", err)
	}
	defer closeConn(conn)

	r.log.Info("reconciler is waiting for the leader lock")
	start := time.Now()
	if err := lockBlocking(ctx, conn.Conn(), leaderLock); err != nil {
		return fmt.Errorf("take leader lock: %w", err)
	}
	if _, err := conn.Exec(ctx, "LISTEN "+NotifyChannel); err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	r.log.Info("reconciler is leading", "waited", time.Since(start).Round(time.Millisecond),
		"resync", r.cfg.ResyncInterval, "idle_timeout", r.cfg.IdleTimeout)

	for {
		pctx, cancel := context.WithTimeout(ctx, passTimeout)
		err := r.reconcilePass(pctx)
		cancel()
		r.observeUnmarkedDue(ctx)
		if err != nil && ctx.Err() == nil {
			r.log.Warn("reconcile pass incomplete", "err", err)
		}
		if err := r.wait(ctx, conn, r.cfg.ResyncInterval); err != nil {
			return err
		}
	}
}

// lockBlocking takes the session advisory lock key on c and returns when it is held, or when ctx
// ends. The query runs on a context that ctx does not cancel: cancelling a query context makes
// pgx close the socket, and a backend that waits for a lock does not notice a closed socket until
// it has the lock, so it would take the lock after the caller gave up. Instead a watcher sends the
// server a cancel request (repeated, because one sent before the backend runs the query is lost)
// and the query ends with SQLSTATE 57014 on a connection that stays in a clean state.
func lockBlocking(ctx context.Context, c *pgx.Conn, key int64) error {
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		select {
		case <-done:
			return
		case <-ctx.Done():
		}
		for {
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			_ = c.PgConn().CancelRequest(cctx)
			cancel()
			select {
			case <-done:
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	}()
	_, err := c.Exec(context.Background(), "SELECT pg_advisory_lock($1)", key)
	close(done)
	<-stopped
	if ctx.Err() != nil {
		// Cancelled, or granted at the moment of shutdown; the caller closes the connection.
		return ctx.Err()
	}
	return err
}

// wait returns after a notification (and a short debounce) or after d.
func (r *Reconciler) wait(ctx context.Context, conn *pgxpool.Conn, d time.Duration) error {
	wctx, cancel := context.WithTimeout(ctx, d)
	_, err := conn.Conn().WaitForNotification(wctx)
	cancel()
	switch {
	case err == nil:
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, context.DeadlineExceeded):
		return nil
	default:
		return fmt.Errorf("wait for notification: %w", err)
	}
	// Drain the rest of the burst.
	for {
		dctx, cancel := context.WithTimeout(ctx, debounce)
		_, err := conn.Conn().WaitForNotification(dctx)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			return fmt.Errorf("wait for notification: %w", err)
		}
	}
}

// closeConn closes the connection instead of returning it to the pool, so the advisory lock
// and the LISTEN registration end with it.
func closeConn(conn *pgxpool.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = conn.Conn().Close(ctx)
	conn.Release()
}
