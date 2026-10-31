// SPDX-License-Identifier: Apache-2.0

package agentmodel_test

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/testcontainers/testcontainers-go"

	"github.com/stateracloud/koinon/internal/agentmodel"
	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
	"github.com/stateracloud/koinon/pricing"
)

const orgDefaultList = `
version: "2000-01-01.1"
entries:
  - id: acme-flash
    provider: openrouter
    match: {glob: "acme/flash"}
    valid_from: 2000-01-01
    prices: {input: "0.20", output: "2"}
    tokenizer: bytes
    ratio: "3.0"
    max_output: 1000000
    source: {url: "https://example.com/test-prices", accessed: 2000-01-01}
  - id: acme-frontier
    provider: openrouter
    match: {glob: "acme/frontier"}
    valid_from: 2000-01-01
    prices: {input: "10", output: "50"}
    tokenizer: bytes
    ratio: "2.3"
    max_output: 128000
    source: {url: "https://example.com/test-prices", accessed: 2000-01-01}
  - id: acme-newer
    provider: openrouter
    match: {glob: "acme/newer"}
    valid_from: 2000-01-01
    prices: {input: "1", output: "5"}
    tokenizer: bytes
    ratio: "3.0"
    max_output: 100000
    source: {url: "https://example.com/test-prices", accessed: 2000-01-01}
  - id: acme-last
    provider: openrouter
    match: {glob: "acme/last"}
    valid_from: 2000-01-01
    prices: {input: "1", output: "5"}
    tokenizer: bytes
    ratio: "3.0"
    max_output: 100000
    source: {url: "https://example.com/test-prices", accessed: 2000-01-01}
`

// The org default is the first served priced model, providers by name and then models by name
// (OrgDefault). Nothing stores it, so a provider added later whose name sorts first silently
// changes the model every agent without a setting runs on (#256 proposes storing the default).
// The kind e2e found this when its quickstart gained a provider named "anthropic-fake" and the
// repo-helper agent ran on another model (#257). These specs pin the rule as it is, so changing
// it, to a stored default or anything else, is a deliberate edit of this file.
var _ = Describe("the org default model", Ordered, func() {
	var (
		pool  *pgxpool.Pool
		svc   *agentmodel.Service
		rv    prices.Resolver
		org   pgtype.UUID
		user  pgtype.UUID
		agent pgtype.UUID
		at    = time.Now()
	)

	provider := func(ctx context.Context, name, model string) {
		GinkgoHelper()
		var pid pgtype.UUID
		Expect(pool.QueryRow(ctx, `INSERT INTO providers (org_id, name, type, url) VALUES ($1, $2, 'openrouter', 'https://example.com/v1') RETURNING id`,
			org, name).Scan(&pid)).To(Succeed())
		_, err := pool.Exec(ctx, `INSERT INTO models (org_id, provider_id, name) VALUES ($1, $2, $3)`, org, pid, model)
		Expect(err).NotTo(HaveOccurred())
	}
	effective := func(ctx context.Context) (string, agentmodel.Source) {
		GinkgoHelper()
		_, v, err := svc.Load(ctx, org, agent, at)
		Expect(err).NotTo(HaveOccurred())
		return v.Effective, v.Source
	}

	modelOf := func(ctx context.Context) string {
		GinkgoHelper()
		m, _ := effective(ctx)
		return m
	}

	BeforeAll(func(ctx SpecContext) {
		if p, err := testcontainers.NewDockerProvider(); err != nil || p.Health(ctx) != nil {
			Skip("Docker is not available")
		}
		pool = pgtest.Start(ctx)
		l, rs, err := pricing.ParseList([]byte(orgDefaultList))
		Expect(err).NotTo(HaveOccurred())
		rv = prices.NewResolver(pricing.Static(l, rs))
		svc = agentmodel.New(pool, rv)
		org, user, _ = pgtest.NewOrg(ctx, pool)
		Expect(pool.QueryRow(ctx, `INSERT INTO agents (org_id, id, display_name, flavor, created_by)
			VALUES ($1, uuidv7(), 'Repo helper', 'opencode', $2) RETURNING id`, org, user).Scan(&agent)).To(Succeed())
	}, NodeTimeout(3*time.Minute))

	It("is the first served priced model by provider name, then model name", func(ctx SpecContext) {
		// The quickstart's providers (test/e2e): "fake" first, "fake-anthropic" after it.
		provider(ctx, "fake-anthropic", "acme/frontier")
		provider(ctx, "fake", "acme/flash")
		Expect(modelOf(ctx)).To(Equal("acme/flash"))
		got, err := agentmodel.OrgDefault(ctx, rv, store.New(pool), org, at)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal("acme/flash"))
		_, src := effective(ctx)
		Expect(src).To(Equal(agentmodel.SourceOrgDefault))
	})

	It("moves to the new model when a provider whose name sorts first is added (#256: the risk, pinned as it is)", func(ctx SpecContext) {
		provider(ctx, "anthropic-fake", "acme/unpriced")
		// acme/unpriced has no price here, so it is not served: the default stays.
		Expect(modelOf(ctx)).To(Equal("acme/flash"), "an unpriced model is never the default")

		// A priced model on a provider named before "fake" takes the default from every agent
		// without a setting. Today that is the rule; with a stored default (#256) this spec is the
		// one to change.
		provider(ctx, "aaa-new", "acme/newer")
		got, src := effective(ctx)
		Expect(got).To(Equal("acme/newer"), "adding a provider that sorts first changes the org default")
		Expect(src).To(Equal(agentmodel.SourceOrgDefault))
	})

	It("never moves an agent whose model is set, which is how a quickstart keeps its model", func(ctx SpecContext) {
		_, err := svc.Set(ctx, org, agent, audit.User(user), "acme/flash", at)
		Expect(err).NotTo(HaveOccurred())
		provider(ctx, "000-first", "acme/last")
		got, src := effective(ctx)
		Expect(got).To(Equal("acme/flash"))
		Expect(src).To(Equal(agentmodel.SourceAgent))
	})
})
