// SPDX-License-Identifier: Apache-2.0

package ledger_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/budget"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/ledger"
	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/internal/money"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/providerusage"
	"github.com/stateracloud/koinon/pricing"
)

// fakeSource answers lookups from a map; an ID without an answer is pending.
type fakeSource struct {
	mu      sync.Mutex
	answers map[string]providerusage.Usage
	calls   []string
}

func (f *fakeSource) Accepts(id string) bool { return strings.HasPrefix(id, "gen-") }

func (f *fakeSource) Serves(string) bool { return true }

func (f *fakeSource) Usage(_ context.Context, _ providerusage.Provider, id string) (providerusage.Usage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, id)
	if u, ok := f.answers[id]; ok {
		return u, nil
	}
	return providerusage.Usage{}, providerusage.ErrPending
}

// callsFor counts the lookups of id (other specs' entries may be settled by the same pass).
func (f *fakeSource) callsFor(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == id {
			n++
		}
	}
	return n
}

// throttlingSource answers 429 with Retry-After while after is set, and pending otherwise.
type throttlingSource struct {
	after time.Duration
	calls int
	ids   []string
}

func (t *throttlingSource) Accepts(id string) bool { return strings.HasPrefix(id, "gen-") }

func (t *throttlingSource) Serves(string) bool { return true }

func (t *throttlingSource) Usage(_ context.Context, _ providerusage.Provider, id string) (providerusage.Usage, error) {
	t.calls++
	t.ids = append(t.ids, id)
	if t.after > 0 {
		return providerusage.Usage{}, &providerusage.RetryAfterError{After: t.after}
	}
	return providerusage.Usage{}, providerusage.ErrPending
}

// roundTrip lets a spec stand in for the network.
type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func (f *fakeSource) answer(id string, u providerusage.Usage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.answers == nil {
		f.answers = map[string]providerusage.Usage{}
	}
	f.answers[id] = u
}

var _ = Describe("Settling responses cut short", func() {
	BeforeEach(needDocker)

	var (
		e       *env
		src     *fakeSource
		clock   time.Time
		in      *ledger.Ingester
		settler *ledger.Settler
		run     store.Run
	)
	BeforeEach(func(ctx SpecContext) {
		e = newEnv(ctx, uniq("org"), "", "1.00")
		e.model(ctx, "openrouter", flash)
		src = &fakeSource{}
		// The specs move the clock by up to a few hours and read the agent's UTC-day limit at the
		// end: a clock that starts within those hours of UTC midnight would cross into the next
		// day, where the day's charges no longer count. Start at noon UTC of the current day.
		clock = time.Now().UTC().Truncate(24 * time.Hour).Add(12 * time.Hour)
		now := func() time.Time { return clock }
		sources := providerusage.Registry{"openrouter": src}
		in = &ledger.Ingester{DB: db, Log: ing.Log, Prices: ing.Prices, Sources: sources, Now: now}
		settler = &ledger.Settler{DB: db, Prices: ing.Prices, Sources: sources, Log: ing.Log, Now: now}
		run = e.startRun(ctx, e.task(ctx), clock.Add(-time.Minute))
	})

	// cut is a response canceled mid-stream: 1000 input logged, no output, max_tokens 200k.
	cut := func(id string) ledger.Entry {
		return ledger.Entry{
			RequestID: uniq("req"), KeyID: "k-test", Agent: e.ref(), Model: flash, Status: 200, MaxTokens: 200_000,
			InputKnown: true, InputTokens: 1000, RequestBytes: 3000, ResponseBytes: -1, ResponseID: id, Time: clock,
		}
	}
	spent := func(ctx SpecContext) (charges, held string, rows int) {
		Expect(db.QueryRow(ctx, `SELECT coalesce(sum(cost_usd), 0)::text, coalesce(sum(reserve_usd), 0)::text, count(*)
			FROM spend_entries WHERE org_id = $1`, e.org).Scan(&charges, &held, &rows)).To(Succeed())
		return ledger.FormatUSD(charges), ledger.FormatUSD(held), rows
	}
	revisions := func(ctx SpecContext) []store.SpendEntryRevision {
		var id pgtype.UUID
		Expect(db.QueryRow(ctx, `SELECT id FROM spend_entries WHERE org_id = $1`, e.org).Scan(&id)).To(Succeed())
		rs, err := q.ListSpendEntryRevisions(ctx, store.ListSpendEntryRevisionsParams{OrgID: e.org, EntryID: id})
		Expect(err).NotTo(HaveOccurred())
		return rs
	}

	It("replaces the provisional entry with the provider's usage and cost, once, with a revision", func(ctx SpecContext) {
		_, err := in.Ingest(ctx, []ledger.Entry{cut("gen-a")})
		Expect(err).NotTo(HaveOccurred())
		charges, held, _ := spent(ctx)
		Expect(charges).To(Equal("0.0002"), "the logged usage: 1000 in at 0.20 per million, no output")
		Expect(held).To(Equal("0.40"), "the bound (1000 in + 200k max_tokens out at 2, $0.4002) minus the charge")
		Expect(ledger.AgentLimit(ctx, q, e.agent, clock)).To(HaveField("Amount", Equal(money.MustParse("0.5998"))))

		By("pending: the attempt is deferred and nothing changes")
		clock = clock.Add(time.Minute)
		_, err = settler.SettleDue(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(e.provisional(ctx)).To(Equal(1))
		Expect(src.callsFor("gen-a")).To(Equal(1))
		var attempts int
		Expect(db.QueryRow(ctx, `SELECT attempts FROM spend_settlements WHERE org_id = $1`, e.org).Scan(&attempts)).To(Succeed())
		Expect(attempts).To(Equal(1))

		By("the provider reports: the entry takes its usage and bill, the reserve is released")
		// Koinon's price for these tokens: 1000 in at 0.20 + 250 out at 2, per million.
		src.answer("gen-a", providerusage.Usage{InputTokens: 1000, OutputTokens: 250, Cost: money.MustParse("0.0007")})
		clock = clock.Add(time.Hour / 2)
		_, err = settler.SettleDue(ctx)
		Expect(err).NotTo(HaveOccurred())
		charges, held, rows := spent(ctx)
		Expect([]any{charges, held, rows}).To(Equal([]any{"0.0007", "0.00", 1}), "replaced in place: no second entry, nothing held")
		Expect(e.lastEntry(ctx).CostFlagged).To(BeTrue(), "still flagged usage_missing from ingest")
		Expect(string(e.lastEntry(ctx).Estimate)).NotTo(ContainSubstring("provider_diff"), "Koinon's price agrees with the bill")
		s := e.lastEntry(ctx)
		Expect(s.OutputTokens).To(Equal(int64(250)))
		Expect(s.Estimated).To(BeFalse())
		Expect(string(s.Estimate)).To(ContainSubstring(`"settled"`))
		Expect(e.provisional(ctx)).To(BeZero())
		Expect(e.run(ctx, run.ID).TokensUsed).To(Equal(int64(1250)), "the provider's extra tokens reach the run")

		rs := revisions(ctx)
		Expect(rs).To(HaveLen(1))
		Expect(rs[0].Reason).To(Equal("provider"))
		Expect(rs[0].Source).To(Equal("openrouter"))
		var before, after map[string]any
		Expect(json.Unmarshal(rs[0].Before, &before)).To(Succeed())
		Expect(json.Unmarshal(rs[0].After, &after)).To(Succeed())
		Expect(before).To(HaveKeyWithValue("output_tokens", BeNumerically("==", 0)))
		Expect(after).To(HaveKeyWithValue("output_tokens", BeNumerically("==", 250)))
		Expect(after).To(HaveKeyWithValue("provisional", false))

		By("nothing left to settle, and a replayed export changes nothing")
		clock = clock.Add(time.Hour)
		_, err = settler.SettleDue(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(src.callsFor("gen-a")).To(Equal(2), "a settled entry is never asked about again")
		Expect(revisions(ctx)).To(HaveLen(1))
	})

	It("keeps the logged usage as final when the provider has no figure by the deadline", func(ctx SpecContext) {
		_, err := in.Ingest(ctx, []ledger.Entry{cut("gen-b")})
		Expect(err).NotTo(HaveOccurred())
		clock = clock.Add(ledger.SettleWindow + time.Minute)
		_, err = settler.SettleDue(ctx)
		Expect(err).NotTo(HaveOccurred())
		charges, held, _ := spent(ctx)
		Expect([]any{charges, held}).To(Equal([]any{"0.0002", "0.00"}), "the logged usage, never max_tokens")
		Expect(e.provisional(ctx)).To(BeZero())
		rs := revisions(ctx)
		Expect(rs).To(HaveLen(1))
		Expect(rs[0].Reason).To(Equal("expired"))
		Expect(rs[0].Source).To(Equal("gateway"), "the logged usage stands")
		Expect(ledger.AgentLimit(ctx, q, e.agent, clock)).To(HaveField("Amount", Equal(money.MustParse("0.9998"))))
	})

	It("does not ask a provider whose key was lost, and settles once the key is entered again", func(ctx SpecContext) {
		_, err := in.Ingest(ctx, []ledger.Entry{cut("gen-lost")})
		Expect(err).NotTo(HaveOccurred())
		_, err = db.Exec(ctx, `UPDATE providers SET api_key = NULL, credential_lost_at = now() WHERE org_id = $1`, e.org)
		Expect(err).NotTo(HaveOccurred())

		By("lost: the provider is not asked, and the entry stays provisional with the reason")
		clock = clock.Add(time.Minute)
		_, err = settler.SettleDue(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(src.callsFor("gen-lost")).To(BeZero(), "no request without the key")
		Expect(e.provisional(ctx)).To(Equal(1), "a key entered again before the deadline can still settle it")
		var lastErr string
		Expect(db.QueryRow(ctx, `SELECT last_error FROM spend_settlements WHERE org_id = $1`, e.org).Scan(&lastErr)).To(Succeed())
		Expect(lastErr).To(ContainSubstring("API key was lost"))

		By("the key is entered again: the next attempt asks the provider and settles")
		_, err = db.Exec(ctx, `UPDATE providers SET credential_lost_at = NULL WHERE org_id = $1`, e.org)
		Expect(err).NotTo(HaveOccurred())
		src.answer("gen-lost", providerusage.Usage{InputTokens: 1000, OutputTokens: 250, Cost: money.MustParse("0.0007")})
		clock = clock.Add(time.Hour)
		_, err = settler.SettleDue(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(src.callsFor("gen-lost")).To(Equal(1))
		Expect(e.provisional(ctx)).To(BeZero())
		Expect(revisions(ctx)).To(ConsistOf(HaveField("Reason", "provider")))
	})

	It("keeps the logged usage as final when the provider's key is still lost at the deadline", func(ctx SpecContext) {
		_, err := in.Ingest(ctx, []ledger.Entry{cut("gen-lost2")})
		Expect(err).NotTo(HaveOccurred())
		_, err = db.Exec(ctx, `UPDATE providers SET api_key = NULL, credential_lost_at = now() WHERE org_id = $1`, e.org)
		Expect(err).NotTo(HaveOccurred())
		clock = clock.Add(ledger.SettleWindow + time.Minute)
		_, err = settler.SettleDue(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(src.callsFor("gen-lost2")).To(BeZero())
		charges, held, _ := spent(ctx)
		Expect([]any{charges, held}).To(Equal([]any{"0.0002", "0.00"}))
		Expect(revisions(ctx)).To(ConsistOf(HaveField("Reason", "expired")))
	})

	It("holds the reserve back from the gateway key only; budgets, alerts and stops count the charges", func(ctx SpecContext) {
		// Limit $1.00: each cut response is charged $0.0002 and holds back $0.40 more ($0.4002).
		for i := range 5 {
			_, err := in.Ingest(ctx, []ledger.Entry{cut("gen-c" + string(rune('0'+i)))})
			Expect(err).NotTo(HaveOccurred())
		}
		l, err := ledger.AgentLimit(ctx, q, e.agent, clock)
		Expect(err).NotTo(HaveOccurred())
		Expect(l.Amount).To(BeZero(), "the key gets nothing while five bounds are pending")
		Expect(e.run(ctx, run.ID).State).To(Equal(store.RunStateRunning), "$0.001 charged: nothing is stopped")
		v, _, err := budget.New(db).GetAgent(ctx, e.org, e.agent.ID, money.MustParse("1.00"), clock)
		Expect(err).NotTo(HaveOccurred())
		Expect(v.SpentToday).To(Equal(money.MustParse("0.001")), "pages show the charges")
		n, err := ledger.StopSpent(ctx, db, e.agent, clock)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(BeZero(), "a key blocked by reserves alone stops no run")
		var notices int
		Expect(db.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE org_id = $1`, e.org).Scan(&notices)).To(Succeed())
		Expect(notices).To(BeZero(), "no 80% or 100% notice for a reserve")

		By("the provider bills little: the settlement releases the reserves and the key's limit is back")
		for i := range 5 {
			src.answer("gen-c"+string(rune('0'+i)), providerusage.Usage{InputTokens: 1000, OutputTokens: 100, Cost: money.MustParse("0.0004")})
		}
		clock = clock.Add(time.Minute)
		_, err = settler.SettleDue(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(e.provisional(ctx)).To(BeZero())
		l2, err := ledger.AgentLimit(ctx, q, e.agent, clock)
		Expect(err).NotTo(HaveOccurred())
		Expect(l2.Amount).To(Equal(money.MustParse("0.998")))
		Expect(l2.Generation).NotTo(Equal(l.Generation), "a fresh gateway counter for the new amount")
	})

	Describe("a settlement that raises the cost to a run limit (ADR 0081, decision 5)", func() {
		canceled := func(ctx SpecContext) map[string]any {
			GinkgoHelper()
			var raw []byte
			Expect(db.QueryRow(ctx, `SELECT payload FROM task_events WHERE org_id = $1 AND run_id = $2 AND kind = 'run.canceled'`,
				e.org, run.ID).Scan(&raw)).To(Succeed())
			var p map[string]any
			Expect(json.Unmarshal(raw, &p)).To(Succeed())
			return p
		}
		limit := func(ctx SpecContext, spendUSD string) {
			GinkgoHelper()
			_, err := db.Exec(ctx, `UPDATE runs SET max_spend_usd = $3::numeric WHERE org_id = $1 AND id = $2`, e.org, run.ID, spendUSD)
			Expect(err).NotTo(HaveOccurred())
		}

		It("cancels the run at its spend limit and names it, with what the agent last said", func(ctx SpecContext) {
			limit(ctx, "0.50")
			_, err := db.Exec(ctx, `UPDATE tasks SET agent_summary = 'the draft is in notes', agent_summary_at = now() WHERE org_id = $1 AND id = $2`, e.org, run.TaskID)
			Expect(err).NotTo(HaveOccurred())
			_, err = in.Ingest(ctx, []ledger.Entry{cut("gen-r1")})
			Expect(err).NotTo(HaveOccurred())
			Expect(e.run(ctx, run.ID).State).To(Equal(store.RunStateRunning), "$0.0002 charged at ingest")

			// 1000 in at 0.20 and 300k out at 2, per million: $0.6002 against the $0.50 limit.
			src.answer("gen-r1", providerusage.Usage{InputTokens: 1000, OutputTokens: 300_000, Cost: money.MustParse("0.6002")})
			clock = clock.Add(time.Minute)
			_, err = settler.SettleDue(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(e.run(ctx, run.ID).State).To(Equal(store.RunStateCanceled))
			p := canceled(ctx)
			Expect(p).To(HaveKeyWithValue("reason", "run_spend_cap"))
			Expect(p).To(HaveKeyWithValue("limit", "run_spend"))
			Expect(p).To(HaveKeyWithValue("summary", "the draft is in notes"))
		})

		It("names the agent's daily limit when the settlement reaches it too", func(ctx SpecContext) {
			limit(ctx, "1.00")
			_, err := in.Ingest(ctx, []ledger.Entry{cut("gen-r2")})
			Expect(err).NotTo(HaveOccurred())
			// 600k out at 2 per million: $1.2002, over the agent's $1.00 a day and the run's $1.00.
			src.answer("gen-r2", providerusage.Usage{InputTokens: 1000, OutputTokens: 600_000, Cost: money.MustParse("1.2002")})
			clock = clock.Add(time.Minute)
			_, err = settler.SettleDue(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(e.run(ctx, run.ID).State).To(Equal(store.RunStateCanceled))
			p := canceled(ctx)
			Expect(p).To(HaveKeyWithValue("reason", "spend_limit"), "the broadest limit reached names the stop")
			Expect(p).To(HaveKeyWithValue("limit", budget.AgentDaily))
		})

		It("leaves the run running when the settlement stays below its limits", func(ctx SpecContext) {
			limit(ctx, "0.50")
			_, err := in.Ingest(ctx, []ledger.Entry{cut("gen-r3")})
			Expect(err).NotTo(HaveOccurred())
			src.answer("gen-r3", providerusage.Usage{InputTokens: 1000, OutputTokens: 100, Cost: money.MustParse("0.0004")})
			clock = clock.Add(time.Minute)
			_, err = settler.SettleDue(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(e.run(ctx, run.ID).State).To(Equal(store.RunStateRunning))
		})
	})

	It("pushes a limit with at most nine fraction digits, rounded down (old issue 245)", func(ctx SpecContext) {
		// A provider's own cost can carry ten digits; the remainder of a $1.00 limit then has ten
		// too, and agentgateway rejects its whole config for a budget with more than nine.
		_, err := db.Exec(ctx, `INSERT INTO spend_entries (org_id, agent_id, request_id, key_id, model, input_tokens, output_tokens, cost_usd, priced, occurred_at)
			VALUES ($1, $2, 'ten-digits', 'k', 'm', 1, 1, 0.0172757592, true, $3)`, e.org, e.agent.ID, clock)
		Expect(err).NotTo(HaveOccurred())
		l, err := ledger.AgentLimit(ctx, q, e.agent, clock)
		Expect(err).NotTo(HaveOccurred())
		Expect(l.Amount).To(Equal(money.MustParse("0.98272424")), "1 - 0.0172757592 = 0.9827242408, rounded down")
		Expect(l.Amount.HasFractionDigits(gateway.BudgetFractionDigits)).To(BeTrue())
		sys, err := ledger.SystemLimit(ctx, q, e.org, clock)
		Expect(err).NotTo(HaveOccurred())
		Expect(sys.Amount.HasFractionDigits(gateway.BudgetFractionDigits)).To(BeTrue())
	})

	It("adds the provider's extra tokens to the run once, even when the settlement is replayed", func(ctx SpecContext) {
		_, err := in.Ingest(ctx, []ledger.Entry{cut("gen-d")})
		Expect(err).NotTo(HaveOccurred())
		src.answer("gen-d", providerusage.Usage{InputTokens: 1000, OutputTokens: 500, Cost: money.MustParse("0.0005"), Cancelled: true})
		clock = clock.Add(time.Minute)
		_, err = settler.SettleDue(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(e.run(ctx, run.ID).TokensUsed).To(Equal(int64(1500)))
		Expect(string(e.lastEntry(ctx).Estimate)).To(ContainSubstring(`"cancelled": true`))

		By("a stale settlement row for the final entry (a second settler, a replay) changes nothing")
		var id pgtype.UUID
		Expect(db.QueryRow(ctx, `SELECT id FROM spend_entries WHERE org_id = $1`, e.org).Scan(&id)).To(Succeed())
		_, err = db.Exec(ctx, `INSERT INTO spend_settlements (org_id, entry_id, provider_id, provider_type, response_id, next_attempt_at, deadline)
			SELECT org_id, id, (SELECT id FROM providers WHERE org_id = $1 LIMIT 1), 'openrouter', 'gen-d', $3::timestamptz, $3::timestamptz + interval '1 hour'
			FROM spend_entries WHERE org_id = $1 AND id = $2`, e.org, id, clock.Add(-time.Second))
		Expect(err).NotTo(HaveOccurred())
		_, err = settler.SettleDue(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(e.run(ctx, run.ID).TokensUsed).To(Equal(int64(1500)), "not 2000")
		Expect(revisions(ctx)).To(HaveLen(1))
		var left int
		Expect(db.QueryRow(ctx, `SELECT count(*) FROM spend_settlements WHERE org_id = $1`, e.org).Scan(&left)).To(Succeed())
		Expect(left).To(BeZero())
	})

	It("caps the lookups of a pass and waits out a provider's Retry-After", func(ctx SpecContext) {
		for i := range 3 {
			_, err := in.Ingest(ctx, []ledger.Entry{cut("gen-e" + string(rune('0'+i)))})
			Expect(err).NotTo(HaveOccurred())
		}
		limited := &throttlingSource{after: 10 * time.Minute}
		capped := &ledger.Settler{DB: db, Prices: ing.Prices, Sources: providerusage.Registry{"openrouter": limited}, Log: ing.Log, Now: func() time.Time { return clock }, MaxPerPass: 2}
		clock = clock.Add(time.Minute)
		_, err := capped.SettleDue(ctx)
		Expect(err).NotTo(HaveOccurred())
		own := 0
		for _, id := range limited.ids {
			if strings.HasPrefix(id, "gen-e") {
				own++
			}
		}
		Expect(own).To(Equal(1), "after a 429 the provider is not asked again in the pass")
		Expect(limited.calls).To(BeNumerically("<=", 2), "and a pass asks at most MaxPerPass times")
		var due int
		Expect(db.QueryRow(ctx, `SELECT count(*) FROM spend_settlements WHERE org_id = $1 AND next_attempt_at < $2`,
			e.org, clock.Add(10*time.Minute)).Scan(&due)).To(Succeed())
		Expect(due).To(BeNumerically("<=", 1), "the rows it saw wait out the Retry-After")

		By("the cap: at most MaxPerPass lookups in a pass")
		clock = clock.Add(11 * time.Minute)
		limited.after, limited.calls = 0, 0
		_, err = capped.SettleDue(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(limited.calls).To(Equal(2), "three of this org's rows are due; the pass asks twice")
	})

	It("charges the provider's bill, not the org's price, and keeps Koinon's price beside it", func(ctx SpecContext) {
		_, err := prices.New(db, nil, priceList).SetOverride(ctx, e.org, audit.User(e.user), prices.OverrideInput{
			Entry: pricing.Entry{
				ID: "ds-contract", Provider: "openrouter", Match: pricing.Match{Glob: "acme/*"},
				Spec:      pricing.Spec{Prices: pricing.Wire{Input: "1", Output: "2"}},
				Tokenizer: pricing.TokenizerBytes, Ratio: "3.0", MaxOutput: 4096,
			},
			ValidFrom: clock.Add(-time.Hour),
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = in.Ingest(ctx, []ledger.Entry{cut("gen-o")})
		Expect(err).NotTo(HaveOccurred())
		src.answer("gen-o", providerusage.Usage{InputTokens: 1000, OutputTokens: 250, Cost: money.MustParse("0.0007")})
		clock = clock.Add(time.Minute)
		_, err = settler.SettleDue(ctx)
		Expect(err).NotTo(HaveOccurred())
		s := e.lastEntry(ctx)
		// The bill counts (ADR 0056, amendment 2026-11-06); the override (1000 in at 1 + 250 out at
		// 2, per million) is Koinon's price, kept beside it and flagged where they differ.
		Expect(ledger.FormatUSD(s.CostUsd)).To(Equal("0.0007"), "OpenRouter's bill, not the org's contract price")
		Expect(s.PriceRef).To(HavePrefix("org#"), "the price Koinon compared it with")
		Expect(s.CostFlagged).To(BeTrue())
		Expect(string(s.Estimate)).To(ContainSubstring(`"provider_diff"`))
		Expect(string(s.Estimate)).To(ContainSubstring(`"koinon_cost_usd": "0.0015"`))
		Expect(string(s.Estimate)).To(ContainSubstring(`"charged": "provider"`))
		rs := revisions(ctx)
		Expect(string(rs[0].After)).To(ContainSubstring(`"cost_usd": "0.000700000000"`))
	})

	It("charges a BYOK request OpenRouter's fee plus the upstream cost it reports", func(ctx SpecContext) {
		_, err := in.Ingest(ctx, []ledger.Entry{cut("gen-b1")})
		Expect(err).NotTo(HaveOccurred())
		// With the org's own provider key, OpenRouter bills a small fee; the provider bills the
		// tokens to the org's key, and OpenRouter reports that as upstream_inference_cost.
		src.answer("gen-b1", providerusage.Usage{
			InputTokens: 1000, OutputTokens: 250, Cost: money.MustParse("0.000001"),
			BYOK: true, UpstreamCost: money.MustParse("0.0007"),
		})
		clock = clock.Add(time.Minute)
		_, err = settler.SettleDue(ctx)
		Expect(err).NotTo(HaveOccurred())
		s := e.lastEntry(ctx)
		Expect(ledger.FormatUSD(s.CostUsd)).To(Equal("0.000701"), "the fee and the upstream cost: what the org pays for the request")
		Expect(string(s.Estimate)).To(ContainSubstring(`"byok": true`))
		Expect(string(s.Estimate)).NotTo(ContainSubstring(`"provider_diff"`), "within 1% of Koinon's price")
	})

	// complete is a response the gateway logged in full: 1000 in, 250 out.
	complete := func(id string) ledger.Entry {
		x := cut(id)
		x.OutputKnown, x.OutputTokens, x.ResponseBytes = true, 250, 1000
		return x
	}

	It("settles a complete response at the bill too, with nothing held back meanwhile", func(ctx SpecContext) {
		_, err := in.Ingest(ctx, []ledger.Entry{complete("gen-c1")})
		Expect(err).NotTo(HaveOccurred())
		charges, held, _ := spent(ctx)
		Expect([]any{charges, held}).To(Equal([]any{"0.0007", "0.00"}), "Koinon's price until the bill arrives; no max_tokens reserve")
		Expect(e.provisional(ctx)).To(Equal(1), "waiting for OpenRouter's total_cost")
		Expect(string(e.lastEntry(ctx).Estimate)).NotTo(ContainSubstring("usage_partial"))

		By("OpenRouter bills more than the price list says (a provider's own surcharge)")
		src.answer("gen-c1", providerusage.Usage{InputTokens: 1000, OutputTokens: 250, Cost: money.MustParse("0.0009")})
		clock = clock.Add(time.Minute)
		_, err = settler.SettleDue(ctx)
		Expect(err).NotTo(HaveOccurred())
		charges, held, rows := spent(ctx)
		Expect([]any{charges, held, rows}).To(Equal([]any{"0.0009", "0.00", 1}), "the bill counts")
		s := e.lastEntry(ctx)
		Expect(e.provisional(ctx)).To(BeZero())
		Expect(s.CostFlagged).To(BeTrue(), "the price list is 22% off the bill")
		Expect(string(s.Estimate)).To(ContainSubstring(`"koinon_cost_usd": "0.0007"`))
		Expect(string(s.Estimate)).To(ContainSubstring(`"provider_cost_usd": "0.0009"`))
		Expect(revisions(ctx)).To(HaveLen(1))
	})

	// #78: agentgateway logs no thinking split for Anthropic Messages; OpenRouter's generation may
	// report one (native_tokens_reasoning), and a generation without one must not erase the split
	// the gateway did log.
	It("takes the thinking split from the bill, keeps the logged one when the bill has none, and says when neither had one", func(ctx SpecContext) {
		split := func(id string) (n int64, known *bool) {
			Expect(db.QueryRow(ctx, `SELECT reasoning_tokens, reasoning_known FROM spend_entries WHERE org_id = $1 AND response_id = $2`,
				e.org, id).Scan(&n, &known)).To(Succeed())
			return n, known
		}
		logged := complete("gen-t1") // chat completions: the gateway logged the split
		logged.OutputTokens = 170    // disjoint: 170 plain output plus 80 reasoning, 250 in all
		logged.ReasoningTokens, logged.ReasoningKnown = 80, true
		_, err := in.Ingest(ctx, []ledger.Entry{logged, complete("gen-t2"), complete("gen-t3")})
		Expect(err).NotTo(HaveOccurred())
		n, known := split("gen-t2")
		Expect(n).To(BeZero())
		Expect(known).To(HaveValue(BeFalse()), "a Messages record without koinon.reasoning: not reported, not 0")

		src.answer("gen-t1", providerusage.Usage{InputTokens: 1000, OutputTokens: 250, Cost: money.MustParse("0.0007")})
		src.answer("gen-t2", providerusage.Usage{InputTokens: 1000, OutputTokens: 250, ReasoningTokens: 120, ReasoningKnown: true, Cost: money.MustParse("0.0007")})
		src.answer("gen-t3", providerusage.Usage{InputTokens: 1000, OutputTokens: 250, Cost: money.MustParse("0.0007")})
		clock = clock.Add(time.Minute)
		_, err = settler.SettleDue(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(e.provisional(ctx)).To(BeZero())

		n, known = split("gen-t1")
		Expect([]any{n, *known}).To(Equal([]any{int64(80), true}), "the bill had no figure: the gateway's split stays")
		n, known = split("gen-t2")
		Expect([]any{n, *known}).To(Equal([]any{int64(120), true}), "the bill's split fills the one the gateway could not log")
		n, known = split("gen-t3")
		Expect([]any{n, *known}).To(Equal([]any{int64(0), false}), "neither reported one: still unknown")
	})

	It("does not wait for a bill of a request the provider refused or failed without usage", func(ctx SpecContext) {
		refused := cut("gen-x4")
		refused.InputKnown, refused.InputTokens, refused.Status = false, 0, 400
		failed := cut("gen-x5")
		failed.InputKnown, failed.InputTokens, failed.Status = false, 0, 502
		_, err := in.Ingest(ctx, []ledger.Entry{refused, failed})
		Expect(err).NotTo(HaveOccurred())
		Expect(e.provisional(ctx)).To(BeZero())
		clock = clock.Add(time.Minute)
		_, err = settler.SettleDue(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(src.callsFor("gen-x4") + src.callsFor("gen-x5")).To(BeZero())
	})

	It("keeps a day of OpenRouter requests within the tolerance of the provider's bill", func(ctx SpecContext) {
		// The tolerance test of issue #47: many requests, each billed a little off the price list
		// (rounding, a surcharge, a discount). Once settled, the ledger is the sum of the bills
		// exactly, and so within CostTolerancePercent (and the cent floor) of OpenRouter's invoice.
		const n = 40
		want := money.USD(0)
		var batch []ledger.Entry
		for i := range n {
			id := fmt.Sprintf("gen-t%02d", i)
			batch = append(batch, complete(id))
			// 0.0007 at the price list; the bill swings from -10% to +9.5% of it.
			bill := money.MustParse("0.0007") * money.USD(200+i) / 210
			src.answer(id, providerusage.Usage{InputTokens: 1000, OutputTokens: 250, Cost: bill})
			var err error
			want, err = want.Add(bill)
			Expect(err).NotTo(HaveOccurred())
		}
		_, err := in.Ingest(ctx, batch)
		Expect(err).NotTo(HaveOccurred())
		clock = clock.Add(time.Minute)
		_, err = settler.SettleDue(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(e.provisional(ctx)).To(BeZero())
		var sum string
		Expect(db.QueryRow(ctx, `SELECT sum(cost_usd)::text FROM spend_entries WHERE org_id = $1`, e.org).Scan(&sum)).To(Succeed())
		got, err := money.ParseNumeric(sum)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(want), "settled entries are the bill to the pico-dollar")
		Expect(ledger.WithinCostTolerance(got, want)).To(BeTrue())
		listed := money.MustParse("0.0007") * n
		Expect(ledger.WithinCostTolerance(listed, want)).To(BeTrue(), "the price list alone is within it here too")
	})

	It("never contacts a provider whose URL is not its source's service", func(ctx SpecContext) {
		// Queued while the (fake) source served the provider; the provider is at example.com.
		_, err := in.Ingest(ctx, []ledger.Entry{cut("gen-s1")})
		Expect(err).NotTo(HaveOccurred())
		var asked []string
		client := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
			asked = append(asked, r.URL.String())
			return nil, errors.New("no network in this spec")
		})}
		real := &ledger.Settler{DB: db, Prices: ing.Prices, Sources: providerusage.Default(client), Log: ing.Log, Now: func() time.Time { return clock }}
		clock = clock.Add(time.Minute)
		_, err = real.SettleDue(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(asked).To(BeEmpty(), "an openrouter-typed provider at example.com gets no request, and no key")
		Expect(e.provisional(ctx)).To(BeZero(), "nothing can settle it: the logged usage is final now")

		By("ingest does not queue it with the real sources")
		realIn := &ledger.Ingester{DB: db, Log: ing.Log, Prices: ing.Prices, Sources: providerusage.Default(client)}
		_, err = realIn.Ingest(ctx, []ledger.Entry{cut("gen-s2")})
		Expect(err).NotTo(HaveOccurred())
		Expect(e.provisional(ctx)).To(BeZero())

		By("but does for a provider at https://openrouter.ai")
		o := newEnv(ctx, uniq("org"), "", "1.00")
		o.modelAt(ctx, "openrouter", flash, "https://openrouter.ai/api/v1")
		x := cut("gen-s3")
		x.Agent = o.ref()
		_, err = realIn.Ingest(ctx, []ledger.Entry{x})
		Expect(err).NotTo(HaveOccurred())
		Expect(o.provisional(ctx)).To(Equal(1))
		Expect(asked).To(BeEmpty(), "ingest never calls a provider")
	})

	It("waits out a provider's Retry-After across passes", func(ctx SpecContext) {
		limited := &throttlingSource{after: 10 * time.Minute}
		now := func() time.Time { return clock }
		reg := providerusage.Registry{"openrouter": limited}
		lin := &ledger.Ingester{DB: db, Log: ing.Log, Prices: ing.Prices, Sources: reg, Now: now}
		lset := &ledger.Settler{DB: db, Prices: ing.Prices, Sources: reg, Log: ing.Log, Now: now}
		_, err := lin.Ingest(ctx, []ledger.Entry{cut("gen-r1")})
		Expect(err).NotTo(HaveOccurred())
		clock = clock.Add(time.Minute)
		_, err = lset.SettleDue(ctx)
		Expect(err).NotTo(HaveOccurred())
		ownCalls := func() int {
			n := 0
			for _, id := range limited.ids {
				if strings.HasPrefix(id, "gen-r") {
					n++
				}
			}
			return n
		}
		Expect(ownCalls()).To(Equal(1))

		By("a new entry of the same provider in a later pass is not asked before the Retry-After")
		_, err = lin.Ingest(ctx, []ledger.Entry{cut("gen-r2")})
		Expect(err).NotTo(HaveOccurred())
		clock = clock.Add(time.Minute)
		_, err = lset.SettleDue(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(ownCalls()).To(Equal(1), "throttled for 10 minutes")
		clock = clock.Add(10 * time.Minute)
		limited.after = 0
		_, err = lset.SettleDue(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(ownCalls()).To(Equal(3), "both asked once the wait is over")
	})

	It("takes orgs in turn, so one busy org cannot use up a pass", func(ctx SpecContext) {
		other := newEnv(ctx, uniq("org"), "", "1.00")
		other.model(ctx, "openrouter", flash)
		// Expire every settlement other specs left behind, so only these two orgs are due.
		later := clock.Add(2 * ledger.SettleWindow)
		_, err := (&ledger.Settler{DB: db, Prices: ing.Prices, Sources: providerusage.Registry{"openrouter": src}, Log: ing.Log, Now: func() time.Time { return later }, MaxPerPass: 100000}).SettleDue(ctx)
		Expect(err).NotTo(HaveOccurred())
		clock = later
		for i := range 3 {
			_, err := in.Ingest(ctx, []ledger.Entry{cut("gen-f" + string(rune('0'+i)))})
			Expect(err).NotTo(HaveOccurred())
		}
		x := cut("gen-g0")
		x.Agent = other.ref()
		_, err = in.Ingest(ctx, []ledger.Entry{x})
		Expect(err).NotTo(HaveOccurred())
		rec := &throttlingSource{}
		clock = clock.Add(time.Minute)
		_, err = (&ledger.Settler{DB: db, Prices: ing.Prices, Sources: providerusage.Registry{"openrouter": rec}, Log: ing.Log, Now: func() time.Time { return clock }, MaxPerPass: 2}).SettleDue(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(rec.ids).To(HaveLen(2))
		Expect(rec.ids).To(ContainElement("gen-g0"), "the other org gets its turn")
	})

	Describe("a streamed Anthropic-format response", func() {
		// partial is what agentgateway logs for a cut Messages stream: message_start's usage.
		partial := func(id string) ledger.Entry {
			x := cut(id)
			x.Format, x.Stream, x.OutputKnown, x.OutputTokens = modelformat.Anthropic, true, true, 1
			return x
		}
		cancelRun := func(ctx SpecContext, at time.Time) {
			_, err := db.Exec(ctx, `UPDATE runs SET state = 'canceled', finished_at = $3, lease_expires_at = NULL WHERE org_id = $1 AND id = $2`, e.org, run.ID, at)
			Expect(err).NotTo(HaveOccurred())
		}

		It("is complete when its run was not canceled, and an absent format is OpenAI", func(ctx SpecContext) {
			// A complete response waits for its bill too, but holds nothing back and is not partial.
			_, err := in.Ingest(ctx, []ledger.Entry{partial("gen-m1")})
			Expect(err).NotTo(HaveOccurred())
			_, held, _ := spent(ctx)
			Expect(held).To(Equal("0.00"))
			Expect(string(e.lastEntry(ctx).Estimate)).NotTo(ContainSubstring("usage_partial"))
			cancelRun(ctx, clock.Add(-time.Second))
			x := partial("gen-m2")
			x.Format = "" // a record from before koinon.format
			_, err = in.Ingest(ctx, []ledger.Entry{x})
			Expect(err).NotTo(HaveOccurred())
			_, held, _ = spent(ctx)
			Expect(held).To(Equal("0.00"), "OpenAI: logged output usage means a complete response")
			Expect(string(e.lastEntry(ctx).Estimate)).NotTo(ContainSubstring("usage_partial"))
		})

		It("is cut when its run was canceled at or before the log time, and settled by generation ID", func(ctx SpecContext) {
			cancelRun(ctx, clock.Add(-time.Second))
			_, err := in.Ingest(ctx, []ledger.Entry{partial("gen-m3")})
			Expect(err).NotTo(HaveOccurred())
			Expect(e.provisional(ctx)).To(Equal(1))
			s := e.lastEntry(ctx)
			Expect(string(s.Estimate)).To(ContainSubstring(`"usage_partial": true`))
			Expect(s.CostFlagged).To(BeTrue())
			src.answer("gen-m3", providerusage.Usage{InputTokens: 1000, OutputTokens: 300, Cost: money.MustParse("0.00032")})
			clock = clock.Add(time.Minute)
			_, err = settler.SettleDue(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(e.lastEntry(ctx).OutputTokens).To(Equal(int64(300)))
			Expect(e.provisional(ctx)).To(BeZero())
		})

		It("is cut when the proxy logged an error for it, whatever its run did", func(ctx SpecContext) {
			x := partial("gen-err")
			x.Error = "Upstream" // agentgateway v1.7.0 for a stream the upstream cut: 200, out=1
			_, err := in.Ingest(ctx, []ledger.Entry{x})
			Expect(err).NotTo(HaveOccurred())
			Expect(e.run(ctx, run.ID).State).To(Equal(store.RunStateRunning), "the run was not canceled")
			Expect(e.provisional(ctx)).To(Equal(1), "settled by generation ID")
			s := e.lastEntry(ctx)
			Expect(string(s.Estimate)).To(ContainSubstring(`"usage_partial": true`))
			Expect(s.OutputTokens).To(Equal(int64(1)), "charged as logged")

			By("an OpenAI-format record with an error and no output is cut too")
			y := cut("gen-err2")
			y.Error = "Upstream"
			_, err = in.Ingest(ctx, []ledger.Entry{y})
			Expect(err).NotTo(HaveOccurred())
			Expect(e.provisional(ctx)).To(Equal(2))
		})

		It("is not cut by an earlier run's cancel: only the attributed run counts", func(ctx SpecContext) {
			t := clock
			cancelRun(ctx, t)                                       // run A canceled at T
			b := e.startRun(ctx, e.task(ctx), t.Add(5*time.Second)) // run B starts at T+5s
			x := partial("gen-ab")
			x.Time = t.Add(30 * time.Second) // B's complete response, logged at T+30s
			_, err := in.Ingest(ctx, []ledger.Entry{x})
			Expect(err).NotTo(HaveOccurred())
			_, held, _ := spent(ctx)
			Expect(held).To(Equal("0.00"), "B was not canceled: its response is complete")
			s := e.lastEntry(ctx)
			Expect(string(s.Estimate)).NotTo(ContainSubstring("usage_partial"))
			var runID pgtype.UUID
			Expect(db.QueryRow(ctx, `SELECT run_id FROM spend_entries WHERE org_id = $1`, e.org).Scan(&runID)).To(Succeed())
			Expect(runID).To(Equal(b.ID))
		})

		It("keeps the cache writes the gateway logged when the provider's figure has none", func(ctx SpecContext) {
			_, err := prices.New(db, nil, priceList).SetOverride(ctx, e.org, audit.User(e.user), prices.OverrideInput{
				Entry: pricing.Entry{
					ID: "cw", Provider: "openrouter", Match: pricing.Match{Glob: flash},
					Spec:      pricing.Spec{Prices: pricing.Wire{Input: "1", Output: "2", CacheWrite: "4"}},
					Tokenizer: pricing.TokenizerBytes, Ratio: "3.0", MaxOutput: 4096,
				},
				ValidFrom: clock.Add(-time.Hour),
			})
			Expect(err).NotTo(HaveOccurred())
			cancelRun(ctx, clock.Add(-time.Second))
			x := partial("gen-cw")
			x.InputTokens = 400 // disjoint buckets: 400 plain input plus 600 cache writes, 1000 in all
			x.CacheWriteTokens = 600
			_, err = in.Ingest(ctx, []ledger.Entry{x})
			Expect(err).NotTo(HaveOccurred())
			Expect(e.provisional(ctx)).To(Equal(1))
			var provInput int64
			Expect(db.QueryRow(ctx, `SELECT input_tokens FROM spend_entries WHERE org_id = $1`, e.org).Scan(&provInput)).To(Succeed())
			Expect(provInput).To(Equal(int64(1000)), "the stored input is the total: 400 plain plus 600 cache writes")
			// OpenRouter's generation: prompt and completion, no cache writes.
			src.answer("gen-cw", providerusage.Usage{InputTokens: 1000, OutputTokens: 100, Cost: money.MustParse("0.003")})
			clock = clock.Add(time.Minute)
			_, err = settler.SettleDue(ctx)
			Expect(err).NotTo(HaveOccurred())
			var cw int64
			var cost string
			Expect(db.QueryRow(ctx, `SELECT cache_write_tokens, cost_usd::text FROM spend_entries WHERE org_id = $1`, e.org).Scan(&cw, &cost)).To(Succeed())
			Expect(cw).To(Equal(int64(600)))
			// 400 input at 1 + 600 cache writes at 4 + 100 output at 2, per million.
			Expect(ledger.FormatUSD(cost)).To(Equal("0.003"))
		})

		It("from a provider that cannot report: charged as logged, the reserve held until the deadline", func(ctx SpecContext) {
			d := newEnv(ctx, uniq("org"), "", "1.00")
			d.model(ctx, "anthropic", flash) // a provider type without a usage source
			_, err := prices.New(db, nil, priceList).SetOverride(ctx, d.org, audit.User(d.user), prices.OverrideInput{
				Entry: pricing.Entry{
					ID: "direct", Provider: "anthropic", Match: pricing.Match{Glob: flash},
					Spec:      pricing.Spec{Prices: pricing.Wire{Input: "0.02", Output: "1"}},
					Tokenizer: pricing.TokenizerBytes, Ratio: "3.0", MaxOutput: 943718,
				},
				ValidFrom: clock.Add(-time.Hour),
			})
			Expect(err).NotTo(HaveOccurred())
			drun := d.startRun(ctx, d.task(ctx), clock.Add(-time.Minute))
			_, err = db.Exec(ctx, `UPDATE runs SET state = 'canceled', finished_at = $3, lease_expires_at = NULL WHERE org_id = $1 AND id = $2`, d.org, drun.ID, clock)
			Expect(err).NotTo(HaveOccurred())
			x := partial("")
			x.Agent = d.ref()
			_, err = in.Ingest(ctx, []ledger.Entry{x})
			Expect(err).NotTo(HaveOccurred())
			Expect(d.provisional(ctx)).To(Equal(1))
			var charge, reserve string
			Expect(db.QueryRow(ctx, `SELECT cost_usd::text, reserve_usd::text FROM spend_entries WHERE org_id = $1`, d.org).Scan(&charge, &reserve)).To(Succeed())
			Expect(ledger.FormatUSD(charge)).To(Equal("0.000021"), "as logged: 1000 in + 1 out")
			Expect(ledger.FormatUSD(reserve)).To(Equal("0.199999"), "the 200k bound held back from the key")

			clock = clock.Add(time.Minute)
			_, err = settler.SettleDue(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(d.provisional(ctx)).To(Equal(1), "a hold is not asked about; it waits for the deadline")
			clock = clock.Add(ledger.SettleWindow)
			_, err = settler.SettleDue(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(d.provisional(ctx)).To(BeZero())
			Expect(db.QueryRow(ctx, `SELECT cost_usd::text, reserve_usd::text FROM spend_entries WHERE org_id = $1`, d.org).Scan(&charge, &reserve)).To(Succeed())
			Expect([]string{ledger.FormatUSD(charge), ledger.FormatUSD(reserve)}).To(Equal([]string{"0.000021", "0.00"}))
		})
	})
})
