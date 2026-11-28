// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/reconciler/leaderterm"
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
	// follows the kubelet's ConfigMap sync (about a minute by default). It does not bound the
	// sandbox work of the pass, which has its own timeout per agent (Config.WorkTimeout).
	passTimeout = 5 * time.Minute
)

// Run reconciles until ctx ends. Only the replica holding the leader lock reconciles; the
// others wait on the lock and take over as soon as the leader releases it (#573). A failed pass
// is logged and retried on the next wake-up. A lead that ends in an error (a lost or refused
// connection) is retried after a short backoff with jitter (retryDelay), not after
// ResyncInterval, so a database failover leaves no long gap without a leader.
func (r *Reconciler) Run(ctx context.Context) error {
	defer r.Close()
	safego.Go(ctx, r.log, "mcp-prober", func(ctx context.Context) { r.RunProber(ctx) })
	failures := 0
	for {
		led, err := r.lead(ctx)
		if ctx.Err() != nil {
			return nil
		}
		failures = nextFailures(failures, led)
		delay := retryDelay(failures, rand.Float64()) //nolint:gosec // jitter, not a secret
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

// nextFailures is the consecutive-failure count after a lead ended in an error. A lead that
// reached the leading state ran passes, so its failure counts as the first; a lead that failed
// before that (acquire, lock, LISTEN) adds to the count, so a repeating setup error backs off.
func nextFailures(failures int, led bool) int {
	if led {
		return 1
	}
	return failures + 1
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
// so the old pass must not go on. led reports whether the replica reached the leading state (it
// ran passes), which resets the retry backoff.
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
	// The term fences this lead's database writes: once a newer leader takes it, the database refuses
	// every statement of this lead (issue 635).
	term, err := leaderterm.Take(ctx, conn.Conn())
	if err != nil {
		return false, err
	}
	if _, err := conn.Exec(ctx, "LISTEN "+NotifyChannel); err != nil {
		return false, fmt.Errorf("listen: %w", err)
	}
	// The gateway asks the prober to sync one server now (#1173). Only the leader probes, so only the
	// leader listens.
	if _, err := conn.Exec(ctx, "LISTEN "+mcpservers.ProbeWakeChannel); err != nil {
		return false, fmt.Errorf("listen: %w", err)
	}
	r.log.Info("reconciler is leading", "waited", time.Since(start).Round(time.Millisecond),
		"resync", r.cfg.ResyncInterval, "idle_timeout", r.cfg.IdleTimeout, "term", term)
	// Registered first, so it runs last: after the work queue stops, no front check adds a mark.
	defer r.endLeadFleet()

	lctx, cancel := context.WithCancelCause(ctx)
	// A refused term ends the whole lead, as a lost lock connection does: a newer leader works now.
	lctx = leaderterm.WithTerm(lctx, term, cancel)
	wake := make(chan struct{}, 1)
	readerDone := make(chan struct{})
	// alive is the time of the last proof that the lock connection works: a notification or the
	// answer to a heartbeat. The fence (gateway.WithFence) refuses a destructive step once it is older
	// than the detection bound, even if the reader itself is stuck (issue 635).
	var alive atomic.Int64
	alive.Store(time.Now().UnixNano())
	bound := r.cfg.LeaderHeartbeat + r.cfg.LeaderPingTimeout
	fence := func() error {
		if age := time.Since(time.Unix(0, alive.Load())); age > bound {
			return fmt.Errorf("reconciler: the leader lock connection was last confirmed %s ago", age.Round(time.Millisecond))
		}
		return nil
	}
	go func() {
		defer close(readerDone)
		for {
			n, err := r.waitOrPing(lctx, conn.Conn(), &alive)
			if err != nil {
				if lctx.Err() == nil {
					cancel(err)
				}
				return
			}
			if n.Channel == mcpservers.ProbeWakeChannel {
				// A prober wake, not a change to reconcile: no pass for it.
				r.wakeProbe(lctx, n.Payload)
				continue
			}
			select {
			case wake <- struct{}{}:
			default:
			}
		}
	}()
	// The reader must be gone before the connection is closed.
	defer func() { cancel(nil); <-readerDone }()

	// The sandbox work of every pass goes to one queue that lives as long as the lead: a pass ends
	// when its own steps end, not when the slowest sandbox call of any agent returns (issue 573). It
	// stops with lctx, which also ends when the connection (and with it the lock) is lost.
	queue := r.newWorkQueue(lctx)
	defer queue.Stop()
	fctx := gateway.WithFence(lctx, fence)
	// The remote agent health check runs while this replica leads, so one replica reads each card.
	safego.Go(lctx, r.log, "remote-agent-checker", func(ctx context.Context) { r.RunRemoteAgentChecker(ctx) })

	for {
		pctx, pcancel := context.WithTimeout(fctx, passTimeout)
		_, err := r.reconcilePass(pctx, queue)
		pcancel()
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

// waitOrPing returns the next notification. While the connection is quiet it checks it every
// LeaderHeartbeat with a query that must be answered within LeaderPingTimeout, and returns an error
// when it is not. A peer that vanished without a TCP reset (hard node loss, partition, a proxy that
// dropped the flow) gives no socket error; TCP keepalive would see it only after about 2.5 minutes,
// and not at all through a proxy that answers the probes itself. Postgres frees the lock earlier
// on its side, so this bound is the time two reconcilers can lead (issue 635). A read timeout does
// not close a pgx connection, so the wait can be repeated on the same connection.
func (r *Reconciler) waitOrPing(ctx context.Context, c *pgx.Conn, alive *atomic.Int64) (*pgconn.Notification, error) {
	for {
		wctx, wcancel := context.WithTimeout(ctx, r.cfg.LeaderHeartbeat)
		n, err := c.WaitForNotification(wctx)
		wcancel()
		switch {
		case err == nil:
			// Shows which NOTIFY starts the next pass. The payload is an org ID or empty, so it is
			// safe to log. Keep it so for every NOTIFY on NotifyChannel.
			r.log.Debug("reconcile wake-up", "channel", n.Channel, "payload", n.Payload)
			alive.Store(time.Now().UnixNano())
			return n, nil
		case ctx.Err() != nil:
			return nil, ctx.Err()
		case !errors.Is(err, context.DeadlineExceeded):
			return nil, fmt.Errorf("wait for notification: %w", err)
		}
		pctx, pcancel := context.WithTimeout(ctx, r.cfg.LeaderPingTimeout)
		_, err = c.Exec(pctx, "SELECT 1")
		pcancel()
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("leader lock connection did not answer within %s: %w", r.cfg.LeaderPingTimeout, err)
		}
		alive.Store(time.Now().UnixNano())
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
