// SPDX-License-Identifier: Apache-2.0

package prices_test

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/internal/pricefetch"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/pricing"
)

// defaultList is a default list pricing each of models at input 1, output 2 for the openrouter provider type.
func defaultList(version string, models ...string) pricing.ListSource {
	var b strings.Builder
	fmt.Fprintf(&b, "version: %q\nentries:\n", version)
	for i, m := range models {
		fmt.Fprintf(&b, "  - id: fake-%d-%s\n    provider: openrouter\n    match: {glob: %q}\n    valid_from: 2000-01-01\n"+
			"    prices: {input: \"1\", output: \"2\"}\n    tokenizer: bytes\n    ratio: \"3.0\"\n    max_output: 100000\n"+
			"    source: {url: \"https://example.com/test-prices\", accessed: 2000-01-01}\n", i, strings.NewReplacer("/", "-", "*", "star").Replace(m), m)
	}
	l, rs, err := pricing.ParseList([]byte(b.String()))
	Expect(err).NotTo(HaveOccurred())
	return pricing.Static(l, rs)
}

func platformEntry(id, model, in, out string, cacheRead string) pricing.Entry {
	return pricing.Entry{
		ID: id, Provider: "openrouter", Match: pricing.Match{Glob: model}, ValidFrom: "2000-01-01",
		Spec:      pricing.Spec{Prices: pricing.Wire{Input: in, Output: out, CacheRead: cacheRead}},
		Tokenizer: "bytes", Ratio: "3.0", MaxOutput: 100000, Source: pricing.Source{URL: "https://example.com/test-prices", Accessed: "2000-01-01"},
	}
}

func resolve(ctx context.Context, svc *prices.Service, src pricing.ListSource, model string) prices.Resolution {
	rv := prices.NewResolver(src)
	r, err := rv.Resolve(ctx, store.New(db), pgtype.UUID{}, prices.Model{Name: model, ProviderType: "openrouter"}, time.Now())
	Expect(err).NotTo(HaveOccurred())
	return r
}

var _ = Describe("the platform price list", func() {
	BeforeEach(func() { needDocker() })

	It("prices a model between the org entries and the default list, by a platform@version#id ref", func(ctx SpecContext) {
		m := uniq()
		src := defaultList("2000-01-01.1", m, "fake/*")
		svc := prices.New(db, slog.Default(), src)

		before := resolve(ctx, svc, src, m)
		Expect(before.Source).To(Equal(prices.SourceDefault))

		v, err := svc.SetPlatformForTest(ctx, []pricing.Entry{platformEntry("platform-"+strings.ReplaceAll(m, "/", "-"), m, "5", "10", "")})
		Expect(err).NotTo(HaveOccurred())

		got := resolve(ctx, svc, src, m)
		Expect(got.Source).To(Equal(prices.SourcePlatform))
		Expect(got.PlatformVersion).To(Equal(v.Version))
		Expect(got.Ref).To(Equal(fmt.Sprintf("platform@%d#platform-%s", v.Version, strings.ReplaceAll(m, "/", "-"))))
		Expect(got.Entry.Spec.Prices.Input).To(Equal("5"))

		// A model the platform list does not name keeps the default price.
		other := uniq()
		Expect(resolve(ctx, svc, defaultList("2000-01-01.1", other), other).Source).To(Equal(prices.SourceDefault))
	})

	It("refuses a version that starts in the past", func(ctx SpecContext) {
		m := uniq()
		svc := prices.New(db, slog.Default(), defaultList("2000-01-01.1", m))
		_, err := svc.SetPlatformForTest(ctx, []pricing.Entry{platformEntry("p-"+strings.ReplaceAll(m, "/", "-"), m, "5", "10", "")}, time.Now().Add(-time.Hour))
		var inv *prices.InvalidError
		Expect(err).To(HaveOccurred())
		Expect(err).To(BeAssignableToTypeOf(inv))
		Expect(err.Error()).To(ContainSubstring("past"))
	})

	It("turns prompt caching on for a platform entry with a cache_read rate (ADR 0068)", func(ctx SpecContext) {
		m := strings.Replace(uniq(), "fake/", "deepseek/", 1) // a vendor with a known cache mode
		src := defaultList("2000-01-01.1", m)
		svc := prices.New(db, slog.Default(), src)
		before := resolve(ctx, svc, src, m)
		Expect(before.Price.Base.CacheRead).To(BeNil(), "the default entry has no cache price")
		Expect(prices.CacheModeFor("openrouter", m, modelformat.Format(""), before.Price)).To(Equal(prices.CacheNone))

		_, err := svc.SetPlatformForTest(ctx, []pricing.Entry{platformEntry("p-"+strings.ReplaceAll(m, "/", "-"), m, "5", "10", "0.5")})
		Expect(err).NotTo(HaveOccurred())
		r := resolve(ctx, svc, src, m)
		Expect(r.Source).To(Equal(prices.SourcePlatform))
		mode := prices.CacheModeFor("openrouter", m, modelformat.Format(""), r.Price)
		Expect(mode.Uncached()).To(BeFalse(), "a cache_read price turns caching on")
	})

	It("rolls back by writing a new version equal to an earlier one", func(ctx SpecContext) {
		m := uniq()
		src := defaultList("2000-01-01.1", m)
		svc := prices.New(db, slog.Default(), src)
		id := "p-" + strings.ReplaceAll(m, "/", "-")
		v1, err := svc.SetPlatformForTest(ctx, []pricing.Entry{platformEntry(id, m, "5", "10", "")})
		Expect(err).NotTo(HaveOccurred())
		v2, err := svc.SetPlatformForTest(ctx, []pricing.Entry{platformEntry(id, m, "6", "12", "")})
		Expect(err).NotTo(HaveOccurred())
		Expect(resolve(ctx, svc, src, m).Entry.Spec.Prices.Input).To(Equal("6"))

		n := auditCount(ctx, "platform_price.rollback")
		v3, err := svc.Rollback(ctx, audit.CLI("test"), v1.Version)
		Expect(err).NotTo(HaveOccurred())
		Expect(v3.Version).To(BeNumerically(">", v2.Version))
		Expect(resolve(ctx, svc, src, m).Entry.Spec.Prices.Input).To(Equal("5"))
		Expect(auditCount(ctx, "platform_price.rollback")).To(Equal(n + 1))
		_, err = svc.Rollback(ctx, audit.CLI("test"), 1<<40)
		Expect(err).To(HaveOccurred())
	})

	It("retires an entry the new default list caught up with, audited with the list version", func(ctx SpecContext) {
		m := uniq()
		id := "p-" + strings.ReplaceAll(m, "/", "-")
		old := prices.New(db, slog.Default(), defaultList("2000-01-01.1", m))
		// The platform entry equals the default price (input 1, output 2) in everything it bills.
		_, err := old.SetPlatformForTest(ctx, []pricing.Entry{platformEntry(id, m, "1", "2", "")})
		Expect(err).NotTo(HaveOccurred())
		// ...but the entry id differs from the default one, so it is kept: ids must match.
		Expect(old.RetireCaughtUp(ctx)).To(BeEmpty())

		caught := prices.New(db, slog.Default(), defaultList("2000-01-01.2", m))
		defID := "fake-0-" + strings.ReplaceAll(m, "/", "-")
		_, err = caught.SetPlatformForTest(ctx, []pricing.Entry{platformEntry(defID, m, "1", "2", "")})
		Expect(err).NotTo(HaveOccurred())
		n := auditCount(ctx, "platform_price.retired")
		retired, err := caught.RetireCaughtUp(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(retired).To(ContainElement(defID))
		Expect(auditCount(ctx, "platform_price.retired")).To(Equal(n + 1))
		Expect(resolve(ctx, caught, defaultList("2000-01-01.2", m), m).Source).To(Equal(prices.SourceDefault))
	})
})

// fakeAdapter serves one model's offer as "openrouter".
type fakeAdapter struct {
	model string
	in    string
	out   string
	calls int
	// err, when set, fails the fetch.
	err error
}

func (f *fakeAdapter) Name() string { return pricefetch.SourceOpenRouter }
func (f *fakeAdapter) Fetch(_ context.Context, now time.Time) (pricefetch.Snapshot, error) {
	f.calls++
	if f.err != nil {
		return pricefetch.Snapshot{}, f.err
	}
	return pricefetch.Snapshot{
		Source: pricefetch.SourceOpenRouter, URL: "https://example.com/fake", Licence: "fake", FetchedAt: now, SHA256: strings.Repeat("0", 64), Count: 1,
		Offers: []pricefetch.Offer{{Model: f.model, ProviderType: "openrouter", Spec: pricing.Spec{Prices: pricing.Wire{Input: f.in, Output: f.out}}}},
	}, nil
}

var _ = Describe("the price refresh", func() {
	BeforeEach(func() { needDocker() })

	newRefresher := func(src pricing.ListSource, a *fakeAdapter, now *time.Time) *prices.Refresher {
		r, err := prices.NewRefresher(prices.New(db, slog.Default(), src), prices.RefreshConfig{
			Adapters: []pricefetch.Adapter{a}, Metrics: prices.NewMetrics(nil), Now: func() time.Time { return *now },
		})
		Expect(err).NotTo(HaveOccurred())
		return r
	}
	proposalFor := func(ctx context.Context, r *prices.Refresher, model, state string) prices.Proposal {
		ps, err := r.Service().ListProposals(ctx, state, "", 1000)
		Expect(err).NotTo(HaveOccurred())
		for _, p := range ps {
			if p.Model == model {
				return p
			}
		}
		Fail("no " + state + " proposal for " + model)
		return prices.Proposal{}
	}

	It("holds a first sighting for review, applies it once stable, and only by itself within bounds", func(ctx SpecContext) {
		m := uniq()
		src := defaultList("2000-01-01.1", m)
		a := &fakeAdapter{model: m, in: "1.1", out: "2.2"} // +10% on both
		now := time.Now()
		r := newRefresher(src, a, &now)

		res, err := r.Run(ctx, prices.TriggerCLI, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Proposals).To(Equal(1))
		Expect(res.Applied).To(BeZero(), "a value seen once is not stable")
		p := proposalFor(ctx, r, m, "open")
		Expect(p.Class).To(Equal(pricefetch.ClassNeedsReview))
		Expect(resolve(ctx, r.Service(), src, m).Source).To(Equal(prices.SourceDefault))

		now = now.Add(25 * time.Hour)
		res, err = r.Run(ctx, prices.TriggerSchedule, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Applied).To(Equal(1))
		got := resolve(ctx, r.Service(), src, m)
		Expect(got.Source).To(Equal(prices.SourcePlatform))
		Expect(got.Entry.Spec.Prices.Input).To(Equal("1.1"))
		Expect(proposalFor(ctx, r, m, "accepted").Class).To(Equal(pricefetch.ClassAuto))
	})

	It("never applies an import by itself, and a person accepts it", func(ctx SpecContext) {
		m := uniq()
		src := defaultList("2000-01-01.1", m)
		a := &fakeAdapter{model: m, in: "1.1", out: "2.2"}
		now := time.Now()
		r := newRefresher(src, a, &now)
		snap, err := a.Fetch(ctx, now)
		Expect(err).NotTo(HaveOccurred())

		n := auditCount(ctx, "price.import.uploaded")
		made, err := r.Import(ctx, audit.CLI("test"), pricefetch.File{Snapshots: []pricefetch.Snapshot{snap}})
		Expect(err).NotTo(HaveOccurred())
		Expect(made).To(Equal(1))
		Expect(auditCount(ctx, "price.import.uploaded")).To(Equal(n + 1))
		p := proposalFor(ctx, r, m, "open")
		Expect(p.Origin).To(Equal(prices.OriginImport))
		Expect(p.Class).NotTo(Equal(pricefetch.ClassAuto))
		Expect(r.AutoApply(ctx)).To(BeZero())

		acc := auditCount(ctx, "price.proposal.accepted")
		res, err := r.Accept(ctx, audit.CLI("test"), p.ID, prices.AcceptOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Version.Version).To(BeNumerically(">", 0))
		Expect(auditCount(ctx, "price.proposal.accepted")).To(Equal(acc + 1))
		Expect(resolve(ctx, r.Service(), src, m).Source).To(Equal(prices.SourcePlatform))
		_, err = r.Accept(ctx, audit.CLI("test"), p.ID, prices.AcceptOptions{})
		Expect(err).To(HaveOccurred(), "a decided proposal cannot be accepted again")
	})

	It("needs the model id typed to accept a change of more than 3x, and remembers a dismissal", func(ctx SpecContext) {
		m := uniq()
		src := defaultList("2000-01-01.1", m)
		a := &fakeAdapter{model: m, in: "4", out: "8"} // 4x
		now := time.Now()
		r := newRefresher(src, a, &now)
		_, err := r.Run(ctx, prices.TriggerCLI, false)
		Expect(err).NotTo(HaveOccurred())
		p := proposalFor(ctx, r, m, "open")
		Expect(p.Class).To(Equal(pricefetch.ClassConfirm))

		_, err = r.Accept(ctx, audit.CLI("test"), p.ID, prices.AcceptOptions{})
		Expect(err).To(HaveOccurred())
		_, err = r.Accept(ctx, audit.CLI("test"), p.ID, prices.AcceptOptions{Confirm: "fake/other"})
		Expect(err).To(HaveOccurred())

		dis := auditCount(ctx, "price.proposal.dismissed")
		Expect(r.Dismiss(ctx, audit.CLI("test"), p.ID)).To(Succeed())
		Expect(auditCount(ctx, "price.proposal.dismissed")).To(Equal(dis + 1))
		res, err := r.Run(ctx, prices.TriggerCLI, false)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Proposals).To(BeZero(), "the same values do not come back after a dismissal")
	})

	It("refuses a proposal that is stale against the price in use", func(ctx SpecContext) {
		m := uniq()
		src := defaultList("2000-01-01.1", m)
		a := &fakeAdapter{model: m, in: "1.1", out: "2.2"}
		now := time.Now()
		r := newRefresher(src, a, &now)
		_, err := r.Run(ctx, prices.TriggerCLI, false)
		Expect(err).NotTo(HaveOccurred())
		p := proposalFor(ctx, r, m, "open")
		_, err = r.Service().SetPlatformForTest(ctx, []pricing.Entry{platformEntry("p-"+strings.ReplaceAll(m, "/", "-"), m, "3", "6", "")})
		Expect(err).NotTo(HaveOccurred())
		now = time.Now() // the new version is valid from the instant it was written
		_, err = r.Accept(ctx, audit.CLI("test"), p.ID, prices.AcceptOptions{})
		Expect(err).To(MatchError(ContainSubstring("changed since")))
	})

	It("makes no request when nobody asks: settings sync alone fetches nothing and audits only a change", func(ctx SpecContext) {
		a := &fakeAdapter{model: uniq(), in: "1", out: "2"}
		now := time.Now()
		r := newRefresher(defaultList("2000-01-01.1"), a, &now)
		Expect(r.SyncSettings(ctx, prices.RefreshOff, 24*time.Hour)).To(Succeed())
		Expect(a.calls).To(BeZero())

		n := auditCount(ctx, "price.refresh.settings_changed")
		Expect(r.SyncSettings(ctx, prices.RefreshOn, 12*time.Hour)).To(Succeed())
		Expect(auditCount(ctx, "price.refresh.settings_changed")).To(Equal(n + 1))
		Expect(r.SyncSettings(ctx, prices.RefreshOn, 12*time.Hour)).To(Succeed())
		Expect(auditCount(ctx, "price.refresh.settings_changed")).To(Equal(n+1), "no change, no event")
		Expect(r.SyncSettings(ctx, prices.RefreshOff, 12*time.Hour)).To(Succeed())
		Expect(auditCount(ctx, "price.refresh.settings_changed")).To(Equal(n + 2))
		Expect(a.calls).To(BeZero())
	})

	It("refuses a bound looser than the default", func() {
		_, err := prices.NewRefresher(prices.New(db, slog.Default(), defaultList("2000-01-01.1")), prices.RefreshConfig{
			Bounds: pricefetch.Bounds{MaxChangePercent: 90, MaxAutoPerDay: 10},
		})
		Expect(err).To(HaveOccurred())
	})
})
