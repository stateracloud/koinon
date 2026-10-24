// SPDX-License-Identifier: Apache-2.0

package ledger_test

import (
	"encoding/json"
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
	"github.com/stateracloud/koinon/pricing"
)

const deepseek = "deepseek/deepseek-v4.1-flash"

// model serves name on a provider of the given type for the env's org.
func (e *env) model(ctx SpecContext, typ, name string) {
	var pid pgtype.UUID
	Expect(db.QueryRow(ctx, `INSERT INTO providers (org_id, name, type, url) VALUES ($1, $2, $3, 'https://example.com/v1') RETURNING id`,
		e.org, uniq("prov"), typ).Scan(&pid)).To(Succeed())
	_, err := db.Exec(ctx, `INSERT INTO models (org_id, provider_id, name) VALUES ($1, $2, $3)`, e.org, pid, name)
	Expect(err).NotTo(HaveOccurred())
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
		e := newEnv(ctx, uniq("org"), "", "10.00", 1<<40)
		e.model(ctx, "openrouter", deepseek)
		// 1M input of which 500k were cache reads, 1k output. The gateway agrees.
		_, err := ing.Ingest(ctx, []ledger.Entry{e.usage(time.Now(), deepseek, 1_000_000, 1000, 500_000, "0.0185")})
		Expect(err).NotTo(HaveOccurred())
		s := e.lastEntry(ctx)
		// 500k * 0.02 + 500k * 0.015 + 1k * 1.00, per million.
		Expect(ledger.FormatUSD(s.CostUsd)).To(Equal("0.0185"))
		Expect(s.CostFlagged).To(BeFalse())
		Expect(s.Estimated).To(BeFalse())
		list, _ := pricing.Default()
		Expect(s.PriceRef).To(Equal(pricing.Ref(list.Version, "openrouter-deepseek-v4-1-flash")))
		Expect(s.CacheReadTokens).To(Equal(int64(500_000)))
	})

	It("charges its own cost and flags the entry when the gateway differs by more than 1%", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "10.00", 1<<40)
		e.model(ctx, "openrouter", deepseek)
		_, err := ing.Ingest(ctx, []ledger.Entry{
			e.usage(time.Now(), deepseek, 1_000_000, 0, 0, "0.0201"), // 0.02 vs 0.0201: 0.5%
			e.usage(time.Now(), deepseek, 1_000_000, 0, 0, "0.03"),   // 50%
		})
		Expect(err).NotTo(HaveOccurred())
		s := e.lastEntry(ctx)
		Expect(s.CostFlagged).To(BeTrue())
		Expect(ledger.FormatUSD(s.CostUsd)).To(Equal("0.02"))
		Expect(ledger.FormatUSD(s.GatewayCostUsd)).To(Equal("0.03"))
		flagged, err := q.ListFlaggedPrices(ctx, store.ListFlaggedPricesParams{OrgID: e.org, Since: pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true}})
		Expect(err).NotTo(HaveOccurred())
		Expect(flagged).To(HaveLen(1))
		Expect(flagged[0].Flagged).To(Equal(int64(1)))
		Expect(flagged[0].GatewayDiff).To(Equal(int64(1)), "the reason is counted")
		Expect(flagged[0].UsageMissing + flagged[0].PriceError + flagged[0].MaxOutputCapped).To(BeZero())
	})

	It("lets an org override win and records its version", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "10.00", 1<<40)
		e.model(ctx, "openrouter", deepseek)
		svc := prices.New(db, nil)
		o, err := svc.SetOverride(ctx, e.org, audit.User(e.user), prices.OverrideInput{
			Entry: pricing.Entry{
				ID: "ds-contract", Provider: "openrouter", Match: pricing.Match{Glob: "deepseek/*"},
				Spec:      pricing.Spec{Prices: pricing.Wire{Input: "1", Output: "2"}},
				Tokenizer: pricing.TokenizerBytes, Ratio: "3.0", MaxOutput: 4096,
			},
			ValidFrom: time.Now().Add(-time.Hour),
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = ing.Ingest(ctx, []ledger.Entry{e.usage(time.Now(), deepseek, 1_000_000, 0, 0, "")})
		Expect(err).NotTo(HaveOccurred())
		s := e.lastEntry(ctx)
		Expect(ledger.FormatUSD(s.CostUsd)).To(Equal("1.00"))
		Expect(s.PriceRef).To(Equal("org#" + o.ID))

		// Requests before the override's valid_from keep the default price.
		_, err = ing.Ingest(ctx, []ledger.Entry{e.usage(time.Now().Add(-2*time.Hour), deepseek, 1_000_000, 0, 0, "")})
		Expect(err).NotTo(HaveOccurred())
		Expect(ledger.FormatUSD(e.lastEntry(ctx).CostUsd)).To(Equal("0.02"))

		var n int
		Expect(db.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'price.set'`, e.org).Scan(&n)).To(Succeed())
		Expect(n).To(Equal(1))
	})

	It("bills a bucket an org price does not name at the rate it would have without the split", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "10.00", 1<<40)
		e.model(ctx, "openrouter", "anthropic/claude-sonnet-5.5")
		_, err := prices.New(db, nil).SetOverride(ctx, e.org, audit.User(e.user), prices.OverrideInput{
			Entry: pricing.Entry{
				ID: "sonnet-mine", Provider: "openrouter", Match: pricing.Match{Glob: "anthropic/claude-sonnet-5.5"},
				Spec:      pricing.Spec{Prices: pricing.Wire{Input: "2", Output: "10"}},
				Tokenizer: pricing.TokenizerBytes, Ratio: "3.0", MaxOutput: 4096,
			},
			ValidFrom: time.Now().Add(-time.Hour),
		})
		Expect(err).NotTo(HaveOccurred())
		u := e.usage(time.Now(), "anthropic/claude-sonnet-5.5", 1_000_000, 0, 0, "")
		u.CacheWriteTokens = 1_000_000
		_, err = ing.Ingest(ctx, []ledger.Entry{u})
		Expect(err).NotTo(HaveOccurred())
		s := e.lastEntry(ctx)
		Expect(ledger.FormatUSD(s.CostUsd)).To(Equal("2.00"), "the cache write is billed at the input rate")
		Expect(s.PriceRef).To(HavePrefix("org#"))
	})

	It("estimates missing usage, marks it and counts it toward the limit", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "0.01", 1<<40)
		e.model(ctx, "openrouter", deepseek)
		t := e.ticket(ctx)
		r := e.startRun(ctx, t, 1<<40, time.Now().Add(-time.Minute))
		x := ledger.Entry{
			RequestID: uniq("req"), KeyID: "k-test", Agent: e.ref(), Model: deepseek, Status: 200,
			MaxTokens: 1000, RequestBytes: 3_000_000, ResponseBytes: -1, Time: time.Now(), // 1M tokens at 3.0 bytes per token
		}
		_, err := ing.Ingest(ctx, []ledger.Entry{x})
		Expect(err).NotTo(HaveOccurred())
		s := e.lastEntry(ctx)
		Expect(s.Estimated).To(BeTrue())
		Expect(s.InputTokens).To(Equal(int64(1_000_000)))
		Expect(s.OutputTokens).To(Equal(int64(1000)), "an admitted response without usage settles at max_tokens")
		Expect(ledger.FormatUSD(s.CostUsd)).To(Equal("0.021"))
		var est map[string]any
		Expect(json.Unmarshal(s.Estimate, &est)).To(Succeed())
		Expect(est).To(HaveKey("estimate"))
		Expect(e.run(ctx, r.ID).State).To(Equal(store.RunStateCanceled), "an estimate over the limit stops the run")

		rep, err := q.SpendReport(ctx, store.SpendReportParams{OrgID: e.org, Since: pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true}})
		Expect(err).NotTo(HaveOccurred())
		Expect(rep).To(HaveLen(1))
		Expect(rep[0].EstimatedRequests).To(Equal(int64(1)))
		Expect(ledger.FormatUSD(rep[0].ReportedUsd)).To(Equal("0.00"))
		Expect(ledger.FormatUSD(rep[0].EstimatedUsd)).To(Equal("0.021"))
	})

	It("raises, never lowers, the estimate with the learned p90", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "100.00", 1<<40)
		e.model(ctx, "openrouter", deepseek)
		var batch []ledger.Entry
		for i := 0; i < prices.MinCalibrationSamples; i++ {
			u := e.usage(time.Now(), deepseek, 1000, 10, 0, "")
			u.RequestBytes = 1000 // 1 token per byte: far denser than the static 3.0 bytes per token
			batch = append(batch, u)
		}
		_, err := ing.Ingest(ctx, batch)
		Expect(err).NotTo(HaveOccurred())
		_, err = ing.Ingest(ctx, []ledger.Entry{{
			RequestID: uniq("req"), KeyID: "k-test", Agent: e.ref(), Model: deepseek, Status: 200,
			RequestBytes: 3000, ResponseBytes: -1, Time: time.Now(),
		}})
		Expect(err).NotTo(HaveOccurred())
		Expect(e.lastEntry(ctx).InputTokens).To(Equal(int64(3000)), "static ratio would say 1000")
	})
})

var _ = Describe("An unusable stored price", func() {
	BeforeEach(needDocker)

	It("records the request unpriced and flagged, and never stops the batch or other orgs", func(ctx SpecContext) {
		bad := newEnv(ctx, uniq("org"), "", "10.00", 1<<40)
		good := newEnv(ctx, uniq("org"), "", "10.00", 1<<40)
		bad.model(ctx, "openrouter", deepseek)
		good.model(ctx, "openrouter", deepseek)
		// A rate from before the stricter validation, and an override that no longer parses.
		_, err := db.Exec(ctx, `INSERT INTO price_overrides (org_id, name, entry, valid_from, created_by) VALUES ($1, 'old', '{"provider":"openrouter","match":{"glob":"deepseek/*"},"validFrom":"2020-01-01","prices":{"input":"1.1234567","output":"1"},"tokenizer":"bytes","ratio":"3.0","maxOutput":10}', now() - interval '1 hour', 'test')`, bad.org)
		Expect(err).NotTo(HaveOccurred())

		res, err := ing.Ingest(ctx, []ledger.Entry{
			bad.usage(time.Now(), deepseek, 1000, 10, 0, "0.5"),
			good.usage(time.Now(), deepseek, 1_000_000, 0, 0, ""),
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Recorded).To(Equal(2))
		b := bad.lastEntry(ctx)
		Expect(b.CostFlagged).To(BeTrue())
		Expect(b.PriceRef).To(BeEmpty())
		Expect(string(b.Estimate)).To(ContainSubstring("price_error"))
		Expect(ledger.FormatUSD(b.CostUsd)).To(Equal("0.50"), "the gateway's cost stands in")
		Expect(ledger.FormatUSD(good.lastEntry(ctx).CostUsd)).To(Equal("0.02"))

		_, err = db.Exec(ctx, `INSERT INTO price_overrides (org_id, name, entry, valid_from, created_by) VALUES ($1, 'broken', '{"id":"broken"}', now() - interval '1 hour', 'test')`, good.org)
		Expect(err).NotTo(HaveOccurred())
		res, err = ing.Ingest(ctx, []ledger.Entry{good.usage(time.Now(), deepseek, 1000, 10, 0, "")})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Recorded).To(Equal(1))
		Expect(good.lastEntry(ctx).CostFlagged).To(BeTrue())
	})

	It("caps a settled output, so an absurd max_tokens blocks neither ingest nor the budgets of anyone else", func(ctx SpecContext) {
		abuser := newEnv(ctx, uniq("org"), "", "10.00", 1<<62)
		other := newEnv(ctx, uniq("org"), "", "10.00", 1<<62)
		abuser.model(ctx, "openrouter", "anthropic/claude-fable-5.1")
		other.model(ctx, "openrouter", deepseek)
		var batch []ledger.Entry
		for i := 0; i < 5; i++ {
			batch = append(batch, ledger.Entry{
				RequestID: uniq("req"), KeyID: "k-test", Agent: abuser.ref(), Model: "anthropic/claude-fable-5.1", Status: 200,
				MaxTokens: 9_000_000_000_000, RequestBytes: 100, ResponseBytes: -1, Time: time.Now(),
			})
		}
		batch = append(batch, other.usage(time.Now(), deepseek, 1_000_000, 0, 0, ""))
		res, err := ing.Ingest(ctx, batch)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Recorded).To(Equal(6))
		s := abuser.lastEntry(ctx)
		Expect(s.Estimated).To(BeTrue())
		Expect(s.CostFlagged).To(BeTrue(), "a 2xx without usage is flagged")
		Expect(string(s.Estimate)).To(ContainSubstring(`"usage_missing": true`))
		Expect(string(s.Estimate)).To(ContainSubstring(`"max_output_capped": true`))
		// 128000 output tokens at $50 per million, five times, is $32; nothing near overflow.
		var sum string
		Expect(db.QueryRow(ctx, `SELECT sum(cost_usd)::text FROM spend_entries WHERE org_id = $1`, abuser.org).Scan(&sum)).To(Succeed())
		Expect(sum).To(HavePrefix("32.0"))

		// The budgets of both orgs still compute.
		_, err = ledger.AgentLimit(ctx, q, abuser.agent, time.Now())
		Expect(err).NotTo(HaveOccurred())
		l, err := ledger.AgentLimit(ctx, q, other.agent, time.Now())
		Expect(err).NotTo(HaveOccurred())
		Expect(l.Amount).To(BeNumerically("~", 9.98, 0.001))

		// A sum beyond what a field could hold saturates instead of failing.
		_, err = db.Exec(ctx, `INSERT INTO spend_entries (org_id, agent_id, request_id, key_id, model, input_tokens, output_tokens, cost_usd, priced, occurred_at)
			VALUES ($1, $2, 'huge', 'k', 'm', 1, 1, 999999999999.5, true, now())`, abuser.org, abuser.agent.ID)
		Expect(err).NotTo(HaveOccurred())
		l, err = ledger.AgentLimit(ctx, q, abuser.agent, time.Now())
		Expect(err).NotTo(HaveOccurred())
		Expect(l.Amount).To(BeZero())
	})

	It("charges an upstream 4xx without usage the input estimate only, flagged usage_missing", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "100.00", 1<<40)
		e.model(ctx, "openrouter", deepseek)
		x := ledger.Entry{
			RequestID: uniq("req"), KeyID: "k-test", Agent: e.ref(), Model: deepseek, Status: 400,
			MaxTokens: 4000, RequestBytes: 3_000_000, ResponseBytes: -1, Time: time.Now(),
		}
		_, err := ing.Ingest(ctx, []ledger.Entry{x})
		Expect(err).NotTo(HaveOccurred())
		s := e.lastEntry(ctx)
		Expect(s.InputTokens).To(Equal(int64(1_000_000)))
		Expect(s.OutputTokens).To(BeZero())
		Expect(ledger.FormatUSD(s.CostUsd)).To(Equal("0.02"))
		Expect(s.CostFlagged).To(BeTrue())
		Expect(string(s.Estimate)).To(ContainSubstring(`"usage_missing": true`))

		x.RequestID, x.Status = uniq("req"), 502
		_, err = ing.Ingest(ctx, []ledger.Entry{x})
		Expect(err).NotTo(HaveOccurred())
		Expect(e.lastEntry(ctx).OutputTokens).To(Equal(int64(4000)), "a 5xx settles output at max_tokens")
	})

	It("settles an aborted response without usage at max_tokens, or at the entry's max_output when it is not logged", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "100.00", 1<<40)
		e.model(ctx, "openrouter", deepseek)
		x := func(max int64) ledger.Entry {
			return ledger.Entry{
				RequestID: uniq("req"), KeyID: "k-test", Agent: e.ref(), Model: deepseek, Status: 200, MaxTokens: max,
				InputKnown: true, InputTokens: 1000, RequestBytes: 3000, ResponseBytes: -1, Time: time.Now(),
			}
		}
		_, err := ing.Ingest(ctx, []ledger.Entry{x(2000)})
		Expect(err).NotTo(HaveOccurred())
		s := e.lastEntry(ctx)
		Expect(s.Estimated).To(BeTrue())
		// 1000 reported input at 0.02 + 2000 output at 1.00, per million.
		Expect(ledger.FormatUSD(s.CostUsd)).To(Equal("0.00202"))
		_, err = ing.Ingest(ctx, []ledger.Entry{x(0)})
		Expect(err).NotTo(HaveOccurred())
		s = e.lastEntry(ctx)
		Expect(string(s.Estimate)).To(ContainSubstring(`"max_output_fallback": true`))
	})
})

var _ = Describe("Budgets", func() {
	BeforeEach(needDocker)

	It("cancels every open run of the org at its monthly cap, naming the limit, and alerts at 80% and 100%", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "100.00", 1<<40)
		e.model(ctx, "openrouter", deepseek)
		svc := budget.New(db)
		Expect(svc.SetOrgCap(ctx, e.org, audit.User(e.user), ptr(money.MustParse("0.10")))).To(Succeed())
		t := e.ticket(ctx)
		r := e.startRun(ctx, t, 1<<40, time.Now().Add(-time.Minute))

		// 0.02 per entry: 20%, 40%, 60%, 80%.
		for i := 0; i < 4; i++ {
			_, err := ing.Ingest(ctx, []ledger.Entry{e.usage(time.Now(), deepseek, 1_000_000, 0, 0, "")})
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(e.run(ctx, r.ID).State).To(Equal(store.RunStateRunning))
		ns, err := q.ListNotifications(ctx, store.ListNotificationsParams{OrgID: e.org, UserID: e.user, MaxRows: 10})
		Expect(err).NotTo(HaveOccurred())
		Expect(ns).To(HaveLen(1))
		Expect(ns[0].Kind).To(Equal(budget.KindWarning))

		_, err = ing.Ingest(ctx, []ledger.Entry{e.usage(time.Now(), deepseek, 1_000_000, 0, 0, "")})
		Expect(err).NotTo(HaveOccurred())
		run := e.run(ctx, r.ID)
		Expect(run.State).To(Equal(store.RunStateCanceled))
		Expect(run.OutcomeReason.String).To(HavePrefix("org monthly cap reached ($0.10 of $0.10"))
		ns, err = q.ListNotifications(ctx, store.ListNotificationsParams{OrgID: e.org, UserID: e.user, MaxRows: 10})
		Expect(err).NotTo(HaveOccurred())
		Expect(ns).To(HaveLen(2))

		// More spend in the same window does not alert again.
		_, err = ing.Ingest(ctx, []ledger.Entry{e.usage(time.Now(), deepseek, 1_000_000, 0, 0, "")})
		Expect(err).NotTo(HaveOccurred())
		ns, _ = q.ListNotifications(ctx, store.ListNotificationsParams{OrgID: e.org, UserID: e.user, MaxRows: 10})
		Expect(ns).To(HaveLen(2))
	})

	It("pushes the smallest remaining amount of the agent, the org and the platform", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "10.00", 1<<40)
		e.model(ctx, "openrouter", deepseek)
		svc := budget.New(db)
		now := time.Now()
		l, err := ledger.AgentLimit(ctx, q, e.agent, now)
		Expect(err).NotTo(HaveOccurred())
		Expect(l.Amount).To(BeNumerically("==", 10), "the template's value is the default")

		Expect(svc.SetAgentDaily(ctx, e.org, e.agent.ID, audit.User(e.user), money.MustParse("5"))).To(Succeed())
		l, _ = ledger.AgentLimit(ctx, q, e.agent, now)
		Expect(l.Amount).To(BeNumerically("==", 5), "an admin's limit replaces the template's")

		Expect(svc.SetOrgCap(ctx, e.org, audit.User(e.user), ptr(money.MustParse("3")))).To(Succeed())
		l, _ = ledger.AgentLimit(ctx, q, e.agent, now)
		Expect(l.Amount).To(BeNumerically("==", 3))

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

		_, err = ing.Ingest(ctx, []ledger.Entry{e.usage(now, deepseek, 50_000_000, 0, 0, "")}) // 1.00
		Expect(err).NotTo(HaveOccurred())
		l, _ = ledger.AgentLimit(ctx, q, e.agent, now)
		Expect(l.Amount).To(BeNumerically("==", 2), "min(5-1, 3-1, 4-1)")

		var n int
		Expect(db.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action LIKE 'budget.%'`, e.org).Scan(&n)).To(Succeed())
		Expect(n).To(Equal(3), "every accepted budget change is audited, refused ones are not")
	})

	It("tells platform admins when an org reaches its ceiling", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "100.00", 1<<40)
		e.model(ctx, "openrouter", deepseek)
		_, err := db.Exec(ctx, `INSERT INTO platform_admins (user_id) VALUES ($1)`, e.user)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx SpecContext) { _, _ = db.Exec(ctx, `DELETE FROM platform_admins WHERE user_id = $1`, e.user) })
		Expect(budget.New(db).SetPlatformCeiling(ctx, e.org, audit.User(e.user), e.user, ptr(money.MustParse("0.02")))).To(Succeed())
		t := e.ticket(ctx)
		r := e.startRun(ctx, t, 1<<40, time.Now().Add(-time.Minute))
		_, err = ing.Ingest(ctx, []ledger.Entry{e.usage(time.Now(), deepseek, 1_000_000, 0, 0, "")})
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
