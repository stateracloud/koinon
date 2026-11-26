// SPDX-License-Identifier: Apache-2.0

package agentd

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The waits between two tries of a busy sweep lock: they double from the first to the last.
const (
	sweepLockFirstWait = 100 * time.Millisecond
	sweepLockMaxWait   = 2 * time.Second
	// sweepLockWait bounds the wait for other sweeps of the same agent. A holder keeps the lock
	// for one sweep (sweepTimeout) and a read of its run. A try-lock has no queue, so a waiter can
	// lose to other waiters; after this wait the run goes back to the queue (requeued) and starts
	// again later, rather than wait for ever.
	sweepLockWait = 3 * sweepTimeout
)

// errSweepBusy is the try of a sweep lock that another sweep of the same agent holds.
var errSweepBusy = errors.New("another sweep of the agent holds the lock")

// sweepLocks serialises the sweeps of one agent without a connection held while a sweep waits
// (old issue 1375). A sweep holds a transaction, and with it a pool connection, for as long as it
// runs: the per-agent advisory lock is a transaction lock, so it goes with the transaction, also
// when the worker dies. Two rules keep those sweeps from draining the pool:
//
//   - A sweep takes the lock with pg_try_advisory_xact_lock. When another sweep of the agent holds
//     it, the transaction ends at once, the connection goes back to the pool, and the sweep tries
//     again after a wait. A blocking pg_advisory_xact_lock would keep the connection for the whole
//     wait (up to a sweep, about 30 s).
//   - At most holders sweeps of the replica hold a transaction at a time, fewer than the pool's
//     connections. A sweep waits for a place without a connection. Each worker sweeps once at a
//     time, but the replica has more workers (default 8) than connections (default max(4, CPUs)),
//     for example after a restart with many parked runs.
type sweepLocks struct {
	db *pgxpool.Pool
	// holders has one token per sweep that may hold a transaction.
	holders chan struct{}
	// wait is the longest wait for a busy lock: sweepLockWait (specs set a shorter one).
	wait time.Duration
}

// newSweepLocks lets half of the pool's connections, and at least one, hold sweeps.
func newSweepLocks(db *pgxpool.Pool) *sweepLocks {
	n := max(1, int(db.Config().MaxConns)/2)
	return &sweepLocks{db: db, holders: make(chan struct{}, n), wait: sweepLockWait}
}

// with runs fn in a transaction that holds agent's sweep lock. When ctx ends, it returns an error
// that wraps ctx's error. When the lock stays busy for l.wait, it returns a requeued error and fn
// does not run.
func (l *sweepLocks) with(ctx context.Context, agent pgtype.UUID, fn func(pgx.Tx) error) error {
	deadline := time.Now().Add(l.wait)
	wait := sweepLockFirstWait
	for {
		err := l.try(ctx, agent, fn)
		if !errors.Is(err, errSweepBusy) {
			return err
		}
		if !time.Now().Before(deadline) {
			return &requeued{reason: "end the processes left in the sandbox: the agent's sweep lock stayed busy for " + l.wait.String()}
		}
		// Jitter, so the waiters of one agent do not try at the same moment.
		t := time.NewTimer(wait/2 + rand.N(wait/2+1)) //nolint:gosec // jitter, not a secret
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
		wait = min(2*wait, sweepLockMaxWait)
	}
}

// try takes a holder token, then tries the lock once. It returns errSweepBusy without fn run when
// another sweep holds the lock.
func (l *sweepLocks) try(ctx context.Context, agent pgtype.UUID, fn func(pgx.Tx) error) error {
	select {
	case l.holders <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-l.holders }()
	return pgx.BeginFunc(ctx, l.db, func(tx pgx.Tx) error {
		var got bool
		if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended('agentd.sweep:' || $1::uuid::text, 0))`, agent).Scan(&got); err != nil {
			return err
		}
		if !got {
			return errSweepBusy
		}
		return fn(tx)
	})
}
