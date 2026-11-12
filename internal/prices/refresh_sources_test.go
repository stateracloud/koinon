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

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/pricefetch"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/pricing"
)

// openaiList is a default list pricing each model at input 1, output 2 for the openai provider type.
func openaiList(version string, models ...string) pricing.ListSource {
	var b strings.Builder
	fmt.Fprintf(&b, "version: %q\nentries:\n", version)
	for i, m := range models {
		fmt.Fprintf(&b, "  - id: fake-oa-%d-%s\n    provider: openai\n    match: {glob: %q}\n    valid_from: 2000-01-01\n"+
			"    prices: {input: \"1\", output: \"2\"}\n    tokenizer: bytes\n    ratio: \"3.0\"\n    max_output: 100000\n"+
			"    source: {url: \"https://example.com/test-prices\", accessed: 2000-01-01}\n", i, strings.NewReplacer("/", "-").Replace(m), m)
	}
	l, rs, err := pricing.ParseList([]byte(b.String()))
	Expect(err).NotTo(HaveOccurred())
	return pricing.Static(l, rs)
}

// sourceAdapter serves one model's offer as an openai price under a given source id; a non-zero
// commitAge makes it a commit-pinned source whose commit is that old when fetched.
type sourceAdapter struct {
	name      string
	model     string
	in, out   string
	commitAge time.Duration
	problems  []string // the source returns the model, but Koinon cannot read its price
}

func (f *sourceAdapter) Name() string { return f.name }
func (f *sourceAdapter) Fetch(_ context.Context, now time.Time) (pricefetch.Snapshot, error) {
	s := pricefetch.Snapshot{
		Source: f.name, URL: "https://example.com/fake", Licence: "fake", FetchedAt: now, SHA256: strings.Repeat("1", 64), Count: 1,
		Offers: []pricefetch.Offer{{Model: f.model, ProviderType: "openai", Spec: pricing.Spec{Prices: pricing.Wire{Input: f.in, Output: f.out}}, MaxOutput: 1000}},
	}
	if len(f.problems) > 0 {
		s.Offers[0].Problems = f.problems
	}
	if f.commitAge > 0 {
		s.CommitSHA, s.CommittedAt = strings.Repeat("a", 40), now.Add(-f.commitAge)
	}
	return s, nil
}

var _ = Describe("the price refresh with several sources", func() {
	BeforeEach(func() { needDocker() })

	newRefresher := func(src pricing.ListSource, now *time.Time, as ...*sourceAdapter) *prices.Refresher {
		var adapters []pricefetch.Adapter
		for _, a := range as {
			adapters = append(adapters, a)
		}
		r, err := prices.NewRefresher(prices.New(db, slog.Default(), src), prices.RefreshConfig{
			Adapters: adapters, Metrics: prices.NewMetrics(nil), Now: func() time.Time { return *now },
		})
		Expect(err).NotTo(HaveOccurred())
		return r
	}
	proposals := func(ctx context.Context, r *prices.Refresher, model, state string) map[string]prices.Proposal {
		ps, err := r.Service().ListProposals(ctx, state, "", 1000)
		Expect(err).NotTo(HaveOccurred())
		out := map[string]prices.Proposal{}
		for _, p := range ps {
			if p.Model == model {
				out[p.Source] = p
			}
		}
		return out
	}
	platformPrice := func(ctx context.Context, r *prices.Refresher, src pricing.ListSource, model string) prices.Resolution {
		rv := prices.NewResolver(src)
		res, err := rv.Resolve(ctx, store.New(db), pgtype.UUID{}, prices.Model{Name: model, ProviderType: "openai"}, time.Now())
		Expect(err).NotTo(HaveOccurred())
		return res
	}

	It("applies a first-hand aggregator's change at once when its commit is a day old, with the commit in the provenance", func(ctx SpecContext) {
		m := uniq()
		src := openaiList("2000-01-01.1", m)
		now := time.Now()
		ll := &sourceAdapter{name: pricefetch.SourceLiteLLM, model: m, in: "1.1", out: "2.2", commitAge: 24 * time.Hour}
		r := newRefresher(src, &now, ll)
		res, err := r.Run(ctx, prices.TriggerCLI, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Applied).To(Equal(1), "a pinned commit needs no second fetch")
		p := proposals(ctx, r, m, "accepted")[pricefetch.SourceLiteLLM]
		Expect(p.Class).To(Equal(pricefetch.ClassAuto))
		Expect(p.Provenance.CommitSHA).To(HaveLen(40))
		Expect(p.Provenance.CommittedAt).To(BeTemporally("~", now.Add(-24*time.Hour), time.Second))
		Expect(platformPrice(ctx, r, src, m).Source).To(Equal(prices.SourcePlatform))
	})

	It("holds a commit that is a minute short of its cooldown for review", func(ctx SpecContext) {
		m := uniq()
		src := openaiList("2000-01-01.1", m)
		now := time.Now()
		ll := &sourceAdapter{name: pricefetch.SourceLiteLLM, model: m, in: "1.1", out: "2.2", commitAge: 24*time.Hour - time.Minute}
		r := newRefresher(src, &now, ll)
		res, err := r.Run(ctx, prices.TriggerCLI, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Applied).To(BeZero())
		p := proposals(ctx, r, m, "open")[pricefetch.SourceLiteLLM]
		Expect(p.Class).To(Equal(pricefetch.ClassNeedsReview))
		Expect(p.FailedRules).To(ContainSubstring("pinning"))
	})

	It("applies one change once when two aggregators agree", func(ctx SpecContext) {
		m := uniq()
		src := openaiList("2000-01-01.1", m)
		now := time.Now()
		ll := &sourceAdapter{name: pricefetch.SourceLiteLLM, model: m, in: "1.1", out: "2.2", commitAge: 30 * time.Hour}
		gp := &sourceAdapter{name: pricefetch.SourceGenaiPrices, model: m, in: "1.1", out: "2.2", commitAge: 30 * time.Hour}
		r := newRefresher(src, &now, ll, gp)
		res, err := r.Run(ctx, prices.TriggerCLI, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Applied).To(Equal(1), "one model, one change")
		Expect(proposals(ctx, r, m, "accepted")).To(HaveLen(1))
		Expect(proposals(ctx, r, m, "open")).To(BeEmpty(), "the other source's proposal is stale and superseded")
		Expect(platformPrice(ctx, r, src, m).Entry.Spec.Prices.Input).To(Equal("1.1"))
	})

	It("holds both for review when two aggregators disagree", func(ctx SpecContext) {
		m := uniq()
		src := openaiList("2000-01-01.1", m)
		now := time.Now()
		ll := &sourceAdapter{name: pricefetch.SourceLiteLLM, model: m, in: "1.1", out: "2.2", commitAge: 30 * time.Hour}
		gp := &sourceAdapter{name: pricefetch.SourceGenaiPrices, model: m, in: "1.05", out: "2.1", commitAge: 30 * time.Hour}
		r := newRefresher(src, &now, ll, gp)
		res, err := r.Run(ctx, prices.TriggerCLI, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Applied).To(BeZero())
		open := proposals(ctx, r, m, "open")
		Expect(open).To(HaveLen(2))
		for _, p := range open {
			Expect(p.Class).To(Equal(pricefetch.ClassNeedsReview))
			Expect(p.FailedRules).To(ContainSubstring("returns other values"))
		}
	})

	It("does not let a source that cannot read a model veto one that can", func(ctx SpecContext) {
		m := uniq()
		src := openaiList("2000-01-01.1", m)
		now := time.Now()
		ll := &sourceAdapter{name: pricefetch.SourceLiteLLM, model: m, in: "1.1", out: "2.2", commitAge: 30 * time.Hour}
		gp := &sourceAdapter{name: pricefetch.SourceGenaiPrices, model: m, in: "1.1", out: "2.2", commitAge: 30 * time.Hour, problems: []string{"a list of price sets"}}
		r := newRefresher(src, &now, gp, ll)
		res, err := r.Run(ctx, prices.TriggerCLI, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Applied).To(Equal(1))
		Expect(proposals(ctx, r, m, "accepted")).To(HaveKey(pricefetch.SourceLiteLLM))
	})

	It("never applies a models.dev proposal, however stable and however many agree", func(ctx SpecContext) {
		m := uniq()
		src := openaiList("2000-01-01.1", m)
		now := time.Now()
		md := &sourceAdapter{name: pricefetch.SourceModelsDev, model: m, in: "1.1", out: "2.2"}
		ll := &sourceAdapter{name: pricefetch.SourceLiteLLM, model: m, in: "1.1", out: "2.2", commitAge: 30 * time.Hour}
		r := newRefresher(src, &now, md)
		for range 2 {
			_, err := r.Run(ctx, prices.TriggerCLI, true)
			Expect(err).NotTo(HaveOccurred())
			now = now.Add(25 * time.Hour)
		}
		p := proposals(ctx, r, m, "open")[pricefetch.SourceModelsDev]
		Expect(p.Class).To(Equal(pricefetch.ClassNeedsReview))
		Expect(p.FailedRules).To(ContainSubstring("only proposes"))
		Expect(platformPrice(ctx, r, src, m).Source).To(Equal(prices.SourceDefault))

		// A first-hand aggregator that agrees applies on its own; models.dev's proposal is then stale.
		r = newRefresher(src, &now, md, ll)
		res, err := r.Run(ctx, prices.TriggerCLI, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Applied).To(Equal(1))
		Expect(proposals(ctx, r, m, "accepted")).To(HaveKey(pricefetch.SourceLiteLLM))
	})
})
