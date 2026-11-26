// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/agentd"
	"github.com/stateracloud/koinon/internal/testing/racescale"
	"github.com/stateracloud/koinon/internal/testing/testenv"
)

// Sweeps that wait for an agent's sweep lock, or for a place to sweep, hold no pool connection,
// so other agentd work still gets one (old issue 1375).
var _ = Describe("sweep locks", func() {
	const (
		poolSize = 4
		sweeps   = 20
	)
	var (
		small *pgxpool.Pool
		locks *agentd.SweepLocks
	)

	BeforeEach(func(ctx SpecContext) {
		testenv.Require(testenv.Docker)
		cfg := db.Config().Copy()
		cfg.MaxConns = poolSize
		var err error
		small, err = pgxpool.NewWithConfig(ctx, cfg)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(small.Close)
		locks = agentd.NewSweepLocks(small)
	})

	newAgent := func() pgtype.UUID { return pgtype.UUID{Bytes: uuid.New(), Valid: true} }

	// otherWorkGetsAConnection checks that a query of other agentd work gets a connection at once
	// and that the sweeps hold at most half of the pool.
	otherWorkGetsAConnection := func(ctx context.Context, g Gomega) {
		qctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		var one int
		g.Expect(small.QueryRow(qctx, `SELECT 1`).Scan(&one)).To(Succeed())
		// A busy try rolls back before BeginFunc returns, so its connection is back in the pool by then.
		g.Expect(small.Stat().AcquiredConns()).To(BeNumerically("<=", poolSize/2))
	}

	// newRelease returns a channel the held sweeps wait on and a function that closes it once. A
	// failed spec closes it too, so no sweep outlives its spec.
	newRelease := func() (chan struct{}, func()) {
		ch := make(chan struct{})
		release := sync.OnceFunc(func() { close(ch) })
		DeferCleanup(release)
		return ch, release
	}

	It("lets many sweeps of one agent wait without connections, and runs them one at a time", func(ctx SpecContext) {
		agent := newAgent()
		held, release := newRelease()
		var inside, entered, maxInside atomic.Int32
		sweep := func(tx pgx.Tx) error {
			n := inside.Add(1)
			defer inside.Add(-1)
			for {
				m := maxInside.Load()
				if n <= m || maxInside.CompareAndSwap(m, n) {
					break
				}
			}
			entered.Add(1)
			<-held
			return nil
		}
		var wg sync.WaitGroup
		errs := make(chan error, sweeps)
		for range sweeps {
			wg.Go(func() { errs <- locks.With(ctx, agent, sweep) })
		}
		Eventually(entered.Load).Should(Equal(int32(1)))
		// The waiters try again and again; each try gives its connection back.
		Consistently(func(g Gomega) {
			otherWorkGetsAConnection(ctx, g)
			g.Expect(entered.Load()).To(Equal(int32(1)))
		}).WithTimeout(racescale.D(2 * time.Second)).WithPolling(100 * time.Millisecond).Should(Succeed())
		// A waiter does not keep a place to sweep either: a sweep of another agent runs now.
		other := make(chan error, 1)
		go func() { other <- locks.With(ctx, newAgent(), func(pgx.Tx) error { return nil }) }()
		Eventually(other).Should(Receive(BeNil()))

		release()
		wg.Wait()
		close(errs)
		for err := range errs {
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(entered.Load()).To(Equal(int32(sweeps)))
		Expect(maxInside.Load()).To(Equal(int32(1)))
	}, SpecTimeout(2*time.Minute))

	It("lets at most half of the pool hold sweeps of different agents", func(ctx SpecContext) {
		held, release := newRelease()
		var entered atomic.Int32
		sweep := func(tx pgx.Tx) error {
			entered.Add(1)
			<-held
			return nil
		}
		var wg sync.WaitGroup
		errs := make(chan error, sweeps)
		for range sweeps {
			agent := newAgent()
			wg.Go(func() { errs <- locks.With(ctx, agent, sweep) })
		}
		Eventually(entered.Load).Should(Equal(int32(poolSize / 2)))
		Consistently(func(g Gomega) {
			otherWorkGetsAConnection(ctx, g)
			g.Expect(entered.Load()).To(Equal(int32(poolSize / 2)))
		}).WithTimeout(racescale.D(time.Second)).WithPolling(100 * time.Millisecond).Should(Succeed())

		release()
		wg.Wait()
		close(errs)
		for err := range errs {
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(entered.Load()).To(Equal(int32(sweeps)))
	}, SpecTimeout(2*time.Minute))

	It("returns the context's error when the context ends while a sweep waits", func(ctx SpecContext) {
		agent := newAgent()
		wait, release := newRelease()
		held := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- locks.With(ctx, agent, func(pgx.Tx) error { close(held); <-wait; return nil })
		}()
		Eventually(held).Should(BeClosed())

		wctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer cancel()
		ran := false
		err := locks.With(wctx, agent, func(pgx.Tx) error { ran = true; return nil })
		Expect(err).To(MatchError(context.DeadlineExceeded))
		Expect(ran).To(BeFalse())

		release()
		Eventually(done).Should(Receive(BeNil()))
	}, SpecTimeout(time.Minute))

	It("sends the run back to the queue when the lock stays busy", func(ctx SpecContext) {
		locks.SetWait(500 * time.Millisecond)
		agent := newAgent()
		wait, release := newRelease()
		held := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- locks.With(ctx, agent, func(pgx.Tx) error { close(held); <-wait; return nil })
		}()
		Eventually(held).Should(BeClosed())

		ran := false
		err := locks.With(ctx, agent, func(pgx.Tx) error { ran = true; return nil })
		Expect(agentd.IsRequeued(err)).To(BeTrue(), "got %v", err)
		Expect(ran).To(BeFalse())
		// The waiter gave its place back: a sweep of another agent runs.
		Expect(locks.With(ctx, newAgent(), func(pgx.Tx) error { return nil })).To(Succeed())

		release()
		Eventually(done).Should(Receive(BeNil()))
	}, SpecTimeout(time.Minute))
})
