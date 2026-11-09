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

// The org default is stored (orgs.default_model, old issue 256), not derived from provider and model names.
// The kind e2e found the derived rule moving the quickstart agent to another model when a provider
// named "anthropic-fake" was added (old issue 257). These specs pin that only an explicit action changes it.
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
		pool = pgtest.Start(ctx) // skips without Docker (testenv)
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

	It("does not move when a provider whose name sorts first is added (old issue 256)", func(ctx SpecContext) {
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

	It("backfills an org marked by migration 00045 with the model the derived rule picked, not an unpriced one", func(ctx SpecContext) {
		old, _, _ := pgtest.NewOrg(ctx, pool)
		add := func(provName, model string) {
			var pid pgtype.UUID
			Expect(pool.QueryRow(ctx, `INSERT INTO providers (org_id, name, type, url) VALUES ($1, $2, 'openrouter', 'https://example.com/v1') RETURNING id`,
				old, provName).Scan(&pid)).To(Succeed())
			_, err := pool.Exec(ctx, `INSERT INTO models (org_id, provider_id, name) VALUES ($1, $2, $3)`, old, pid, model)
			Expect(err).NotTo(HaveOccurred())
		}
		add("aaa", "acme/unpriced")
		add("bbb", "acme/flash")
		_, err := pool.Exec(ctx, `UPDATE orgs SET default_model = NULL, default_model_pending = true WHERE id = $1`, old)
		Expect(err).NotTo(HaveOccurred())

		_, err = agentmodel.BackfillDefaults(ctx, pool, rv, at)
		Expect(err).NotTo(HaveOccurred())
		got, err := agentmodel.OrgDefault(ctx, store.New(pool), old)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal("acme/flash"))
		var reason string
		Expect(pool.QueryRow(ctx, `SELECT summary->'after'->>'reason' FROM audit_events WHERE org_id = $1 AND action = 'org.default_model.set'`, old).Scan(&reason)).To(Succeed())
		Expect(reason).To(Equal("migrated"))

		// Once only: a cleared default stays cleared on the next start.
		Expect(svc.SetDefault(ctx, old, audit.User(user), "", at)).To(Succeed())
		n, err := agentmodel.BackfillDefaults(ctx, pool, rv, at)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(BeZero())
		got, _ = agentmodel.OrgDefault(ctx, store.New(pool), old)
		Expect(got).To(BeEmpty())
	})
})

// A model on an ollama-type provider at ollama.com is billed per token by Ollama, so the default
// list does not price it at zero (issue 310): Check refuses it as unpriced until an org price exists.
var _ = Describe("a model on an ollama-type provider at ollama.com", func() {
	It("is unpriced, so Check refuses it and ListServed leaves it out", func(ctx SpecContext) {
		pool := pgtest.Start(ctx) // skips without Docker (testenv)
		org, _, _ := pgtest.NewOrg(ctx, pool)
		var pid pgtype.UUID
		Expect(pool.QueryRow(ctx, `INSERT INTO providers (org_id, name, type, url) VALUES ($1, 'cloud', 'ollama', 'https://ollama.com') RETURNING id`, org).Scan(&pid)).To(Succeed())
		_, err := pool.Exec(ctx, `INSERT INTO models (org_id, provider_id, name) VALUES ($1, $2, 'gpt-oss:120b')`, org, pid)
		Expect(err).NotTo(HaveOccurred())
		rv := prices.NewResolver(pricing.Embedded())

		err = agentmodel.Check(ctx, rv, store.New(pool), org, "gpt-oss:120b", time.Now())
		var unusable *agentmodel.UnusableError
		Expect(errors.As(err, &unusable)).To(BeTrue())
		Expect(unusable.Reason).To(Equal("unpriced"))
		served, err := agentmodel.ListServed(ctx, rv, store.New(pool), org, time.Now())
		Expect(err).NotTo(HaveOccurred())
		Expect(served).To(BeEmpty())
	}, SpecTimeout(3*time.Minute))
})
