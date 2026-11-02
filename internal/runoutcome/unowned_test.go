// SPDX-License-Identifier: Apache-2.0

package runoutcome_test

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/jackc/pgx/v5"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/runoutcome"
)

// cancelUnowned cancels the world's run as the control plane, in a transaction of its own.
func (w *world) cancelUnowned(ctx context.Context, c runoutcome.Cancellation) (store.Run, error) {
	var out store.Run
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var err error
		out, err = runoutcome.CancelUnowned(ctx, q.WithTx(tx), w.org, w.run.ID, c)
		return err
	})
	return out, err
}

func payloadOf(raw []byte) map[string]any {
	GinkgoHelper()
	var body map[string]any
	Expect(json.Unmarshal(raw, &body)).To(Succeed())
	return body
}

var _ = Describe("CancelUnowned", func() {
	It("cancels a live run without an epoch: run event, then the task event by the actor", func(ctx SpecContext) {
		w := newWorld(ctx, true)
		run, err := w.cancelUnowned(ctx, runoutcome.Cancellation{
			Reason: runoutcome.ReasonSpendLimit, Message: "the agent's daily budget is spent",
			Class: store.RunFailureClassBudget, Actor: runoutcome.Actor{UserID: w.user},
			Extra: map[string]any{"limit": "agent_daily", "failureClass": "ignored"},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(run.State).To(Equal(store.RunStateCanceled))
		Expect(run.OutcomeReason.String).To(Equal("the agent's daily budget is spent"))
		Expect(run.FailureClass.RunFailureClass).To(Equal(store.RunFailureClassBudget))

		Expect(w.runKinds(ctx)).To(Equal([]string{runoutcome.KindRunCanceled}))
		evs := w.taskEvents(ctx)
		Expect(evs).To(HaveLen(1))
		Expect(evs[0].Kind).To(Equal(runoutcome.KindRunCanceled))
		Expect(evs[0].ActorKind).To(Equal(store.TaskActorKindUser))
		Expect(evs[0].ActorUserID).To(Equal(w.user))
		body := payloadOf(evs[0].Payload)
		Expect(body).To(HaveKeyWithValue("reason", "spend_limit"))
		Expect(body).To(HaveKeyWithValue("message", "the agent's daily budget is spent"))
		Expect(body).To(HaveKeyWithValue("limit", "agent_daily"))
		// The run's failure class wins over an extra field of the same name.
		Expect(body).To(HaveKeyWithValue("failureClass", "budget"))
		// A cancel leaves the task as it is (ADR 0065).
		Expect(w.taskState(ctx)).NotTo(BeElementOf(store.TaskStateCompleted, store.TaskStateFailed))
	})

	It("cancels nothing of a run that has ended, and the owner is fenced by a run it did cancel", func(ctx SpecContext) {
		w := newWorld(ctx, true)
		Expect(w.own.Succeed(ctx, w.openTurn(ctx))).To(Succeed())
		_, err := w.cancelUnowned(ctx, runoutcome.Cancellation{Reason: runoutcome.ReasonUser, Message: "too late", Actor: runoutcome.System})
		Expect(err).To(MatchError(pgx.ErrNoRows))
		Expect(w.current(ctx).State).To(Equal(store.RunStateSucceeded))
		Expect(w.taskKinds(ctx)).NotTo(ContainElement(runoutcome.KindRunCanceled))

		w2 := newWorld(ctx, true)
		_, err = w2.cancelUnowned(ctx, runoutcome.Cancellation{Reason: runoutcome.ReasonUser, Message: "stop", Actor: runoutcome.System})
		Expect(err).NotTo(HaveOccurred())
		Expect(w2.own.Succeed(ctx, w2.openTurn(ctx))).To(MatchError(runoutcome.ErrFenced))
		Expect(w2.own.FenceKind()).To(Equal(runoutcome.FenceCanceled))
		Expect(w2.current(ctx).State).To(Equal(store.RunStateCanceled))
	})

	// The control plane and the run's owner end the run at the same moment: whichever commits first
	// is the run's only outcome, and the other writes nothing. A transaction holding the run's row
	// keeps both in flight; they queue on the row in the order given, and the first one wins.
	DescribeTable("resolves a race with an owner ending the run to a single outcome",
		func(ctx SpecContext, ownerFirst bool) {
			w := newWorld(ctx, true)
			turn := w.openTurn(ctx)
			hold, err := pool.Begin(ctx)
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = hold.Rollback(context.WithoutCancel(ctx)) }()
			_, err = q.WithTx(hold).LockRun(ctx, store.LockRunParams{OrgID: w.org, ID: w.run.ID})
			Expect(err).NotTo(HaveOccurred())

			var wg sync.WaitGroup
			var ownErr, cancelErr error
			owner := func() {
				defer GinkgoRecover()
				defer wg.Done()
				ownErr = w.own.Succeed(ctx, turn)
			}
			cancel := func() {
				defer GinkgoRecover()
				defer wg.Done()
				_, cancelErr = w.cancelUnowned(ctx, runoutcome.Cancellation{
					Reason: runoutcome.ReasonSpendLimit, Message: "budget spent",
					Class: store.RunFailureClassBudget, Actor: runoutcome.System,
				})
			}
			first, second := cancel, owner
			if ownerFirst {
				first, second = owner, cancel
			}
			wg.Add(2)
			go first()
			Eventually(ctx, waitingOnLocks).Should(Equal(1), "the first writer waits on the run's row")
			go second()
			Eventually(ctx, waitingOnLocks).Should(Equal(2), "the second writer queues behind it")
			Expect(hold.Commit(ctx)).To(Succeed())
			wg.Wait()

			run := w.current(ctx)
			closing := 0
			for _, k := range w.runKinds(ctx) {
				if k == "run.succeeded" || k == runoutcome.KindRunCanceled {
					closing++
				}
			}
			Expect(closing).To(Equal(1), "one closing run event")
			if ownerFirst {
				Expect(run.State).To(Equal(store.RunStateSucceeded))
				Expect(ownErr).NotTo(HaveOccurred())
				Expect(cancelErr).To(MatchError(pgx.ErrNoRows), "the cancel matched no live run")
				Expect(w.taskKinds(ctx)).NotTo(ContainElement(runoutcome.KindRunCanceled))
				Expect(w.taskState(ctx)).To(Equal(store.TaskStateCompleted))
				return
			}
			Expect(run.State).To(Equal(store.RunStateCanceled))
			Expect(run.FailureClass.RunFailureClass).To(Equal(store.RunFailureClassBudget))
			Expect(cancelErr).NotTo(HaveOccurred())
			Expect(ownErr).To(MatchError(runoutcome.ErrFenced))
			Expect(w.own.FenceKind()).To(Equal(runoutcome.FenceCanceled))
			Expect(w.taskKinds(ctx)).To(ContainElement(runoutcome.KindRunCanceled))
			Expect(w.taskState(ctx)).NotTo(Equal(store.TaskStateCompleted))
			Expect(w.turnRow(ctx).EndedAt.Valid).To(BeTrue(), "the fenced owner still ends its turn row")
		},
		Entry("the owner commits first: the cancel finds no live run", true),
		Entry("the cancel commits first: the owner's ending is fenced", false),
	)
})

// waitingOnLocks is how many sessions of this suite's database wait on a lock.
func waitingOnLocks(ctx context.Context) int {
	var n int
	Expect(pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n)).To(Succeed())
	return n
}

var _ = Describe("CancelAtLimit", func() {
	It("carries what the agent last said with the cancel", func(ctx SpecContext) {
		w := newWorld(ctx, true)
		w.says(ctx, w.own, "half of the work is done")
		var run store.Run
		Expect(pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			var err error
			run, err = runoutcome.CancelAtLimit(ctx, tx, w.org, w.run.ID, runoutcome.Cancellation{
				Reason: runoutcome.ReasonRequestCap, Message: "request limit reached",
				Class: store.RunFailureClassBudget, Actor: runoutcome.System,
				Extra: map[string]any{"model_requests": 3},
			})
			return err
		})).To(Succeed())
		Expect(run.State).To(Equal(store.RunStateCanceled))
		evs := w.taskEvents(ctx)
		Expect(evs).To(HaveLen(1))
		body := payloadOf(evs[0].Payload)
		Expect(body).To(HaveKeyWithValue("reason", "request_cap"))
		Expect(body).To(HaveKeyWithValue("model_requests", BeNumerically("==", 3)))
		Expect(body).To(HaveKeyWithValue("summarySource", "reply"))
		Expect(body).To(HaveKeyWithValue("summary", ContainSubstring("half of the work is done")))
	})
})

var _ = Describe("the time limit", func() {
	It("writes the same note whether the owner or the sweep stops the run", func(ctx SpecContext) {
		owned := newWorld(ctx, true)
		owned.says(ctx, owned.own, "wrapping up")
		Expect(owned.own.CancelAtTimeLimit(ctx, owned.openTurn(ctx), "wall-clock cap of 30m0s reached")).To(Succeed())
		ownerNote := payloadOf(owned.taskEvents(ctx)[0].Payload)

		swept := newWorld(ctx, true)
		swept.says(ctx, swept.own, "wrapping up")
		// Put the run past its cap, as the sweep sees it.
		_, err := pool.Exec(ctx, `UPDATE runs SET started_at = now() - interval '2 hours' WHERE org_id = $1 AND id = $2`, swept.org, swept.run.ID)
		Expect(err).NotTo(HaveOccurred())
		var runs []store.Run
		Expect(pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			runs, err = runoutcome.CancelOverTimeLimit(ctx, tx, swept.org)
			return err
		})).To(Succeed())
		Expect(runs).To(HaveLen(1))
		sweepNote := payloadOf(swept.taskEvents(ctx)[0].Payload)

		for _, k := range []string{"reason", "message", "maxWallClockSeconds", "summary", "summarySource"} {
			Expect(sweepNote).To(HaveKeyWithValue(k, ownerNote[k]), k)
		}
		Expect(ownerNote).To(HaveKeyWithValue("reason", "wall_clock_cap"))
		Expect(ownerNote).To(HaveKeyWithValue("summarySource", "reply"))
	})
})
