// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"

	"github.com/jackc/pgx/v5"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
)

func demand(ctx context.Context, f fixture) bool {
	act, err := q.GetAgentRunActivity(ctx, store.GetAgentRunActivityParams{OrgID: f.org, AgentID: f.agent.ID})
	Expect(err).NotTo(HaveOccurred())
	return act.HasDemand
}

func markUnhealthy(ctx context.Context, f fixture) {
	_, err := q.MarkAgentUnhealthy(ctx, store.MarkAgentUnhealthyParams{OrgID: f.org, ID: f.agent.ID, Reason: "test"})
	Expect(err).NotTo(HaveOccurred())
}

func resolvePark(ctx context.Context, f fixture, r store.Run) {
	_, err := q.ResolveRunPark(ctx, store.ResolveRunParkParams{OrgID: f.org, ID: r.ID})
	Expect(err).NotTo(HaveOccurred())
}

// parkedRun claims the agent's only queued run and parks it.
func parkedRun(ctx context.Context, f fixture) store.Run {
	parked, err := park(ctx, markRunning(ctx, mustClaim(ctx, f.org, "agentd-a")))
	Expect(err).NotTo(HaveOccurred())
	return parked
}

// has_demand keeps a sandbox up and starts a stopped one, so it must be true exactly when
// ClaimNextRun would claim a run (old issue 99).
var _ = Describe("run demand", func() {
	It("ignores a queued run behind a parked run of its task, until the parked run finishes", func(ctx SpecContext) {
		f := newFixture(ctx)
		enqueue(ctx, f.org, f.agent.ID, f.task.ID)
		parked := parkedRun(ctx, f)
		enqueue(ctx, f.org, f.agent.ID, f.task.ID)
		Expect(demand(ctx, f)).To(BeFalse(), "the new run cannot start while its task's run is parked")

		resolvePark(ctx, f, parked)
		Expect(demand(ctx, f)).To(BeTrue(), "the resolved parked run itself is claimable")
		resumed := markRunning(ctx, mustClaim(ctx, f.org, "agentd-a"))
		Expect(resumed.ID).To(Equal(parked.ID))
		_, err := finish(ctx, resumed, store.RunStateSucceeded)
		Expect(err).NotTo(HaveOccurred())
		Expect(demand(ctx, f)).To(BeTrue(), "the queued run starts the sandbox as before")
	})

	It("ignores queued runs of an unhealthy agent until it is resumed", func(ctx SpecContext) {
		f := newFixture(ctx)
		enqueue(ctx, f.org, f.agent.ID, f.task.ID)
		Expect(demand(ctx, f)).To(BeTrue())
		markUnhealthy(ctx, f)
		Expect(demand(ctx, f)).To(BeFalse())
		_, err := q.ResumeAgent(ctx, store.ResumeAgentParams{OrgID: f.org, ID: f.agent.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(demand(ctx, f)).To(BeTrue())
	})

	DescribeTable("agrees with ClaimNextRun",
		func(ctx SpecContext, setup func(ctx context.Context, f fixture), want bool) {
			f := newFixture(ctx)
			setup(ctx, f)
			Expect(demand(ctx, f)).To(Equal(want), "has_demand")
			_, err := claim(ctx, f.org, "agentd-claim")
			if want {
				Expect(err).NotTo(HaveOccurred(), "ClaimNextRun")
			} else {
				Expect(err).To(MatchError(pgx.ErrNoRows), "ClaimNextRun")
			}
		},
		Entry("nothing queued", func(ctx context.Context, f fixture) {}, false),
		Entry("one queued run", func(ctx context.Context, f fixture) {
			enqueue(ctx, f.org, f.agent.ID, f.task.ID)
		}, true),
		Entry("unhealthy agent", func(ctx context.Context, f fixture) {
			enqueue(ctx, f.org, f.agent.ID, f.task.ID)
			markUnhealthy(ctx, f)
		}, false),
		Entry("parked sibling on the task", func(ctx context.Context, f fixture) {
			enqueue(ctx, f.org, f.agent.ID, f.task.ID)
			parkedRun(ctx, f)
			enqueue(ctx, f.org, f.agent.ID, f.task.ID)
		}, false),
		Entry("parked run on another task", func(ctx context.Context, f fixture) {
			enqueue(ctx, f.org, f.agent.ID, f.task.ID)
			parkedRun(ctx, f)
			enqueue(ctx, f.org, f.agent.ID, newTask(ctx, f.org, f.user, f.agent.ID).ID)
		}, true),
		Entry("parked run whose park is unresolved", func(ctx context.Context, f fixture) {
			enqueue(ctx, f.org, f.agent.ID, f.task.ID)
			parkedRun(ctx, f)
		}, false),
		Entry("parked run whose park is resolved", func(ctx context.Context, f fixture) {
			enqueue(ctx, f.org, f.agent.ID, f.task.ID)
			resolvePark(ctx, f, parkedRun(ctx, f))
		}, true),
		Entry("resolved parked run of an unhealthy agent", func(ctx context.Context, f fixture) {
			enqueue(ctx, f.org, f.agent.ID, f.task.ID)
			resolvePark(ctx, f, parkedRun(ctx, f))
			markUnhealthy(ctx, f)
		}, false),
		Entry("the agent already has an active run", func(ctx context.Context, f fixture) {
			enqueue(ctx, f.org, f.agent.ID, f.task.ID)
			mustClaim(ctx, f.org, "agentd-a")
			enqueue(ctx, f.org, f.agent.ID, newTask(ctx, f.org, f.user, f.agent.ID).ID)
		}, false),
		Entry("the task's earlier run finished", func(ctx context.Context, f fixture) {
			enqueue(ctx, f.org, f.agent.ID, f.task.ID)
			_, err := finish(ctx, markRunning(ctx, mustClaim(ctx, f.org, "agentd-a")), store.RunStateSucceeded)
			Expect(err).NotTo(HaveOccurred())
			enqueue(ctx, f.org, f.agent.ID, f.task.ID)
		}, true),
	)
})
