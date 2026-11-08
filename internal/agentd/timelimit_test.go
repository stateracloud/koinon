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
		Expect(notices(ctx, r)).To(Equal(1))
		Expect(testutil.ToFloat64(m.WrapUpNotices.WithLabelValues("time"))).To(Equal(1.0))
		Eventually(func() float64 { return testutil.ToFloat64(m.TimeLimitStops) }).Should(Equal(1.0))
	})

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

	It("tells the agent at 80% of its model requests", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		t := e.task(ctx, markWait)
		r := limited(ctx, e, t, 10, "")
		m := agentd.NewMetrics(prometheus.NewRegistry())
		e.pool("agentd-a", fast(m))
		Eventually(func() int { p, _, _, _ := e.harness.snapshot(); return len(p) }).WithTimeout(10 * time.Second).Should(Equal(1))
		spend(ctx, e, r, 8, "0.001")
		Eventually(func() map[string]any { return noticeOf(ctx, r) }).WithTimeout(10 * time.Second).ShouldNot(BeNil())
		p := noticeOf(ctx, r)
		Expect(p).To(HaveKeyWithValue("limit", "requests"))
		Expect(p["modelRequests"]).To(BeEquivalentTo(8))
		Expect(p["maxModelRequests"]).To(BeEquivalentTo(10))
		Eventually(func() int { p, _, _, _ := e.harness.snapshot(); return len(p) }).Should(Equal(2))
		prompts, _, _, _ := e.harness.snapshot()
		Expect(prompts[1]).To(HavePrefix(limitWrapUpMarker))
		Expect(prompts[1]).To(ContainSubstring("8 of the 10 model requests"))
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
		spend(ctx, e, r, 1, "0.81")
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
})
