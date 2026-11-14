// SPDX-License-Identifier: Apache-2.0

package prices_test

import (
	"context"
	"errors"
	"fmt"
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
	// failEndpoints makes the endpoints route answer like a rate limit (a fake 429) while it is set.
	failEndpoints *atomic.Bool
	// asked records the models the endpoints route was asked about, in order.
	asked *[]string
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
	if c.asked != nil {
		*c.asked = append(*c.asked, model)
	}
	if c.failEndpoints != nil && c.failEndpoints.Load() {
		return pricefetch.ModelEndpoints{}, errors.New("fake openrouter: 429 Too Many Requests")
	}
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
	var (
		failEndpoints atomic.Bool
		asked         []string
		listedModels  []string
	)
	run := func(ctx context.Context, mode prices.RefreshMode) {
		GinkgoHelper()
		models := listedModels
		if models == nil {
			models = []string{model}
		}
		r, err := prices.NewRefresher(prices.New(pool, slog.Default(), defaultList("2000-01-01.1", models...)), prices.RefreshConfig{
			Adapters: []pricefetch.Adapter{&catalogAdapter{url: srv.URL, models: models, failEndpoints: &failEndpoints, asked: &asked}},
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
		failEndpoints.Store(false)
		asked, listedModels = nil, nil
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

	It("does not store the catalog under RefreshOn when no openrouter provider exists", func(ctx SpecContext) {
		run(ctx, prices.RefreshOn)
		Expect(hits.Load()).To(BeNumerically(">", 0), "on fetches the prices regardless")
		Expect(countRows(ctx, pool, `SELECT count(*) FROM openrouter_catalog`)).To(BeZero())
		Expect(countRows(ctx, pool, `SELECT count(*) FROM openrouter_models`)).To(BeZero())
	})

	It("does not store again for an in-use model the list does not carry", func(ctx SpecContext) {
		addProvider(ctx, pool, "openrouter", model, "fake/not-listed")
		run(ctx, prices.RefreshAuto)
		first := hits.Load()
		Expect(first).To(BeEquivalentTo(2), "the list and the endpoints of the listed model only")
		Expect(countRows(ctx, pool, `SELECT count(*) FROM openrouter_catalog`)).To(Equal(1))

		now = now.Add(25 * time.Hour)
		run(ctx, prices.RefreshAuto)
		Expect(hits.Load()).To(Equal(first+1), "the list only: no second store, no endpoints request")
		Expect(countRows(ctx, pool, `SELECT count(*) FROM openrouter_catalog WHERE source = 'openrouter@2026-11-11T08:00:00Z'`)).To(Equal(1))
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

	It("asks the endpoints route again on the next tick after a round that failed, though the catalog is not due", func(ctx SpecContext) {
		addProvider(ctx, pool, "openrouter", model)
		failEndpoints.Store(true)
		run(ctx, prices.RefreshAuto)
		Expect(asked).To(Equal([]string{model}))
		Expect(countRows(ctx, pool, `SELECT count(*) FROM openrouter_models WHERE endpoints_fetched_at IS NOT NULL`)).To(BeZero(), "a failed round stores nothing")

		now = now.Add(25 * time.Hour) // the catalog is stored: not due
		run(ctx, prices.RefreshAuto)
		Expect(asked).To(Equal([]string{model, model}), "the next tick retries")
		Expect(countRows(ctx, pool, `SELECT count(*) FROM openrouter_catalog WHERE source = 'openrouter@2026-11-11T08:00:00Z'`)).To(Equal(1), "the catalog was not stored again")
		Expect(countRows(ctx, pool, `SELECT count(*) FROM openrouter_models WHERE endpoints_fetched_at IS NOT NULL`)).To(BeZero(), "still failing")

		failEndpoints.Store(false)
		now = now.Add(25 * time.Hour)
		run(ctx, prices.RefreshAuto)
		Expect(asked).To(HaveLen(3))
		Expect(countRows(ctx, pool, `SELECT count(*) FROM openrouter_models WHERE endpoints_fetched_at IS NOT NULL`)).To(Equal(1))

		now = now.Add(25 * time.Hour)
		run(ctx, prices.RefreshAuto)
		Expect(asked).To(HaveLen(3), "fetched endpoints are not asked for again inside the week")
	})

	It("retries only the models a half-done round did not store", func(ctx SpecContext) {
		a, b := "fake/model-a", "fake/model-b"
		listedModels = []string{a, b}
		addProvider(ctx, pool, "openrouter", a, b)
		run(ctx, prices.RefreshAuto)
		Expect(asked).To(Equal([]string{a, b}))
		// Model b lost its endpoints (a round that stopped after a): only b is due.
		_, err := pool.Exec(ctx, `UPDATE openrouter_models SET endpoints_fetched_at = NULL WHERE model = $1`, b)
		Expect(err).NotTo(HaveOccurred())
		asked = nil
		now = now.Add(25 * time.Hour)
		run(ctx, prices.RefreshAuto)
		Expect(asked).To(Equal([]string{b}))
	})

	It("covers more than the cap of models in use over successive rounds, and says how many were left", func(ctx SpecContext) {
		n := orcatalog.MaxEndpointModels + 5
		for i := range n {
			listedModels = append(listedModels, fmt.Sprintf("fake/m-%03d", i))
		}
		addProvider(ctx, pool, "openrouter", listedModels...)
		run(ctx, prices.RefreshAuto)
		Expect(asked).To(HaveLen(orcatalog.MaxEndpointModels), "the first round stops at the cap")
		Expect(countRows(ctx, pool, `SELECT count(*) FROM openrouter_models WHERE endpoints_fetched_at IS NOT NULL`)).To(Equal(orcatalog.MaxEndpointModels))

		asked = nil
		now = now.Add(25 * time.Hour)
		run(ctx, prices.RefreshAuto)
		Expect(asked).To(HaveLen(5), "the next round asks only the models the first one left")
		Expect(asked).To(ContainElements("fake/m-100", "fake/m-104"))
		Expect(countRows(ctx, pool, `SELECT count(*) FROM openrouter_models WHERE endpoints_fetched_at IS NOT NULL`)).To(Equal(n))

		asked = nil
		now = now.Add(25 * time.Hour)
		run(ctx, prices.RefreshAuto)
		Expect(asked).To(BeEmpty(), "every model is fresh")
	})
})
