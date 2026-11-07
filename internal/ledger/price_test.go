// SPDX-License-Identifier: Apache-2.0

package ledger_test

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/budget"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/ledger"
	"github.com/stateracloud/koinon/internal/money"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/providerusage"
	"github.com/stateracloud/koinon/pricing"
)

// The models of testdata/prices.yaml, the default list these specs run against (see the suite).
// Every money value below is computed from that file's prices, per million tokens.
const (
	flash    = "acme/flash"    // input 0.20, output 2, cache read 0.05
	frontier = "acme/frontier" // input 10, output 50, cache read 0.25, cache write 12.5; max_output 128000
)

// model serves name on a provider of the given type for the env's org.
func (e *env) model(ctx SpecContext, typ, name string) pgtype.UUID {
	return e.modelAt(ctx, typ, name, "https://example.com/v1")
}

// modelAt is model with the provider at url; it returns the provider's ID.
func (e *env) modelAt(ctx SpecContext, typ, name, url string) pgtype.UUID {
	var pid pgtype.UUID
	Expect(db.QueryRow(ctx, `INSERT INTO providers (org_id, name, type, url) VALUES ($1, $2, $3, $4) RETURNING id`,
		e.org, uniq("prov"), typ, url).Scan(&pid)).To(Succeed())
	_, err := db.Exec(ctx, `INSERT INTO models (org_id, provider_id, name) VALUES ($1, $2, $3)`, e.org, pid, name)
	Expect(err).NotTo(HaveOccurred())
	return pid
}

// provisional counts the env's provisional entries.
func (e *env) provisional(ctx SpecContext) int {
	var n int
	Expect(db.QueryRow(ctx, `SELECT count(*) FROM spend_entries WHERE org_id = $1 AND provisional`, e.org).Scan(&n)).To(Succeed())
	return n
}

func (e *env) usage(t time.Time, model string, in, out, cacheRead int64, gwCost string) ledger.Entry {
	return ledger.Entry{
		RequestID: uniq("req"), KeyID: "k-test", Agent: e.ref(), Model: model,
		InputTokens: in, OutputTokens: out, InputKnown: true, OutputKnown: true, CacheReadTokens: cacheRead,
		CostUSD: gwCost, RequestBytes: -1, ResponseBytes: -1, Time: t,
	}
}

type spendRow struct {
	InputTokens, OutputTokens, CacheReadTokens int64
	CostUsd, GatewayCostUsd, PriceRef          string
	Estimated, CostFlagged                     bool
	Estimate                                   []byte
}

func (e *env) lastEntry(ctx SpecContext) spendRow {
	var s spendRow
	Expect(db.QueryRow(ctx, `SELECT input_tokens, output_tokens, cache_read_tokens, cost_usd::text, coalesce(gateway_cost_usd::text, ''),
		coalesce(price_ref, ''), estimated, cost_flagged, coalesce(estimate, '{}'::jsonb)
		FROM spend_entries WHERE org_id = $1 ORDER BY created_at DESC, id DESC LIMIT 1`, e.org).Scan(
		&s.InputTokens, &s.OutputTokens, &s.CacheReadTokens, &s.CostUsd, &s.GatewayCostUsd, &s.PriceRef, &s.Estimated, &s.CostFlagged, &s.Estimate)).To(Succeed())
	return s
}

var _ = Describe("Cost truth", func() {
	BeforeEach(needDocker)

	It("recomputes the cost from raw usage and the dated default price, in disjoint buckets", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "10.00")
		e.model(ctx, "openrouter", flash)
		// 500k input and 500k cache reads (the record's buckets are disjoint), 1k output. The gateway agrees.
		_, err := ing.Ingest(ctx, []ledger.Entry{e.usage(time.Now(), flash, 500_000, 1000, 500_000, "0.127")})
		Expect(err).NotTo(HaveOccurred())
		s := e.lastEntry(ctx)
		// 500k * 0.20 + 500k * 0.05 + 1k * 2, per million.
		Expect(ledger.FormatUSD(s.CostUsd)).To(Equal("0.127"))
		Expect(s.CostFlagged).To(BeFalse())
		Expect(s.Estimated).To(BeFalse())
		list, _ := priceList.Current()
		Expect(list.Version).To(Equal("2000-01-01.1"), "the specs run against the fixture list")
		Expect(s.PriceRef).To(Equal(pricing.Ref(list.Version, "acme-flash")))
		Expect(s.CacheReadTokens).To(Equal(int64(500_000)))
		Expect(s.InputTokens).To(Equal(int64(1_000_000)), "the entry stores the provider's total: input plus cache")
	})

	// Thinking is on by default (ADR 0069 amendment 2026-10-29), and a provider bills thinking tokens as
	// output. agentgateway's token semantics put them inside output (llm.outputTokens includes reasoning),
	// with the reasoning part reported beside it when the provider splits it out.
	It("bills reasoning tokens as output, records them, and shows them in the spend report", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "100.00")
		e.model(ctx, "openrouter", flash)
		reasoning := func() (n int64) {
			Expect(db.QueryRow(ctx, `SELECT reasoning_tokens FROM spend_entries WHERE org_id = $1 ORDER BY created_at DESC, id DESC LIMIT 1`, e.org).Scan(&n)).To(Succeed())
			return n
		}

		By("600k output tokens and 400k reasoning (disjoint buckets): all billed at the output rate, none twice")
		u := e.usage(time.Now(), flash, 1_000_000, 600_000, 0, "")
		u.ReasoningTokens = 400_000
		_, err := ing.Ingest(ctx, []ledger.Entry{u})
		Expect(err).NotTo(HaveOccurred())
		s := e.lastEntry(ctx)
		// 1M input at 0.20 plus 1M output at 2; the 400k reasoning tokens are in the output.
		Expect(ledger.FormatUSD(s.CostUsd)).To(Equal("2.20"))
		Expect(s.OutputTokens).To(Equal(int64(1_000_000)), "the entry stores the provider's total: output plus reasoning")
		Expect(reasoning()).To(Equal(int64(400_000)))
		Expect(s.CostFlagged).To(BeFalse())

		By("a provider that does not split thinking out (Anthropic Messages) bills it as the output it is")
		_, err = ing.Ingest(ctx, []ledger.Entry{e.usage(time.Now(), flash, 1_000_000, 1_000_000, 0, "")})
		Expect(err).NotTo(HaveOccurred())
		Expect(ledger.FormatUSD(e.lastEntry(ctx).CostUsd)).To(Equal("2.20"))
		Expect(reasoning()).To(BeZero())

		By("the report counts the reasoning tokens inside the output tokens")
		rep, err := q.SpendReport(ctx, store.SpendReportParams{OrgID: e.org, Since: pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true}})
		Expect(err).NotTo(HaveOccurred())
		Expect(rep).To(HaveLen(1))
		Expect(rep[0].OutputTokens).To(Equal(int64(2_000_000)))
		Expect(rep[0].ReasoningTokens).To(Equal(int64(400_000)))
		Expect(ledger.FormatUSD(rep[0].ReportedUsd)).To(Equal("4.40"))

		By("a price that names a reasoning rate bills the reasoning part at it, and the rest of the output at the output rate")
		_, err = prices.New(db, nil, priceList).SetOverride(ctx, e.org, audit.User(e.user), prices.OverrideInput{
			Entry: pricing.Entry{
				ID: "flash-reasoning", Provider: "openrouter", Match: pricing.Match{Glob: flash},
				Spec:      pricing.Spec{Prices: pricing.Wire{Input: "1", Output: "2", Reasoning: "4"}},
				Tokenizer: pricing.TokenizerBytes, Ratio: "3.0", MaxOutput: 4096,
			},
			ValidFrom: time.Now().Add(-time.Hour),
		})
		Expect(err).NotTo(HaveOccurred())
		u = e.usage(time.Now(), flash, 1_000_000, 600_000, 0, "")
		u.ReasoningTokens = 400_000
		_, err = ing.Ingest(ctx, []ledger.Entry{u})
		Expect(err).NotTo(HaveOccurred())
		// 1M input at 1, 600k output at 2, 400k reasoning at 4.
		Expect(ledger.FormatUSD(e.lastEntry(ctx).CostUsd)).To(Equal("3.80"))
	})

	// #78: agentgateway v1.7.0 logs no koinon.reasoning for Anthropic Messages, whose thinking is inside
	// the output count. The entry records the split as unknown, so a 0 is never read as "no thinking",
	// and a split the provider did report is a figure even at 0.
	It("records whether the provider reported the thinking split, and the report says so per model", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "100.00")
		e.model(ctx, "openrouter", flash)
		e.model(ctx, "openrouter", frontier)
		known := func() (k *bool) {
			Expect(db.QueryRow(ctx, `SELECT reasoning_known FROM spend_entries WHERE org_id = $1 ORDER BY created_at DESC, id DESC LIMIT 1`, e.org).Scan(&k)).To(Succeed())
			return k
		}
		report := func() map[string]bool {
			rep, err := q.SpendReport(ctx, store.SpendReportParams{OrgID: e.org, Since: pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true}})
			Expect(err).NotTo(HaveOccurred())
			out := map[string]bool{}
			for _, r := range rep {
				out[r.Model] = r.ReasoningReported
			}
			return out
		}

		By("a Messages record without the split: unknown, and its thinking is billed inside the output")
		_, err := ing.Ingest(ctx, []ledger.Entry{e.usage(time.Now(), frontier, 1000, 1000, 0, "")})
		Expect(err).NotTo(HaveOccurred())
		Expect(known()).To(HaveValue(BeFalse()))
		Expect(e.lastEntry(ctx).OutputTokens).To(Equal(int64(1000)))

		By("a chat-completions record that reports 0 reasoning tokens: known, 0 is a figure")
		u := e.usage(time.Now(), flash, 1000, 1000, 0, "")
		u.ReasoningKnown = true
		_, err = ing.Ingest(ctx, []ledger.Entry{u})
		Expect(err).NotTo(HaveOccurred())
		Expect(known()).To(HaveValue(BeTrue()))
		Expect(report()).To(Equal(map[string]bool{flash: true, frontier: false}))

		By("an entry written before reasoning_known existed reads as reported when its split is above 0")
		u = e.usage(time.Now(), frontier, 1000, 1000, 0, "")
		u.ReasoningTokens, u.ReasoningKnown = 300, true
		_, err = ing.Ingest(ctx, []ledger.Entry{u})
		Expect(err).NotTo(HaveOccurred())
		_, err = db.Exec(ctx, `UPDATE spend_entries SET reasoning_known = NULL WHERE org_id = $1 AND reasoning_tokens = 300`, e.org)
		Expect(err).NotTo(HaveOccurred())
		Expect(report()).To(Equal(map[string]bool{flash: true, frontier: true}))
	})

	It("charges its own cost and flags the entry when the gateway differs by more than 1%", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "10.00")
		e.model(ctx, "openrouter", flash)
		_, err := ing.Ingest(ctx, []ledger.Entry{
			e.usage(time.Now(), flash, 1_000_000, 0, 0, "0.201"), // 0.20 vs 0.201: 0.5%
			e.usage(time.Now(), flash, 1_000_000, 0, 0, "0.30"),  // 50%
		})
		Expect(err).NotTo(HaveOccurred())
		s := e.lastEntry(ctx)
		Expect(s.CostFlagged).To(BeTrue())
		Expect(ledger.FormatUSD(s.CostUsd)).To(Equal("0.20"))
		Expect(ledger.FormatUSD(s.GatewayCostUsd)).To(Equal("0.30"))
		flagged, err := q.ListFlaggedPrices(ctx, store.ListFlaggedPricesParams{OrgID: e.org, Since: pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true}})
		Expect(err).NotTo(HaveOccurred())
		Expect(flagged).To(HaveLen(1))
		Expect(flagged[0].Flagged).To(Equal(int64(1)))
		Expect(flagged[0].GatewayDiff).To(Equal(int64(1)), "the reason is counted")
		Expect(flagged[0].UsageMissing + flagged[0].PriceError + flagged[0].MaxOutputCapped).To(BeZero())
	})

	It("lets an org override win and records its version", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "10.00")
		e.model(ctx, "openrouter", flash)
		svc := prices.New(db, nil, priceList)
		o, err := svc.SetOverride(ctx, e.org, audit.User(e.user), prices.OverrideInput{
			Entry: pricing.Entry{
				ID: "ds-contract", Provider: "openrouter", Match: pricing.Match{Glob: "acme/*"},
				Spec:      pricing.Spec{Prices: pricing.Wire{Input: "1", Output: "2"}},
				Tokenizer: pricing.TokenizerBytes, Ratio: "3.0", MaxOutput: 4096,
			},
			ValidFrom: time.Now().Add(-time.Hour),
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = ing.Ingest(ctx, []ledger.Entry{e.usage(time.Now(), flash, 1_000_000, 0, 0, "")})
		Expect(err).NotTo(HaveOccurred())
		s := e.lastEntry(ctx)
		Expect(ledger.FormatUSD(s.CostUsd)).To(Equal("1.00"))
		Expect(s.PriceRef).To(Equal("org#" + o.ID))

		// Requests before the override's valid_from keep the default price.
		_, err = ing.Ingest(ctx, []ledger.Entry{e.usage(time.Now().Add(-2*time.Hour), flash, 1_000_000, 0, 0, "")})
		Expect(err).NotTo(HaveOccurred())
		Expect(ledger.FormatUSD(e.lastEntry(ctx).CostUsd)).To(Equal("0.20"))

		var n int
		Expect(db.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'price.set'`, e.org).Scan(&n)).To(Succeed())
		Expect(n).To(Equal(1))
	})

	It("bills a bucket an org price does not name at the rate it would have without the split", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "10.00")
		e.model(ctx, "openrouter", frontier)
		_, err := prices.New(db, nil, priceList).SetOverride(ctx, e.org, audit.User(e.user), prices.OverrideInput{
			Entry: pricing.Entry{
				ID: "sonnet-mine", Provider: "openrouter", Match: pricing.Match{Glob: frontier},
				Spec:      pricing.Spec{Prices: pricing.Wire{Input: "2", Output: "10"}},
				Tokenizer: pricing.TokenizerBytes, Ratio: "3.0", MaxOutput: 4096,
			},
			ValidFrom: time.Now().Add(-time.Hour),
		})
		Expect(err).NotTo(HaveOccurred())
		u := e.usage(time.Now(), frontier, 0, 0, 0, "") // 1M tokens, all cache write
		u.CacheWriteTokens = 1_000_000
		_, err = ing.Ingest(ctx, []ledger.Entry{u})
		Expect(err).NotTo(HaveOccurred())
		s := e.lastEntry(ctx)
		Expect(ledger.FormatUSD(s.CostUsd)).To(Equal("2.00"), "the cache write is billed at the input rate")
		Expect(s.PriceRef).To(HavePrefix("org#"))
	})

	It("bills 1-hour cache writes at their own rate, the 5-minute rest at cache_write, and stores the split (#270)", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "10.00")
		e.model(ctx, "openrouter", frontier)
		_, err := prices.New(db, nil, priceList).SetOverride(ctx, e.org, audit.User(e.user), prices.OverrideInput{
			Entry: pricing.Entry{
				ID: "sonnet-1h", Provider: "openrouter", Match: pricing.Match{Glob: frontier},
				Spec:      pricing.Spec{Prices: pricing.Wire{Input: "2", Output: "10", CacheWrite: "2.5", CacheWrite1h: "4"}},
				Tokenizer: pricing.TokenizerBytes, Ratio: "3.0", MaxOutput: 4096,
			},
			ValidFrom: time.Now().Add(-time.Hour),
		})
		Expect(err).NotTo(HaveOccurred())
		u := e.usage(time.Now(), frontier, 0, 0, 0, "")
		u.CacheWriteTokens, u.CacheWrite1hTokens = 1_000_000, 400_000
		_, err = ing.Ingest(ctx, []ledger.Entry{u})
		Expect(err).NotTo(HaveOccurred())
		s := e.lastEntry(ctx)
		// 600K at 2.5 plus 400K at 4 per million.
		Expect(ledger.FormatUSD(s.CostUsd)).To(Equal("3.10"))
		var total, oneHour int64
		Expect(db.QueryRow(ctx, `SELECT input_tokens, cache_write_1h_tokens FROM spend_entries WHERE org_id = $1`, e.org).Scan(&total, &oneHour)).To(Succeed())
		Expect(total).To(Equal(int64(1_000_000)), "input_tokens still counts every write once")
		Expect(oneHour).To(Equal(int64(400_000)))

		// A 1-hour figure above the writes is clamped to them.
		over := e.usage(time.Now(), frontier, 0, 0, 0, "")
		over.CacheWriteTokens, over.CacheWrite1hTokens = 1_000_000, 5_000_000
		_, err = ing.Ingest(ctx, []ledger.Entry{over})
		Expect(err).NotTo(HaveOccurred())
		Expect(ledger.FormatUSD(e.lastEntry(ctx).CostUsd)).To(Equal("4.00"))
	})

	It("estimates missing usage, marks it and counts it toward the limit", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "0.01")
		e.model(ctx, "openrouter", flash)
		t := e.task(ctx)
		r := e.startRun(ctx, t, time.Now().Add(-time.Minute))
		x := ledger.Entry{
			RequestID: uniq("req"), KeyID: "k-test", Agent: e.ref(), Model: flash, Status: 200,
			MaxTokens: 1000, RequestBytes: 3_000_000, ResponseBytes: -1, Time: time.Now(), // 1M tokens at 3.0 bytes per token
		}
		_, err := ing.Ingest(ctx, []ledger.Entry{x})
		Expect(err).NotTo(HaveOccurred())
		s := e.lastEntry(ctx)
		Expect(s.Estimated).To(BeTrue())
		Expect(s.InputTokens).To(Equal(int64(1_000_000)))
		Expect(s.OutputTokens).To(BeZero(), "nothing was streamed: no output is charged, never max_tokens")
		Expect(ledger.FormatUSD(s.CostUsd)).To(Equal("0.20"))
		var est map[string]any
		Expect(json.Unmarshal(s.Estimate, &est)).To(Succeed())
		Expect(est).To(HaveKey("estimate"))
		Expect(e.run(ctx, r.ID).State).To(Equal(store.RunStateCanceled), "an estimate over the limit stops the run")

		rep, err := q.SpendReport(ctx, store.SpendReportParams{OrgID: e.org, Since: pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true}})
		Expect(err).NotTo(HaveOccurred())
		Expect(rep).To(HaveLen(1))
		Expect(rep[0].EstimatedRequests).To(Equal(int64(1)))
		Expect(ledger.FormatUSD(rep[0].ReportedUsd)).To(Equal("0.00"))
		Expect(ledger.FormatUSD(rep[0].EstimatedUsd)).To(Equal("0.20"))
	})

	It("raises, never lowers, the estimate with the learned p90", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "100.00")
		e.model(ctx, "openrouter", flash)
		var batch []ledger.Entry
		for i := 0; i < prices.MinCalibrationSamples; i++ {
			u := e.usage(time.Now(), flash, 1000, 10, 0, "")
			u.RequestBytes = 1000 // 1 token per byte: far denser than the static 3.0 bytes per token
			batch = append(batch, u)
		}
		_, err := ing.Ingest(ctx, batch)
		Expect(err).NotTo(HaveOccurred())
		_, err = ing.Ingest(ctx, []ledger.Entry{{
			RequestID: uniq("req"), KeyID: "k-test", Agent: e.ref(), Model: flash, Status: 200,
			RequestBytes: 3000, ResponseBytes: -1, Time: time.Now(),
		}})
		Expect(err).NotTo(HaveOccurred())
		Expect(e.lastEntry(ctx).InputTokens).To(Equal(int64(3000)), "static ratio would say 1000")
	})
})

var _ = Describe("An unusable stored price", func() {
	BeforeEach(needDocker)

	It("records the request unpriced and flagged, and never stops the batch or other orgs", func(ctx SpecContext) {
		bad := newEnv(ctx, uniq("org"), "", "10.00")
		good := newEnv(ctx, uniq("org"), "", "10.00")
		bad.model(ctx, "openrouter", flash)
		good.model(ctx, "openrouter", flash)
		// A rate from before the stricter validation, and an override that no longer parses.
		_, err := db.Exec(ctx, `INSERT INTO price_overrides (org_id, name, entry, valid_from, created_by) VALUES ($1, 'old', '{"provider":"openrouter","match":{"glob":"acme/*"},"validFrom":"2020-01-01","prices":{"input":"1.1234567","output":"1"},"tokenizer":"bytes","ratio":"3.0","maxOutput":10}', now() - interval '1 hour', 'test')`, bad.org)
		Expect(err).NotTo(HaveOccurred())

		res, err := ing.Ingest(ctx, []ledger.Entry{
			bad.usage(time.Now(), flash, 1000, 10, 0, "0.5"),
			good.usage(time.Now(), flash, 1_000_000, 0, 0, ""),
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Recorded).To(Equal(2))
		b := bad.lastEntry(ctx)
		Expect(b.CostFlagged).To(BeTrue())
		Expect(b.PriceRef).To(BeEmpty())
		Expect(string(b.Estimate)).To(ContainSubstring("price_error"))
		Expect(ledger.FormatUSD(b.CostUsd)).To(Equal("0.50"), "the gateway's cost stands in")
		Expect(ledger.FormatUSD(good.lastEntry(ctx).CostUsd)).To(Equal("0.20"))

		_, err = db.Exec(ctx, `INSERT INTO price_overrides (org_id, name, entry, valid_from, created_by) VALUES ($1, 'broken', '{"id":"broken"}', now() - interval '1 hour', 'test')`, good.org)
		Expect(err).NotTo(HaveOccurred())
		res, err = ing.Ingest(ctx, []ledger.Entry{good.usage(time.Now(), flash, 1000, 10, 0, "")})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Recorded).To(Equal(1))
		Expect(good.lastEntry(ctx).CostFlagged).To(BeTrue())
	})

	It("caps the bound of an absurd max_tokens, which blocks neither ingest nor the budgets of anyone else", func(ctx SpecContext) {
		abuser := newEnv(ctx, uniq("org"), "", "10.00")
		other := newEnv(ctx, uniq("org"), "", "10.00")
		abuser.model(ctx, "openrouter", frontier)
		other.model(ctx, "openrouter", flash)
		var batch []ledger.Entry
		for i := 0; i < 5; i++ {
			batch = append(batch, ledger.Entry{
				RequestID: uniq("req"), KeyID: "k-test", Agent: abuser.ref(), Model: frontier, Status: 200,
				MaxTokens: 9_000_000_000_000, RequestBytes: 100, ResponseBytes: -1, Time: time.Now(),
			})
		}
		batch = append(batch, other.usage(time.Now(), flash, 1_000_000, 0, 0, ""))
		res, err := ing.Ingest(ctx, batch)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Recorded).To(Equal(6))
		s := abuser.lastEntry(ctx)
		Expect(s.Estimated).To(BeTrue())
		Expect(s.CostFlagged).To(BeTrue(), "a 2xx without usage is flagged")
		Expect(string(s.Estimate)).To(ContainSubstring(`"usage_missing": true`))
		Expect(string(s.Estimate)).To(ContainSubstring(`"max_output_capped": true`))
		// Nothing was streamed and the provider cannot settle it: only the input estimate is charged,
		// never the max_tokens bound.
		var sum string
		Expect(db.QueryRow(ctx, `SELECT sum(cost_usd + reserve_usd)::text FROM spend_entries WHERE org_id = $1`, abuser.org).Scan(&sum)).To(Succeed())
		Expect(sum).To(HavePrefix("0.0"))

		// The budgets of both orgs still compute.
		_, err = ledger.AgentLimit(ctx, q, abuser.agent, time.Now())
		Expect(err).NotTo(HaveOccurred())
		l, err := ledger.AgentLimit(ctx, q, other.agent, time.Now())
		Expect(err).NotTo(HaveOccurred())
		Expect(l.Amount).To(Equal(money.MustParse("9.80")))

		// A sum beyond what a field could hold saturates instead of failing.
		_, err = db.Exec(ctx, `INSERT INTO spend_entries (org_id, agent_id, request_id, key_id, model, input_tokens, output_tokens, cost_usd, priced, occurred_at)
			VALUES ($1, $2, 'huge', 'k', 'm', 1, 1, 999999999999.5, true, now())`, abuser.org, abuser.agent.ID)
		Expect(err).NotTo(HaveOccurred())
		l, err = ledger.AgentLimit(ctx, q, abuser.agent, time.Now())
		Expect(err).NotTo(HaveOccurred())
		Expect(l.Amount).To(BeZero())
	})

	Describe("requests the provider refused with a 4xx and no usage (ADR 0056, amendment 2026-10-29)", func() {
		rejected := func(e *env, status int64) ledger.Entry {
			return ledger.Entry{
				RequestID: uniq("req"), KeyID: "k-test", Agent: e.ref(), Model: flash, Status: status,
				UpstreamKnown: true, Upstream: true,
				MaxTokens: 4000, RequestBytes: 3_000_000, ResponseBytes: -1, Time: time.Now(),
			}
		}
		// customProvider serves flash on a provider type with no documented billing, priced by an org entry.
		customProvider := func(ctx SpecContext) *env {
			e := newEnv(ctx, uniq("org"), "", "100.00")
			e.model(ctx, "acme-gateway", flash)
			_, err := prices.New(db, nil, priceList).SetOverride(ctx, e.org, audit.User(e.user), prices.OverrideInput{
				Entry: pricing.Entry{
					ID: "custom-flash", Provider: "acme-gateway", Match: pricing.Match{Glob: flash},
					Spec:      pricing.Spec{Prices: pricing.Wire{Input: "0.20", Output: "2"}},
					Tokenizer: pricing.TokenizerBytes, Ratio: "3.0", MaxOutput: 1000000,
				},
				ValidFrom: time.Now().Add(-time.Hour),
			})
			Expect(err).NotTo(HaveOccurred())
			return e
		}
		stored := func(ctx SpecContext, e *env) (status int) {
			GinkgoHelper()
			var st *int
			Expect(db.QueryRow(ctx, `SELECT upstream_status FROM spend_entries WHERE org_id = $1 ORDER BY created_at DESC, id DESC LIMIT 1`, e.org).Scan(&st)).To(Succeed())
			if st != nil {
				status = *st
			}
			return status
		}

		for _, typ := range []string{"anthropic", "openrouter", "openai"} {
			It("charges nothing for a 400 from "+typ+", which does not bill it, and keeps the entry flagged rejected_by_upstream", func(ctx SpecContext) {
				e := newEnv(ctx, uniq("org"), "", "100.00")
				e.model(ctx, typ, flash)
				if typ != "openrouter" {
					_, err := prices.New(db, nil, priceList).SetOverride(ctx, e.org, audit.User(e.user), prices.OverrideInput{
						Entry: pricing.Entry{
							ID: "p-" + typ, Provider: typ, Match: pricing.Match{Glob: flash},
							Spec:      pricing.Spec{Prices: pricing.Wire{Input: "0.20", Output: "2"}},
							Tokenizer: pricing.TokenizerBytes, Ratio: "3.0", MaxOutput: 1000000,
						},
						ValidFrom: time.Now().Add(-time.Hour),
					})
					Expect(err).NotTo(HaveOccurred())
				}
				run := e.startRun(ctx, e.task(ctx), time.Now().Add(-time.Minute))
				_, err := ing.Ingest(ctx, []ledger.Entry{rejected(e, 400)})
				Expect(err).NotTo(HaveOccurred())
				s := e.lastEntry(ctx)
				Expect(s.InputTokens).To(BeZero(), "no phantom tokens either")
				Expect(s.OutputTokens).To(BeZero())
				Expect(ledger.FormatUSD(s.CostUsd)).To(Equal("0.00"))
				Expect(s.Estimated).To(BeFalse(), "the usage is zero, not estimated")
				Expect(s.CostFlagged).To(BeFalse(), "not a price problem")
				Expect(string(s.Estimate)).To(ContainSubstring(`"rejected_by_upstream": true`))
				Expect(string(s.Estimate)).To(ContainSubstring(`"charge_waived": "provider type ` + typ + ` does not bill refused requests"`))
				Expect(string(s.Estimate)).To(ContainSubstring(`"waived_input_tokens": 1000000`), "what the estimate would have charged stays visible")
				Expect(string(s.Estimate)).NotTo(ContainSubstring("usage_missing"))
				Expect(stored(ctx, e)).To(Equal(400))
				Expect(e.run(ctx, run.ID).TokensUsed).To(BeZero(), "the run's token cap is not charged")
			})
		}

		It("keeps the conservative charge, the input estimate only, for a provider type with no documented billing", func(ctx SpecContext) {
			e := customProvider(ctx)
			_, err := ing.Ingest(ctx, []ledger.Entry{rejected(e, 400)})
			Expect(err).NotTo(HaveOccurred())
			s := e.lastEntry(ctx)
			Expect(s.InputTokens).To(Equal(int64(1_000_000)))
			Expect(s.OutputTokens).To(BeZero())
			Expect(ledger.FormatUSD(s.CostUsd)).To(Equal("0.20"))
			Expect(s.CostFlagged).To(BeTrue())
			Expect(string(s.Estimate)).To(ContainSubstring(`"usage_missing": true`))
			Expect(string(s.Estimate)).To(ContainSubstring(`"rejected_by_upstream": true`))
			Expect(stored(ctx, e)).To(Equal(400))
		})

		It("charges a record from a config that cannot say who answered the same way", func(ctx SpecContext) {
			e := newEnv(ctx, uniq("org"), "", "100.00")
			e.model(ctx, "openrouter", flash)
			x := rejected(e, 400)
			x.UpstreamKnown, x.Upstream = false, false
			_, err := ing.Ingest(ctx, []ledger.Entry{x})
			Expect(err).NotTo(HaveOccurred())
			Expect(ledger.FormatUSD(e.lastEntry(ctx).CostUsd)).To(Equal("0.00"), "openrouter does not bill a refused request")
			Expect(stored(ctx, e)).To(BeZero(), "without koinon.upstream the status is not known to be the provider's")
		})

		// Old issue 241: the per-provider-type flag decides, not the status. A provider type that
		// documents it does not bill refused requests is charged nothing for any 4xx; an unknown or
		// custom one keeps the conservative input charge for any 4xx, 401 and 403 included.
		for _, status := range []int64{401, 402, 403, 404, 413, 429} {
			It(fmt.Sprintf("charges nothing for a provider's %d when its type documents not billing it", status), func(ctx SpecContext) {
				e := newEnv(ctx, uniq("org"), "", "100.00")
				e.model(ctx, "openrouter", flash)
				_, err := ing.Ingest(ctx, []ledger.Entry{rejected(e, status)})
				Expect(err).NotTo(HaveOccurred())
				s := e.lastEntry(ctx)
				Expect(ledger.FormatUSD(s.CostUsd)).To(Equal("0.00"))
				Expect(s.InputTokens).To(BeZero())
				Expect(string(s.Estimate)).To(ContainSubstring(`"charge_waived": "provider type openrouter does not bill refused requests"`))
				Expect(stored(ctx, e)).To(Equal(int(status)))
			})

			It(fmt.Sprintf("keeps the conservative charge for a provider's %d on a custom provider type", status), func(ctx SpecContext) {
				e := customProvider(ctx)
				_, err := ing.Ingest(ctx, []ledger.Entry{rejected(e, status)})
				Expect(err).NotTo(HaveOccurred())
				s := e.lastEntry(ctx)
				Expect(ledger.FormatUSD(s.CostUsd)).To(Equal("0.20"))
				Expect(s.InputTokens).To(Equal(int64(1_000_000)))
				Expect(string(s.Estimate)).NotTo(ContainSubstring("charge_waived"))
				Expect(string(s.Estimate)).To(ContainSubstring(`"rejected_by_upstream": true`))
				Expect(stored(ctx, e)).To(Equal(int(status)), "still recorded, with the provider's status")
			})
		}

		It("charges a 5xx without usage its input estimate only: not waived, no reserve, the provider's status kept (old issue 340)", func(ctx SpecContext) {
			e := newEnv(ctx, uniq("org"), "", "100.00")
			e.model(ctx, "openrouter", flash)
			x := rejected(e, 502)
			_, err := ing.Ingest(ctx, []ledger.Entry{x})
			Expect(err).NotTo(HaveOccurred())
			s := e.lastEntry(ctx)
			Expect(s.OutputTokens).To(BeZero(), "a 5xx is charged what it streamed, nothing here")
			Expect(s.InputTokens).To(Equal(int64(1_000_000)))
			Expect(string(s.Estimate)).To(ContainSubstring(`"failed_by_upstream": true`))
			Expect(s.Estimated).To(BeTrue())
			Expect(string(s.Estimate)).To(ContainSubstring(`"usage_missing": true`))
			Expect(string(s.Estimate)).NotTo(ContainSubstring("charge_waived"))
			Expect(stored(ctx, e)).To(Equal(502))
		})

		It("settles a record without a status as it did", func(ctx SpecContext) {
			e := newEnv(ctx, uniq("org"), "", "100.00")
			e.model(ctx, "openrouter", flash)
			x := rejected(e, 0)
			x.UpstreamKnown, x.Upstream = false, false
			_, err := ing.Ingest(ctx, []ledger.Entry{x})
			Expect(err).NotTo(HaveOccurred())
			s := e.lastEntry(ctx)
			Expect(s.InputTokens).To(BeNumerically(">", 0), "the client left before the headers: the provider may have read the prompt")
			Expect(string(s.Estimate)).NotTo(ContainSubstring("charge_waived"))
		})
	})

	It("charges an aborted response the usage the gateway logged and holds max_tokens back from the key while the provider's figure is pending", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "100.00")
		e.model(ctx, "openrouter", flash)
		x := func(max int64, responseID string) ledger.Entry {
			return ledger.Entry{
				RequestID: uniq("req"), KeyID: "k-test", Agent: e.ref(), Model: flash, Status: 200, MaxTokens: max,
				InputKnown: true, InputTokens: 1000, RequestBytes: 3000, ResponseBytes: -1, ResponseID: responseID, Time: time.Now(),
			}
		}
		By("a provider that cannot report its own usage: the logged usage is final")
		_, err := ing.Ingest(ctx, []ledger.Entry{x(2000, "")})
		Expect(err).NotTo(HaveOccurred())
		s := e.lastEntry(ctx)
		Expect(s.Estimated).To(BeTrue())
		Expect(s.OutputTokens).To(BeZero(), "no output was logged: none is charged, never max_tokens")
		// 1000 reported input at 0.20 per million.
		Expect(ledger.FormatUSD(s.CostUsd)).To(Equal("0.0002"))
		Expect(e.provisional(ctx)).To(BeZero())

		By("a provider that can: provisional, with the bound held back from the key, not in budgets or reports")
		settling := &ledger.Ingester{DB: db, Log: ing.Log, Prices: ing.Prices, Sources: providerusage.Registry{"openrouter": &fakeSource{}}}
		_, err = settling.Ingest(ctx, []ledger.Entry{x(2000, "gen-1")})
		Expect(err).NotTo(HaveOccurred())
		Expect(e.provisional(ctx)).To(Equal(1))
		var reserve string
		Expect(db.QueryRow(ctx, `SELECT reserve_usd::text FROM spend_entries WHERE org_id = $1 AND provisional`, e.org).Scan(&reserve)).To(Succeed())
		// The bound: 1000 input + 2000 output is 0.0042; the charge 0.0002 is not held twice.
		Expect(ledger.FormatUSD(reserve)).To(Equal("0.004"))
		l, err := ledger.AgentLimit(ctx, q, e.agent, time.Now())
		Expect(err).NotTo(HaveOccurred())
		Expect(l.Amount).To(Equal(money.MustParse("99.9956")), "the key's limit holds the reserve back")
		v, _, err := budget.New(db).GetAgent(ctx, e.org, e.agent.ID, money.MustParse("100.00"), time.Now())
		Expect(err).NotTo(HaveOccurred())
		Expect(v.SpentToday).To(Equal(money.MustParse("0.0004")), "the budget counts the charges only")
		rep, err := q.SpendReport(ctx, store.SpendReportParams{OrgID: e.org, Since: pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true}})
		Expect(err).NotTo(HaveOccurred())
		Expect(ledger.FormatUSD(rep[0].EstimatedUsd)).To(Equal("0.0004"), "reports count the charges only")
		Expect(ledger.FormatUSD(rep[0].ReservedUsd)).To(Equal("0.004"))
		Expect(rep[0].ProvisionalRequests).To(Equal(int64(1)))

		By("without max_tokens the entry's max_output is the bound, flagged")
		_, err = ing.Ingest(ctx, []ledger.Entry{x(0, "")})
		Expect(err).NotTo(HaveOccurred())
		s = e.lastEntry(ctx)
		Expect(string(s.Estimate)).To(ContainSubstring(`"max_output_fallback": true`))
		Expect(s.OutputTokens).To(BeZero())
	})
})

var _ = Describe("Budgets", func() {
	BeforeEach(needDocker)

	It("cancels every open run of the org at its monthly cap, naming the limit, and alerts at 80% and 100%", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "100.00")
		e.model(ctx, "openrouter", flash)
		svc := budget.New(db)
		Expect(svc.SetOrgCap(ctx, e.org, audit.User(e.user), ptr(money.MustParse("1.00")))).To(Succeed())
		t := e.task(ctx)
		r := e.startRun(ctx, t, time.Now().Add(-time.Minute))

		// 0.20 per entry: 20%, 40%, 60%, 80%.
		for i := 0; i < 4; i++ {
			_, err := ing.Ingest(ctx, []ledger.Entry{e.usage(time.Now(), flash, 1_000_000, 0, 0, "")})
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(e.run(ctx, r.ID).State).To(Equal(store.RunStateRunning))
		ns, err := q.ListNotifications(ctx, store.ListNotificationsParams{OrgID: e.org, UserID: e.user, MaxRows: 10})
		Expect(err).NotTo(HaveOccurred())
		Expect(ns).To(HaveLen(1))
		Expect(ns[0].Kind).To(Equal(budget.KindWarning))

		_, err = ing.Ingest(ctx, []ledger.Entry{e.usage(time.Now(), flash, 1_000_000, 0, 0, "")})
		Expect(err).NotTo(HaveOccurred())
		run := e.run(ctx, r.ID)
		Expect(run.State).To(Equal(store.RunStateCanceled))
		Expect(run.OutcomeReason.String).To(HavePrefix("org monthly cap reached ($1.00 of $1.00"))
		ns, err = q.ListNotifications(ctx, store.ListNotificationsParams{OrgID: e.org, UserID: e.user, MaxRows: 10})
		Expect(err).NotTo(HaveOccurred())
		Expect(ns).To(HaveLen(2))

		// More spend in the same window does not alert again.
		_, err = ing.Ingest(ctx, []ledger.Entry{e.usage(time.Now(), flash, 1_000_000, 0, 0, "")})
		Expect(err).NotTo(HaveOccurred())
		ns, _ = q.ListNotifications(ctx, store.ListNotificationsParams{OrgID: e.org, UserID: e.user, MaxRows: 10})
		Expect(ns).To(HaveLen(2))
	})

	It("pushes the smallest remaining amount of the agent, the org and the platform", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "10.00")
		e.model(ctx, "openrouter", flash)
		svc := budget.New(db)
		now := time.Now()
		l, err := ledger.AgentLimit(ctx, q, e.agent, now)
		Expect(err).NotTo(HaveOccurred())
		Expect(l.Amount).To(Equal(money.MustParse("10")), "the template's value is the default")

		Expect(svc.SetAgentDaily(ctx, e.org, e.agent.ID, audit.User(e.user), money.MustParse("5"))).To(Succeed())
		l, _ = ledger.AgentLimit(ctx, q, e.agent, now)
		Expect(l.Amount).To(Equal(money.MustParse("5")), "an admin's limit replaces the template's")

		Expect(svc.SetOrgCap(ctx, e.org, audit.User(e.user), ptr(money.MustParse("3")))).To(Succeed())
		l, _ = ledger.AgentLimit(ctx, q, e.agent, now)
		Expect(l.Amount).To(Equal(money.MustParse("3")))

		// Only a platform admin may set the ceiling; the cap must fit under it.
		err = svc.SetPlatformCeiling(ctx, e.org, audit.User(e.user), e.user, ptr(money.MustParse("2")))
		Expect(err).To(MatchError(budget.ErrForbidden))
		_, err = db.Exec(ctx, `INSERT INTO platform_admins (user_id) VALUES ($1)`, e.user)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx SpecContext) { _, _ = db.Exec(ctx, `DELETE FROM platform_admins WHERE user_id = $1`, e.user) })
		err = svc.SetPlatformCeiling(ctx, e.org, audit.User(e.user), e.user, ptr(money.MustParse("2")))
		Expect(err).To(HaveOccurred(), "the cap of 3 is above a ceiling of 2")
		Expect(svc.SetPlatformCeiling(ctx, e.org, audit.User(e.user), e.user, ptr(money.MustParse("4")))).To(Succeed())
		Expect(svc.SetOrgCap(ctx, e.org, audit.User(e.user), ptr(money.MustParse("5")))).NotTo(Succeed(), "the cap cannot exceed the ceiling")

		_, err = ing.Ingest(ctx, []ledger.Entry{e.usage(now, flash, 5_000_000, 0, 0, "")}) // 1.00
		Expect(err).NotTo(HaveOccurred())
		l, _ = ledger.AgentLimit(ctx, q, e.agent, now)
		Expect(l.Amount).To(Equal(money.MustParse("2")), "min(5-1, 3-1, 4-1)")

		var n int
		Expect(db.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action LIKE 'budget.%'`, e.org).Scan(&n)).To(Succeed())
		Expect(n).To(Equal(3), "every accepted budget change is audited, refused ones are not")
	})

	It("tells platform admins when an org reaches its ceiling", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "100.00")
		e.model(ctx, "openrouter", flash)
		_, err := db.Exec(ctx, `INSERT INTO platform_admins (user_id) VALUES ($1)`, e.user)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx SpecContext) { _, _ = db.Exec(ctx, `DELETE FROM platform_admins WHERE user_id = $1`, e.user) })
		Expect(budget.New(db).SetPlatformCeiling(ctx, e.org, audit.User(e.user), e.user, ptr(money.MustParse("0.20")))).To(Succeed())
		t := e.task(ctx)
		r := e.startRun(ctx, t, time.Now().Add(-time.Minute))
		_, err = ing.Ingest(ctx, []ledger.Entry{e.usage(time.Now(), flash, 1_000_000, 0, 0, "")})
		Expect(err).NotTo(HaveOccurred())
		Expect(e.run(ctx, r.ID).OutcomeReason.String).To(HavePrefix("platform ceiling reached"))
		var kinds []string
		rows, err := db.Query(ctx, `SELECT kind FROM notifications WHERE org_id = $1 AND payload->>'level' = 'platform_ceiling' ORDER BY kind`, e.org)
		Expect(err).NotTo(HaveOccurred())
		defer rows.Close()
		for rows.Next() {
			var k string
			Expect(rows.Scan(&k)).To(Succeed())
			kinds = append(kinds, k)
		}
		Expect(kinds).To(Equal([]string{budget.KindReached, budget.KindWarning}))
	})
})

func ptr[T any](v T) *T { return &v }

var _ = Describe("Prices migrated from model rates", func() {
	BeforeEach(needDocker)

	// What migration 00014 writes: the model's rates, generic settings and a pending note.
	migrated := func(ctx SpecContext, org pgtype.UUID, provider, model, prices string) {
		GinkgoHelper()
		_, err := db.Exec(ctx, `INSERT INTO price_overrides (org_id, name, entry, valid_from, created_by) VALUES ($1, $2,
			jsonb_build_object('provider', $3::text, 'match', jsonb_build_object('regex', '(?-i:' || replace($4::text, '.', '\.') || ')'),
			'validFrom', '2026-10-01', 'prices', $5::jsonb, 'tokenizer', 'bytes', 'ratio', '3.0', 'maxOutput', 131072,
			'source', jsonb_build_object('url', '', 'accessed', ''), 'note', 'migrated:pending:' || $4::text),
			'2026-10-01T00:00:00Z', 'migration 00014')`, org, "model-"+uniq("m"), provider, model, prices)
		Expect(err).NotTo(HaveOccurred())
	}
	read := func(ctx SpecContext, org pgtype.UUID) pricing.Entry {
		GinkgoHelper()
		var raw []byte
		Expect(db.QueryRow(ctx, `SELECT entry FROM price_overrides WHERE org_id = $1`, org).Scan(&raw)).To(Succeed())
		var e pricing.Entry
		Expect(json.Unmarshal(raw, &e)).To(Succeed())
		return e
	}

	It("takes what the model's rates never set from the matching default entry, once", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "10.00")
		migrated(ctx, e.org, "openrouter", frontier, `{"input":"2","output":"10"}`)
		svc := prices.New(db, nil, priceList)
		n, err := svc.CompleteMigrated(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(BeNumerically(">=", 1))
		got := read(ctx, e.org)
		_, defs := priceList.Current()
		d, ok := pricing.Select(defs, "openrouter", frontier, time.Now())
		Expect(ok).To(BeTrue())
		Expect(got.Prices.Input).To(Equal("2"), "the model's own rates stay")
		Expect(got.Prices.Output).To(Equal("10"))
		Expect(got.Prices.CacheWrite).To(Equal(d.Entry.Prices.CacheWrite))
		Expect(got.Prices.CacheWrite).NotTo(BeEmpty(), "Claude bills cache writes above the input rate")
		Expect(got.Ratio).To(Equal(d.Entry.Ratio))
		Expect(got.HiddenThinking).To(Equal(d.Entry.HiddenThinking))
		Expect(got.MaxOutput).To(Equal(d.Entry.MaxOutput))
		Expect(got.Note).To(HavePrefix("Migrated from the rates set on the model; the other settings come from the default entry"))
		Expect(prices.NeedsReview("migration 00014", got.Note)).To(BeFalse())
		checked := got
		checked.ID = "x"
		_, err = checked.Resolve(true)
		Expect(err).NotTo(HaveOccurred())

		By("a second run changes nothing")
		again, err := svc.CompleteMigrated(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(read(ctx, e.org)).To(Equal(got))
		Expect(again).To(BeNumerically(">=", 0))
	})

	It("keeps generic settings and asks for a review when no default entry matches", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "10.00")
		migrated(ctx, e.org, "openai", "acme/custom-model", `{"input":"1","output":"2"}`)
		_, err := prices.New(db, nil, priceList).CompleteMigrated(ctx)
		Expect(err).NotTo(HaveOccurred())
		got := read(ctx, e.org)
		Expect(got.Tokenizer).To(Equal(pricing.TokenizerBytes))
		Expect(got.Ratio).To(Equal("3.0"))
		Expect(got.MaxOutput).To(Equal(131072))
		Expect(got.Prices.CacheWrite).To(BeEmpty())
		Expect(got.Note).To(Equal(prices.NoteReview))
		Expect(prices.NeedsReview("migration 00014", got.Note)).To(BeTrue())
	})
})
