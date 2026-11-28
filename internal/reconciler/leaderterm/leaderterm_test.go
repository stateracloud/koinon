// SPDX-License-Identifier: Apache-2.0

package leaderterm_test

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/reconciler/leaderterm"
)

var _ = Describe("the reconciler leader term (#635)", func() {
	var db leaderterm.DB

	BeforeEach(func(ctx SpecContext) {
		db = leaderterm.DB{Pool: pool}
		_, err := pool.Exec(ctx, "TRUNCATE leaderterm_spec")
		Expect(err).NotTo(HaveOccurred())
	})

	// take increments the term as a new leader does, on its own connection.
	take := func(ctx context.Context) int64 {
		GinkgoHelper()
		c, err := pool.Acquire(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer c.Release()
		term, err := leaderterm.Take(ctx, c.Conn())
		Expect(err).NotTo(HaveOccurred())
		return term
	}
	rows := func(ctx context.Context) int {
		GinkgoHelper()
		var n int
		Expect(pool.QueryRow(ctx, "SELECT count(*) FROM leaderterm_spec").Scan(&n)).To(Succeed())
		return n
	}
	// lostRecorder is the lost callback of a lead: it records each error the database refused.
	type lostRecorder struct {
		mu   sync.Mutex
		errs []error
	}
	record := func(l *lostRecorder) func(error) {
		return func(err error) { l.mu.Lock(); defer l.mu.Unlock(); l.errs = append(l.errs, err) }
	}

	It("gives each new leader a larger term", func(ctx SpecContext) {
		first := take(ctx)
		Expect(take(ctx)).To(Equal(first + 1))
	})

	It("lets the current leader write on every path", func(ctx SpecContext) {
		lost := &lostRecorder{}
		lctx := leaderterm.WithTerm(ctx, take(ctx), record(lost))

		_, err := db.Exec(lctx, "INSERT INTO leaderterm_spec (v) VALUES (1)")
		Expect(err).NotTo(HaveOccurred())
		var v int
		Expect(db.QueryRow(lctx, "INSERT INTO leaderterm_spec (v) VALUES (2) RETURNING v").Scan(&v)).To(Succeed())
		Expect(v).To(Equal(2))
		r, err := db.Query(lctx, "SELECT v FROM leaderterm_spec ORDER BY v")
		Expect(err).NotTo(HaveOccurred())
		got, err := pgx.CollectRows(r, pgx.RowTo[int])
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal([]int{1, 2}))
		tx, err := db.Begin(lctx)
		Expect(err).NotTo(HaveOccurred())
		_, err = tx.Exec(lctx, "INSERT INTO leaderterm_spec (v) VALUES (3)")
		Expect(err).NotTo(HaveOccurred())
		Expect(tx.Commit(lctx)).To(Succeed())

		Expect(rows(ctx)).To(Equal(3))
		Expect(lost.errs).To(BeEmpty())
	})

	It("refuses each write of an old leader once a newer leader took the term", func(ctx SpecContext) {
		lost := &lostRecorder{}
		old := leaderterm.WithTerm(ctx, take(ctx), record(lost))
		take(ctx) // the new leader

		_, err := db.Exec(old, "INSERT INTO leaderterm_spec (v) VALUES (1)")
		Expect(err).To(MatchError(leaderterm.ErrStale))
		err = db.QueryRow(old, "INSERT INTO leaderterm_spec (v) VALUES (2) RETURNING v").Scan(new(int))
		Expect(err).To(MatchError(leaderterm.ErrStale))
		_, err = db.Query(old, "INSERT INTO leaderterm_spec (v) VALUES (3) RETURNING v")
		Expect(err).To(MatchError(leaderterm.ErrStale))
		_, err = db.Begin(old)
		Expect(err).To(MatchError(leaderterm.ErrStale))
		_, err = leaderterm.TxDB{DB: pool}.Begin(old)
		Expect(err).To(MatchError(leaderterm.ErrStale))

		Expect(rows(ctx)).To(BeZero(), "no write of the old term landed")
		Expect(lost.errs).To(HaveLen(5), "the old leader is told at each refusal")
		for _, e := range lost.errs {
			Expect(e).To(MatchError(leaderterm.ErrStale))
		}
	})

	It("does not let a write of the old term commit after the newer term", func(ctx SpecContext) {
		old := leaderterm.WithTerm(ctx, take(ctx), nil)
		tx, err := db.Begin(old)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = tx.Rollback(context.Background()) })
		_, err = tx.Exec(old, "INSERT INTO leaderterm_spec (v) VALUES (1)")
		Expect(err).NotTo(HaveOccurred())

		// The new leader takes the term while the old transaction is open: it must wait for it.
		taken := make(chan int64, 1)
		go func() {
			defer GinkgoRecover()
			taken <- take(context.Background())
		}()
		Consistently(taken, 300*time.Millisecond).ShouldNot(Receive(), "the new term waits for the open write of the old term")
		Expect(tx.Commit(old)).To(Succeed())
		Eventually(taken).Should(Receive())

		Expect(rows(ctx)).To(Equal(1), "the write committed before the new term")
		_, err = db.Exec(old, "INSERT INTO leaderterm_spec (v) VALUES (2)")
		Expect(err).To(MatchError(leaderterm.ErrStale), "and the old term is refused after it")
		Expect(rows(ctx)).To(Equal(1))
	})

	It("refuses a write of the old term that starts while the newer term waits", func(ctx SpecContext) {
		old := leaderterm.WithTerm(ctx, take(ctx), nil)
		tx, err := db.Begin(old)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = tx.Rollback(context.Background()) })

		taken := make(chan int64, 1)
		go func() {
			defer GinkgoRecover()
			taken <- take(context.Background())
		}()
		Consistently(taken, 200*time.Millisecond).ShouldNot(Receive(), "the new term waits for the open transaction")

		// A second write of the old term queues behind the waiting take, not in front of it.
		wrote := make(chan error, 1)
		go func() {
			_, err := db.Exec(old, "INSERT INTO leaderterm_spec (v) VALUES (1)")
			wrote <- err
		}()
		Consistently(wrote, 200*time.Millisecond).ShouldNot(Receive(), "the write waits behind the take")

		Expect(tx.Commit(old)).To(Succeed())
		Eventually(taken).Should(Receive())
		var werr error
		Eventually(wrote).Should(Receive(&werr))
		Expect(werr).To(MatchError(leaderterm.ErrStale), "the write sees the newer term")
		Expect(rows(ctx)).To(BeZero())
	})

	It("reports an error of the implicit commit through the rows of a query", func(ctx SpecContext) {
		_, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS leaderterm_deferred (
			v int UNIQUE DEFERRABLE INITIALLY DEFERRED)`)
		Expect(err).NotTo(HaveOccurred())
		lctx := leaderterm.WithTerm(ctx, take(ctx), nil)
		// Two equal rows break the deferred unique check at commit, after the rows were returned.
		r, err := db.Query(lctx, "INSERT INTO leaderterm_deferred (v) VALUES (7), (7) RETURNING v")
		Expect(err).NotTo(HaveOccurred())
		_, err = pgx.CollectRows(r, pgx.RowTo[int])
		Expect(err).To(HaveOccurred(), "the failed commit is not lost")
		var n int
		Expect(pool.QueryRow(ctx, "SELECT count(*) FROM leaderterm_deferred").Scan(&n)).To(Succeed())
		Expect(n).To(BeZero())
	})

	It("bounds how long a transaction of a lead may be idle, so a vanished leader cannot hold the next one", func(ctx SpecContext) {
		lctx := leaderterm.WithTerm(ctx, take(ctx), nil)
		tx, err := db.Begin(lctx)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = tx.Rollback(context.Background()) })
		var v string
		Expect(tx.QueryRow(lctx, "SHOW idle_in_transaction_session_timeout").Scan(&v)).To(Succeed())
		Expect(v).To(Equal("10s"))
	})

	It("refuses a transaction that is not READ COMMITTED, where the check could read an old term", func(ctx SpecContext) {
		term := take(ctx)
		tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = tx.Rollback(context.Background()) })
		_, err = tx.Exec(ctx, "SELECT reconciler_check_term($1)", term)
		Expect(err).To(MatchError(ContainSubstring("needs READ COMMITTED")))
	})

	It("passes the statements of a context without a term through unchanged", func(ctx SpecContext) {
		take(ctx)
		_, err := db.Exec(ctx, "INSERT INTO leaderterm_spec (v) VALUES (1)")
		Expect(err).NotTo(HaveOccurred())
		Expect(rows(ctx)).To(Equal(1))
		_, ok := leaderterm.From(ctx)
		Expect(ok).To(BeFalse())
	})
})
