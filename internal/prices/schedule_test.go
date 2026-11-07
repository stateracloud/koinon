// SPDX-License-Identifier: Apache-2.0

package prices_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/pricefetch"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
	"github.com/stateracloud/koinon/pricing"
)

// freshDB is a database of its own for a spec that needs to know the whole install: no platform
// admin, or no provider of a type, anywhere.
func freshDB(ctx context.Context) *pgxpool.Pool { return pgtest.Start(ctx) }

// grantAdmin makes a platform admin in pool and returns their personal org.
func grantAdmin(ctx context.Context, pool *pgxpool.Pool) pgtype.UUID {
	org, user, _ := pgtest.NewOrg(ctx, pool)
	_, err := pool.Exec(ctx, `INSERT INTO platform_admins (user_id) VALUES ($1)`, user)
	Expect(err).NotTo(HaveOccurred())
	return org
}

// addProvider adds an org with an enabled provider of the type and the named models.
func addProvider(ctx context.Context, pool *pgxpool.Pool, providerType string, models ...string) {
	org, _, _ := pgtest.NewOrg(ctx, pool)
	var pid pgtype.UUID
	Expect(pool.QueryRow(ctx, `INSERT INTO providers (org_id, name, type, url) VALUES ($1, $2, $3, 'https://example.com/v1') RETURNING id`,
		org, "fake-"+providerType, providerType).Scan(&pid)).To(Succeed())
	for _, m := range models {
		_, err := pool.Exec(ctx, `INSERT INTO models (org_id, provider_id, name) VALUES ($1, $2, $3)`, org, pid, m)
		Expect(err).NotTo(HaveOccurred())
	}
}

func countRows(ctx context.Context, pool *pgxpool.Pool, query string, args ...any) int {
	var n int
	Expect(pool.QueryRow(ctx, query, args...).Scan(&n)).To(Succeed())
	return n
}

// httpAdapter is an "openrouter" adapter that GETs a test server, which counts the requests: the
// proof that a mode made a request, or none.
type httpAdapter struct {
	url   string
	model string
}

func (h *httpAdapter) Name() string { return pricefetch.SourceOpenRouter }
func (h *httpAdapter) Fetch(ctx context.Context, now time.Time) (pricefetch.Snapshot, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.url, nil)
	if err != nil {
		return pricefetch.Snapshot{}, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return pricefetch.Snapshot{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return pricefetch.Snapshot{}, err
	}
	return pricefetch.Snapshot{
		Source: pricefetch.SourceOpenRouter, URL: h.url, Licence: "fake", FetchedAt: now, SHA256: strings.Repeat("0", 64), Count: 1,
		Offers: []pricefetch.Offer{{Model: h.model, ProviderType: "openrouter", Spec: pricing.Spec{Prices: pricing.Wire{Input: "1.1", Output: "2.2"}}}},
	}, nil
}

func countingServer() (*httptest.Server, *atomic.Int32) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	DeferCleanup(srv.Close)
	return srv, &hits
}

func refresherOn(pool *pgxpool.Pool, src pricing.ListSource, a pricefetch.Adapter, now *time.Time, b ...pricefetch.Bounds) *prices.Refresher {
	cfg := prices.RefreshConfig{Adapters: []pricefetch.Adapter{a}, Metrics: prices.NewMetrics(nil), Now: func() time.Time { return *now }}
	if len(b) > 0 {
		cfg.Bounds = b[0]
	}
	r, err := prices.NewRefresher(prices.New(pool, slog.Default(), src), cfg)
	Expect(err).NotTo(HaveOccurred())
	return r
}

func openProposal(ctx context.Context, r *prices.Refresher, model string) prices.Proposal {
	ps, err := r.Service().ListProposals(ctx, prices.StateOpen, "", 1000)
	Expect(err).NotTo(HaveOccurred())
	for _, p := range ps {
		if p.Model == model {
			return p
		}
	}
	Fail("no open proposal for " + model)
	return prices.Proposal{}
}

var _ = Describe("the price refresh modes (maintainer decision 2026-11-07)", func() {
	BeforeEach(func() { needDocker() })

	const model = "fake/model-a"
	tick := func(ctx context.Context, pool *pgxpool.Pool, mode prices.RefreshMode, url string) {
		now := time.Now()
		r := refresherOn(pool, defaultList("2000-01-01.1", model), &httpAdapter{url: url, model: model}, &now)
		_, err := r.Tick(ctx, mode, 24*time.Hour)
		Expect(err).NotTo(HaveOccurred())
	}

	It("auto fetches OpenRouter while an openrouter provider exists", func(ctx SpecContext) {
		pool := freshDB(ctx)
		addProvider(ctx, pool, "openrouter", model)
		srv, hits := countingServer()
		tick(ctx, pool, prices.RefreshAuto, srv.URL)
		Expect(hits.Load()).To(BeEquivalentTo(1))
	})

	It("auto makes no request while no openrouter provider exists", func(ctx SpecContext) {
		pool := freshDB(ctx)
		addProvider(ctx, pool, "openai", "fake/model-b") // another type does not count
		srv, hits := countingServer()
		tick(ctx, pool, prices.RefreshAuto, srv.URL)
		Expect(hits.Load()).To(BeZero())
		Expect(countRows(ctx, pool, `SELECT count(*) FROM price_source_fetches`)).To(BeZero())
	})

	It("off makes no request, even with an openrouter provider", func(ctx SpecContext) {
		pool := freshDB(ctx)
		addProvider(ctx, pool, "openrouter", model)
		srv, hits := countingServer()
		tick(ctx, pool, prices.RefreshOff, srv.URL)
		Expect(hits.Load()).To(BeZero())
	})

	It("on fetches without any provider", func(ctx SpecContext) {
		pool := freshDB(ctx)
		srv, hits := countingServer()
		tick(ctx, pool, prices.RefreshOn, srv.URL)
		Expect(hits.Load()).To(BeEquivalentTo(1))
	})

	// The scheduled tick runs no more than the recorded setting (#328 review): a koinon-api whose
	// install audit event is refused (no writer grant yet, ADR 0100 decision 1.1a) keeps fetching as
	// the recorded row says until its own setting and event are recorded.
	It("holds the schedule to the recorded setting until the configured one and its event are recorded", func(ctx SpecContext) {
		pool := freshDB(ctx)
		srv, hits := countingServer()
		now := time.Now()
		r := refresherOn(pool, defaultList("2000-01-01.1", model), &httpAdapter{url: srv.URL, model: model}, &now)
		scheduled := func(mode prices.RefreshMode, iv time.Duration) (prices.RefreshMode, time.Duration) {
			GinkgoHelper()
			eff, eiv, err := r.Effective(ctx, mode, iv)
			Expect(err).NotTo(HaveOccurred())
			_, err = r.Tick(ctx, eff, eiv)
			Expect(err).NotTo(HaveOccurred())
			return eff, eiv
		}

		By("nothing recorded yet: the default, auto, which fetches nothing without an openrouter provider")
		eff, _ := scheduled(prices.RefreshOn, 12*time.Hour)
		Expect(eff).To(Equal(prices.RefreshAuto))
		Expect(hits.Load()).To(BeZero())

		By("the database refuses the install event: the setting is not recorded and on does not run")
		_, err := pool.Exec(ctx, `CREATE FUNCTION deny_install_audit() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN RAISE EXCEPTION 'permission denied for table install_audit_events' USING ERRCODE = '42501'; END $$`)
		Expect(err).NotTo(HaveOccurred())
		_, err = pool.Exec(ctx, `CREATE TRIGGER aaa_deny_install_audit BEFORE INSERT ON install_audit_events FOR EACH ROW EXECUTE FUNCTION deny_install_audit()`)
		Expect(err).NotTo(HaveOccurred())
		Expect(r.SyncSettings(ctx, prices.RefreshOn, 12*time.Hour)).To(MatchError(audit.ErrInstallWriteDenied))
		Expect(countRows(ctx, pool, `SELECT count(*) FROM price_refresh_settings`)).To(BeZero(), "the setting rolled back with its event")
		eff, _ = scheduled(prices.RefreshOn, 12*time.Hour)
		Expect(eff).To(Equal(prices.RefreshAuto))
		Expect(hits.Load()).To(BeZero(), "no fetch without the event")

		By("the grant lands: the retry records the setting and its event, and the next tick runs on")
		_, err = pool.Exec(ctx, `DROP TRIGGER aaa_deny_install_audit ON install_audit_events`)
		Expect(err).NotTo(HaveOccurred())
		Expect(r.SyncSettings(ctx, prices.RefreshOn, 12*time.Hour)).To(Succeed())
		Expect(countRows(ctx, pool, `SELECT count(*) FROM install_audit_events WHERE action = 'price.refresh.settings_changed'`)).To(Equal(1))
		var iv time.Duration
		eff, iv = scheduled(prices.RefreshOn, 12*time.Hour)
		Expect(eff).To(Equal(prices.RefreshOn))
		Expect(iv).To(Equal(12 * time.Hour))
		Expect(hits.Load()).To(BeEquivalentTo(1))

		By("a replica that fetches less runs its own setting at once; one that fetches more is held back")
		eff, _, err = r.Effective(ctx, prices.RefreshOff, 12*time.Hour)
		Expect(err).NotTo(HaveOccurred())
		Expect(eff).To(Equal(prices.RefreshOff))
		eff, iv, err = r.Effective(ctx, prices.RefreshOn, 6*time.Hour)
		Expect(err).NotTo(HaveOccurred())
		Expect(eff).To(Equal(prices.RefreshOn))
		Expect(iv).To(Equal(12*time.Hour), "not more often than recorded")
		Expect(r.SyncSettings(ctx, prices.RefreshAuto, 24*time.Hour)).To(Succeed())
		eff, iv, err = r.Effective(ctx, prices.RefreshOn, 12*time.Hour)
		Expect(err).NotTo(HaveOccurred())
		Expect(eff).To(Equal(prices.RefreshAuto), "an old replica during a rolling upgrade")
		Expect(iv).To(Equal(24 * time.Hour))
	})

	It("parses only auto, off and on", func() {
		for _, s := range []string{"auto", "off", "on"} {
			Expect(prices.ParseRefreshMode(s)).To(BeEquivalentTo(s))
		}
		_, err := prices.ParseRefreshMode("true")
		Expect(err).To(HaveOccurred())
	})

	It("counts the models no price covers, by provider type", func(ctx SpecContext) {
		pool := freshDB(ctx)
		addProvider(ctx, pool, "openrouter", model, "fake/model-unpriced-1", "fake/model-unpriced-2")
		now := time.Now()
		m := prices.NewMetrics(nil)
		r, err := prices.NewRefresher(prices.New(pool, slog.Default(), defaultList("2000-01-01.1", model)), prices.RefreshConfig{Metrics: m, Now: func() time.Time { return now }})
		Expect(err).NotTo(HaveOccurred())
		got, err := r.ReportUnpriced(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(map[string]int{"openrouter": 2}))
		Expect(testutil.ToFloat64(m.Unpriced.WithLabelValues("openrouter"))).To(BeEquivalentTo(2))
	})
})

var _ = Describe("price changes with no platform admin (a fresh install, ADR 0100)", func() {
	BeforeEach(func() { needDocker() })

	installCount := func(ctx context.Context, pool *pgxpool.Pool, action string) int {
		return countRows(ctx, pool, `SELECT count(*) FROM install_audit_events WHERE action = $1`, action)
	}

	It("syncs the settings, retires caught-up entries and auto-applies, each one install event of the named system actor", func(ctx SpecContext) {
		pool := freshDB(ctx)
		m := uniq()
		id := "fake-0-" + strings.ReplaceAll(m, "/", "-")
		// A platform entry written while an admin existed, who is gone again.
		grantAdmin(ctx, pool)
		_, err := prices.New(pool, slog.Default(), defaultList("2000-01-01.1", m)).SetPlatformForTest(ctx, []pricing.Entry{platformEntry(id, m, "1", "2", "")})
		Expect(err).NotTo(HaveOccurred())
		_, err = pool.Exec(ctx, `DELETE FROM platform_admins`)
		Expect(err).NotTo(HaveOccurred())
		orgAudited := countRows(ctx, pool, `SELECT count(*) FROM audit_events`)
		installBefore := countRows(ctx, pool, `SELECT count(*) FROM install_audit_events`)
		setBefore := installCount(ctx, pool, "platform_price.set")

		now := time.Now()
		other := uniq()
		a := &fakeAdapter{model: other, in: "1.1", out: "2.2"}
		r := refresherOn(pool, defaultList("2000-01-01.2", m, other), a, &now)

		Expect(r.SyncSettings(ctx, prices.RefreshOn, 12*time.Hour)).To(Succeed(), "a changed setting is recorded without an admin")
		Expect(countRows(ctx, pool, `SELECT count(*) FROM price_refresh_settings WHERE mode = 'on'`)).To(Equal(1))
		Expect(installCount(ctx, pool, "price.refresh.settings_changed")).To(Equal(1))

		retired, err := r.Service().RetireCaughtUp(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(retired).To(ConsistOf(id))
		Expect(installCount(ctx, pool, "platform_price.retired")).To(Equal(1))

		_, err = r.Tick(ctx, prices.RefreshOn, 12*time.Hour)
		Expect(err).NotTo(HaveOccurred())
		now = now.Add(25 * time.Hour)
		res, err := r.Tick(ctx, prices.RefreshOn, 12*time.Hour)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Applied).To(Equal(1), "the auto-apply no longer waits for an admin: the install chain audits it")
		const actor = "system koinon-api:price-refresh"
		Expect(countRows(ctx, pool, `SELECT count(*) FROM platform_price_versions WHERE actor = $1`, actor)).To(BeNumerically(">=", 1))
		Expect(installCount(ctx, pool, "platform_price.set")).To(Equal(setBefore + 1))
		Expect(installCount(ctx, pool, "price.proposal.accepted")).To(Equal(1))
		Expect(countRows(ctx, pool, `SELECT count(*) FROM install_audit_events WHERE actor_kind = 'system' AND actor_id = 'koinon-api:price-refresh'`)).
			To(Equal(countRows(ctx, pool, `SELECT count(*) FROM install_audit_events`)-installBefore), "every event names the system actor")
		Expect(countRows(ctx, pool, `SELECT count(*) FROM audit_events`)).To(Equal(orgAudited), "nothing is written into any org")
		verdict, err := audit.VerifyInstall(ctx, store.New(pool))
		Expect(err).NotTo(HaveOccurred())
		Expect(verdict.OK()).To(BeTrue(), "%+v", verdict.Break)
	})

	It("writes a person's decision as one install event of a cli actor, with no admin and no personal-org copy", func(ctx SpecContext) {
		pool := freshDB(ctx)
		m := uniq()
		now := time.Now()
		r := refresherOn(pool, defaultList("2000-01-01.1", m), &fakeAdapter{model: m, in: "1.1", out: "2.2"}, &now)
		_, err := r.Run(ctx, prices.TriggerCLI, false)
		Expect(err).NotTo(HaveOccurred())
		p := openProposal(ctx, r, m)
		orgAudited := countRows(ctx, pool, `SELECT count(*) FROM audit_events`)

		Expect(r.Dismiss(ctx, audit.CLI("prices dismiss"), p.ID)).To(Succeed())
		Expect(countRows(ctx, pool, `SELECT count(*) FROM install_audit_events WHERE action = 'price.proposal.dismissed' AND actor_kind = 'cli' AND actor_id = 'prices dismiss'`)).To(Equal(1))
		Expect(countRows(ctx, pool, `SELECT count(*) FROM audit_events`)).To(Equal(orgAudited))
		Expect(countRows(ctx, pool, `SELECT count(*) FROM price_proposals WHERE id = $1 AND decided_by = 'cli prices dismiss'`, p.ID)).To(Equal(1))

		p2 := uniq()
		a2 := &fakeAdapter{model: p2, in: "1.1", out: "2.2", maxOutput: 4096}
		r2 := refresherOn(pool, defaultList("2000-01-01.1", m, p2), a2, &now)
		_, err = r2.Run(ctx, prices.TriggerCLI, false)
		Expect(err).NotTo(HaveOccurred())
		_, err = r2.Accept(ctx, audit.CLI("prices accept"), openProposal(ctx, r2, p2).ID, prices.AcceptOptions{MaxOutput: 4096})
		Expect(err).NotTo(HaveOccurred())
		Expect(countRows(ctx, pool, `SELECT count(*) FROM install_audit_events WHERE action = 'platform_price.set' AND actor_kind = 'cli'`)).To(Equal(1))
		Expect(countRows(ctx, pool, `SELECT count(*) FROM audit_events`)).To(Equal(orgAudited))
	})
})

var _ = Describe("auto-apply under the Refresher's own bounds", func() {
	BeforeEach(func() { needDocker() })

	It("does not apply a 20% proposal a CLI fetch classified auto when its bound is 10%", func(ctx SpecContext) {
		pool := freshDB(ctx)
		m := uniq()
		src := defaultList("2000-01-01.1", m)
		a := &fakeAdapter{model: m, in: "1.2", out: "2.4"} // +20%
		now := time.Now()
		cli := refresherOn(pool, src, a, &now) // the default bounds, 25%
		_, err := cli.Run(ctx, prices.TriggerCLI, false)
		Expect(err).NotTo(HaveOccurred())
		now = now.Add(25 * time.Hour)
		_, err = cli.Run(ctx, prices.TriggerCLI, false)
		Expect(err).NotTo(HaveOccurred())
		Expect(openProposal(ctx, cli, m).Class).To(Equal(pricefetch.ClassAuto))

		tight := refresherOn(pool, src, a, &now, pricefetch.Bounds{MaxChangePercent: 10, MaxAutoPerDay: 10})
		Expect(tight.AutoApply(ctx)).To(BeZero())
		p := openProposal(ctx, tight, m)
		Expect(p.Class).To(Equal(pricefetch.ClassNeedsReview))
		Expect(p.FailedRules).To(ContainSubstring("more than 10%"))
		Expect(countRows(ctx, pool, `SELECT count(*) FROM platform_price_versions`)).To(BeZero())
	})

	It("gives the prices commands the bounds koinon-api started with", func(ctx SpecContext) {
		pool := freshDB(ctx)
		now := time.Now()
		svc := prices.New(pool, slog.Default(), defaultList("2000-01-01.1"))
		Expect(svc.StoredBounds(ctx)).To(Equal(pricefetch.DefaultBounds))
		r := refresherOn(pool, defaultList("2000-01-01.1"), &fakeAdapter{model: uniq()}, &now, pricefetch.Bounds{MaxChangePercent: 10, MaxAutoPerDay: 3})
		Expect(r.SyncSettings(ctx, prices.RefreshAuto, 24*time.Hour)).To(Succeed())
		Expect(svc.StoredBounds(ctx)).To(Equal(pricefetch.Bounds{MaxChangePercent: 10, MaxAutoPerDay: 3}))
	})

	It("skips auto-apply for a source whose fetch failed this run", func(ctx SpecContext) {
		pool := freshDB(ctx)
		m := uniq()
		src := defaultList("2000-01-01.1", m)
		a := &fakeAdapter{model: m, in: "1.1", out: "2.2"}
		now := time.Now()
		r := refresherOn(pool, src, a, &now)
		for range 2 {
			_, err := r.Run(ctx, prices.TriggerCLI, false)
			Expect(err).NotTo(HaveOccurred())
			now = now.Add(25 * time.Hour)
		}
		Expect(openProposal(ctx, r, m).Class).To(Equal(pricefetch.ClassAuto))

		a.err = errors.New("fake source down")
		res, err := r.Tick(ctx, prices.RefreshOn, 24*time.Hour)
		Expect(err).To(HaveOccurred())
		Expect(res.Applied).To(BeZero())
		Expect(openProposal(ctx, r, m).Class).To(Equal(pricefetch.ClassAuto), "it waits for the next good fetch")

		a.err = nil
		now = now.Add(25 * time.Hour)
		res, err = r.Tick(ctx, prices.RefreshOn, 24*time.Hour)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Applied).To(Equal(1))
	})
})

var _ = Describe("a new model whose source reports no output limit", func() {
	BeforeEach(func() { needDocker() })

	It("waits for review and is accepted with --max-output", func(ctx SpecContext) {
		pool := freshDB(ctx)
		grantAdmin(ctx, pool)
		m := uniq()
		addProvider(ctx, pool, "openrouter", m) // in the catalog, priced by no entry
		now := time.Now()
		r := refresherOn(pool, defaultList("2000-01-01.1"), &fakeAdapter{model: m, in: "1", out: "2"}, &now)
		_, err := r.Run(ctx, prices.TriggerCLI, false)
		Expect(err).NotTo(HaveOccurred())
		p := openProposal(ctx, r, m)
		Expect(p.Class).To(Equal(pricefetch.ClassNeedsReview), "not refused")
		Expect(p.FailedRules).To(ContainSubstring("max_output must be set"))

		_, err = r.Accept(ctx, audit.CLI("test"), p.ID, prices.AcceptOptions{})
		Expect(err).To(MatchError(ContainSubstring("--max-output")))
		_, err = r.Accept(ctx, audit.CLI("test"), p.ID, prices.AcceptOptions{MaxOutput: 4096})
		Expect(err).NotTo(HaveOccurred())

		var res prices.Resolution
		Expect(pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			var err error
			res, err = prices.NewResolver(defaultList("2000-01-01.1")).Resolve(ctx, store.New(tx), pgtype.UUID{}, prices.Model{Name: m, ProviderType: "openrouter"}, time.Now())
			return err
		})).To(Succeed())
		Expect(res.Source).To(Equal(prices.SourcePlatform))
		Expect(res.Entry.MaxOutput).To(Equal(4096))
	})
})

var _ = Describe("a new model the source prices fully (ADR 0098, amendment of 2026-11-07)", func() {
	BeforeEach(func() { needDocker() })

	It("is applied by the scheduled auto-apply after two stable fetches, as system koinon-api:price-refresh", func(ctx SpecContext) {
		pool := freshDB(ctx)
		grantAdmin(ctx, pool)
		const m = "fake/model-new"
		addProvider(ctx, pool, "openrouter", m) // in the catalog, priced by no entry
		now := time.Now()
		r := refresherOn(pool, defaultList("2000-01-01.1"), &fakeAdapter{model: m, in: "1", out: "2", maxOutput: 8192}, &now)

		res, err := r.Run(ctx, prices.TriggerSchedule, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Applied).To(BeZero(), "a value seen once is not stable")
		p := openProposal(ctx, r, m)
		Expect(p.Class).To(Equal(pricefetch.ClassNeedsReview))
		Expect(p.FailedRules).To(ContainSubstring("pinning"))
		Expect(p.FailedRules).NotTo(ContainSubstring("no Koinon price"))

		now = now.Add(25 * time.Hour)
		res, err = r.Run(ctx, prices.TriggerSchedule, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Applied).To(Equal(1))

		var got prices.Resolution
		Expect(pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			var err error
			got, err = prices.NewResolver(defaultList("2000-01-01.1")).Resolve(ctx, store.New(tx), pgtype.UUID{}, prices.Model{Name: m, ProviderType: "openrouter"}, now)
			return err
		})).To(Succeed())
		Expect(got.Source).To(Equal(prices.SourcePlatform))
		Expect(got.Entry.ID).To(Equal(pricefetch.EntryID("openrouter", m)))
		Expect(got.Entry.Spec.Prices.Input).To(Equal("1"))
		Expect(got.Entry.MaxOutput).To(Equal(8192))
		Expect(countRows(ctx, pool, `SELECT count(*) FROM price_proposals WHERE model = $1 AND state = 'accepted' AND decided_by = 'system koinon-api:price-refresh'`, m)).To(Equal(1))
	})
})
