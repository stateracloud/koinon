// SPDX-License-Identifier: Apache-2.0

package scheduler_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/runoutcome"
	"github.com/stateracloud/koinon/internal/scheduler"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

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
			locked, err := scheduler.LockTask(ctx, q.WithTx(tx), w.org, w.task.ID)
			Expect(locked.ID).To(Equal(w.task.ID))
			return err
		})).To(Succeed())

		_, err := scheduler.LockTask(ctx, q, w.org, w.task.ID)
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
		Expect(err).To(HaveOccurred(), "runs_one_queued_per_task")
	})

	It("ends a run that is requeued while another run of the task is queued, as superseded", func(ctx SpecContext) {
		w := newWorld(ctx)
		first, _, err := w.enqueue(ctx, w.task)
		Expect(err).NotTo(HaveOccurred())
		claimed, err := q.ClaimNextRun(ctx, store.ClaimNextRunParams{OrgID: w.org, OwnerID: "test", LeaseSeconds: 60})
		Expect(err).NotTo(HaveOccurred())
		Expect(claimed.ID).To(Equal(first.ID))
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
