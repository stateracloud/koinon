// SPDX-License-Identifier: Apache-2.0

package prices_test

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
	"github.com/stateracloud/koinon/pricing"
)

var _ = Describe("Service.ExplainOrg", func() {
	BeforeEach(func() { needDocker() })

	It("names the layer of each served model and the price an org price replaces, as the gateway resolves them", func(ctx SpecContext) {
		org, _, _ := pgtest.NewOrg(ctx, db)
		var pid pgtype.UUID
		Expect(db.QueryRow(ctx, `INSERT INTO providers (org_id, name, type, url) VALUES ($1, 'explain-org', 'openrouter', 'https://example.com/v1') RETURNING id`, org).Scan(&pid)).To(Succeed())

		overOnPlatform, overOnDefault, platformOnly, defaultOnly, unpriced := uniq(), uniq(), uniq(), uniq(), uniq()
		for _, m := range []string{overOnPlatform, overOnDefault, platformOnly, defaultOnly, unpriced} {
			_, err := db.Exec(ctx, `INSERT INTO models (org_id, provider_id, name) VALUES ($1, $2, $3)`, org, pid, m)
			Expect(err).NotTo(HaveOccurred())
		}
		// The default list prices every model but the last at input 1, output 2.
		src := defaultList("2000-01-01.1", overOnPlatform, overOnDefault, platformOnly, defaultOnly)
		svc := prices.New(db, slog.Default(), src)
		_, err := svc.SetPlatformForTest(ctx, []pricing.Entry{
			platformEntry("p-over", overOnPlatform, "5", "10", ""), platformEntry("p-only", platformOnly, "6", "12", ""),
		})
		Expect(err).NotTo(HaveOccurred())
		for i, m := range []string{overOnPlatform, overOnDefault} {
			e := platformEntry(fmt.Sprintf("org-over-%d-%d", i, seq.Load()), m, "0.5", "1", "")
			e.ValidFrom = "2000-01-01"
			_, err := svc.SetOverride(ctx, org, audit.CLI("test"), prices.OverrideInput{Entry: e})
			Expect(err).NotTo(HaveOccurred())
		}

		got, err := svc.ExplainOrg(ctx, org)
		Expect(err).NotTo(HaveOccurred())
		byModel := map[string]prices.ModelPrice{}
		for _, mp := range got {
			byModel[mp.Model] = mp
		}
		Expect(byModel).To(HaveLen(5))

		// The gateway's price of each model is Resolver.Resolve for the model of the served catalog.
		rv := prices.NewResolver(src)
		for _, m := range []string{overOnPlatform, overOnDefault, platformOnly, defaultOnly} {
			want, err := rv.Resolve(ctx, store.New(db), org, prices.Model{Name: m, ProviderType: "openrouter", ProviderURL: "https://example.com/v1"}, time.Now())
			Expect(err).NotTo(HaveOccurred())
			have := byModel[m]
			Expect(have.UnpricedReason).To(BeEmpty(), m)
			Expect(have.Resolution.Source).To(Equal(want.Source), m)
			Expect(have.Resolution.Ref).To(Equal(want.Ref), m)
			Expect(have.Resolution.Price).To(Equal(want.Price), m)
		}

		Expect(byModel[overOnPlatform].Resolution.Source).To(Equal(prices.SourceOrg))
		Expect(byModel[overOnPlatform].Replaces).NotTo(BeNil())
		Expect(byModel[overOnPlatform].Replaces.Source).To(Equal(prices.SourcePlatform))
		Expect(byModel[overOnPlatform].Replaces.Entry.Prices.Input).To(Equal("5"))

		Expect(byModel[overOnDefault].Resolution.Source).To(Equal(prices.SourceOrg))
		Expect(byModel[overOnDefault].Replaces).NotTo(BeNil())
		Expect(byModel[overOnDefault].Replaces.Source).To(Equal(prices.SourceDefault))
		Expect(byModel[overOnDefault].Replaces.Entry.Prices.Input).To(Equal("1"))

		Expect(byModel[platformOnly].Resolution.Source).To(Equal(prices.SourcePlatform))
		Expect(byModel[platformOnly].Replaces).To(BeNil())
		Expect(byModel[defaultOnly].Resolution.Source).To(Equal(prices.SourceDefault))
		Expect(byModel[defaultOnly].Replaces).To(BeNil())

		Expect(byModel[unpriced].UnpricedReason).To(Equal(prices.ReasonNoPrice))
		Expect(byModel[unpriced].Resolution.Source).To(BeEmpty())
	})
})
