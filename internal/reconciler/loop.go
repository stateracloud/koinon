// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"errors"
	"fmt"
	"time"

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
// others retry every ResyncInterval. A failed pass is logged and retried on the next wake-up.
func (r *Reconciler) Run(ctx context.Context) error {
	defer r.plan.Close()
	safego.Go(ctx, r.log, "mcp-prober", func(ctx context.Context) { r.RunProber(ctx) })
	for {
		err := r.lead(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, errNotLeader) {
			r.log.Debug("another replica holds the reconciler lock")
		} else {
			r.log.Warn("reconcile loop restarting", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(r.cfg.ResyncInterval):
		}
	}
}

var errNotLeader = errors.New("reconciler: not the leader")

// lead takes the leader lock and LISTENs on one dedicated connection, so that losing the
// connection releases the lock and stops the loop together.
func (r *Reconciler) lead(ctx context.Context) error {
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection: %w", err)
	}
	defer closeConn(conn)

	var leader bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", leaderLock).Scan(&leader); err != nil {
		return fmt.Errorf("take leader lock: %w", err)
	}
	if !leader {
		return errNotLeader
	}
	if _, err := conn.Exec(ctx, "LISTEN "+NotifyChannel); err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	r.log.Info("reconciler is leading", "resync", r.cfg.ResyncInterval, "idle_timeout", r.cfg.IdleTimeout)

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
