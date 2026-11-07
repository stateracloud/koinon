// SPDX-License-Identifier: Apache-2.0

package ledger_test

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/ledger"
	"github.com/stateracloud/koinon/internal/money"
	"github.com/stateracloud/koinon/internal/providerusage"
	"github.com/stateracloud/koinon/internal/scheduler"
)

// Run limits (issue koinon-private#243, ADR 0081): a run ends at the first of its model-request limit, its spend
// limit and its time limit. Tokens are reported, not enforced.
var _ = Describe("run limits", func() {
	BeforeEach(needDocker)

	// cacheHeavy is a request of a long agent loop: a big prompt read from the provider's cache, a
	// short answer, a fraction of a cent.
	cacheHeavy := func(e *env, cost string) ledger.Entry {
		// 500 plain input beside the cache parts (the record's buckets are disjoint): 180k in all.
		x := e.entry(time.Now(), cost, 500, 400)
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

	It("does not cancel a cache-heavy run of many requests below its request and spend limits (issue koinon-private#243)", func(ctx SpecContext) {
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
		x.OutputTokens, x.ReasoningTokens = 280, 120 // 400 output in all
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

	// ADR 0056, amendment 2026-10-31 (koinon-private#340): a failed call costs its input estimate only, never
	// max_tokens, holds no reserve even from a provider that could settle it, and still counts.
	It("charges three failed 5xx calls their input estimates only, and counts all three requests", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "10.00")
		e.model(ctx, "openrouter", flash)
		r := e.startLimitedRun(ctx, e.task(ctx), runLimits{requests: 4}, time.Now().Add(-time.Minute))
		failed := func() ledger.Entry {
			return ledger.Entry{
				RequestID: uniq("req"), KeyID: "k-test", Agent: e.ref(), Model: flash, Status: 502,
				UpstreamKnown: true, Upstream: true, MaxTokens: 32_000, ResponseID: uniq("gen"),
				RequestBytes: 3_000_000, ResponseBytes: -1, Time: time.Now(), // 1M input tokens at 3.0 bytes per token
			}
		}
		settling := &ledger.Ingester{DB: db, Log: ing.Log, Prices: ing.Prices, Sources: providerusage.Registry{"openrouter": &fakeSource{}}}
		_, err := settling.Ingest(ctx, []ledger.Entry{failed(), failed(), failed()})
		Expect(err).NotTo(HaveOccurred())

		var n int
		var cost, reserve string
		Expect(db.QueryRow(ctx, `SELECT count(*), sum(cost_usd)::text, coalesce(sum(reserve_usd), 0)::text FROM spend_entries
			WHERE org_id = $1 AND output_tokens = 0 AND input_tokens = 1000000 AND estimated AND NOT provisional`, e.org).Scan(&n, &cost, &reserve)).To(Succeed())
		Expect(n).To(Equal(3))
		Expect(ledger.FormatUSD(cost)).To(Equal("0.60"), "three input estimates of $0.20, no output")
		Expect(ledger.FormatUSD(reserve)).To(Equal("0.00"), "no max_tokens bound is held back")
		Expect(e.run(ctx, r.ID).State).To(Equal(store.RunStateRunning), "three requests are below the limit of four")

		_, err = ing.Ingest(ctx, []ledger.Entry{e.entry(time.Now(), "0.001", 10, 1)})
		Expect(err).NotTo(HaveOccurred())
		Expect(e.run(ctx, r.ID).State).To(Equal(store.RunStateCanceled), "the fourth request reaches the limit: the three failed ones counted")
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
			Expect(l.Amount).To(Equal(money.MustParse("10.0")), "no run: the agent's daily limit")

			r := e.startLimitedRun(ctx, e.task(ctx), runLimits{requests: 100, spendUSD: "0.50"}, now.Add(-time.Minute))
			l, err = ledger.AgentLimit(ctx, q, e.agent, now)
			Expect(err).NotTo(HaveOccurred())
			Expect(l.Amount).To(Equal(money.MustParse("0.50")), "the run level is the smallest remaining")

			_, err = ing.Ingest(ctx, []ledger.Entry{e.entry(now, "0.20", 10, 1)})
			Expect(err).NotTo(HaveOccurred())
			l, err = ledger.AgentLimit(ctx, q, e.agent, now)
			Expect(err).NotTo(HaveOccurred())
			Expect(l.Amount).To(Equal(money.MustParse("0.30")), "net of what the run has spent")

			e.finishRun(ctx, r.ID, now)
			l, err = ledger.AgentLimit(ctx, q, e.agent, now)
			Expect(err).NotTo(HaveOccurred())
			Expect(l.Amount).To(Equal(money.MustParse("9.80")), "the run is over: the agent's daily remainder")
		})

		It("does not hold a pending response's reserve back at the run level (it can exceed the whole limit)", func(ctx SpecContext) {
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
			Expect(l.Amount).To(Equal(money.MustParse("0.40")), "$0.50 less the $0.10 charge; the $0.25 reserve is held at the agent's level only")
		})

		It("keeps a run whose limit a cut response's reserve exceeds running, with a positive run level", func(ctx SpecContext) {
			e := newEnv(ctx, uniq("org"), "", "10.00")
			now := time.Now()
			r := e.startLimitedRun(ctx, e.task(ctx), runLimits{requests: 100, spendUSD: "0.30"}, now.Add(-time.Minute))
			_, err := db.Exec(ctx, `INSERT INTO spend_entries (org_id, agent_id, run_id, task_id, request_id, key_id, model,
				input_tokens, output_tokens, cost_usd, priced, occurred_at, provisional, reserve_usd)
				VALUES ($1, $2, $3, $4, 'req-big-reserve', 'k-1', 'm', 10, 0, 0.0002, true, now(), true, 0.40)`,
				e.org, e.agent.ID, r.ID, r.TaskID)
			Expect(err).NotTo(HaveOccurred())
			l, err := ledger.AgentLimit(ctx, q, e.agent, now)
			Expect(err).NotTo(HaveOccurred())
			Expect(l.Amount).To(Equal(money.MustParse("0.2998")), "the run's remaining spend: the wrap-up turn is not refused")
			n, err := ledger.StopSpent(ctx, db, e.agent, now)
			Expect(err).NotTo(HaveOccurred())
			Expect(n).To(BeZero())
		})
	})

	Describe("attributing requests to the run that makes them", func() {
		// parkedThenResumed builds the normal sequence of one agent: run A parks (input_required),
		// the agent's next run B starts and ends, and A resumes right after. A keeps its first
		// started_at, which is older than B's.
		parkedThenResumed := func(ctx SpecContext, e *env, l runLimits) (a, b store.Run) {
			GinkgoHelper()
			now := time.Now()
			a = e.startLimitedRun(ctx, e.task(ctx), l, now.Add(-10*time.Minute))
			_, err := db.Exec(ctx, `UPDATE runs SET state = 'parked', park_reason = 'input_required', parked_at = $3,
				owner_id = NULL, lease_expires_at = NULL, heartbeat_at = NULL WHERE org_id = $1 AND id = $2`, e.org, a.ID, now.Add(-9*time.Minute))
			Expect(err).NotTo(HaveOccurred())
			b = e.startLimitedRun(ctx, e.task(ctx), runLimits{requests: 100, spendUSD: "5.00"}, now.Add(-5*time.Minute))
			e.finishRun(ctx, b.ID, now.Add(-10*time.Second))
			_, err = db.Exec(ctx, `UPDATE runs SET state = 'starting', park_reason = NULL, parked_at = NULL, owner_id = 'test',
				lease_expires_at = now() + interval '1 hour', claimed_at = $3 WHERE org_id = $1 AND id = $2`, e.org, a.ID, now.Add(-time.Second))
			Expect(err).NotTo(HaveOccurred())
			return e.run(ctx, a.ID), e.run(ctx, b.ID)
		}

		It("books a resumed parked run's requests to it, not to the run that finished just before", func(ctx SpecContext) {
			e := newEnv(ctx, uniq("org"), "", "10.00")
			a, b := parkedThenResumed(ctx, e, runLimits{requests: 2, spendUSD: "5.00"})
			_, err := ing.Ingest(ctx, []ledger.Entry{e.entry(time.Now(), "0.001", 10, 1), e.entry(time.Now(), "0.001", 10, 1)})
			Expect(err).NotTo(HaveOccurred())

			var forA, forB int
			Expect(db.QueryRow(ctx, `SELECT count(*) FILTER (WHERE run_id = $2), count(*) FILTER (WHERE run_id = $3)
				FROM spend_entries WHERE org_id = $1`, e.org, a.ID, b.ID).Scan(&forA, &forB)).To(Succeed())
			Expect([]int{forA, forB}).To(Equal([]int{2, 0}))
			Expect(e.run(ctx, a.ID).State).To(Equal(store.RunStateCanceled), "the 2nd request reaches A's limit of 2")
			Expect(canceledPayload(ctx, e, e.run(ctx, a.ID))).To(HaveKeyWithValue("reason", "request_cap"))
		})

		It("still books a response finishing after its run ended to that run, within the grace", func(ctx SpecContext) {
			e := newEnv(ctx, uniq("org"), "", "10.00")
			now := time.Now()
			r := e.startLimitedRun(ctx, e.task(ctx), runLimits{requests: 100, spendUSD: "5.00"}, now.Add(-time.Minute))
			e.finishRun(ctx, r.ID, now.Add(-10*time.Second))
			_, err := ing.Ingest(ctx, []ledger.Entry{e.entry(now, "0.001", 10, 1)})
			Expect(err).NotTo(HaveOccurred())
			var n int
			Expect(db.QueryRow(ctx, `SELECT count(*) FROM spend_entries WHERE org_id = $1 AND run_id = $2`, e.org, r.ID).Scan(&n)).To(Succeed())
			Expect(n).To(Equal(1))
		})

		It("books a late response of the finished run to it, after the parked run resumed", func(ctx SpecContext) {
			e := newEnv(ctx, uniq("org"), "", "10.00")
			a, b := parkedThenResumed(ctx, e, runLimits{requests: 100, spendUSD: "5.00"})
			_, err := ing.Ingest(ctx, []ledger.Entry{e.entry(time.Now().Add(-20*time.Second), "0.001", 10, 1)})
			Expect(err).NotTo(HaveOccurred())
			var forA, forB int
			Expect(db.QueryRow(ctx, `SELECT count(*) FILTER (WHERE run_id = $2), count(*) FILTER (WHERE run_id = $3)
				FROM spend_entries WHERE org_id = $1`, e.org, a.ID, b.ID).Scan(&forA, &forB)).To(Succeed())
			Expect([]int{forA, forB}).To(Equal([]int{0, 1}), "B was executing 20 s ago; A had not been claimed yet")
		})
	})

	Describe("the stop at a run limit", func() {
		It("carries what the agent last said when the reconciler's stop is the one that cancels", func(ctx SpecContext) {
			e := newEnv(ctx, uniq("org"), "", "10.00")
			now := time.Now()
			task := e.task(ctx)
			r := e.startLimitedRun(ctx, task, runLimits{requests: 100, spendUSD: "0.50"}, now.Add(-time.Minute))
			_, err := db.Exec(ctx, `UPDATE tasks SET agent_summary = 'half done: the report is in section 2', agent_summary_at = now() WHERE org_id = $1 AND id = $2`, e.org, task.ID)
			Expect(err).NotTo(HaveOccurred())
			// Spend recorded without the ledger's check (an admin lowered the limit afterwards).
			_, err = db.Exec(ctx, `INSERT INTO spend_entries (org_id, agent_id, run_id, task_id, request_id, key_id, model,
				input_tokens, output_tokens, cost_usd, priced, occurred_at) VALUES ($1, $2, $3, $4, 'req-late', 'k-1', 'm', 10, 1, 0.60, true, now())`,
				e.org, e.agent.ID, r.ID, r.TaskID)
			Expect(err).NotTo(HaveOccurred())

			n, err := ledger.StopSpent(ctx, db, e.agent, now)
			Expect(err).NotTo(HaveOccurred())
			Expect(n).To(Equal(1))
			run := e.run(ctx, r.ID)
			Expect(run.State).To(Equal(store.RunStateCanceled))
			p := canceledPayload(ctx, e, run)
			Expect(p).To(HaveKeyWithValue("reason", "run_spend_cap"))
			Expect(p).To(HaveKeyWithValue("summary", "half done: the report is in section 2"))
			Expect(p).To(HaveKey("model_requests"))
		})

		It("carries the summary at the ledger's own cancel too", func(ctx SpecContext) {
			e := newEnv(ctx, uniq("org"), "", "10.00")
			task := e.task(ctx)
			r := e.startLimitedRun(ctx, task, runLimits{requests: 1, spendUSD: "5.00"}, time.Now().Add(-time.Minute))
			_, err := db.Exec(ctx, `UPDATE tasks SET agent_summary = 'nearly there', agent_summary_at = now() WHERE org_id = $1 AND id = $2`, e.org, task.ID)
			Expect(err).NotTo(HaveOccurred())
			_, err = ing.Ingest(ctx, []ledger.Entry{e.entry(time.Now(), "0.001", 10, 1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(canceledPayload(ctx, e, e.run(ctx, r.ID))).To(HaveKeyWithValue("summary", "nearly there"))
		})

		It("serialises concurrent requests of one run so the request limit is exact", func(ctx SpecContext) {
			e := newEnv(ctx, uniq("org"), "", "10.00")
			r := e.startLimitedRun(ctx, e.task(ctx), runLimits{requests: 4, spendUSD: "5.00"}, time.Now().Add(-time.Minute))
			var wg sync.WaitGroup
			for range 4 {
				wg.Add(1)
				go func() {
					defer GinkgoRecover()
					defer wg.Done()
					_, err := ing.Ingest(ctx, []ledger.Entry{e.entry(time.Now(), "0.001", 10, 1)})
					Expect(err).NotTo(HaveOccurred())
				}()
			}
			wg.Wait()
			Expect(e.run(ctx, r.ID).State).To(Equal(store.RunStateCanceled), "the 4th of 4 concurrent requests reaches the limit")
			Expect(canceledPayload(ctx, e, e.run(ctx, r.ID))).To(HaveKeyWithValue("reason", "request_cap"))
		})
	})
})
