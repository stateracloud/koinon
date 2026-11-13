// SPDX-License-Identifier: Apache-2.0

package runoutcome_test

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/runoutcome"
	"github.com/stateracloud/koinon/internal/scheduler"
)

// The step that follows a run canceled at its time limit (issue #99) runs in the cancel's
// transaction (Config.AfterTimeLimit).
var _ = Describe("AfterTimeLimit", func() {
	ownerWith := func(w *world, step runoutcome.RunStep) *runoutcome.Owner {
		return runoutcome.New(runoutcome.Config{DB: pool, Queries: q, Run: w.run, Metrics: w.metrics, AfterTimeLimit: step})
	}

	It("queues a continuation run of the task in the cancel's transaction", func(ctx SpecContext) {
		w := newWorld(ctx, true)
		own := ownerWith(w, scheduler.ContinueAfterTimeLimit)
		Expect(own.CancelAtTimeLimit(ctx, w.openTurn(ctx), "wall-clock cap of 30m0s reached")).To(Succeed())

		Expect(w.current(ctx).State).To(Equal(store.RunStateCanceled))
		queued, err := q.HasQueuedRunForTask(ctx, store.HasQueuedRunForTaskParams{OrgID: w.org, TaskID: w.task})
		Expect(err).NotTo(HaveOccurred())
		Expect(queued).To(BeTrue())
		task, err := q.GetTask(ctx, store.GetTaskParams{OrgID: w.org, ID: w.task})
		Expect(err).NotTo(HaveOccurred())
		Expect(task.ContinuationCount).To(Equal(int32(1)))
	})

	It("keeps the cancel when the step fails: a task's configuration problem must not keep its run past the cap", func(ctx SpecContext) {
		w := newWorld(ctx, true)
		boom := errors.New("boom")
		own := ownerWith(w, func(context.Context, *store.Queries, store.Run) error { return boom })
		Expect(own.CancelAtTimeLimit(ctx, w.openTurn(ctx), "wall-clock cap of 30m0s reached")).To(Succeed())
		Expect(w.current(ctx).State).To(Equal(store.RunStateCanceled))
	})

	It("is not called for any other end of a run", func(ctx SpecContext) {
		w := newWorld(ctx, true)
		called := false
		own := ownerWith(w, func(context.Context, *store.Queries, store.Run) error { called = true; return nil })
		Expect(own.Fail(ctx, w.openTurn(ctx), "boom", store.RunFailureClassFatal)).To(Succeed())
		Expect(called).To(BeFalse())
	})
})
