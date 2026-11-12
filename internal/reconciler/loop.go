// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"fmt"
	"math/rand/v2"
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
// is logged and retried on the next wake-up. A lead that ends in an error (a lost or refused
// connection) is retried after a short backoff with jitter (retryDelay), not after
// ResyncInterval, so a database failover leaves no long gap without a leader.
func (r *Reconciler) Run(ctx context.Context) error {
	defer r.plan.Close()
	safego.Go(ctx, r.log, "mcp-prober", func(ctx context.Context) { r.RunProber(ctx) })
	failures := 0
	for {
		led, err := r.lead(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if led {
			failures = 0
		}
		failures++
		delay := retryDelay(failures, rand.Float64())
		r.log.Warn("reconcile loop restarting", "err", err, "retry_in", delay)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
	}
}

const (
	retryBase = time.Second
	retryCap  = 8 * time.Second
)

// retryDelay is the wait before the failures-th consecutive failed lead is retried: retryBase
// doubled per failure up to retryCap, plus jitter of up to the same again (jitter is in [0, 1)).
// The first retry waits 1 to 2 s.
func retryDelay(failures int, jitter float64) time.Duration {
	d := retryBase
	for i := 1; i < failures && d < retryCap; i++ {
		d *= 2
	}
	d = min(d, retryCap)
	return d + time.Duration(jitter*float64(d))
}

// lead takes the leader lock and LISTENs on one dedicated connection, so that losing the
// connection releases the lock and stops the loop together. A replica that is not the leader
// blocks in pg_advisory_lock until the holder's session ends, so a rollout hands over at once
// instead of after a poll interval. The lock is a session lock: it needs the session-level
// connection that LISTEN needs anyway (a transaction-mode pooler cannot serve this process).
//
// While leading, one goroutine reads the connection (notifications; pgx connections are not safe
// for concurrent use). If the connection fails it cancels the lead context, which cancels a pass
// that is running: Postgres has released the lock by then and the next leader may start at once,
// so the old pass must not go on. led reports whether the lock was taken.
func (r *Reconciler) lead(ctx context.Context) (led bool, _ error) {
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return false, fmt.Errorf("acquire connection: %w", err)
	}
	defer closeConn(conn)

	r.log.Info("reconciler is waiting for the leader lock")
	start := time.Now()
	if err := lockBlocking(ctx, conn.Conn(), leaderLock); err != nil {
		return false, fmt.Errorf("take leader lock: %w", err)
	}
	if _, err := conn.Exec(ctx, "LISTEN "+NotifyChannel); err != nil {
		return true, fmt.Errorf("listen: %w", err)
	}
	r.log.Info("reconciler is leading", "waited", time.Since(start).Round(time.Millisecond),
		"resync", r.cfg.ResyncInterval, "idle_timeout", r.cfg.IdleTimeout)

	lctx, cancel := context.WithCancelCause(ctx)
	wake := make(chan struct{}, 1)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			if _, err := conn.Conn().WaitForNotification(lctx); err != nil {
				if lctx.Err() == nil {
					cancel(fmt.Errorf("wait for notification: %w", err))
				}
				return
			}
			select {
			case wake <- struct{}{}:
			default:
			}
		}
	}()
	// The reader must be gone before the connection is closed.
	defer func() { cancel(nil); <-readerDone }()

	for {
		pctx, pcancel := context.WithTimeout(lctx, passTimeout)
		err := r.reconcilePass(pctx)
		pcancel()
		r.observeUnmarkedDue(lctx)
		if err != nil && lctx.Err() == nil {
			r.log.Warn("reconcile pass incomplete", "err", err)
		}
		if err := r.sleep(lctx, r.cfg.ResyncInterval, wake); err != nil {
			if cause := context.Cause(lctx); ctx.Err() == nil && cause != nil {
				return true, cause
			}
			return true, err
		}
	}
}

// lockBlocking takes the session advisory lock key on c and returns when it is held, or when ctx
// ends. When ctx ends, pgx abandons the query, closes the connection and (asyncClose, bounded to
// 15 s, in the background) sends the server a cancel request, so the backend does not stay queued
// and take the lock after the caller gave up. The connection is closed in every error case; the
// caller closes it too (closeConn). A cancelled context returns at once, even when the server
// cannot be reached.
func lockBlocking(ctx context.Context, c *pgx.Conn, key int64) error {
	_, err := c.Exec(ctx, "SELECT pg_advisory_lock($1)", key)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// sleep returns nil after a notification on wake (and a short debounce) or after d, and
// ctx's error when ctx ends.
func (r *Reconciler) sleep(ctx context.Context, d time.Duration, wake <-chan struct{}) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	case <-wake:
	}
	// Drain the rest of the burst.
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wake:
		case <-time.After(debounce):
			return nil
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
