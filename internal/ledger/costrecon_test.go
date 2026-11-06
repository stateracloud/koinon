// SPDX-License-Identifier: Apache-2.0

package ledger_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/ledger"
	"github.com/stateracloud/koinon/internal/money"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/providerusage"
	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/pricing"
)

// fakeCost is a cost API: a fixed answer per UTC day, an error for a day without one.
type fakeCost struct {
	mu    sync.Mutex
	days  map[string]money.USD
	asked []providerusage.CostProvider
}

func (*fakeCost) Serves(u string) bool     { return strings.HasPrefix(u, "https://api.anthropic.com") }
func (*fakeCost) ValidScope(s string) bool { return s != "" }
func (f *fakeCost) set(day time.Time, v string) {
	f.mu.Lock()
	f.days[day.Format(time.DateOnly)] = money.MustParse(v)
	f.mu.Unlock()
}
func (f *fakeCost) calls() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.asked) }

func (f *fakeCost) DayCost(_ context.Context, p providerusage.CostProvider, day time.Time) (money.USD, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, p)
	v, ok := f.days[day.Format(time.DateOnly)]
	if !ok {
		return 0, errors.New("the cost report is not ready")
	}
	return v, nil
}

var _ = Describe("WithinCostTolerance", func() {
	DescribeTable("the ledger against the provider's bill",
		func(ledgerUSD, providerUSD string, ok bool) {
			Expect(ledger.WithinCostTolerance(money.MustParse(ledgerUSD), money.MustParse(providerUSD))).To(Equal(ok))
		},
		Entry("equal", "12.34", "12.34", true),
		Entry("2% under", "98", "100", true),
		Entry("2% over", "102", "100", true),
		Entry("just over 2%", "102.01", "100", false),
		Entry("just under -2%", "97.99", "100", false),
		Entry("a cent off a small day", "0.03", "0.02", true),
		Entry("more than a cent off a small day", "0.04", "0.02", false),
		Entry("nothing billed, nothing in the ledger", "0", "0", true),
		Entry("billed but missing from the ledger", "0", "1", false),
	)
})

var _ = Describe("Reconciling direct providers against their cost API", func() {
	BeforeEach(needDocker)

	const model = "claude-test"
	var (
		e     *env
		pid   pgtype.UUID
		cost  *fakeCost
		clock time.Time
		today time.Time
		rec   *ledger.CostReconciler
		keys  *secretstore.Keyring
	)
	BeforeEach(func(ctx SpecContext) {
		// Only this spec's provider holds a cost key: the reconciliation lists every org's.
		_, err := db.Exec(ctx, `UPDATE providers SET cost_api_key = NULL, cost_scope = NULL, cost_reconcile_since = NULL WHERE cost_api_key IS NOT NULL`)
		Expect(err).NotTo(HaveOccurred())
		e = newEnv(ctx, uniq("org"), "", "100.00")
		id := e.modelAt(ctx, "anthropic", model, "https://api.anthropic.com")
		pid = id
		_, err = prices.New(db, nil, priceList).SetOverride(ctx, e.org, audit.User(e.user), prices.OverrideInput{
			Entry: pricing.Entry{
				ID: "claude-contract", Provider: "anthropic", Match: pricing.Match{Glob: model},
				Spec:      pricing.Spec{Prices: pricing.Wire{Input: "1", Output: "2"}},
				Tokenizer: pricing.TokenizerBytes, Ratio: "3.0", MaxOutput: 4096,
			},
			ValidFrom: time.Now().Add(-7 * 24 * time.Hour),
		})
		Expect(err).NotTo(HaveOccurred())

		keys, err = secretstore.NewKeyring(map[string][]byte{"k1": []byte(strings.Repeat("k", 32))}, "k1")
		Expect(err).NotTo(HaveOccurred())
		env, err := keys.Seal(catalog.CostKeyBinding(e.org, id), []byte("koinon_test_not-a-real-token"))
		Expect(err).NotTo(HaveOccurred())
		sealed, err := json.Marshal(env)
		Expect(err).NotTo(HaveOccurred())
		today = time.Now().UTC().Truncate(24 * time.Hour)
		// The key was stored a week ago: every day the reconciliation looks at is after it.
		_, err = db.Exec(ctx, `UPDATE providers SET cost_api_key = $3, cost_scope = 'wrkspc_test', cost_reconcile_since = $4
			WHERE org_id = $1 AND id = $2`, e.org, id, sealed, today.AddDate(0, 0, -7))
		Expect(err).NotTo(HaveOccurred())

		clock = today.Add(12 * time.Hour)
		cost = &fakeCost{days: map[string]money.USD{}}
		rec = &ledger.CostReconciler{
			DB: db, Keys: keys, Sources: providerusage.CostRegistry{"anthropic": cost}, Log: ing.Log,
			Metrics: ledger.NewCostMetrics(nil, []string{"anthropic"}), Now: func() time.Time { return clock },
		}
	})

	// charge ingests one request at t: 1000 in at 1 + 250 out at 2 per million, $0.0015.
	charge := func(ctx SpecContext, t time.Time) {
		x := e.usage(t, model, 1000, 250, 0, "")
		_, err := ing.Ingest(ctx, []ledger.Entry{x})
		Expect(err).NotTo(HaveOccurred())
	}
	type recRow struct {
		Ledger, Diff, Status string
		Provider, Error      *string
		Entries, Attempts    int
	}
	row := func(ctx SpecContext, day time.Time) recRow {
		var r recRow
		Expect(db.QueryRow(ctx, `SELECT ledger_usd::text, coalesce(diff_usd::text, ''), status, provider_usd::text, error, entries, attempts
			FROM provider_cost_reconciliations WHERE org_id = $1 AND provider_id = $2 AND day = $3`, e.org, pid, day.Format(time.DateOnly)).
			Scan(&r.Ledger, &r.Diff, &r.Status, &r.Provider, &r.Error, &r.Entries, &r.Attempts)).To(Succeed())
		return r
	}

	It("records each complete day's difference, per provider key, and never the day still running", func(ctx SpecContext) {
		d1, d2, d3 := today.AddDate(0, 0, -1), today.AddDate(0, 0, -2), today.AddDate(0, 0, -3)
		charge(ctx, d1.Add(time.Hour))
		charge(ctx, d1.Add(2*time.Hour))
		charge(ctx, d2.Add(time.Hour))
		charge(ctx, today.Add(time.Hour))

		By("the entries carry their provider")
		var withProvider int
		Expect(db.QueryRow(ctx, `SELECT count(*) FROM spend_entries WHERE org_id = $1 AND provider_id = $2`, e.org, pid).
			Scan(&withProvider)).To(Succeed())
		Expect(withProvider).To(Equal(4))

		cost.set(d1, "0.0030")  // the ledger's $0.003 exactly
		cost.set(d2, "0.0500")  // the ledger has $0.0015: far off
		cost.set(today, "9.99") // never asked: the day is not over
		n, err := rec.ReconcileDue(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(ledger.CostReconciliationDays))
		Expect(cost.asked).To(HaveEach(providerusage.CostProvider{AdminKey: "koinon_test_not-a-real-token", Scope: "wrkspc_test"}))

		r1 := row(ctx, d1)
		Expect([]any{r1.Status, ledger.FormatUSD(r1.Ledger), r1.Entries, ledger.FormatUSD(*r1.Provider), ledger.FormatUSD(r1.Diff)}).
			To(Equal([]any{ledger.CostOK, "0.003", 2, "0.003", "0.00"}))
		r2 := row(ctx, d2)
		Expect([]any{r2.Status, ledger.FormatUSD(r2.Ledger), r2.Entries, ledger.FormatUSD(*r2.Provider)}).
			To(Equal([]any{ledger.CostOutOfTolerance, "0.0015", 1, "0.05"}))
		Expect(r2.Diff).To(HavePrefix("-0.0485"))
		r3 := row(ctx, d3)
		Expect(r3.Status).To(Equal(ledger.CostError))
		Expect(r3.Provider).To(BeNil())
		Expect(*r3.Error).To(ContainSubstring("not ready"))
		var todays int
		Expect(db.QueryRow(ctx, `SELECT count(*) FROM provider_cost_reconciliations WHERE org_id = $1 AND day = $2`,
			e.org, today.Format(time.DateOnly)).Scan(&todays)).To(Succeed())
		Expect(todays).To(BeZero())
		Expect(testutil.ToFloat64(rec.Metrics.Reconciliations.WithLabelValues("anthropic", ledger.CostOK))).To(Equal(1.0))
		Expect(testutil.ToFloat64(rec.Metrics.Reconciliations.WithLabelValues("anthropic", ledger.CostOutOfTolerance))).To(Equal(1.0))
		Expect(testutil.ToFloat64(rec.Metrics.Reconciliations.WithLabelValues("anthropic", ledger.CostError))).To(Equal(1.0))

		By("the ledger is never changed by it")
		var total string
		Expect(db.QueryRow(ctx, `SELECT sum(cost_usd)::text FROM spend_entries WHERE org_id = $1`, e.org).Scan(&total)).To(Succeed())
		Expect(ledger.FormatUSD(total)).To(Equal("0.006"))

		By("a recorded day stands for a day; a failed one is retried after an hour")
		asked := cost.calls()
		clock = clock.Add(30 * time.Minute)
		n, err = rec.ReconcileDue(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(BeZero())
		Expect(cost.calls()).To(Equal(asked))
		cost.set(d3, "0")
		clock = clock.Add(31 * time.Minute)
		n, err = rec.ReconcileDue(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(1), "only the failed day")
		r3 = row(ctx, d3)
		Expect([]any{r3.Status, r3.Attempts}).To(Equal([]any{ledger.CostOK, 2}))
		Expect(r3.Error).To(BeNil())

		By("the next day: the revised report replaces the record")
		cost.set(d2, "0.0015")
		clock = clock.Add(23 * time.Hour)
		_, err = rec.ReconcileDue(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(row(ctx, d2).Status).To(Equal(ledger.CostOK))
	})

	It("reads no day that started before the cost key was stored", func(ctx SpecContext) {
		d1, d2 := today.AddDate(0, 0, -1), today.AddDate(0, 0, -2)
		// Stored during d2: d2's earlier entries could predate provider_id, so d2 and d3 are not read.
		_, err := db.Exec(ctx, `UPDATE providers SET cost_reconcile_since = $3 WHERE org_id = $1 AND id = $2`,
			e.org, pid, d2.Add(5*time.Hour))
		Expect(err).NotTo(HaveOccurred())
		charge(ctx, d2.Add(time.Hour))
		cost.set(d1, "0")
		cost.set(d2, "0.0500")
		n, err := rec.ReconcileDue(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(1))
		Expect(cost.calls()).To(Equal(1))
		Expect(row(ctx, d1).Status).To(Equal(ledger.CostOK))
		var older int
		Expect(db.QueryRow(ctx, `SELECT count(*) FROM provider_cost_reconciliations WHERE org_id = $1 AND day < $2`,
			e.org, d1.Format(time.DateOnly)).Scan(&older)).To(Succeed())
		Expect(older).To(BeZero())
	})

	It("keeps a deleted provider's entries, with no provider", func(ctx SpecContext) {
		charge(ctx, today.AddDate(0, 0, -1).Add(time.Hour))
		_, err := db.Exec(ctx, `DELETE FROM models WHERE org_id = $1 AND provider_id = $2`, e.org, pid)
		Expect(err).NotTo(HaveOccurred())
		_, err = db.Exec(ctx, `DELETE FROM providers WHERE org_id = $1 AND id = $2`, e.org, pid)
		Expect(err).NotTo(HaveOccurred())
		var entries, orphans int
		Expect(db.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE provider_id IS NOT NULL) FROM spend_entries WHERE org_id = $1`, e.org).
			Scan(&entries, &orphans)).To(Succeed())
		Expect([]int{entries, orphans}).To(Equal([]int{1, 0}))
	})

	It("asks no provider without a cost key", func(ctx SpecContext) {
		_, err := db.Exec(ctx, `UPDATE providers SET cost_api_key = NULL, cost_scope = NULL, cost_reconcile_since = NULL WHERE org_id = $1`, e.org)
		Expect(err).NotTo(HaveOccurred())
		n, err := rec.ReconcileDue(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(BeZero())
		Expect(cost.calls()).To(BeZero())
	})

	It("records an error when the key cannot be opened", func(ctx SpecContext) {
		other, err := secretstore.NewKeyring(map[string][]byte{"k2": []byte(strings.Repeat("x", 32))}, "k2")
		Expect(err).NotTo(HaveOccurred())
		rec.Keys = other
		_, err = rec.ReconcileDue(ctx)
		Expect(err).NotTo(HaveOccurred(), "a provider's failure is recorded, not the pass's")
		r := row(ctx, today.AddDate(0, 0, -1))
		Expect(r.Status).To(Equal(ledger.CostError))
		Expect(*r.Error).To(ContainSubstring("open the cost API key"))
		Expect(*r.Error).NotTo(ContainSubstring("koinon_test_not-a-real-token"))
		Expect(cost.calls()).To(BeZero())
	})
})
