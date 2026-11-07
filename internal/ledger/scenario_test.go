// SPDX-License-Identifier: Apache-2.0

package ledger_test

import (
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/ledger"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/pricing"
	"github.com/stateracloud/koinon/test/scenarios"
)

// The ledger of the kind e2e's repo-helper scenario (test/scenarios), replayed without a cluster:
// the quickstart's provider type is openai, which does not bill a refused request, so the title
// request the fake model refuses is waived, not estimated (old issue 241, old issue 253), and the streamed requests
// carry usage. The e2e once still asserted EstimatedRequests >= 1 after old issue 253 (old issue 258); a rule change
// now fails here first.
var _ = Describe("the repo-helper scenario's ledger", func() {
	BeforeEach(needDocker)

	l := scenarios.PullRequest.Ledger

	// scenarioEnv is an org that serves flash on an openai provider, priced by an org entry (the
	// fixture list prices it for openrouter), with one running run.
	scenarioEnv := func(ctx SpecContext) (*env, store.Run) {
		e := newEnv(ctx, uniq("org"), "", "100.00")
		e.model(ctx, "openai", flash)
		_, err := prices.New(db, nil, priceList).SetOverride(ctx, e.org, audit.User(e.user), prices.OverrideInput{
			Entry: pricing.Entry{
				ID: "scenario-openai", Provider: "openai", Match: pricing.Match{Glob: flash},
				Spec:      pricing.Spec{Prices: pricing.Wire{Input: "0.20", Output: "2", CacheRead: "0.05"}},
				Tokenizer: pricing.TokenizerBytes, Ratio: "3.0", MaxOutput: 1000000,
			},
			ValidFrom: time.Now().Add(-time.Hour),
		})
		Expect(err).NotTo(HaveOccurred())
		return e, e.startRun(ctx, e.task(ctx), time.Now().Add(-time.Minute))
	}
	// refusedTitle is the title request the fake model refuses: a 400 from the provider, no usage.
	refusedTitle := func(e *env, at time.Time) ledger.Entry {
		return ledger.Entry{
			RequestID: uniq("req"), KeyID: "k-test", Agent: e.ref(), Model: flash, Status: int64(l.RefusedStatus),
			UpstreamKnown: true, Upstream: true, MaxTokens: 4000, RequestBytes: 3_000, ResponseBytes: -1, Time: at,
		}
	}
	// streamed is a request the provider answered with usage, as the script's turns report it: in is
	// the provider's total input and includes cached; the usage record carries disjoint buckets.
	streamed := func(e *env, at time.Time, in, cached, out int64, cost string) ledger.Entry {
		x := e.usage(at, flash, in-cached, out, cached, cost)
		x.Status = 200
		return x
	}
	entries := func(ctx SpecContext, e *env) (refused, answered []scenarios.Entry) {
		GinkgoHelper()
		rows, err := db.Query(ctx, `SELECT coalesce(upstream_status, 0), input_tokens, output_tokens, cost_usd = 0, coalesce(estimate->>'charge_waived', ''), estimated
			FROM spend_entries WHERE org_id = $1 ORDER BY created_at, id`, e.org)
		Expect(err).NotTo(HaveOccurred())
		defer rows.Close()
		for rows.Next() {
			var x scenarios.Entry
			Expect(rows.Scan(&x.UpstreamStatus, &x.Input, &x.Output, &x.CostZero, &x.ChargeWaived, &x.Estimated)).To(Succeed())
			if x.UpstreamStatus != 0 {
				refused = append(refused, x)
			} else {
				answered = append(answered, x)
			}
		}
		Expect(rows.Err()).NotTo(HaveOccurred())
		return refused, answered
	}
	totals := func(ctx SpecContext, e *env, run pgtype.UUID) scenarios.Totals {
		GinkgoHelper()
		rows, err := q.SumRunCosts(ctx, store.SumRunCostsParams{OrgID: e.org, RunIds: []pgtype.UUID{run}})
		Expect(err).NotTo(HaveOccurred())
		if len(rows) == 0 {
			return scenarios.Totals{}
		}
		return scenarios.Totals{Requests: rows[0].Requests, EstimatedRequests: rows[0].EstimatedRequests, InputTokens: rows[0].InputTokens, OutputTokens: rows[0].OutputTokens}
	}

	It("waives the refused title request and records the streamed ones with their usage, whichever is ingested first", func(ctx SpecContext) {
		e, run := scenarioEnv(ctx)
		t0 := time.Now().Add(-30 * time.Second)

		By("the refused entry reaches the ledger first (the gateway exports in batches, old issue 291)")
		_, err := ing.Ingest(ctx, []ledger.Entry{refusedTitle(e, t0.Add(time.Second))})
		Expect(err).NotTo(HaveOccurred())
		first := totals(ctx, e, run.ID)
		Expect(first.Requests).To(BeNumerically(">", 0), "the run has a request: this is what a careless wait stops at")
		Expect(l.Settled(first)).To(BeFalse(), "a run with only the zero-token refusal is not settled: %+v", first)

		By("then the streamed requests, the script's two first turns, the second one reading the cache")
		// 1200 in / 60 out at 0.20 and 2 per million; then 1350 in of which 1200 cached, 24 out.
		_, err = ing.Ingest(ctx, []ledger.Entry{
			streamed(e, t0, 1200, 0, 60, "0.00036"),
			streamed(e, t0.Add(2*time.Second), 1350, 1200, 24, "0.000138"),
		})
		Expect(err).NotTo(HaveOccurred())

		got := totals(ctx, e, run.ID)
		Expect(l.Settled(got)).To(BeTrue(), "%+v", got)
		Expect(l.CheckTotals(got)).To(Succeed())
		refused, answered := entries(ctx, e)
		Expect(refused).To(HaveLen(1))
		Expect(l.CheckRefused(refused[0])).To(Succeed())
		Expect(answered).To(HaveLen(2))
		for _, a := range answered {
			Expect(l.CheckStreamed(a)).To(Succeed())
		}
		Expect(e.run(ctx, run.ID).TokensUsed).To(BeEquivalentTo(1200+60+1350+24), "the waived refusal adds nothing to the run's tokens")
	})

	It("records the same ledger when the streamed requests come first", func(ctx SpecContext) {
		e, run := scenarioEnv(ctx)
		t0 := time.Now().Add(-30 * time.Second)
		_, err := ing.Ingest(ctx, []ledger.Entry{
			streamed(e, t0, 1200, 0, 60, "0.00036"),
			refusedTitle(e, t0.Add(time.Second)),
		})
		Expect(err).NotTo(HaveOccurred())
		got := totals(ctx, e, run.ID)
		Expect(l.Settled(got)).To(BeTrue(), "%+v", got)
		Expect(l.CheckTotals(got)).To(Succeed())
	})

	It("charges the cache read at the cache-read rate", func(ctx SpecContext) {
		e, _ := scenarioEnv(ctx)
		// 150 uncached input at 0.20, 1200 cache read at 0.05, 24 output at 2, per million.
		_, err := ing.Ingest(ctx, []ledger.Entry{streamed(e, time.Now().Add(-time.Second), 1350, 1200, 24, "0.000138")})
		Expect(err).NotTo(HaveOccurred())
		s := e.lastEntry(ctx)
		Expect(ledger.FormatUSD(s.CostUsd)).To(Equal("0.000138"))
		Expect(s.CacheReadTokens).To(BeEquivalentTo(1200))
		Expect(s.CostFlagged).To(BeFalse(), "the gateway's figure and the ledger's agree")
	})
})
