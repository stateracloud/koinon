// SPDX-License-Identifier: Apache-2.0

package prices_test

import (
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
	"github.com/stateracloud/koinon/pricing"
)

var _ = Describe("Resolver.Find", func() {
	BeforeEach(func() { needDocker() })

	It("reports a model nobody enabled as having no price", func(ctx SpecContext) {
		rv := prices.NewResolver(defaultList("2026-10-24.1", "fake/known"))
		qt, err := rv.Find(ctx, store.New(db), adminOrg, uniq(), time.Now())
		Expect(err).To(MatchError(prices.ErrUnpriced))
		Expect(qt.Model.Name).To(BeEmpty())
		reason, ok := prices.Unpriceable(err)
		Expect(ok).To(BeTrue())
		Expect(reason).To(Equal(prices.ReasonNoPrice))
	})

	// agentd's modelFacts keeps the model's format and provider type from this Quote.
	It("returns the enabled model with ErrUnpriced when no entry prices it", func(ctx SpecContext) {
		org, _, _ := pgtest.NewOrg(ctx, db)
		var pid pgtype.UUID
		Expect(db.QueryRow(ctx, `INSERT INTO providers (org_id, name, type, url) VALUES ($1, 'unpriced-find', 'openrouter', 'https://example.com/v1') RETURNING id`, org).Scan(&pid)).To(Succeed())
		name := uniq()
		_, err := db.Exec(ctx, `INSERT INTO models (org_id, provider_id, name) VALUES ($1, $2, $3)`, org, pid, name)
		Expect(err).NotTo(HaveOccurred())

		// The default list prices another model; the org has no override and the platform no entry.
		rv := prices.NewResolver(defaultList("2026-10-24.1", "fake/known"))
		qt, err := rv.Find(ctx, store.New(db), org, name, time.Now())
		Expect(err).To(MatchError(prices.ErrUnpriced))
		reason, ok := prices.Unpriceable(err)
		Expect(ok).To(BeTrue())
		Expect(reason).To(Equal(prices.ReasonNoPrice))
		Expect(qt.Model.Name).To(Equal(name))
		Expect(qt.Model.Format).NotTo(BeEmpty())
		Expect(qt.Model.ProviderType).To(Equal("openrouter"))
	})
})

var _ = Describe("Service.ModelWarnings", func() {
	BeforeEach(func() { needDocker() })

	It("gives an ollama provider at ollama.com no warning: the model is unpriced", func(ctx SpecContext) {
		var providerID pgtype.UUID
		Expect(db.QueryRow(ctx, `INSERT INTO providers (org_id, name, type, url) VALUES ($1, 'ollama-cloud-warn', 'ollama', 'https://ollama.com/v1') RETURNING id`, adminOrg).Scan(&providerID)).To(Succeed())
		svc := prices.New(db, slog.Default(), pricing.Embedded())
		Expect(svc.ModelWarnings(ctx, adminOrg, providerID, "gpt-oss:120b", modelformat.Default("ollama", "gpt-oss:120b"))).To(BeEmpty())
	})
})
