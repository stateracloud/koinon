// SPDX-License-Identifier: Apache-2.0

package agentmodel_test

import (
	"context"
	"errors"
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

// The org default is stored (orgs.default_model, #256), not derived from provider and model names.
// The kind e2e found the derived rule moving the quickstart agent to another model when a provider
// named "anthropic-fake" was added (#257). These specs pin that only an explicit action changes it.
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

	problem := func(ctx context.Context) string {
		GinkgoHelper()
		_, v, err := svc.Load(ctx, org, agent, at)
		Expect(err).NotTo(HaveOccurred())
		return v.Problem
	}
	auditCount := func(ctx context.Context) int {
		GinkgoHelper()
		var n int
		Expect(pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'org.default_model.set'`, org).Scan(&n)).To(Succeed())
		return n
	}

	It("is not derived: an org with served models and no stored default has none", func(ctx SpecContext) {
		provider(ctx, "fake-anthropic", "acme/frontier")
		provider(ctx, "fake", "acme/flash")
		got, err := agentmodel.OrgDefault(ctx, store.New(pool), org)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(BeEmpty())
		eff, src := effective(ctx)
		Expect(eff).To(BeEmpty())
		Expect(src).To(Equal(agentmodel.SourceNone))
		Expect(problem(ctx)).To(Equal(agentmodel.NoDefaultMessage))
	})

	It("is set by an admin to a model the org serves and prices, and audited", func(ctx SpecContext) {
		provider(ctx, "anthropic-fake", "acme/unpriced")
		err := svc.SetDefault(ctx, org, audit.User(user), "acme/unpriced", at)
		var bad *agentmodel.InvalidError
		Expect(errors.As(err, &bad)).To(BeTrue(), "an unpriced model cannot be the default")
		Expect(svc.SetDefault(ctx, org, audit.User(user), "acme/nope", at)).To(HaveOccurred())

		Expect(svc.SetDefault(ctx, org, audit.User(user), "acme/flash", at)).To(Succeed())
		got, src := effective(ctx)
		Expect(got).To(Equal("acme/flash"))
		Expect(src).To(Equal(agentmodel.SourceOrgDefault))
		Expect(auditCount(ctx)).To(Equal(1))
		Expect(svc.SetDefault(ctx, org, audit.User(user), "acme/flash", at)).To(Succeed())
		Expect(auditCount(ctx)).To(Equal(1), "setting the value it has is not a change")
	})

	It("does not move when a provider whose name sorts first is added (#256)", func(ctx SpecContext) {
		provider(ctx, "aaa-new", "acme/newer")
		got, src := effective(ctx)
		Expect(got).To(Equal("acme/flash"))
		Expect(src).To(Equal(agentmodel.SourceOrgDefault))
	})

	It("is cleared by an admin, and the agent then has a clear problem", func(ctx SpecContext) {
		Expect(svc.SetDefault(ctx, org, audit.User(user), "", at)).To(Succeed())
		Expect(modelOf(ctx)).To(BeEmpty())
		Expect(problem(ctx)).To(Equal(agentmodel.NoDefaultMessage))
		Expect(auditCount(ctx)).To(Equal(2))
		Expect(svc.SetDefault(ctx, org, audit.User(user), "acme/flash", at)).To(Succeed())
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
