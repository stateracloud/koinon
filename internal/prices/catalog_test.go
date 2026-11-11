// SPDX-License-Identifier: Apache-2.0

package prices_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/orcatalog"
	"github.com/stateracloud/koinon/internal/pricefetch"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/pricing"
)

// catalogAdapter is a fake OpenRouter that answers from a test server (never the real API): the list
// carries metadata, and the endpoints route is asked per model. The server counts every request.
type catalogAdapter struct {
	url    string
	models []string
}

func (c *catalogAdapter) Name() string { return pricefetch.SourceOpenRouter }

func (c *catalogAdapter) get(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func (c *catalogAdapter) Fetch(ctx context.Context, now time.Time) (pricefetch.Snapshot, error) {
	if err := c.get(ctx, c.url+"/models"); err != nil {
		return pricefetch.Snapshot{}, err
	}
	snap := pricefetch.Snapshot{Source: pricefetch.SourceOpenRouter, URL: c.url, Licence: "fake", FetchedAt: now, SHA256: strings.Repeat("0", 64), Count: len(c.models)}
	for _, m := range c.models {
		snap.Offers = append(snap.Offers, pricefetch.Offer{Model: m, ProviderType: "openrouter", Spec: pricing.Spec{Prices: pricing.Wire{Input: "1.1", Output: "2.2"}}})
		snap.Meta = append(snap.Meta, pricefetch.ModelMeta{Model: m, Name: "Fake " + m, ContextLength: 200000, MaxOutput: 64000})
	}
	return snap, nil
}

func (c *catalogAdapter) FetchEndpoints(ctx context.Context, model string, now time.Time) (pricefetch.ModelEndpoints, error) {
	if err := c.get(ctx, c.url+"/models/"+model+"/endpoints"); err != nil {
		return pricefetch.ModelEndpoints{}, err
	}
	return pricefetch.ModelEndpoints{Model: model, FetchedAt: now, Endpoints: []pricefetch.Endpoint{{Provider: "Fake Up"}}}, nil
}

var _ = Describe("the OpenRouter catalog fetch (ADR 0070 amendment of 2026-11-11, #543)", func() {
	BeforeEach(func() { needDocker() })

	const model = "fake/model-a"
	var (
		pool *pgxpool.Pool
		hits *atomic.Int32
		srv  *httptest.Server
		now  time.Time
	)
	run := func(ctx context.Context, mode prices.RefreshMode) {
		GinkgoHelper()
		r, err := prices.NewRefresher(prices.New(pool, slog.Default(), defaultList("2000-01-01.1", model)), prices.RefreshConfig{
			Adapters: []pricefetch.Adapter{&catalogAdapter{url: srv.URL, models: []string{model}}},
			Metrics:  prices.NewMetrics(nil), Now: func() time.Time { return now }, Catalog: orcatalog.New(pool),
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = r.TickForTest(ctx, mode, 24*time.Hour)
		Expect(err).NotTo(HaveOccurred())
	}
	BeforeEach(func(ctx SpecContext) {
		pool = freshDB(ctx)
		srv, hits = countingServer()
		now = time.Date(2026, 11, 11, 8, 0, 0, 0, time.UTC)
	})

	It("stores the catalog per install while an openrouter provider exists, with the endpoints of the models in use", func(ctx SpecContext) {
		addProvider(ctx, pool, "openrouter", model)
		run(ctx, prices.RefreshAuto)

		Expect(hits.Load()).To(BeEquivalentTo(2), "the list and one endpoints request")
		got, ok, err := orcatalog.New(pool).Effective(ctx, model)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(got.Name).To(Equal("Fake " + model))
		Expect(got.Source).To(Equal("openrouter@2026-11-11T08:00:00Z"))
		Expect(got.Endpoints).To(HaveLen(1))
		Expect(got.ContextLength).To(BeNil(), "a limit waits for a person: models.dev applies")
		ps, err := orcatalog.New(pool).ListProposals(ctx, orcatalog.StateOpen, 10)
		Expect(err).NotTo(HaveOccurred())
		Expect(ps).To(HaveLen(2))
	})

	It("makes no request and stores nothing when the setting is off", func(ctx SpecContext) {
		addProvider(ctx, pool, "openrouter", model)
		run(ctx, prices.RefreshOff)
		Expect(hits.Load()).To(BeZero())
		Expect(countRows(ctx, pool, `SELECT count(*) FROM openrouter_models`)).To(BeZero())
	})

	It("makes no request while no openrouter provider exists", func(ctx SpecContext) {
		run(ctx, prices.RefreshAuto)
		Expect(hits.Load()).To(BeZero())
		Expect(countRows(ctx, pool, `SELECT count(*) FROM openrouter_catalog`)).To(BeZero())
	})

	It("stores the metadata weekly: the daily price tick and a restart inside the week do not", func(ctx SpecContext) {
		addProvider(ctx, pool, "openrouter", model)
		run(ctx, prices.RefreshAuto)
		first := hits.Load()

		now = now.Add(25 * time.Hour) // the next price fetch (ADR 0098 interval 24h)
		run(ctx, prices.RefreshAuto)
		Expect(hits.Load()).To(Equal(first+1), "the price fetch reads the list again, and nothing else: no endpoints request")
		Expect(countRows(ctx, pool, `SELECT count(*) FROM openrouter_catalog WHERE source = 'openrouter@2026-11-11T08:00:00Z'`)).To(Equal(1))

		now = now.Add(7 * 24 * time.Hour)
		run(ctx, prices.RefreshAuto)
		Expect(countRows(ctx, pool, `SELECT count(*) FROM openrouter_catalog WHERE source = 'openrouter@2026-11-19T09:00:00Z'`)).To(Equal(1))
	})
})
