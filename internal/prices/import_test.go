// SPDX-License-Identifier: Apache-2.0

package prices_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/pricefetch"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/pricing"
)

// proposalsOf lists the proposals of a model in a state, by origin.
func proposalsOf(ctx context.Context, r *prices.Refresher, model, state string) map[string]prices.Proposal {
	ps, err := r.Service().ListProposals(ctx, state, "", 1000)
	Expect(err).NotTo(HaveOccurred())
	out := map[string]prices.Proposal{}
	for _, p := range ps {
		if p.Model == model {
			Expect(out).NotTo(HaveKey(p.Origin), "one %s proposal per origin", state)
			out[p.Origin] = p
		}
	}
	return out
}

func importSnap(ctx context.Context, r *prices.Refresher, snap pricefetch.Snapshot) (int, error) {
	return r.Import(ctx, audit.CLI("test"), pricefetch.File{Format: pricefetch.FileFormat, Snapshots: []pricefetch.Snapshot{snap}})
}

var _ = Describe("an import beside a fetch with the same values", func() {
	BeforeEach(func() { needDocker() })

	It("keeps a fetch proposal's provenance when an import of the same values comes later, and still auto-applies it", func(ctx SpecContext) {
		m := uniq()
		src := defaultList("2000-01-01.1", m)
		a := &fakeAdapter{model: m, in: "1.1", out: "2.2"}
		now := time.Now()
		r := refresherOn(db, src, a, &now)

		_, err := r.Run(ctx, prices.TriggerCLI, false)
		Expect(err).NotTo(HaveOccurred())
		snap, err := a.Fetch(ctx, now)
		Expect(err).NotTo(HaveOccurred())
		_, err = importSnap(ctx, r, snap)
		Expect(err).NotTo(HaveOccurred())

		open := proposalsOf(ctx, r, m, prices.StateOpen)
		Expect(open).To(HaveKey(prices.OriginFetch))
		Expect(open).To(HaveKey(prices.OriginImport))
		Expect(open[prices.OriginFetch].Provenance.VouchedBy).To(BeEmpty(), "the import did not rewrite the fetch's provenance")
		Expect(open[prices.OriginImport].Provenance.VouchedBy).To(Equal("cli test"))

		now = now.Add(25 * time.Hour)
		res, err := r.Run(ctx, prices.TriggerSchedule, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Applied).To(Equal(1))
		acc := proposalsOf(ctx, r, m, prices.StateAccepted)
		Expect(acc).To(HaveKey(prices.OriginFetch))
		Expect(acc).NotTo(HaveKey(prices.OriginImport), "an import is never applied by itself")
		got := resolve(ctx, r.Service(), src, m)
		Expect(got.Source).To(Equal(prices.SourcePlatform))
		pv, ok, err := r.Service().PlatformVersionAt(ctx, time.Now())
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		for _, e := range pv.Entries {
			if e.Entry.ID == got.Entry.ID {
				Expect(e.Provenance.VouchedBy).To(BeEmpty())
				Expect(e.Provenance.Approver).To(Equal("system koinon-api:price-refresh"))
			}
		}
	})

	It("lets a fetch auto-apply although an import of the same values came first", func(ctx SpecContext) {
		m := uniq()
		src := defaultList("2000-01-01.1", m)
		a := &fakeAdapter{model: m, in: "1.1", out: "2.2"}
		now := time.Now()
		r := refresherOn(db, src, a, &now)
		snap, err := a.Fetch(ctx, now)
		Expect(err).NotTo(HaveOccurred())
		_, err = importSnap(ctx, r, snap)
		Expect(err).NotTo(HaveOccurred())

		res, err := r.Run(ctx, prices.TriggerCLI, false)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Proposals).To(Equal(1), "the fetch makes its own proposal")
		now = now.Add(25 * time.Hour)
		res, err = r.Run(ctx, prices.TriggerSchedule, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Applied).To(Equal(1))
		Expect(proposalsOf(ctx, r, m, prices.StateAccepted)).To(HaveKey(prices.OriginFetch))
		Expect(proposalsOf(ctx, r, m, prices.StateOpen)).To(HaveKey(prices.OriginImport))
	})
})

var _ = Describe("what an import file says that the install cannot check", func() {
	BeforeEach(func() { needDocker() })

	const model = "fake/model-a"
	snapAt := func(at time.Time, count int) pricefetch.Snapshot {
		return pricefetch.Snapshot{
			Source: pricefetch.SourceOpenRouter, URL: "https://example.com/fake", Licence: "fake", FetchedAt: at, SHA256: strings.Repeat("0", 64), Count: count,
			Offers: []pricefetch.Offer{{Model: model, ProviderType: "openrouter", Spec: pricing.Spec{Prices: pricing.Wire{Input: "1.1", Output: "2.2"}}}},
		}
	}
	setup := func(ctx context.Context) (*pgxpool.Pool, *prices.Refresher, *fakeAdapter, *time.Time) {
		pool := freshDB(ctx)
		grantAdmin(ctx, pool)
		now := time.Now()
		a := &fakeAdapter{model: model, in: "1.1", out: "2.2"}
		return pool, refresherOn(pool, defaultList("2000-01-01.1", model), a, &now), a, &now
	}

	It("does not delay the scheduled fetch, whatever fetch time the file carries", func(ctx SpecContext) {
		pool, r, a, now := setup(ctx)
		_, err := r.Tick(ctx, prices.RefreshOn, 24*time.Hour)
		Expect(err).NotTo(HaveOccurred())
		Expect(a.calls).To(Equal(1))

		*now = now.Add(25 * time.Hour)
		_, err = importSnap(ctx, r, snapAt(*now, 1)) // the newest row of the source, by fetched_at
		Expect(err).NotTo(HaveOccurred())
		_, err = r.Tick(ctx, prices.RefreshOn, 24*time.Hour)
		Expect(err).NotTo(HaveOccurred())
		Expect(a.calls).To(Equal(2), "the import is not the install's last fetch")
		Expect(countRows(ctx, pool, `SELECT count(*) FROM price_source_fetches WHERE trigger = 'import' AND fetched_at = $1`, *now)).To(Equal(1),
			"the file's time is kept as provenance")
	})

	It("does not move the half-of-last-good floor of a live fetch", func(ctx SpecContext) {
		_, r, a, _ := setup(ctx)
		_, err := importSnap(ctx, r, snapAt(time.Now().Add(-time.Hour), 1_000_000))
		Expect(err).NotTo(HaveOccurred())
		_, err = r.Run(ctx, prices.TriggerCLI, false)
		Expect(err).NotTo(HaveOccurred(), "the live fetch's one model is compared with the install's own fetches only")
		Expect(a.calls).To(Equal(1))
	})

	It("refuses a snapshot without a fetch time, from the future, or with a count that does not fit its offers", func(ctx SpecContext) {
		pool, r, _, now := setup(ctx)
		for name, snap := range map[string]pricefetch.Snapshot{
			"no fetchedAt":       snapAt(time.Time{}, 1),
			"future fetchedAt":   snapAt(now.Add(time.Hour), 1),
			"count below offers": snapAt(*now, 0),
			"count over int32":   snapAt(*now, 1<<31),
		} {
			_, err := importSnap(ctx, r, snap)
			var inv *prices.InvalidError
			Expect(err).To(BeAssignableToTypeOf(inv), name)
		}
		Expect(countRows(ctx, pool, `SELECT count(*) FROM price_source_fetches`)).To(BeZero())
	})
})

var _ = Describe("a stored platform entry this binary refuses", func() {
	BeforeEach(func() { needDocker() })

	It("is skipped and counted: its model falls through to the default list, the rest still price, and retirement only warns", func(ctx SpecContext) {
		pool := freshDB(ctx)
		grantAdmin(ctx, pool)
		good, broken := uniq(), uniq()
		src := defaultList("2000-01-01.1", good, broken)
		svc := prices.New(pool, slog.Default(), src)
		// The good entry equals the default price under the default's id: retirement would remove it.
		goodID := "fake-0-" + strings.ReplaceAll(good, "/", "-")
		v, err := svc.SetPlatformForTest(ctx, []pricing.Entry{platformEntry(goodID, good, "1", "2", "")})
		Expect(err).NotTo(HaveOccurred())
		// An entry valid under an older binary's rules and refused by this one (here: a rate that is
		// not a number), and one that no longer decodes at all.
		be, err := json.Marshal(platformEntry("p-broken", broken, "not-a-number", "10", ""))
		Expect(err).NotTo(HaveOccurred())
		_, err = pool.Exec(ctx, `INSERT INTO platform_price_entries (version, entry_id, entry, reviewed, provenance) VALUES ($1, 'p-broken', $2, '{}', '{}'), ($1, 'p-undecodable', '{"prices": 7}', '{}', '{}')`,
			v.Version, be)
		Expect(err).NotTo(HaveOccurred())

		m := prices.NewMetrics(nil)
		rv := prices.NewResolver(src)
		g, err := rv.Resolve(ctx, store.New(pool), pgtype.UUID{}, prices.Model{Name: good, ProviderType: "openrouter"}, time.Now())
		Expect(err).NotTo(HaveOccurred())
		Expect(g.Source).To(Equal(prices.SourcePlatform))
		b, err := rv.Resolve(ctx, store.New(pool), pgtype.UUID{}, prices.Model{Name: broken, ProviderType: "openrouter"}, time.Now())
		Expect(err).NotTo(HaveOccurred(), "one bad entry does not fail the version")
		Expect(b.Source).To(Equal(prices.SourceDefault))
		Expect(testutil.ToFloat64(m.PlatformBadEntries)).To(BeEquivalentTo(2))

		pv, ok, err := svc.PlatformVersionAt(ctx, time.Now())
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(pv.BadEntries).To(Equal(2))
		Expect(pv.Entries).To(HaveLen(1))

		retired, err := prices.New(pool, slog.Default(), src).RetireCaughtUp(ctx)
		Expect(err).NotTo(HaveOccurred(), "retirement warns, it never fails the start")
		Expect(retired).To(BeEmpty())
		Expect(countRows(ctx, pool, `SELECT count(*) FROM platform_price_versions`)).To(Equal(1))

		// A person's rollback to that version writes it without the bad entries, and says so.
		_, err = svc.Rollback(ctx, audit.CLI("test"), v.Version)
		Expect(err).NotTo(HaveOccurred())
		Expect(countRows(ctx, pool, `SELECT count(*) FROM install_audit_events WHERE action = 'platform_price.rollback' AND (summary->'after'->>'droppedBadEntries')::int = 2`)).To(Equal(1))
	})

	It("keeps the auto-apply from writing a version that would leave the bad entries out", func(ctx SpecContext) {
		pool := freshDB(ctx)
		grantAdmin(ctx, pool)
		m := uniq()
		src := defaultList("2000-01-01.1", m)
		v, err := prices.New(pool, slog.Default(), src).SetPlatformForTest(ctx, []pricing.Entry{platformEntry("p-other", uniq(), "5", "10", "")})
		Expect(err).NotTo(HaveOccurred())
		_, err = pool.Exec(ctx, `INSERT INTO platform_price_entries (version, entry_id, entry, reviewed, provenance) VALUES ($1, 'p-undecodable', '{"prices": 7}', '{}', '{}')`, v.Version)
		Expect(err).NotTo(HaveOccurred())

		now := time.Now()
		r := refresherOn(pool, src, &fakeAdapter{model: m, in: "1.1", out: "2.2"}, &now)
		_, err = r.Run(ctx, prices.TriggerCLI, false)
		Expect(err).NotTo(HaveOccurred())
		now = now.Add(25 * time.Hour)
		res, err := r.Run(ctx, prices.TriggerSchedule, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Applied).To(BeZero())
		Expect(openProposal(ctx, r, m).Class).To(Equal(pricefetch.ClassAuto), "it waits for a person")
	})
})
