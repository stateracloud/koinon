// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"github.com/jackc/pgx/v5"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/orgsettings"
)

// Per-org run concurrency (plan item DB2, ADR 0066): run_claimable holds an org to its cap.
var _ = Describe("per-org run concurrency", func() {
	It("stops claiming at the org's cap, frees a slot when a run finishes, and leaves other orgs alone", func(ctx SpecContext) {
		a := newFixture(ctx)
		b := newFixture(ctx)
		cap2 := 2
		Expect(orgsettings.Set(ctx, q, a.org, orgsettings.Override{MaxConcurrentRuns: &cap2})).To(Succeed())

		// Four agents with one queued run each.
		for range 4 {
			agent, _ := newAgent(ctx, a.org, a.user)
			enqueue(ctx, a.org, agent.ID, newTask(ctx, a.org, a.user, agent.ID).ID)
		}
		enqueue(ctx, b.org, b.agent.ID, b.task.ID)

		first := mustClaim(ctx, a.org, "agentd-a")
		mustClaim(ctx, a.org, "agentd-a")
		_, err := claim(ctx, a.org, "agentd-a")
		Expect(err).To(MatchError(pgx.ErrNoRows), "the third claim waits: the org is at its cap of 2")

		Expect(mustClaim(ctx, b.org, "agentd-b").OrgID).To(Equal(b.org), "another org is not held back")

		first = markRunning(ctx, first)
		_, err = claim(ctx, a.org, "agentd-a")
		Expect(err).To(MatchError(pgx.ErrNoRows), "a running run still counts")
		_, err = finish(ctx, first, store.RunStateSucceeded)
		Expect(err).NotTo(HaveOccurred())
		mustClaim(ctx, a.org, "agentd-a")
	})

	It("defaults to 20 owned runs per org", func(ctx SpecContext) {
		f := newFixture(ctx)
		for range 21 {
			agent, _ := newAgent(ctx, f.org, f.user)
			enqueue(ctx, f.org, agent.ID, newTask(ctx, f.org, f.user, agent.ID).ID)
		}
		for range orgsettings.DefaultMaxConcurrentRuns {
			mustClaim(ctx, f.org, "agentd-a")
		}
		_, err := claim(ctx, f.org, "agentd-a")
		Expect(err).To(MatchError(pgx.ErrNoRows))
	})

	It("does not count a queued run held back by the cap as demand", func(ctx SpecContext) {
		f := newFixture(ctx)
		one := 1
		Expect(orgsettings.Set(ctx, q, f.org, orgsettings.Override{MaxConcurrentRuns: &one})).To(Succeed())
		other, _ := newAgent(ctx, f.org, f.user)
		enqueue(ctx, f.org, other.ID, newTask(ctx, f.org, f.user, other.ID).ID)
		enqueue(ctx, f.org, f.agent.ID, f.task.ID)
		Expect(demand(ctx, f)).To(BeTrue())

		claimed := mustClaim(ctx, f.org, "agentd-a")
		Expect(claimed.AgentID).NotTo(BeZero())
		// The org is at its cap, so whichever agent was not claimed has no demand.
		unclaimed := f
		if claimed.AgentID == f.agent.ID {
			unclaimed.agent.ID = other.ID
		}
		Expect(demand(ctx, unclaimed)).To(BeFalse())
	})
})
