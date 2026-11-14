// SPDX-License-Identifier: Apache-2.0

package scheduler_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/runlock"
	"github.com/stateracloud/koinon/internal/runoutcome"
	"github.com/stateracloud/koinon/internal/scheduler"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

// ago is a pointer to a duration, for the table of wait cases.
func ago(d time.Duration) *time.Duration { return &d }

var (
	pool *pgxpool.Pool
	q    *store.Queries
)

func TestScheduler(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Scheduler Suite")
}

var _ = BeforeSuite(func(ctx context.Context) {
	pool = pgtest.Start(ctx)
	q = store.New(pool)
})

type world struct {
	org, user, agent pgtype.UUID
	task             store.Task
}

// newWorld is an org with an active agent and a task of it that has no run.
func newWorld(ctx context.Context) *world {
	GinkgoHelper()
	w := &world{}
	w.org, w.user, _ = pgtest.NewOrg(ctx, pool)
	a, err := q.CreateAgent(ctx, store.CreateAgentParams{OrgID: w.org, DisplayName: pgtest.Uniq("agent"), Harness: harness.OpenCode, CreatedBy: w.user})
	Expect(err).NotTo(HaveOccurred())
	v, err := q.CreateAgentVersion(ctx, store.CreateAgentVersionParams{OrgID: w.org, AgentID: a.ID, Config: []byte(`{"model":"fake/m","instructions":"x"}`), CreatedBy: w.user})
	Expect(err).NotTo(HaveOccurred())
	_, err = q.ActivateAgent(ctx, store.ActivateAgentParams{OrgID: w.org, ID: a.ID, VersionID: v.ID, ActivatedBy: w.user})
	Expect(err).NotTo(HaveOccurred())
	w.agent = a.ID
	w.task = w.newTask(ctx)
	return w
}

func (w *world) newTask(ctx context.Context) store.Task {
	GinkgoHelper()
	t, err := q.CreateTask(ctx, store.CreateTaskParams{OrgID: w.org, Title: "t", AgentID: w.agent, CreatedByKind: store.TaskCreatorKindUser, CreatedByUserID: w.user})
	Expect(err).NotTo(HaveOccurred())
	return t
}

// blindQueuedCheck is a transaction whose HasQueuedRunForTask says no, as if a caller had read the
// answer before another transaction queued a run. It lets a spec reach the insert that the unique
// index refuses.
type blindQueuedCheck struct{ pgx.Tx }

type noRow struct{}

func (noRow) Scan(dest ...any) error {
	*dest[0].(*bool) = false
	return nil
}

func (b blindQueuedCheck) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "name: HasQueuedRunForTask") {
		return noRow{}
	}
	return b.Tx.QueryRow(ctx, sql, args...)
}

// enqueue queues a run of the task in its own transaction.
func (w *world) enqueue(ctx context.Context, t store.Task) (run store.Run, queued bool, err error) {
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		run, queued, err = scheduler.Enqueue(ctx, q.WithTx(tx), t)
		return err
	})
	return run, queued, err
}

func (w *world) queuedRuns(ctx context.Context, t store.Task) int {
	GinkgoHelper()
	runs, err := q.ListRunsForTask(ctx, store.ListRunsForTaskParams{OrgID: w.org, TaskID: t.ID})
	Expect(err).NotTo(HaveOccurred())
	n := 0
	for _, r := range runs {
		if r.State == store.RunStateQueued {
			n++
		}
	}
	return n
}

var _ = Describe("one queued run per task", func() {
	It("takes the task lock in a transaction and refuses to run outside one", func(ctx SpecContext) {
		w := newWorld(ctx)
		Expect(pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			locked, err := runlock.LockTask(ctx, q.WithTx(tx), w.org, w.task.ID)
			Expect(locked.ID).To(Equal(w.task.ID))
			return err
		})).To(Succeed())

		_, err := runlock.LockTask(ctx, q, w.org, w.task.ID)
		Expect(err).To(MatchError(ContainSubstring("outside a transaction")))
	})

	It("queues one run and says no to the second", func(ctx SpecContext) {
		w := newWorld(ctx)
		_, queued, err := w.enqueue(ctx, w.task)
		Expect(err).NotTo(HaveOccurred())
		Expect(queued).To(BeTrue())
		_, queued, err = w.enqueue(ctx, w.task)
		Expect(err).NotTo(HaveOccurred())
		Expect(queued).To(BeFalse())
		Expect(w.queuedRuns(ctx, w.task)).To(Equal(1))
	})

	It("queues one run when many enqueue at once", func(ctx SpecContext) {
		w := newWorld(ctx)
		const n = 8
		results := make(chan bool, n)
		for range n {
			go func() {
				defer GinkgoRecover()
				_, queued, err := w.enqueue(ctx, w.task)
				Expect(err).NotTo(HaveOccurred())
				results <- queued
			}()
		}
		got := 0
		for range n {
			if <-results {
				got++
			}
		}
		Expect(got).To(Equal(1))
		Expect(w.queuedRuns(ctx, w.task)).To(Equal(1))
	})

	It("is refused by the database when a caller skips the lock", func(ctx SpecContext) {
		w := newWorld(ctx)
		_, err := q.CreateRun(ctx, store.CreateRunParams{OrgID: w.org, AgentID: w.agent, TaskID: w.task.ID, MaxWallClockSeconds: 1800})
		Expect(err).NotTo(HaveOccurred())
		_, err = q.CreateRun(ctx, store.CreateRunParams{OrgID: w.org, AgentID: w.agent, TaskID: w.task.ID, MaxWallClockSeconds: 1800})
		var pgErr *pgconn.PgError
		Expect(errors.As(err, &pgErr)).To(BeTrue(), "the error is a database error: %v", err)
		Expect(pgErr.Code).To(Equal("23505"))
		Expect(pgErr.ConstraintName).To(Equal("runs_one_queued_per_task"))
	})

	It("maps the refusal to ErrQueuedRunExists when the check before the insert is blind", func(ctx SpecContext) {
		w := newWorld(ctx)
		_, _, err := w.enqueue(ctx, w.task)
		Expect(err).NotTo(HaveOccurred())
		err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			_, _, err := scheduler.Enqueue(ctx, store.New(blindQueuedCheck{tx}), w.task)
			return err
		})
		Expect(err).To(MatchError(scheduler.ErrQueuedRunExists))
		Expect(w.queuedRuns(ctx, w.task)).To(Equal(1))
	})

	It("ends a run that is requeued while another run of the task is queued, as superseded", func(ctx SpecContext) {
		w := newWorld(ctx)
		first, _, err := w.enqueue(ctx, w.task)
		Expect(err).NotTo(HaveOccurred())
		claimed, err := q.ClaimNextRun(ctx, store.ClaimNextRunParams{OrgID: w.org, OwnerID: "test", LeaseSeconds: 60})
		Expect(err).NotTo(HaveOccurred())
		Expect(claimed.ID).To(Equal(first.ID))
		// The first run has waited for its sandbox for ten minutes.
		waitStart := pgtype.Timestamptz{Time: time.Now().Add(-10 * time.Minute).Truncate(time.Microsecond), Valid: true}
		_, err = q.MarkRunSandboxWait(ctx, store.MarkRunSandboxWaitParams{Started: waitStart, OrgID: w.org, ID: first.ID, Epoch: claimed.Epoch})
		Expect(err).NotTo(HaveOccurred())
		// A message arrives while the first run waits for its sandbox: it queues a second run.
		second, queued, err := w.enqueue(ctx, w.task)
		Expect(err).NotTo(HaveOccurred())
		Expect(queued).To(BeTrue())

		own := runoutcome.New(runoutcome.Config{DB: pool, Queries: q, Run: claimed})
		Expect(own.Requeue(ctx, runoutcome.Turn{}, "the sandbox is not ready")).To(Succeed())

		got, err := q.GetRun(ctx, store.GetRunParams{OrgID: w.org, ID: first.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.State).To(Equal(store.RunStateCanceled))
		Expect(got.OutcomeReason.String).To(ContainSubstring("superseded"))
		Expect(w.queuedRuns(ctx, w.task)).To(Equal(1))
		other, err := q.GetRun(ctx, store.GetRunParams{OrgID: w.org, ID: second.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(other.State).To(Equal(store.RunStateQueued))
		Expect(other.SandboxWaitStartedAt.Valid).To(BeTrue(), "the queued run carries the wait start")
		Expect(other.SandboxWaitStartedAt.Time).To(BeTemporally("==", waitStart.Time))

		runEvents, err := q.ListRunEventsAfter(ctx, store.ListRunEventsAfterParams{OrgID: w.org, RunID: first.ID, MaxEvents: 100})
		Expect(err).NotTo(HaveOccurred())
		var runKinds []string
		for _, e := range runEvents {
			runKinds = append(runKinds, e.Kind)
		}
		Expect(runKinds).To(ContainElement(runoutcome.KindRunCanceled))
		Expect(runKinds).NotTo(ContainElement("run.requeued"))
		taskEvents, err := q.ListTaskEvents(ctx, store.ListTaskEventsParams{OrgID: w.org, TaskID: w.task.ID, MaxEvents: 100})
		Expect(err).NotTo(HaveOccurred())
		found := false
		for _, e := range taskEvents {
			if e.Kind == runoutcome.KindRunCanceled && e.RunID == first.ID {
				found = true
				Expect(string(e.Payload)).To(ContainSubstring(string(runoutcome.ReasonRequeueSuperseded)))
			}
		}
		Expect(found).To(BeTrue(), "a task event says why the run was canceled")
	})

	It("does not deadlock when a cancel and a requeue meet on one starting run", func(ctx SpecContext) {
		for range 10 {
			w := newWorld(ctx)
			first, _, err := w.enqueue(ctx, w.task)
			Expect(err).NotTo(HaveOccurred())
			claimed, err := q.ClaimNextRun(ctx, store.ClaimNextRunParams{OrgID: w.org, OwnerID: "test", LeaseSeconds: 60})
			Expect(err).NotTo(HaveOccurred())
			own := runoutcome.New(runoutcome.Config{DB: pool, Queries: q, Run: claimed})
			start := make(chan struct{})
			cancelErr := make(chan error, 1)
			requeueErr := make(chan error, 1)
			go func() {
				defer GinkgoRecover()
				<-start
				cancelErr <- pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
					_, err := runoutcome.CancelUnowned(ctx, q.WithTx(tx), w.org, first.ID,
						runoutcome.Cancellation{Reason: runoutcome.ReasonUser, Message: "canceled by a person"})
					return err
				})
			}()
			go func() {
				defer GinkgoRecover()
				<-start
				requeueErr <- own.Requeue(ctx, runoutcome.Turn{}, "the sandbox is not ready")
			}()
			close(start)
			// A lost race is a fence or a missing row; a deadlock is any other error.
			if err := <-cancelErr; err != nil {
				Expect(err).To(MatchError(pgx.ErrNoRows))
			}
			if err := <-requeueErr; err != nil {
				Expect(err).To(MatchError(runoutcome.ErrFenced))
			}
		}
	})

	// The run that ends hands its sandbox wait start to the queued run: the earlier start wins.
	waitCases := []struct {
		name          string
		first, queued *time.Duration // how long ago the wait of R and of Q started; nil: no start
	}{
		{"keeps the queued run's earlier start", ago(5 * time.Minute), ago(20 * time.Minute)},
		{"gives the queued run the earlier start of the ended run", ago(20 * time.Minute), ago(5 * time.Minute)},
		{"gives the start of the ended run to a queued run that has none", ago(10 * time.Minute), nil},
		{"leaves the queued run alone when the ended run has no start", nil, ago(5 * time.Minute)},
		{"leaves the queued run without a start when neither has one", nil, nil},
	}
	for _, c := range waitCases {
		It("on a supersede "+c.name, func(ctx SpecContext) {
			w := newWorld(ctx)
			first, _, err := w.enqueue(ctx, w.task)
			Expect(err).NotTo(HaveOccurred())
			claimed, err := q.ClaimNextRun(ctx, store.ClaimNextRunParams{OrgID: w.org, OwnerID: "test", LeaseSeconds: 60})
			Expect(err).NotTo(HaveOccurred())
			at := func(d *time.Duration) pgtype.Timestamptz {
				if d == nil {
					return pgtype.Timestamptz{}
				}
				return pgtype.Timestamptz{Time: time.Now().Add(-*d).Truncate(time.Microsecond), Valid: true}
			}
			firstAt, queuedAt := at(c.first), at(c.queued)
			if firstAt.Valid {
				_, err = q.MarkRunSandboxWait(ctx, store.MarkRunSandboxWaitParams{Started: firstAt, OrgID: w.org, ID: first.ID, Epoch: claimed.Epoch})
				Expect(err).NotTo(HaveOccurred())
			}
			second, queued, err := w.enqueue(ctx, w.task)
			Expect(err).NotTo(HaveOccurred())
			Expect(queued).To(BeTrue())
			if queuedAt.Valid {
				_, err = pool.Exec(ctx, `UPDATE runs SET sandbox_wait_started_at = $3 WHERE org_id = $1 AND id = $2`, w.org, second.ID, queuedAt)
				Expect(err).NotTo(HaveOccurred())
			}
			wantAt := queuedAt
			if firstAt.Valid && (!queuedAt.Valid || firstAt.Time.Before(queuedAt.Time)) {
				wantAt = firstAt
			}

			own := runoutcome.New(runoutcome.Config{DB: pool, Queries: q, Run: claimed})
			Expect(own.Requeue(ctx, runoutcome.Turn{}, "the sandbox is not ready")).To(Succeed())
			other, err := q.GetRun(ctx, store.GetRunParams{OrgID: w.org, ID: second.ID})
			Expect(err).NotTo(HaveOccurred())
			Expect(other.SandboxWaitStartedAt.Valid).To(Equal(wantAt.Valid))
			if wantAt.Valid {
				Expect(other.SandboxWaitStartedAt.Time).To(BeTemporally("==", wantAt.Time))
			}
		})
	}

	It("waits for the task lock without holding the queued run, then ends as superseded", func(ctx SpecContext) {
		w := newWorld(ctx)
		first, _, err := w.enqueue(ctx, w.task)
		Expect(err).NotTo(HaveOccurred())
		claimed, err := q.ClaimNextRun(ctx, store.ClaimNextRunParams{OrgID: w.org, OwnerID: "test", LeaseSeconds: 60})
		Expect(err).NotTo(HaveOccurred())
		Expect(claimed.ID).To(Equal(first.ID))
		second, queued, err := w.enqueue(ctx, w.task)
		Expect(err).NotTo(HaveOccurred())
		Expect(queued).To(BeTrue())

		// A task-first path (setState) holds the task lock.
		taskTx, err := pool.Begin(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = taskTx.Rollback(ctx) }()
		_, err = runlock.LockTask(ctx, q.WithTx(taskTx), w.org, w.task.ID)
		Expect(err).NotTo(HaveOccurred())

		var taskPID int32
		Expect(taskTx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&taskPID)).To(Succeed())

		// Requeue runs on its own connection, so we know its backend pid.
		reqConn, err := pool.Acquire(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer reqConn.Release()
		var reqPID int32
		Expect(reqConn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&reqPID)).To(Succeed())

		own := runoutcome.New(runoutcome.Config{DB: reqConn, Queries: q, Run: claimed})
		done := make(chan error, 1)
		go func() {
			defer GinkgoRecover()
			done <- own.Requeue(ctx, runoutcome.Turn{}, "the sandbox is not ready")
		}()

		// Requeue waits on the task lock: its backend is blocked by the task transaction's backend.
		Eventually(func(g Gomega) {
			var blocked bool
			g.Expect(pool.QueryRow(ctx, `SELECT $1 = ANY(pg_blocking_pids($2))`, taskPID, reqPID).Scan(&blocked)).To(Succeed())
			g.Expect(blocked).To(BeTrue())
		}, 10*time.Second, 50*time.Millisecond).Should(Succeed())
		Expect(done).NotTo(Receive())

		// ...and does not hold the queued run: another connection takes its lock at once.
		probe, err := pool.Begin(ctx)
		Expect(err).NotTo(HaveOccurred())
		var id pgtype.UUID
		Expect(probe.QueryRow(ctx, `SELECT id FROM runs WHERE org_id = $1 AND id = $2 FOR UPDATE NOWAIT`, w.org, second.ID).Scan(&id)).To(Succeed())
		Expect(probe.Rollback(ctx)).To(Succeed())

		Expect(taskTx.Rollback(ctx)).To(Succeed())
		Eventually(done, 10*time.Second).Should(Receive(BeNil()))
		ended, err := q.GetRun(ctx, store.GetRunParams{OrgID: w.org, ID: first.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(ended.State).To(Equal(store.RunStateCanceled))
		var reason string
		Expect(pool.QueryRow(ctx, `SELECT payload->>'reason' FROM run_events WHERE org_id = $1 AND run_id = $2 AND kind = 'run.canceled'`, w.org, first.ID).Scan(&reason)).To(Succeed())
		Expect(reason).To(Equal("requeue_superseded"))
		Expect(w.queuedRuns(ctx, w.task)).To(Equal(1))
	})

	It("still requeues a run when no other run of the task is queued", func(ctx SpecContext) {
		w := newWorld(ctx)
		first, _, err := w.enqueue(ctx, w.task)
		Expect(err).NotTo(HaveOccurred())
		claimed, err := q.ClaimNextRun(ctx, store.ClaimNextRunParams{OrgID: w.org, OwnerID: "test", LeaseSeconds: 60})
		Expect(err).NotTo(HaveOccurred())
		own := runoutcome.New(runoutcome.Config{DB: pool, Queries: q, Run: claimed})
		Expect(own.Requeue(ctx, runoutcome.Turn{}, "the sandbox is not ready")).To(Succeed())
		got, err := q.GetRun(ctx, store.GetRunParams{OrgID: w.org, ID: first.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.State).To(Equal(store.RunStateQueued))
	})
})
