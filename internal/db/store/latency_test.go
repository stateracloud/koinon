// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/orgsettings"
)

// Latency defaults (plan item LT1): interactive work is claimed before scheduled work, a run whose
// sandbox is not ready goes back to the queue, and the org's tunables fall back to the platform's.
var _ = Describe("latency defaults", func() {
	enqueueOrigin := func(ctx SpecContext, f fixture, origin store.RunOrigin) store.Run {
		GinkgoHelper()
		agent, _ := newAgent(ctx, f.org, f.user)
		task := newTask(ctx, f.org, f.user, agent.ID)
		r, err := q.CreateRun(ctx, store.CreateRunParams{
			OrgID: f.org, AgentID: agent.ID, TaskID: task.ID, MaxWallClockSeconds: 1800,
			Origin: store.NullRunOrigin{RunOrigin: origin, Valid: true},
		})
		Expect(err).NotTo(HaveOccurred())
		return r
	}

	It("defaults a run to interactive", func(ctx SpecContext) {
		f := newFixture(ctx)
		Expect(enqueue(ctx, f.org, f.agent.ID, f.task.ID).Origin).To(Equal(store.RunOriginInteractive))
	})

	It("claims an interactive run before a scheduled one queued earlier", func(ctx SpecContext) {
		f := newFixture(ctx)
		scheduled := enqueueOrigin(ctx, f, store.RunOriginScheduled)
		scheduled2 := enqueueOrigin(ctx, f, store.RunOriginScheduled)
		interactive := enqueueOrigin(ctx, f, store.RunOriginInteractive)

		Expect(mustClaim(ctx, f.org, "agentd-a").ID).To(Equal(interactive.ID))
		Expect(mustClaim(ctx, f.org, "agentd-a").ID).To(Equal(scheduled.ID), "then FIFO among the scheduled")
		Expect(mustClaim(ctx, f.org, "agentd-a").ID).To(Equal(scheduled2.ID))
	})

	It("puts a starting run back in the queue, behind its origin's older runs, and a later claim takes a new epoch", func(ctx SpecContext) {
		f := newFixture(ctx)
		first := enqueue(ctx, f.org, f.agent.ID, f.task.ID)
		other := enqueueOrigin(ctx, f, store.RunOriginInteractive)
		claimed := mustClaim(ctx, f.org, "agentd-a")
		Expect(claimed.ID).To(Equal(first.ID))

		r, err := q.RequeueRun(ctx, store.RequeueRunParams{OrgID: f.org, ID: claimed.ID, Epoch: claimed.Epoch})
		Expect(err).NotTo(HaveOccurred())
		Expect(r.State).To(Equal(store.RunStateQueued))
		Expect(r.OwnerID.Valid).To(BeFalse())
		Expect(r.LeaseExpiresAt.Valid).To(BeFalse())
		Expect(r.ReadyAt.Valid).To(BeTrue())

		_, err = q.RequeueRun(ctx, store.RequeueRunParams{OrgID: f.org, ID: claimed.ID, Epoch: claimed.Epoch})
		Expect(err).To(MatchError(pgx.ErrNoRows), "only a starting run, only by its owner")

		Expect(mustClaim(ctx, f.org, "agentd-b").ID).To(Equal(other.ID))
		again := mustClaim(ctx, f.org, "agentd-b")
		Expect(again.ID).To(Equal(first.ID))
		Expect(again.Epoch).To(BeNumerically(">", claimed.Epoch))
	})

	It("does not count the wait for a sandbox against the wall-clock cap", func(ctx SpecContext) {
		f := newFixture(ctx)
		enqueue(ctx, f.org, f.agent.ID, f.task.ID)
		claimed := mustClaim(ctx, f.org, "agentd-a")
		// The run waits 40 minutes for its sandbox, longer than its 30 minute cap, then goes back.
		_, err := pool.Exec(ctx, `UPDATE runs SET claimed_at = claimed_at - interval '40 minutes',
			started_at = started_at - interval '40 minutes' WHERE org_id = $1 AND id = $2`, f.org, claimed.ID)
		Expect(err).NotTo(HaveOccurred())
		r, err := q.RequeueRun(ctx, store.RequeueRunParams{OrgID: f.org, ID: claimed.ID, Epoch: claimed.Epoch})
		Expect(err).NotTo(HaveOccurred())
		Expect(r.ParkedSeconds).To(BeNumerically(">=", 40*60))

		again := mustClaim(ctx, f.org, "agentd-b")
		Expect(again.ID).To(Equal(claimed.ID))
		swept, err := q.CancelRunsOverWallClock(ctx, store.CancelRunsOverWallClockParams{OrgID: f.org, OutcomeReason: "wall-clock cap"})
		Expect(err).NotTo(HaveOccurred())
		Expect(swept).To(BeEmpty(), "the time in the queue was credited")
	})

	Describe("the sandbox wait cap", func() {
		waitStart := func(ctx SpecContext, f fixture, id pgtype.UUID) pgtype.Timestamptz {
			GinkgoHelper()
			var t pgtype.Timestamptz
			Expect(pool.QueryRow(ctx, `SELECT sandbox_wait_started_at FROM runs WHERE org_id = $1 AND id = $2`, f.org, id).Scan(&t)).To(Succeed())
			return t
		}

		It("keeps the sandbox wait start across a requeue", func(ctx SpecContext) {
			f := newFixture(ctx)
			enqueue(ctx, f.org, f.agent.ID, f.task.ID)
			claimed := mustClaim(ctx, f.org, "agentd-a")
			t0 := time.Now().Add(-10 * time.Minute).Truncate(time.Microsecond)
			got, err := q.MarkRunSandboxWait(ctx, store.MarkRunSandboxWaitParams{
				OrgID: f.org, ID: claimed.ID, Epoch: claimed.Epoch, Started: pgtype.Timestamptz{Time: t0, Valid: true},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(got.Time).To(BeTemporally("==", t0))
			got, err = q.MarkRunSandboxWait(ctx, store.MarkRunSandboxWaitParams{
				OrgID: f.org, ID: claimed.ID, Epoch: claimed.Epoch, Started: pgtype.Timestamptz{Time: t0.Add(time.Minute), Valid: true},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(got.Time).To(BeTemporally("==", t0), "the first requeue keeps the start")

			_, err = q.RequeueRun(ctx, store.RequeueRunParams{OrgID: f.org, ID: claimed.ID, Epoch: claimed.Epoch})
			Expect(err).NotTo(HaveOccurred())
			again := mustClaim(ctx, f.org, "agentd-b")
			Expect(waitStart(ctx, f, again.ID).Time).To(BeTemporally("==", t0))
			markRunning(ctx, again)
			Expect(waitStart(ctx, f, again.ID).Time).To(BeTemporally("==", t0), "only ClearSandboxWaitForAgent clears it")

			_, err = q.MarkRunSandboxWait(ctx, store.MarkRunSandboxWaitParams{
				OrgID: f.org, ID: again.ID, Epoch: again.Epoch - 1, Started: pgtype.Timestamptz{Time: t0, Valid: true},
			})
			Expect(err).To(MatchError(pgx.ErrNoRows), "fenced by the epoch")
		})

		It("clears the waits of all of an agent's runs and no other agent's", func(ctx SpecContext) {
			f := newFixture(ctx)
			queued := enqueue(ctx, f.org, f.agent.ID, f.task.ID)
			other := newTask(ctx, f.org, f.user, f.agent.ID)
			second := enqueue(ctx, f.org, f.agent.ID, other.ID)
			otherAgent, _ := newAgent(ctx, f.org, f.user)
			otherTask := newTask(ctx, f.org, f.user, otherAgent.ID)
			foreign := enqueue(ctx, f.org, otherAgent.ID, otherTask.ID)
			_, err := pool.Exec(ctx, `UPDATE runs SET sandbox_wait_started_at = now() WHERE org_id = $1 AND id = ANY($2)`,
				f.org, []pgtype.UUID{queued.ID, second.ID, foreign.ID})
			Expect(err).NotTo(HaveOccurred())
			n, err := q.ClearSandboxWaitForAgent(ctx, store.ClearSandboxWaitForAgentParams{OrgID: f.org, AgentID: f.agent.ID})
			Expect(err).NotTo(HaveOccurred())
			Expect(n).To(BeEquivalentTo(2))
			Expect(waitStart(ctx, f, queued.ID).Valid).To(BeFalse())
			Expect(waitStart(ctx, f, second.ID).Valid).To(BeFalse())
			Expect(waitStart(ctx, f, foreign.ID).Valid).To(BeTrue())
		})

		It("records the wrap-up notice once", func(ctx SpecContext) {
			f := newFixture(ctx)
			enqueue(ctx, f.org, f.agent.ID, f.task.ID)
			r := markRunning(ctx, mustClaim(ctx, f.org, "agentd-a"))
			n, err := q.MarkRunWrapUpNotice(ctx, store.MarkRunWrapUpNoticeParams{OrgID: f.org, ID: r.ID, Epoch: r.Epoch + 1})
			Expect(err).NotTo(HaveOccurred())
			Expect(n).To(BeZero(), "a stale epoch records nothing")
			n, err = q.MarkRunWrapUpNotice(ctx, store.MarkRunWrapUpNoticeParams{OrgID: f.org, ID: r.ID, Epoch: r.Epoch})
			Expect(err).NotTo(HaveOccurred())
			Expect(n).To(BeEquivalentTo(1))
			n, err = q.MarkRunWrapUpNotice(ctx, store.MarkRunWrapUpNoticeParams{OrgID: f.org, ID: r.ID, Epoch: r.Epoch})
			Expect(err).NotTo(HaveOccurred())
			Expect(n).To(BeZero())
		})
	})

	It("does not requeue a running run", func(ctx SpecContext) {
		r := runIn(ctx, store.RunStateRunning)
		_, err := q.RequeueRun(ctx, store.RequeueRunParams{OrgID: r.OrgID, ID: r.ID, Epoch: r.Epoch})
		Expect(err).To(MatchError(pgx.ErrNoRows))
	})

	It("reports a run parked on input_required as warm, and one parked on approval as not", func(ctx SpecContext) {
		for reason, warm := range map[store.RunParkReason]bool{
			store.RunParkReasonInputRequired: true, store.RunParkReasonA2a: true,
			store.RunParkReasonApproval: false, store.RunParkReasonBudget: false,
		} {
			f := newFixture(ctx)
			enqueue(ctx, f.org, f.agent.ID, f.task.ID)
			r := markRunning(ctx, mustClaim(ctx, f.org, "agentd-a"))
			_, err := q.ParkRun(ctx, store.ParkRunParams{OrgID: f.org, ID: r.ID, Epoch: r.Epoch, ParkReason: reason})
			Expect(err).NotTo(HaveOccurred())
			act, err := q.GetAgentRunActivity(ctx, store.GetAgentRunActivityParams{OrgID: f.org, AgentID: f.agent.ID})
			Expect(err).NotTo(HaveOccurred())
			Expect(act.WarmSince.Valid).To(Equal(warm), string(reason))
		}
	})

	Describe("org settings", func() {
		It("falls back to the platform defaults and applies overrides per value", func(ctx SpecContext) {
			f := newFixture(ctx)
			def := orgsettings.Defaults()
			got, err := orgsettings.Get(ctx, q, f.org, def)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(orgsettings.Settings{
				IdleTimeout: 10 * time.Minute, ParkWarm: 15 * time.Minute, QueueWait: 5 * time.Minute,
				MaxConcurrentRuns: 20, TriggerConcurrency: 4, TriggerRatePerMinute: 60,
				SandboxWaitCap: time.Hour, RunContentRetention: 30 * 24 * time.Hour,
				HomeBackupRetention:   7 * 24 * time.Hour,
				ContinuationTaskSpend: orgsettings.DefaultContinuationTaskSpend,
			}))

			warm := 30 * time.Minute
			Expect(orgsettings.Set(ctx, q, f.org, orgsettings.Override{ParkWarm: &warm})).To(Succeed())
			got, err = orgsettings.Get(ctx, q, f.org, def)
			Expect(err).NotTo(HaveOccurred())
			Expect(got.ParkWarm).To(Equal(30 * time.Minute))
			Expect(got.IdleTimeout).To(Equal(10 * time.Minute))

			capd := 20 * time.Minute
			Expect(orgsettings.Set(ctx, q, f.org, orgsettings.Override{SandboxWaitCap: &capd})).To(Succeed())
			got, err = orgsettings.Get(ctx, q, f.org, def)
			Expect(err).NotTo(HaveOccurred())
			Expect(got.SandboxWaitCap).To(Equal(20 * time.Minute))
			tooShortCap := time.Minute
			Expect(orgsettings.Set(ctx, q, f.org, orgsettings.Override{SandboxWaitCap: &tooShortCap})).NotTo(Succeed(), "the cap is at least 5 minutes")

			Expect(orgsettings.Set(ctx, q, f.org, orgsettings.Override{})).To(Succeed())
			got, _ = orgsettings.Get(ctx, q, f.org, def)
			Expect(got).To(Equal(def), "clearing an override restores the default")
		})

		It("rejects values outside the bounds", func(ctx SpecContext) {
			f := newFixture(ctx)
			tooShort := time.Second
			Expect(orgsettings.Set(ctx, q, f.org, orgsettings.Override{IdleTimeout: &tooShort})).NotTo(Succeed())
		})
	})
})
