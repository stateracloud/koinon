// SPDX-License-Identifier: Apache-2.0

package ledger_test

import (
	"encoding/json"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/ledger"
	"github.com/stateracloud/koinon/internal/scheduler"
)

// Run limits (issue #243, ADR 0080): a run ends at the first of its model-request limit, its spend
// limit and its time limit. Tokens are reported, not enforced.
var _ = Describe("run limits", func() {
	BeforeEach(needDocker)

	// cacheHeavy is a request of a long agent loop: a big prompt read from the provider's cache, a
	// short answer, a fraction of a cent.
	cacheHeavy := func(e *env, cost string) ledger.Entry {
		x := e.entry(time.Now(), cost, 180_000, 400)
		x.CacheReadTokens, x.CacheWriteTokens = 178_000, 1_500
		return x
	}
	canceledPayload := func(ctx SpecContext, e *env, r store.Run) map[string]any {
		GinkgoHelper()
		var raw []byte
		Expect(db.QueryRow(ctx, `SELECT payload FROM task_events WHERE org_id = $1 AND run_id = $2 AND kind = 'run.canceled'`,
			e.org, r.ID).Scan(&raw)).To(Succeed())
		var p map[string]any
		Expect(json.Unmarshal(raw, &p)).To(Succeed())
		return p
	}

	It("does not cancel a cache-heavy run of many requests below its request and spend limits (issue #243)", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "10.00")
		r := e.startLimitedRun(ctx, e.task(ctx), runLimits{requests: 100, spendUSD: "2.00"}, time.Now().Add(-time.Minute))
		var batch []ledger.Entry
		for range 45 {
			batch = append(batch, cacheHeavy(e, "0.012"))
		}
		_, err := ing.Ingest(ctx, batch)
		Expect(err).NotTo(HaveOccurred())

		run := e.run(ctx, r.ID)
		Expect(run.State).To(Equal(store.RunStateRunning), "45 requests and $0.54 are below 100 and $2.00")
		Expect(run.TokensUsed).To(BeNumerically(">", 8_000_000), "far over the 2M-token cap that stopped the rc.5 research runs")
	})

	It("reports the tokens of a run split by type and cancels on none of them", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "10.00")
		r := e.startLimitedRun(ctx, e.task(ctx), runLimits{requests: 100, spendUSD: "2.00"}, time.Now().Add(-time.Minute))
		// A run queued before run limits carried a token cap; it is stored and no longer read.
		_, err := db.Exec(ctx, `UPDATE runs SET max_tokens = 1000 WHERE org_id = $1 AND id = $2`, e.org, r.ID)
		Expect(err).NotTo(HaveOccurred())
		x := cacheHeavy(e, "0.01")
		x.ReasoningTokens = 120
		_, err = ing.Ingest(ctx, []ledger.Entry{x})
		Expect(err).NotTo(HaveOccurred())

		Expect(e.run(ctx, r.ID).State).To(Equal(store.RunStateRunning), "the deprecated max_tokens cancels nothing")
		u, err := q.GetRunUsage(ctx, store.GetRunUsageParams{OrgID: e.org, RunID: r.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect([]int64{u.Requests, u.InputTokens, u.CacheReadTokens, u.CacheWriteTokens, u.OutputTokens, u.ReasoningTokens}).
			To(Equal([]int64{1, 180_000, 178_000, 1_500, 400, 120}))
	})

	It("cancels the run when it has made as many model requests as its limit", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "10.00")
		r := e.startLimitedRun(ctx, e.task(ctx), runLimits{requests: 5, spendUSD: "2.00"}, time.Now().Add(-time.Minute))
		for i := 1; i <= 4; i++ {
			_, err := ing.Ingest(ctx, []ledger.Entry{e.entry(time.Now(), "0.001", 10, 1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(e.run(ctx, r.ID).State).To(Equal(store.RunStateRunning), fmt.Sprintf("after request %d of 5", i))
		}
		_, err := ing.Ingest(ctx, []ledger.Entry{e.entry(time.Now(), "0.001", 10, 1)})
		Expect(err).NotTo(HaveOccurred())

		run := e.run(ctx, r.ID)
		Expect(run.State).To(Equal(store.RunStateCanceled))
		Expect(run.OutcomeReason.String).To(Equal("model request limit reached (5 of 5 requests)"))
		Expect(run.FailureClass.RunFailureClass).To(Equal(store.RunFailureClassBudget))
		p := canceledPayload(ctx, e, run)
		Expect(p).To(HaveKeyWithValue("reason", "request_cap"))
		Expect(p).To(HaveKeyWithValue("limit", "requests"))
		Expect(p["model_requests"]).To(BeEquivalentTo(5))
	})

	It("counts every request through the key, estimated ones and a failed one included", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "10.00")
		r := e.startLimitedRun(ctx, e.task(ctx), runLimits{requests: 3}, time.Now().Add(-time.Minute))
		failed := e.entry(time.Now(), "", 0, 0)
		failed.Status = 502
		_, err := ing.Ingest(ctx, []ledger.Entry{e.entry(time.Now(), "0.001", 10, 1), failed, e.entry(time.Now(), "0.001", 10, 1)})
		Expect(err).NotTo(HaveOccurred())
		Expect(e.run(ctx, r.ID).State).To(Equal(store.RunStateCanceled))
	})

	It("cancels the run when its settled spend reaches the spend limit, and names it", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "10.00")
		r := e.startLimitedRun(ctx, e.task(ctx), runLimits{requests: 100, spendUSD: "0.05"}, time.Now().Add(-time.Minute))
		_, err := ing.Ingest(ctx, []ledger.Entry{e.entry(time.Now(), "0.02", 10, 1), e.entry(time.Now(), "0.02", 10, 1)})
		Expect(err).NotTo(HaveOccurred())
		Expect(e.run(ctx, r.ID).State).To(Equal(store.RunStateRunning), "$0.04 of $0.05")

		_, err = ing.Ingest(ctx, []ledger.Entry{e.entry(time.Now(), "0.011", 10, 1)})
		Expect(err).NotTo(HaveOccurred())
		run := e.run(ctx, r.ID)
		Expect(run.State).To(Equal(store.RunStateCanceled))
		Expect(run.OutcomeReason.String).To(Equal("run spend limit reached ($0.051 of $0.05)"))
		Expect(run.FailureClass.RunFailureClass).To(Equal(store.RunFailureClassBudget))
		p := canceledPayload(ctx, e, run)
		Expect(p).To(HaveKeyWithValue("reason", "run_spend_cap"))
		Expect(p).To(HaveKeyWithValue("limit", "run_spend"))
		Expect(p).To(HaveKeyWithValue("spend_limit_usd", "0.05"))
	})

	It("cancels the run when a settlement raises its spend to the limit", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "10.00")
		r := e.startLimitedRun(ctx, e.task(ctx), runLimits{requests: 100, spendUSD: "0.05"}, time.Now().Add(-time.Minute))
		_, err := ing.Ingest(ctx, []ledger.Entry{e.entry(time.Now(), "0.03", 10, 1)})
		Expect(err).NotTo(HaveOccurred())
		Expect(e.run(ctx, r.ID).State).To(Equal(store.RunStateRunning))
		// The entry's cost is revised upwards, as a provider settlement does.
		_, err = db.Exec(ctx, `UPDATE spend_entries SET cost_usd = 0.06 WHERE org_id = $1 AND run_id = $2`, e.org, r.ID)
		Expect(err).NotTo(HaveOccurred())
		_, err = scheduler.CheckRunLimits(ctx, db, e.org, r.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(e.run(ctx, r.ID).State).To(Equal(store.RunStateCanceled))
		_, err = scheduler.CheckRunLimits(ctx, db, e.org, r.ID)
		Expect(err).To(HaveOccurred(), "a finished run is not checked again")
	})

	It("names the agent's daily limit, not the run's, when both are reached by one request", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "0.05")
		r := e.startLimitedRun(ctx, e.task(ctx), runLimits{requests: 100, spendUSD: "0.05"}, time.Now().Add(-time.Minute))
		_, err := ing.Ingest(ctx, []ledger.Entry{e.entry(time.Now(), "0.06", 10, 1)})
		Expect(err).NotTo(HaveOccurred())
		Expect(canceledPayload(ctx, e, e.run(ctx, r.ID))).To(HaveKeyWithValue("reason", "spend_limit"))
	})

	Describe("the gateway key limit", func() {
		It("takes the run's remaining spend as the run level while the agent has a run", func(ctx SpecContext) {
			e := newEnv(ctx, uniq("org"), "", "10.00")
			now := time.Now()
			l, err := ledger.AgentLimit(ctx, q, e.agent, now)
			Expect(err).NotTo(HaveOccurred())
			Expect(l.Amount).To(BeNumerically("~", 10.0, 1e-9), "no run: the agent's daily limit")

			r := e.startLimitedRun(ctx, e.task(ctx), runLimits{requests: 100, spendUSD: "0.50"}, now.Add(-time.Minute))
			l, err = ledger.AgentLimit(ctx, q, e.agent, now)
			Expect(err).NotTo(HaveOccurred())
			Expect(l.Amount).To(BeNumerically("~", 0.50, 1e-9), "the run level is the smallest remaining")

			_, err = ing.Ingest(ctx, []ledger.Entry{e.entry(now, "0.20", 10, 1)})
			Expect(err).NotTo(HaveOccurred())
			l, err = ledger.AgentLimit(ctx, q, e.agent, now)
			Expect(err).NotTo(HaveOccurred())
			Expect(l.Amount).To(BeNumerically("~", 0.30, 1e-9), "net of what the run has spent")

			e.finishRun(ctx, r.ID, now)
			l, err = ledger.AgentLimit(ctx, q, e.agent, now)
			Expect(err).NotTo(HaveOccurred())
			Expect(l.Amount).To(BeNumerically("~", 9.80, 1e-9), "the run is over: the agent's daily remainder")
		})

		It("holds back a pending response's reserve from the run level too", func(ctx SpecContext) {
			e := newEnv(ctx, uniq("org"), "", "10.00")
			now := time.Now()
			r := e.startLimitedRun(ctx, e.task(ctx), runLimits{requests: 100, spendUSD: "0.50"}, now.Add(-time.Minute))
			_, err := db.Exec(ctx, `INSERT INTO spend_entries (org_id, agent_id, run_id, task_id, request_id, key_id, model,
				input_tokens, output_tokens, cost_usd, priced, occurred_at, provisional, reserve_usd)
				VALUES ($1, $2, $3, $4, 'req-reserve', 'k-1', 'm', 10, 0, 0.10, true, now(), true, 0.25)`,
				e.org, e.agent.ID, r.ID, r.TaskID)
			Expect(err).NotTo(HaveOccurred())
			l, err := ledger.AgentLimit(ctx, q, e.agent, now)
			Expect(err).NotTo(HaveOccurred())
			Expect(l.Amount).To(BeNumerically("~", 0.15, 1e-9), "$0.50 less the $0.10 charge and the $0.25 reserve")
		})
	})
})
