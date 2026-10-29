// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"encoding/json"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/stateracloud/koinon/internal/agentd"
	"github.com/stateracloud/koinon/internal/db/store"
)

// The warning before the time limit (issue #183; ADR 0066 amendment 2026-10-28).
var _ = Describe("the time limit", func() {
	// capped enqueues a run with a wall-clock cap of seconds.
	capped := func(ctx SpecContext, e *env, t store.Task, seconds int32) store.Run {
		GinkgoHelper()
		r, err := q.CreateRun(ctx, store.CreateRunParams{
			OrgID: e.org, AgentID: e.agent.ID, TaskID: t.ID, MaxWallClockSeconds: seconds, MaxTokens: 1000,
		})
		Expect(err).NotTo(HaveOccurred())
		return r
	}
	canceledPayload := func(ctx SpecContext, e *env, t store.Task) map[string]any {
		GinkgoHelper()
		for _, ev := range taskEvents(ctx, e, t) {
			if ev.Kind == "run.canceled" {
				var p map[string]any
				Expect(json.Unmarshal(ev.Payload, &p)).To(Succeed())
				return p
			}
		}
		Fail("no run.canceled task event")
		return nil
	}
	notices := func(ctx SpecContext, r store.Run) int {
		n := 0
		for _, k := range kinds(events(ctx, r)) {
			if k == "run.wrap_up_notice" {
				n++
			}
		}
		return n
	}
	withNotice := func(d time.Duration, m *agentd.Metrics) func(*agentd.Config) {
		return func(c *agentd.Config) {
			c.WrapUpNotice = d
			if m != nil {
				c.Metrics = m
			}
		}
	}

	It("tells the agent to wrap up before the time limit and posts the reply at the limit", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		t := e.task(ctx, markWait)
		r := capped(ctx, e, t, 4)
		m := agentd.NewMetrics(prometheus.NewRegistry())
		e.pool("agentd-a", withNotice(2*time.Second, m))

		Eventually(func() []string { return kinds(events(ctx, r)) }).WithTimeout(10 * time.Second).Should(ContainElement("run.wrap_up_notice"))
		Eventually(runState(ctx, r)).WithTimeout(15 * time.Second).Should(Equal(store.RunStateCanceled))
		prompts, _, cancels, _ := e.harness.snapshot()
		Expect(prompts).To(HaveLen(2))
		Expect(prompts[1]).To(HavePrefix(wrapUpMarker))
		Expect(prompts[1]).To(ContainSubstring("time limit of 4s"))
		Expect(cancels).To(Equal(2), "one to interrupt the turn for the notice, one at the limit")
		Expect(getRun(ctx, r).OutcomeReason.String).To(ContainSubstring("wall-clock cap"))

		p := canceledPayload(ctx, e, t)
		Expect(p).To(HaveKeyWithValue("reason", "wall_clock_cap"))
		Expect(p).To(HaveKeyWithValue("summarySource", "reply"))
		Expect(p["summary"]).To(ContainSubstring("Done so far: A."))
		Expect(p["summary"]).To(ContainSubstring("working"), "the reply is the text since the last park")
		Expect(getRun(ctx, r).WrapUpNoticeAt.Valid).To(BeTrue())
		Expect(testutil.ToFloat64(m.WrapUpNotices)).To(Equal(1.0))
		Eventually(func() float64 { return testutil.ToFloat64(m.TimeLimitStops) }).Should(Equal(1.0))
	})

	It("sends no notice when the cap is not longer than the notice", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		t := e.task(ctx, markWait)
		r := capped(ctx, e, t, 4)
		e.pool("agentd-a", withNotice(10*time.Second, nil))
		Eventually(runState(ctx, r)).WithTimeout(15 * time.Second).Should(Equal(store.RunStateCanceled))
		prompts, _, _, _ := e.harness.snapshot()
		Expect(prompts).To(HaveLen(1))
		Expect(notices(ctx, r)).To(BeZero())
		Expect(getRun(ctx, r).WrapUpNoticeAt.Valid).To(BeFalse())
	})

	It("prepends the notice when a turn starts inside the window", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		t := e.task(ctx, markWait)
		r := capped(ctx, e, t, 8)
		claimed, err := q.ClaimNextRun(ctx, store.ClaimNextRunParams{OrgID: e.org, OwnerID: "dead-owner", LeaseSeconds: 60})
		Expect(err).NotTo(HaveOccurred())
		Expect(claimed.ID).To(Equal(r.ID))
		_, err = q.MarkRunRunning(ctx, store.MarkRunRunningParams{OrgID: e.org, ID: r.ID, Epoch: claimed.Epoch})
		Expect(err).NotTo(HaveOccurred())
		_, err = db.Exec(ctx, `UPDATE runs SET started_at = now() - interval '4 seconds', lease_expires_at = now() - interval '1 minute'
			WHERE org_id = $1 AND id = $2`, e.org, r.ID)
		Expect(err).NotTo(HaveOccurred())

		e.pool("agentd-a", withNotice(5*time.Second, nil))
		Eventually(func() int { p, _, _, _ := e.harness.snapshot(); return len(p) }).WithTimeout(10 * time.Second).Should(Equal(1))
		prompts, _, cancels, _ := e.harness.snapshot()
		Expect(prompts[0]).To(ContainSubstring(wrapUpMarker))
		Expect(cancels).To(BeZero(), "the turn was not interrupted for the notice")
		Eventually(runState(ctx, r)).WithTimeout(15 * time.Second).Should(Equal(store.RunStateCanceled))
		Expect(notices(ctx, r)).To(Equal(1))
	})

	It("records no notice for a run taken over after its cap has passed", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		t := e.task(ctx, markWait)
		r := capped(ctx, e, t, 8)
		claimed, err := q.ClaimNextRun(ctx, store.ClaimNextRunParams{OrgID: e.org, OwnerID: "dead-owner", LeaseSeconds: 60})
		Expect(err).NotTo(HaveOccurred())
		Expect(claimed.ID).To(Equal(r.ID))
		_, err = q.MarkRunRunning(ctx, store.MarkRunRunningParams{OrgID: e.org, ID: r.ID, Epoch: claimed.Epoch})
		Expect(err).NotTo(HaveOccurred())
		_, err = db.Exec(ctx, `UPDATE runs SET started_at = now() - interval '1 minute', lease_expires_at = now() - interval '1 minute'
			WHERE org_id = $1 AND id = $2`, e.org, r.ID)
		Expect(err).NotTo(HaveOccurred())

		m := agentd.NewMetrics(prometheus.NewRegistry())
		e.pool("agentd-a", withNotice(5*time.Second, m))
		Eventually(runState(ctx, r)).WithTimeout(15 * time.Second).Should(Equal(store.RunStateCanceled))
		prompts, _, _, _ := e.harness.snapshot()
		for _, p := range prompts {
			Expect(p).NotTo(ContainSubstring(wrapUpMarker), "the agent was never told to wrap up")
		}
		Expect(notices(ctx, r)).To(BeZero(), "no run.wrap_up_notice event for a notice nobody got")
		Expect(getRun(ctx, r).WrapUpNoticeAt.Valid).To(BeFalse())
		Expect(testutil.ToFloat64(m.WrapUpNotices)).To(BeZero())
	})

	It("honours an end of turn that races the notice", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		e.harness.set(func(h *harness) { h.endOnCancel = true })
		t := e.task(ctx, markWait)
		r := capped(ctx, e, t, 4)
		e.pool("agentd-a", withNotice(2*time.Second, nil))
		Eventually(runState(ctx, r)).WithTimeout(15 * time.Second).Should(Equal(store.RunStateSucceeded))
		prompts, _, _, _ := e.harness.snapshot()
		Expect(prompts).To(HaveLen(1), "no wrap-up prompt follows an end of turn")
		Expect(e.harness.maxPromptsInFlight()).To(Equal(1))
		Expect(notices(ctx, r)).To(BeZero())
	})

	It("sends no second prompt when the harness ignores the cancel", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		e.harness.set(func(h *harness) { h.ignoreCancel = true })
		t := e.task(ctx, markWait)
		r := capped(ctx, e, t, 4)
		e.pool("agentd-a", withNotice(2*time.Second, nil), func(c *agentd.Config) { c.CancelGrace = 300 * time.Millisecond })
		Eventually(runState(ctx, r)).WithTimeout(15 * time.Second).Should(Equal(store.RunStateCanceled))
		prompts, _, _, _ := e.harness.snapshot()
		Expect(prompts).To(HaveLen(1))
		Expect(e.harness.maxPromptsInFlight()).To(Equal(1))
		Expect(getRun(ctx, r).WrapUpNoticeAt.Valid).To(BeTrue(), "no new notice is armed")
		Expect(notices(ctx, r)).To(BeZero(), "no notice was sent, so none is recorded")
		Expect(canceledPayload(ctx, e, t)).To(HaveKeyWithValue("reason", "wall_clock_cap"))
	})

	It("ends the run at the cap, not as failed, when the harness honours the cancel after the grace", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		e.harness.set(func(h *harness) { h.lateCancel = 800 * time.Millisecond })
		t := e.task(ctx, markWait)
		r := capped(ctx, e, t, 4)
		e.pool("agentd-a", withNotice(2*time.Second, nil), func(c *agentd.Config) { c.CancelGrace = 300 * time.Millisecond })
		Eventually(runState(ctx, r)).WithTimeout(15 * time.Second).Should(Equal(store.RunStateCanceled))
		prompts, _, _, _ := e.harness.snapshot()
		Expect(prompts).To(HaveLen(1))
		Expect(notices(ctx, r)).To(BeZero())
		Expect(canceledPayload(ctx, e, t)).To(HaveKeyWithValue("reason", "wall_clock_cap"))
	})

	DescribeTable("tells the agent about a tool call refused while the wrap-up cancel was in flight",
		func(ctx SpecContext, late time.Duration) {
			e := newEnv(ctx, store.AgentFlavorOpencode)
			e.harness.set(func(h *harness) { h.lateCancel = late; h.refuseAfterCancel = true })
			t := e.task(ctx, markWait)
			r := capped(ctx, e, t, 4)
			e.pool("agentd-a", withNotice(2*time.Second, nil), func(c *agentd.Config) { c.CancelGrace = 300 * time.Millisecond })
			Eventually(func() int {
				prompts, _, _, _ := e.harness.snapshot()
				return len(prompts)
			}).WithTimeout(15 * time.Second).Should(BeNumerically(">=", 2))
			prompts, _, _, _ := e.harness.snapshot()
			Expect(prompts[1]).To(ContainSubstring("Your last action was refused"))
			// The refusal note is handled like any other: the run goes on and ends (the harness
			// answers the note with an end of turn, or the cap comes first), but never as failed.
			Eventually(runState(ctx, r)).WithTimeout(15 * time.Second).Should(BeElementOf(store.RunStateSucceeded, store.RunStateCanceled))
			Expect(getRun(ctx, r).FailureClass.Valid).To(BeFalse(), "a refusal is not a fatal failure")
		},
		Entry("the harness ends the turn within the grace", time.Duration(0)),
		Entry("the harness ends the turn after the grace", 800*time.Millisecond),
	)

	It("uses the task's notes when the agent said nothing", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		e.harness.set(func(h *harness) { h.noChunk = true })
		t := e.task(ctx, markWait)
		_, err := db.Exec(ctx, `UPDATE tasks SET agent_summary = 'notes text', agent_summary_at = now() WHERE org_id = $1 AND id = $2`, e.org, t.ID)
		Expect(err).NotTo(HaveOccurred())
		r := capped(ctx, e, t, 2)
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(15 * time.Second).Should(Equal(store.RunStateCanceled))
		p := canceledPayload(ctx, e, t)
		Expect(p).To(HaveKeyWithValue("summary", "notes text"))
		Expect(p).To(HaveKeyWithValue("summarySource", "notes"))
		Expect(strings.TrimSpace(p["message"].(string))).To(Equal("Stopped at the time limit of 2s."))
	})
})
