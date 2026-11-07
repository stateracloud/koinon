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

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/agentd"
	"github.com/stateracloud/koinon/internal/db/store"
)

// The warning before the time limit (old issue 183; ADR 0066 amendment 2026-10-28).
var _ = Describe("the time limit", func() {
	// capped enqueues a run with a wall-clock cap of seconds.
	capped := func(ctx SpecContext, e *env, t store.Task, seconds int32) store.Run {
		GinkgoHelper()
		r, err := q.CreateRun(ctx, store.CreateRunParams{
			OrgID: e.org, AgentID: e.agent.ID, TaskID: t.ID, MaxWallClockSeconds: seconds,
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
		Expect(testutil.ToFloat64(m.WrapUpNotices.WithLabelValues("time"))).To(Equal(1.0))
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
		Expect(testutil.ToFloat64(m.WrapUpNotices.WithLabelValues("time"))).To(BeZero())
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

// The warning before the model-request and spend limits (ADR 0081): at about 80% of whichever
// limit is nearest, read from the ledger while a turn runs. The ledger, not agentd, ends the run at
// the limit; these specs stop at the notice.
var _ = Describe("the request and spend limits", func() {
	// limited enqueues a run with a long time limit and the given request and spend limits.
	limited := func(ctx SpecContext, e *env, t store.Task, requests int64, spend string) store.Run {
		GinkgoHelper()
		p := store.CreateRunParams{OrgID: e.org, AgentID: e.agent.ID, TaskID: t.ID, MaxWallClockSeconds: 3600}
		if requests > 0 {
			p.MaxModelRequests = pgtype.Int8{Int64: requests, Valid: true}
		}
		if spend != "" {
			p.MaxSpendUsd = pgtype.Text{String: spend, Valid: true}
		}
		r, err := q.CreateRun(ctx, p)
		Expect(err).NotTo(HaveOccurred())
		return r
	}
	// spend adds n ledger entries of cost each to the run, as the ledger would.
	spend := func(ctx SpecContext, e *env, r store.Run, n int, cost string) {
		GinkgoHelper()
		for range n {
			_, err := db.Exec(ctx, `INSERT INTO spend_entries (org_id, agent_id, run_id, task_id, request_id, key_id, model,
				input_tokens, output_tokens, cost_usd, priced, occurred_at)
				VALUES ($1, $2, $3, $4, gen_random_uuid()::text, 'k-1', 'm', 100, 20, $5::numeric, true, now())`,
				e.org, e.agent.ID, r.ID, r.TaskID, cost)
			Expect(err).NotTo(HaveOccurred())
		}
	}
	noticeOf := func(ctx SpecContext, r store.Run) map[string]any {
		for _, ev := range events(ctx, r) {
			if ev.Kind == "run.wrap_up_notice" {
				var p map[string]any
				Expect(json.Unmarshal(ev.Payload, &p)).To(Succeed())
				return p
			}
		}
		return nil
	}
	fast := func(m *agentd.Metrics) func(*agentd.Config) {
		return func(c *agentd.Config) { c.UsagePoll = 100 * time.Millisecond; c.Metrics = m }
	}

	It("tells the agent at 80% of its model requests, once", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		t := e.task(ctx, markWait)
		r := limited(ctx, e, t, 10, "")
		m := agentd.NewMetrics(prometheus.NewRegistry())
		e.pool("agentd-a", fast(m))
		Eventually(func() int { p, _, _, _ := e.harness.snapshot(); return len(p) }).WithTimeout(10 * time.Second).Should(Equal(1))
		spend(ctx, e, r, 7, "0.001")
		Consistently(func() map[string]any { return noticeOf(ctx, r) }, "1s", "100ms").Should(BeNil(), "7 of 10 is below 80%")
		spend(ctx, e, r, 1, "0.001")
		Eventually(func() map[string]any { return noticeOf(ctx, r) }).WithTimeout(10 * time.Second).ShouldNot(BeNil())
		p := noticeOf(ctx, r)
		Expect(p).To(HaveKeyWithValue("limit", "requests"))
		Expect(p["modelRequests"]).To(BeEquivalentTo(8))
		Expect(p["maxModelRequests"]).To(BeEquivalentTo(10))
		Eventually(func() int { p, _, _, _ := e.harness.snapshot(); return len(p) }).Should(Equal(2))
		prompts, _, _, _ := e.harness.snapshot()
		Expect(prompts[1]).To(HavePrefix(limitWrapUpMarker))
		Expect(prompts[1]).To(ContainSubstring("8 of the 10 model requests"))
		spend(ctx, e, r, 1, "0.001")
		Consistently(func() int { p, _, _, _ := e.harness.snapshot(); return len(p) }, "1s", "100ms").Should(Equal(2), "the notice is sent once")
		Expect(testutil.ToFloat64(m.WrapUpNotices.WithLabelValues("requests"))).To(Equal(1.0))
		Expect(testutil.ToFloat64(m.WrapUpNotices.WithLabelValues("time"))).To(BeZero())
	})

	It("tells the agent at 80% of its spend limit", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		t := e.task(ctx, markWait)
		r := limited(ctx, e, t, 1000, "1.00")
		m := agentd.NewMetrics(prometheus.NewRegistry())
		e.pool("agentd-a", fast(m))
		Eventually(func() int { p, _, _, _ := e.harness.snapshot(); return len(p) }).WithTimeout(10 * time.Second).Should(Equal(1))
		spend(ctx, e, r, 1, "0.79")
		Consistently(func() map[string]any { return noticeOf(ctx, r) }, "1s", "100ms").Should(BeNil())
		spend(ctx, e, r, 1, "0.02")
		Eventually(func() map[string]any { return noticeOf(ctx, r) }).WithTimeout(10 * time.Second).ShouldNot(BeNil())
		p := noticeOf(ctx, r)
		Expect(p).To(HaveKeyWithValue("limit", "spend"))
		Expect(p).To(HaveKeyWithValue("spentUSD", "0.81"))
		Expect(p).To(HaveKeyWithValue("maxSpendUSD", "1.00"))
		Eventually(func() int { p, _, _, _ := e.harness.snapshot(); return len(p) }).Should(Equal(2))
		prompts, _, _, _ := e.harness.snapshot()
		Expect(prompts[1]).To(ContainSubstring("$0.81 of the $1.00 it may spend"))
		Expect(testutil.ToFloat64(m.WrapUpNotices.WithLabelValues("spend"))).To(Equal(1.0))
	})

	It("names the limit that is nearest when two are in their window", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		t := e.task(ctx, markWait)
		r := limited(ctx, e, t, 10, "1.00")
		// 9 of 10 requests (90%) and $0.85 of $1.00 (85%), both over 80%, are in the ledger before
		// the first turn is sent: the notice is prepended to it, and names the nearer, the requests.
		spend(ctx, e, r, 8, "0.001")
		spend(ctx, e, r, 1, "0.842")
		e.pool("agentd-a", fast(agentd.NewMetrics(prometheus.NewRegistry())))
		Eventually(func() int { p, _, _, _ := e.harness.snapshot(); return len(p) }).WithTimeout(10 * time.Second).Should(BeNumerically(">=", 1))
		prompts, _, _, _ := e.harness.snapshot()
		Expect(prompts[0]).To(ContainSubstring("9 of the 10 model requests"))
		Expect(noticeOf(ctx, r)).To(HaveKeyWithValue("limit", "requests"))
	})

	It("sends no notice for a run with neither a request nor a spend limit", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		t := e.task(ctx, markWait)
		r := limited(ctx, e, t, 0, "")
		spend(ctx, e, r, 50, "0.5")
		e.pool("agentd-a", fast(agentd.NewMetrics(prometheus.NewRegistry())))
		Eventually(func() int { p, _, _, _ := e.harness.snapshot(); return len(p) }).WithTimeout(10 * time.Second).Should(Equal(1))
		Consistently(func() map[string]any { return noticeOf(ctx, r) }, "1s", "100ms").Should(BeNil())
	})
})
