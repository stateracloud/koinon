// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"fmt"
	"slices"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
)

var allStates = []store.RunState{
	store.RunStateQueued, store.RunStateStarting, store.RunStateRunning, store.RunStateParked,
	store.RunStateSucceeded, store.RunStateFailed, store.RunStateCanceled,
}

// legal is the run state machine: ADR 0030 plus cancel from every
// non-terminal state.
var legal = map[[2]store.RunState]bool{
	{store.RunStateQueued, store.RunStateStarting}:   true,
	{store.RunStateQueued, store.RunStateCanceled}:   true,
	{store.RunStateStarting, store.RunStateQueued}:   true, // the sandbox was not ready in time (LT1)
	{store.RunStateStarting, store.RunStateRunning}:  true,
	{store.RunStateStarting, store.RunStateFailed}:   true,
	{store.RunStateStarting, store.RunStateCanceled}: true,
	{store.RunStateRunning, store.RunStateParked}:    true,
	{store.RunStateRunning, store.RunStateSucceeded}: true,
	{store.RunStateRunning, store.RunStateFailed}:    true,
	{store.RunStateRunning, store.RunStateCanceled}:  true,
	{store.RunStateParked, store.RunStateStarting}:   true,
	{store.RunStateParked, store.RunStateCanceled}:   true,
}

var _ = Describe("runs", func() {
	Describe("legal transitions through the queries", func() {
		It("queued -> starting -> running -> succeeded", func(ctx SpecContext) {
			f := newFixture(ctx)
			r := enqueue(ctx, f.org, f.agent.ID, f.task.ID)
			Expect(r.ReadyAt.Valid).To(BeTrue())

			r = mustClaim(ctx, f.org, "agentd-a")
			Expect(r.OwnerID.String).To(Equal("agentd-a"))
			Expect(r.Epoch).To(BeEquivalentTo(1))
			Expect(r.LeaseExpiresAt.Valid).To(BeTrue())
			Expect(r.StartedAt.Valid).To(BeTrue())
			Expect(r.ReadyAt.Valid).To(BeFalse())
			Expect(r.AgentVersionID).To(Equal(f.version.ID))

			r = markRunning(ctx, r)
			Expect(r.State).To(Equal(store.RunStateRunning))

			r, err := finish(ctx, r, store.RunStateSucceeded)
			Expect(err).NotTo(HaveOccurred())
			Expect(r.State).To(Equal(store.RunStateSucceeded))
			Expect(r.FinishedAt.Valid).To(BeTrue())
			Expect(r.LeaseExpiresAt.Valid).To(BeFalse())
		})

		DescribeTable("the owner finishes a run",
			func(ctx SpecContext, from, to store.RunState) {
				r := runIn(ctx, from)
				r, err := finish(ctx, r, to)
				Expect(err).NotTo(HaveOccurred())
				Expect(r.State).To(Equal(to))
				Expect(r.OutcomeReason.String).To(Equal("test"))
			},
			Entry("running -> failed", store.RunStateRunning, store.RunStateFailed),
			Entry("running -> canceled", store.RunStateRunning, store.RunStateCanceled),
			Entry("starting -> failed", store.RunStateStarting, store.RunStateFailed),
			Entry("starting -> canceled", store.RunStateStarting, store.RunStateCanceled),
		)

		It("running -> parked -> starting after the park is resolved", func(ctx SpecContext) {
			r := runIn(ctx, store.RunStateParked)
			Expect(r.ParkReason.RunParkReason).To(Equal(store.RunParkReasonApproval))
			Expect(r.OwnerID.Valid).To(BeTrue())
			Expect(r.LeaseExpiresAt.Valid).To(BeFalse())

			_, err := claim(ctx, r.OrgID, "agentd-b")
			Expect(err).To(MatchError(pgx.ErrNoRows), "an unresolved park is not claimable")

			r, err = q.ResolveRunPark(ctx, store.ResolveRunParkParams{OrgID: r.OrgID, ID: r.ID})
			Expect(err).NotTo(HaveOccurred())
			Expect(r.State).To(Equal(store.RunStateParked))

			resumed := mustClaim(ctx, r.OrgID, "agentd-b")
			Expect(resumed.ID).To(Equal(r.ID))
			Expect(resumed.Epoch).To(Equal(r.Epoch + 1))
			Expect(resumed.ParkReason.Valid).To(BeFalse())
			markRunning(ctx, resumed)
		})

		DescribeTable("control-plane cancel",
			func(ctx SpecContext, from store.RunState) {
				r := runIn(ctx, from)
				r, err := cancel(ctx, r)
				Expect(err).NotTo(HaveOccurred())
				Expect(r.State).To(Equal(store.RunStateCanceled))
				Expect(r.OutcomeReason.String).To(Equal("user canceled"))
			},
			Entry("queued -> canceled", store.RunStateQueued),
			Entry("starting -> canceled", store.RunStateStarting),
			Entry("running -> canceled", store.RunStateRunning),
			Entry("parked -> canceled", store.RunStateParked),
		)
	})

	Describe("illegal transitions through the queries match no rows", func() {
		DescribeTable("MarkRunRunning",
			func(ctx SpecContext, from store.RunState) {
				r := runIn(ctx, from)
				_, err := q.MarkRunRunning(ctx, store.MarkRunRunningParams{OrgID: r.OrgID, ID: r.ID, Epoch: r.Epoch})
				Expect(err).To(MatchError(pgx.ErrNoRows))
				Expect(getRun(ctx, r).State).To(Equal(from))
			},
			Entry("from queued", store.RunStateQueued),
			Entry("from running", store.RunStateRunning),
			Entry("from parked", store.RunStateParked),
			Entry("from succeeded", store.RunStateSucceeded),
			Entry("from canceled", store.RunStateCanceled),
		)

		DescribeTable("ParkRun",
			func(ctx SpecContext, from store.RunState) {
				r := runIn(ctx, from)
				_, err := park(ctx, r)
				Expect(err).To(MatchError(pgx.ErrNoRows))
				Expect(getRun(ctx, r).State).To(Equal(from))
			},
			Entry("from queued", store.RunStateQueued),
			Entry("from starting", store.RunStateStarting),
			Entry("from parked", store.RunStateParked),
			Entry("from failed", store.RunStateFailed),
		)

		DescribeTable("FinishRun",
			func(ctx SpecContext, from, to store.RunState) {
				r := runIn(ctx, from)
				_, err := finish(ctx, r, to)
				Expect(err).To(MatchError(pgx.ErrNoRows))
				Expect(getRun(ctx, r).State).To(Equal(from))
			},
			Entry("starting -> succeeded", store.RunStateStarting, store.RunStateSucceeded),
			Entry("queued -> failed", store.RunStateQueued, store.RunStateFailed),
			Entry("parked -> succeeded", store.RunStateParked, store.RunStateSucceeded),
			Entry("running -> parked", store.RunStateRunning, store.RunStateParked),
			Entry("running -> queued", store.RunStateRunning, store.RunStateQueued),
			Entry("succeeded -> failed", store.RunStateSucceeded, store.RunStateFailed),
			Entry("canceled -> succeeded", store.RunStateCanceled, store.RunStateSucceeded),
		)

		DescribeTable("CancelRun on a finished run",
			func(ctx SpecContext, from store.RunState) {
				r := runIn(ctx, from)
				_, err := cancel(ctx, r)
				Expect(err).To(MatchError(pgx.ErrNoRows))
				Expect(getRun(ctx, r).State).To(Equal(from))
			},
			Entry("succeeded", store.RunStateSucceeded),
			Entry("failed", store.RunStateFailed),
			Entry("canceled", store.RunStateCanceled),
		)

		It("ResolveRunPark on a run that is not parked", func(ctx SpecContext) {
			r := runIn(ctx, store.RunStateRunning)
			_, err := q.ResolveRunPark(ctx, store.ResolveRunParkParams{OrgID: r.OrgID, ID: r.ID})
			Expect(err).To(MatchError(pgx.ErrNoRows))
		})

		It("does not queue a run for an agent that is not active", func(ctx SpecContext) {
			org, user := newOrg(ctx)
			draft, err := q.CreateAgent(ctx, store.CreateAgentParams{
				OrgID: org, DisplayName: "draft", Flavor: store.AgentFlavorOpencode, CreatedBy: user,
			})
			Expect(err).NotTo(HaveOccurred())
			t := newTask(ctx, org, user, draft.ID)
			_, err = q.CreateRun(ctx, store.CreateRunParams{
				OrgID: org, AgentID: draft.ID, TaskID: t.ID, MaxWallClockSeconds: 1800,
			})
			Expect(err).To(MatchError(pgx.ErrNoRows))
		})
	})

	Describe("the transition trigger", func() {
		// Every (from, to) pair, written directly so that only the trigger
		// stands between the write and the table. The other columns are set so
		// the CHECK constraints hold for the target state.
		for _, from := range allStates {
			for _, to := range allStates {
				if from == to {
					continue
				}
				ok := legal[[2]store.RunState{from, to}]
				It(fmt.Sprintf("%s %s -> %s", map[bool]string{true: "allows", false: "rejects"}[ok], from, to),
					func(ctx SpecContext) {
						r := runIn(ctx, from)
						_, err := pool.Exec(ctx, `
							UPDATE runs SET
							    state = $3::text::run_state,
							    owner_id = coalesce(owner_id, 'raw'),
							    lease_expires_at = CASE WHEN $3::text IN ('starting', 'running') THEN now() + interval '1 minute' END,
							    ready_at = CASE WHEN $3::text = 'queued' THEN now() END,
							    park_reason = CASE WHEN $3::text = 'parked' THEN 'approval'::run_park_reason END,
							    parked_at = CASE WHEN $3::text = 'parked' THEN now() END,
							    finished_at = CASE WHEN $3::text IN ('succeeded', 'failed', 'canceled') THEN now() END
							WHERE org_id = $1 AND id = $2`,
							r.OrgID, r.ID, string(to))
						if ok {
							Expect(err).NotTo(HaveOccurred())
						} else {
							Expect(err).To(MatchError(ContainSubstring("illegal run state transition")))
						}
					})
			}
		}

		It("requires new runs to be queued", func(ctx SpecContext) {
			f := newFixture(ctx)
			_, err := pool.Exec(ctx, `
				INSERT INTO runs (org_id, agent_id, agent_version_id, task_id, state, owner_id,
				                  lease_expires_at, max_tokens)
				VALUES ($1, $2, $3, $4, 'starting', 'raw', now(), 1)`,
				f.org, f.agent.ID, f.version.ID, f.task.ID)
			Expect(err).To(MatchError(ContainSubstring("must be created queued")))
		})

		It("rejects a decreasing epoch", func(ctx SpecContext) {
			r := runIn(ctx, store.RunStateRunning)
			_, err := pool.Exec(ctx, `UPDATE runs SET epoch = epoch - 1 WHERE org_id = $1 AND id = $2`, r.OrgID, r.ID)
			Expect(err).To(MatchError(ContainSubstring("epoch must not decrease")))
		})
	})

	Describe("the orgs a worker polls (issue 378)", func() {
		It("lists an org only while a run in it can be claimed or taken over", func(ctx SpecContext) {
			polled := func(org pgtype.UUID) bool {
				GinkgoHelper()
				ids, err := q.ListOrgIDsWithRunWork(ctx)
				Expect(err).NotTo(HaveOccurred())
				return slices.Contains(ids, org)
			}
			idle := newFixture(ctx)
			Expect(polled(idle.org)).To(BeFalse(), "an org with no run costs a worker nothing")

			f := newFixture(ctx)
			enqueue(ctx, f.org, f.agent.ID, f.task.ID)
			Expect(polled(f.org)).To(BeTrue(), "a queued run can be claimed")

			r := markRunning(ctx, mustClaim(ctx, f.org, "agentd-a"))
			Expect(polled(f.org)).To(BeFalse(), "a run under a live lease is nobody's work")

			expireLease(ctx, r)
			Expect(polled(f.org)).To(BeTrue(), "an expired lease can be taken over")

			_, err := q.TakeOverExpiredRun(ctx, store.TakeOverExpiredRunParams{OrgID: f.org, OwnerID: "agentd-b", LeaseSeconds: lease})
			Expect(err).NotTo(HaveOccurred())
			Expect(polled(f.org)).To(BeFalse())
		})
	})

	Describe("ownership epoch", func() {
		It("rejects every write of a stale owner after a takeover", func(ctx SpecContext) {
			f := newFixture(ctx)
			enqueue(ctx, f.org, f.agent.ID, f.task.ID)
			old := markRunning(ctx, mustClaim(ctx, f.org, "agentd-a"))

			_, err := q.TakeOverExpiredRun(ctx, store.TakeOverExpiredRunParams{OrgID: f.org, OwnerID: "agentd-b", LeaseSeconds: lease})
			Expect(err).To(MatchError(pgx.ErrNoRows), "the lease has not expired yet")

			expireLease(ctx, old)
			cur, err := q.TakeOverExpiredRun(ctx, store.TakeOverExpiredRunParams{OrgID: f.org, OwnerID: "agentd-b", LeaseSeconds: lease})
			Expect(err).NotTo(HaveOccurred())
			Expect(cur.ID).To(Equal(old.ID))
			Expect(cur.State).To(Equal(store.RunStateRunning))
			Expect(cur.OwnerID.String).To(Equal("agentd-b"))
			Expect(cur.Epoch).To(Equal(old.Epoch + 1))

			agent, err := q.GetAgent(ctx, store.GetAgentParams{OrgID: f.org, ID: f.agent.ID})
			Expect(err).NotTo(HaveOccurred())
			Expect(agent.Epoch).To(Equal(cur.Epoch))

			_, err = q.HeartbeatRun(ctx, store.HeartbeatRunParams{OrgID: f.org, ID: old.ID, Epoch: old.Epoch, LeaseSeconds: lease})
			Expect(err).To(MatchError(pgx.ErrNoRows))
			_, err = q.AppendRunEvent(ctx, store.AppendRunEventParams{OrgID: f.org, RunID: old.ID, Epoch: old.Epoch, Kind: "session.update", Payload: []byte(`{}`)})
			Expect(err).To(MatchError(pgx.ErrNoRows))
			_, err = park(ctx, old)
			Expect(err).To(MatchError(pgx.ErrNoRows))
			_, err = finish(ctx, old, store.RunStateSucceeded)
			Expect(err).To(MatchError(pgx.ErrNoRows))
			_, err = q.SetTaskHarnessSession(ctx, store.SetTaskHarnessSessionParams{
				OrgID: f.org, ID: f.task.ID, RunID: old.ID, Epoch: old.Epoch,
				HarnessSessionID: "stale", HarnessSessionCwd: "/home/agent",
			})
			Expect(err).To(MatchError(pgx.ErrNoRows))

			after := getRun(ctx, old)
			Expect(after.OwnerID.String).To(Equal("agentd-b"))
			Expect(after.State).To(Equal(store.RunStateRunning))
			Expect(after.LastEventSeq).To(BeZero())

			_, err = q.HeartbeatRun(ctx, store.HeartbeatRunParams{OrgID: f.org, ID: cur.ID, Epoch: cur.Epoch, LeaseSeconds: lease})
			Expect(err).NotTo(HaveOccurred())
			_, err = finish(ctx, cur, store.RunStateSucceeded)
			Expect(err).NotTo(HaveOccurred())
		})

		It("rejects the owner from before a park once the run is reclaimed", func(ctx SpecContext) {
			old := runIn(ctx, store.RunStateParked)
			_, err := q.ResolveRunPark(ctx, store.ResolveRunParkParams{OrgID: old.OrgID, ID: old.ID})
			Expect(err).NotTo(HaveOccurred())
			cur := mustClaim(ctx, old.OrgID, "agentd-b")

			_, err = q.MarkRunRunning(ctx, store.MarkRunRunningParams{OrgID: old.OrgID, ID: old.ID, Epoch: old.Epoch})
			Expect(err).To(MatchError(pgx.ErrNoRows))
			_, err = finish(ctx, old, store.RunStateFailed)
			Expect(err).To(MatchError(pgx.ErrNoRows))
			_, err = q.MarkRunRunning(ctx, store.MarkRunRunningParams{OrgID: cur.OrgID, ID: cur.ID, Epoch: cur.Epoch})
			Expect(err).NotTo(HaveOccurred())
		})

		It("lets a canceled run's owner write nothing more", func(ctx SpecContext) {
			r := runIn(ctx, store.RunStateRunning)
			_, err := cancel(ctx, r)
			Expect(err).NotTo(HaveOccurred())
			_, err = q.HeartbeatRun(ctx, store.HeartbeatRunParams{OrgID: r.OrgID, ID: r.ID, Epoch: r.Epoch, LeaseSeconds: lease})
			Expect(err).To(MatchError(pgx.ErrNoRows))
			_, err = finish(ctx, r, store.RunStateSucceeded)
			Expect(err).To(MatchError(pgx.ErrNoRows))
		})
	})

	Describe("claiming", func() {
		It("keeps one active run per agent", func(ctx SpecContext) {
			f := newFixture(ctx)
			first := enqueue(ctx, f.org, f.agent.ID, f.task.ID)
			second := enqueue(ctx, f.org, f.agent.ID, f.task.ID)

			Expect(mustClaim(ctx, f.org, "agentd-a").ID).To(Equal(first.ID))
			_, err := claim(ctx, f.org, "agentd-b")
			Expect(err).To(MatchError(pgx.ErrNoRows), "the agent already has an active run")

			running := markRunning(ctx, getRun(ctx, first))
			_, err = claim(ctx, f.org, "agentd-b")
			Expect(err).To(MatchError(pgx.ErrNoRows))

			_, err = finish(ctx, running, store.RunStateSucceeded)
			Expect(err).NotTo(HaveOccurred())
			Expect(mustClaim(ctx, f.org, "agentd-b").ID).To(Equal(second.ID))
		})

		It("enforces one active run per agent in the schema", func(ctx SpecContext) {
			f := newFixture(ctx)
			enqueue(ctx, f.org, f.agent.ID, f.task.ID)
			other := enqueue(ctx, f.org, f.agent.ID, f.task.ID)
			mustClaim(ctx, f.org, "agentd-a")
			_, err := pool.Exec(ctx, `
				UPDATE runs SET state = 'starting', owner_id = 'raw', lease_expires_at = now(), ready_at = NULL
				WHERE org_id = $1 AND id = $2`, f.org, other.ID)
			Expect(err).To(MatchError(ContainSubstring("runs_one_active_per_agent")))
		})

		It("does not count a parked run as active", func(ctx SpecContext) {
			f := newFixture(ctx)
			first := enqueue(ctx, f.org, f.agent.ID, f.task.ID)
			second := enqueue(ctx, f.org, f.agent.ID, newTask(ctx, f.org, f.user, f.agent.ID).ID)
			parked, err := park(ctx, markRunning(ctx, mustClaim(ctx, f.org, "agentd-a")))
			Expect(err).NotTo(HaveOccurred())
			Expect(parked.ID).To(Equal(first.ID))

			active := mustClaim(ctx, f.org, "agentd-a")
			Expect(active.ID).To(Equal(second.ID))

			// The resolved park waits for the agent's slot.
			_, err = q.ResolveRunPark(ctx, store.ResolveRunParkParams{OrgID: f.org, ID: first.ID})
			Expect(err).NotTo(HaveOccurred())
			_, err = claim(ctx, f.org, "agentd-b")
			Expect(err).To(MatchError(pgx.ErrNoRows))

			_, err = finish(ctx, active, store.RunStateFailed)
			Expect(err).NotTo(HaveOccurred())
			Expect(mustClaim(ctx, f.org, "agentd-b").ID).To(Equal(first.ID))
		})

		It("keeps a task's next run waiting while another of its runs is owned or parked", func(ctx SpecContext) {
			f := newFixture(ctx)
			first := enqueue(ctx, f.org, f.agent.ID, f.task.ID)
			second := enqueue(ctx, f.org, f.agent.ID, f.task.ID)
			parked, err := park(ctx, markRunning(ctx, mustClaim(ctx, f.org, "agentd-a")))
			Expect(err).NotTo(HaveOccurred())
			Expect(parked.ID).To(Equal(first.ID))
			_, err = claim(ctx, f.org, "agentd-a")
			Expect(err).To(MatchError(pgx.ErrNoRows), "one harness session per task")

			_, err = q.ResolveRunPark(ctx, store.ResolveRunParkParams{OrgID: f.org, ID: first.ID})
			Expect(err).NotTo(HaveOccurred())
			resumed := markRunning(ctx, mustClaim(ctx, f.org, "agentd-a"))
			Expect(resumed.ID).To(Equal(first.ID))
			Expect(resumed.ParkedSeconds).To(BeNumerically(">=", 0))
			_, err = finish(ctx, resumed, store.RunStateSucceeded)
			Expect(err).NotTo(HaveOccurred())
			Expect(mustClaim(ctx, f.org, "agentd-a").ID).To(Equal(second.ID))
		})

		// old issue 27's per-task rule with old issue 28's takeover columns: a run whose lease was released or
		// expired is still owned, so the task's next run waits through every takeover, and
		// the next run sees how far the taken-over run prompted.
		It("keeps a task's next run waiting while another of its runs is taken over", func(ctx SpecContext) {
			f := newFixture(ctx)
			first := enqueue(ctx, f.org, f.agent.ID, f.task.ID)
			second := enqueue(ctx, f.org, f.agent.ID, f.task.ID)
			a := markRunning(ctx, mustClaim(ctx, f.org, "agentd-a"))
			Expect(a.ID).To(Equal(first.ID))
			Expect(a.Takeovers).To(BeZero())
			_, err := q.SetRunPromptThrough(ctx, store.SetRunPromptThroughParams{OrgID: f.org, ID: a.ID, Epoch: a.Epoch, PromptThroughSeq: 3})
			Expect(err).NotTo(HaveOccurred())

			// The exec stream dropped: agentd releases the lease.
			_, err = q.ReleaseRunLease(ctx, store.ReleaseRunLeaseParams{OrgID: f.org, ID: a.ID, Epoch: a.Epoch})
			Expect(err).NotTo(HaveOccurred())
			_, err = claim(ctx, f.org, "agentd-b")
			Expect(err).To(MatchError(pgx.ErrNoRows), "a released run still holds its task")
			b, err := q.TakeOverExpiredRun(ctx, store.TakeOverExpiredRunParams{OrgID: f.org, OwnerID: "agentd-b", LeaseSeconds: lease})
			Expect(err).NotTo(HaveOccurred())
			Expect(b.ID).To(Equal(first.ID))
			Expect(b.Takeovers).To(BeEquivalentTo(1))

			// The new owner is killed: its lease expires and a third worker takes over.
			expireLease(ctx, b)
			_, err = claim(ctx, f.org, "agentd-c")
			Expect(err).To(MatchError(pgx.ErrNoRows), "an expired run still holds its task")
			c, err := q.TakeOverExpiredRun(ctx, store.TakeOverExpiredRunParams{OrgID: f.org, OwnerID: "agentd-c", LeaseSeconds: lease})
			Expect(err).NotTo(HaveOccurred())
			Expect(c.Takeovers).To(BeEquivalentTo(2))
			Expect(c.PromptThroughSeq.Int64).To(BeEquivalentTo(3))
			_, err = finish(ctx, c, store.RunStateSucceeded)
			Expect(err).NotTo(HaveOccurred())

			next := mustClaim(ctx, f.org, "agentd-a")
			Expect(next.ID).To(Equal(second.ID))
			Expect(next.Takeovers).To(BeZero())
			through, err := q.GetTaskPromptedThrough(ctx, store.GetTaskPromptedThroughParams{OrgID: f.org, TaskID: f.task.ID, RunID: next.ID})
			Expect(err).NotTo(HaveOccurred())
			Expect(through).To(BeEquivalentTo(3))
		})

		It("claims in FIFO order, across agents too", func(ctx SpecContext) {
			f := newFixture(ctx)
			other, _ := newAgent(ctx, f.org, f.user)
			otherTask := newTask(ctx, f.org, f.user, other.ID)

			a1 := enqueue(ctx, f.org, f.agent.ID, f.task.ID)
			b1 := enqueue(ctx, f.org, other.ID, otherTask.ID)
			a2 := enqueue(ctx, f.org, f.agent.ID, f.task.ID)
			a3 := enqueue(ctx, f.org, f.agent.ID, f.task.ID)

			Expect(mustClaim(ctx, f.org, "w").ID).To(Equal(a1.ID))
			Expect(mustClaim(ctx, f.org, "w").ID).To(Equal(b1.ID))
			_, err := claim(ctx, f.org, "w")
			Expect(err).To(MatchError(pgx.ErrNoRows))

			var order []pgtype.UUID
			order = append(order, a1.ID)
			for range 2 {
				cur := markRunning(ctx, getRun(ctx, store.Run{OrgID: f.org, ID: order[len(order)-1]}))
				_, err := finish(ctx, cur, store.RunStateSucceeded)
				Expect(err).NotTo(HaveOccurred())
				order = append(order, mustClaim(ctx, f.org, "w").ID)
			}
			Expect(order).To(Equal([]pgtype.UUID{a1.ID, a2.ID, a3.ID}))
		})

		It("skips agents another claimer holds, without blocking", func(ctx SpecContext) {
			f := newFixture(ctx)
			other, _ := newAgent(ctx, f.org, f.user)
			otherTask := newTask(ctx, f.org, f.user, other.ID)
			a1 := enqueue(ctx, f.org, f.agent.ID, f.task.ID)
			enqueue(ctx, f.org, f.agent.ID, f.task.ID)
			b1 := enqueue(ctx, f.org, other.ID, otherTask.ID)

			tx, err := pool.Begin(ctx)
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = tx.Rollback(ctx) }()
			held, err := q.WithTx(tx).ClaimNextRun(ctx, store.ClaimNextRunParams{OrgID: f.org, OwnerID: "tx", LeaseSeconds: lease})
			Expect(err).NotTo(HaveOccurred())
			Expect(held.ID).To(Equal(a1.ID))

			// The first claim is uncommitted: its agent row is locked, so this
			// claim skips that agent's second run and takes the other agent's.
			Expect(mustClaim(ctx, f.org, "w").ID).To(Equal(b1.ID))
			Expect(tx.Commit(ctx)).To(Succeed())
		})

		// A claim whose snapshot predates a concurrent winner's commit, but whose lock on the
		// agent row comes after it, re-checks the agent row and sees the run as claimable
		// again; the run update then matches nothing. It must not have bumped the agent's
		// epoch on the way, or the next owner gets a skipped epoch.
		It("bumps the agent epoch only for the claim that wins", func(ctx SpecContext) {
			const rounds, claimers = 40, 12
			for range rounds {
				f := newFixture(ctx)
				enqueue(ctx, f.org, f.agent.ID, f.task.ID)
				start := make(chan struct{})
				won := make(chan store.Run, claimers)
				var wg sync.WaitGroup
				for i := range claimers {
					wg.Go(func() {
						defer GinkgoRecover()
						<-start
						r, err := claim(ctx, f.org, fmt.Sprintf("w%d", i))
						if err == nil {
							won <- r
							return
						}
						Expect(err).To(MatchError(pgx.ErrNoRows))
					})
				}
				close(start)
				wg.Wait()
				close(won)
				Expect(won).To(HaveLen(1))
				winner := <-won
				agent, err := q.GetAgent(ctx, store.GetAgentParams{OrgID: f.org, ID: f.agent.ID})
				Expect(err).NotTo(HaveOccurred())
				Expect(agent.Epoch).To(Equal(winner.Epoch), "a losing claim bumped the agent epoch")
			}
		})

		It("never claims another org's runs", func(ctx SpecContext) {
			f := newFixture(ctx)
			enqueue(ctx, f.org, f.agent.ID, f.task.ID)
			otherOrg, _ := newOrg(ctx)
			_, err := claim(ctx, otherOrg, "w")
			Expect(err).To(MatchError(pgx.ErrNoRows))
		})

		It("cancels a Done task's queued runs only", func(ctx SpecContext) {
			f := newFixture(ctx)
			active := enqueue(ctx, f.org, f.agent.ID, f.task.ID)
			mustClaim(ctx, f.org, "w")
			q1 := enqueue(ctx, f.org, f.agent.ID, f.task.ID)
			q2 := enqueue(ctx, f.org, f.agent.ID, f.task.ID)

			canceled, err := q.CancelQueuedRunsForTask(ctx, store.CancelQueuedRunsForTaskParams{
				OrgID: f.org, TaskID: f.task.ID, OutcomeReason: "task moved to Done",
			})
			Expect(err).NotTo(HaveOccurred())
			ids := []pgtype.UUID{}
			for _, r := range canceled {
				Expect(r.State).To(Equal(store.RunStateCanceled))
				ids = append(ids, r.ID)
			}
			Expect(ids).To(ConsistOf(q1.ID, q2.ID))
			Expect(getRun(ctx, active).State).To(Equal(store.RunStateStarting))
		})
	})

	Describe("run events", func() {
		It("orders events per run and lists after a sequence number", func(ctx SpecContext) {
			r := runIn(ctx, store.RunStateRunning)
			for i := range 5 {
				e, err := q.AppendRunEvent(ctx, store.AppendRunEventParams{
					OrgID: r.OrgID, RunID: r.ID, Epoch: r.Epoch, Kind: "session.update",
					Payload: fmt.Appendf(nil, `{"i":%d}`, i),
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(e.Seq).To(BeEquivalentTo(i + 1))
				Expect(e.Epoch).To(Equal(r.Epoch))
			}
			ctl, err := q.AppendRunControlEvent(ctx, store.AppendRunControlEventParams{
				OrgID: r.OrgID, RunID: r.ID, Kind: "run.cancel_requested", Payload: []byte(`{}`),
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(ctl.Seq).To(BeEquivalentTo(6))

			page, err := q.ListRunEventsAfter(ctx, store.ListRunEventsAfterParams{OrgID: r.OrgID, RunID: r.ID, AfterSeq: 2, MaxEvents: 3})
			Expect(err).NotTo(HaveOccurred())
			Expect(seqs(page)).To(Equal([]int64{3, 4, 5}))

			rest, err := q.ListRunEventsAfter(ctx, store.ListRunEventsAfterParams{OrgID: r.OrgID, RunID: r.ID, AfterSeq: 5, MaxEvents: 100})
			Expect(err).NotTo(HaveOccurred())
			Expect(seqs(rest)).To(Equal([]int64{6}))

			otherOrg, _ := newOrg(ctx)
			none, err := q.ListRunEventsAfter(ctx, store.ListRunEventsAfterParams{OrgID: otherOrg, RunID: r.ID, AfterSeq: 0, MaxEvents: 100})
			Expect(err).NotTo(HaveOccurred())
			Expect(none).To(BeEmpty())
		})

		It("assigns gap-free sequence numbers to concurrent appends", func(ctx SpecContext) {
			r := runIn(ctx, store.RunStateRunning)
			const n = 40
			var wg sync.WaitGroup
			errs := make(chan error, n)
			for i := range n {
				wg.Go(func() {
					_, err := q.AppendRunEvent(ctx, store.AppendRunEventParams{
						OrgID: r.OrgID, RunID: r.ID, Epoch: r.Epoch, Kind: "session.update",
						Payload: fmt.Appendf(nil, `{"i":%d}`, i),
					})
					errs <- err
				})
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				Expect(err).NotTo(HaveOccurred())
			}
			all, err := q.ListRunEventsAfter(ctx, store.ListRunEventsAfterParams{OrgID: r.OrgID, RunID: r.ID, AfterSeq: 0, MaxEvents: 1000})
			Expect(err).NotTo(HaveOccurred())
			want := make([]int64, n)
			for i := range want {
				want[i] = int64(i + 1)
			}
			Expect(seqs(all)).To(Equal(want))
		})

		It("rejects owner appends once the run is no longer active", func(ctx SpecContext) {
			r := runIn(ctx, store.RunStateSucceeded)
			_, err := q.AppendRunEvent(ctx, store.AppendRunEventParams{
				OrgID: r.OrgID, RunID: r.ID, Epoch: r.Epoch, Kind: "session.update", Payload: []byte(`{}`),
			})
			Expect(err).To(MatchError(pgx.ErrNoRows))
		})
	})
})

func seqs(events []store.RunEvent) []int64 {
	out := make([]int64, len(events))
	for i, e := range events {
		out[i] = e.Seq
	}
	return out
}
