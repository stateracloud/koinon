// SPDX-License-Identifier: Apache-2.0

package catalog_test

import (
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

var _ = Describe("Service", func() {
	var (
		svc   *catalog.Service
		org   pgtype.UUID
		user  pgtype.UUID
		actor audit.Actor
	)
	BeforeEach(func(ctx SpecContext) {
		svc = catalog.New(pool, ring, nil)
		org, user, _ = pgtest.NewOrg(ctx, pool)
		actor = audit.User(user)
	})

	events := func(ctx SpecContext, action string) []store.AuditEvent {
		GinkgoHelper()
		rows, err := store.New(pool).ListAuditEvents(ctx, store.ListAuditEventsParams{OrgID: org, PageSize: 100})
		Expect(err).NotTo(HaveOccurred())
		var out []store.AuditEvent
		for _, r := range rows {
			if r.Action == action {
				out = append(out, r)
			}
		}
		return out
	}
	create := func(ctx SpecContext, name, key string) catalog.Provider {
		GinkgoHelper()
		p, err := svc.CreateProvider(ctx, org, actor, user, catalog.ProviderInput{
			Name: name, Type: "openrouter", URL: "https://openrouter.ai/api/v1", APIKey: key,
		})
		Expect(err).NotTo(HaveOccurred())
		return p
	}

	Describe("providers", func() {
		It("seals the key bound to its row and never returns it", func(ctx SpecContext) {
			p := create(ctx, "openrouter", "sk-or-secret")
			Expect(p.HasKey).To(BeTrue())
			raw, _ := json.Marshal(p)
			Expect(string(raw)).NotTo(ContainSubstring("sk-or-secret"))

			var env []byte
			Expect(pool.QueryRow(ctx, `SELECT api_key FROM providers WHERE id = $1`, p.ID).Scan(&env)).To(Succeed())
			Expect(string(env)).NotTo(ContainSubstring("sk-or-secret"))
			var e secretstore.Envelope
			Expect(json.Unmarshal(env, &e)).To(Succeed())
			Expect(e.KEKID).To(Equal("test-1"))
			id := mustUUID(p.ID)
			got, err := catalog.OpenKey(ring, org, id, env)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal("sk-or-secret"))

			By("a copy in another org's row does not open")
			other, otherUser, _ := pgtest.NewOrg(ctx, pool)
			_, err = catalog.OpenKey(ring, other, id, env)
			Expect(err).To(MatchError(secretstore.ErrOpen))
			_ = otherUser
		})

		It("audits create, update and delete without the key", func(ctx SpecContext) {
			p := create(ctx, "openrouter", "sk-1")
			newKey, newURL := "sk-2", "https://openrouter.ai/api/v1/"
			_, err := svc.UpdateProvider(ctx, org, actor, p.ID, catalog.ProviderPatch{APIKey: &newKey, URL: &newURL})
			Expect(err).NotTo(HaveOccurred())
			Expect(svc.DeleteProvider(ctx, org, actor, p.ID)).To(Succeed())

			for _, a := range []string{"provider.create", "provider.update", "provider.delete"} {
				Expect(events(ctx, a)).To(HaveLen(1), a)
			}
			for _, e := range events(ctx, "provider.update") {
				Expect(e.ActorKind).To(Equal("user"))
				Expect(string(e.Summary)).To(ContainSubstring(`"keyChanged": true`))
				Expect(string(e.Summary)).NotTo(ContainSubstring("sk-"))
			}
		})

		It("keeps names unique per org, not across orgs", func(ctx SpecContext) {
			create(ctx, "dup", "")
			_, err := svc.CreateProvider(ctx, org, actor, user, catalog.ProviderInput{Name: "dup", Type: "ollama", URL: "http://x/v1"})
			Expect(err).To(MatchError(catalog.ErrConflict))
			other, otherUser, _ := pgtest.NewOrg(ctx, pool)
			_, err = svc.CreateProvider(ctx, other, audit.User(otherUser), otherUser, catalog.ProviderInput{Name: "dup", Type: "ollama", URL: "http://x/v1"})
			Expect(err).NotTo(HaveOccurred())
		})

		DescribeTable("validates", func(ctx SpecContext, in catalog.ProviderInput) {
			_, err := svc.CreateProvider(ctx, org, actor, user, in)
			var inv *catalog.InvalidError
			Expect(errors.As(err, &inv)).To(BeTrue(), "%v", err)
		},
			Entry("a bad name", catalog.ProviderInput{Name: "Bad Name", Type: "openai", URL: "https://x/v1"}),
			Entry("a URL with credentials", catalog.ProviderInput{Name: "a", Type: "openai", URL: "https://user:pw@x/v1"}),
			Entry("a non-http URL", catalog.ProviderInput{Name: "a", Type: "openai", URL: "ftp://x/v1"}),
			Entry("a bad type", catalog.ProviderInput{Name: "a", Type: "Open AI", URL: "https://x/v1"}),
		)

		It("cannot store a key without key-encryption keys", func(ctx SpecContext) {
			noKeys := catalog.New(pool, nil, nil)
			_, err := noKeys.CreateProvider(ctx, org, actor, user, catalog.ProviderInput{Name: "k", Type: "openai", URL: "https://x/v1", APIKey: "sk"})
			Expect(err).To(MatchError(catalog.ErrNoKeys))
			_, err = noKeys.CreateProvider(ctx, org, actor, user, catalog.ProviderInput{Name: "keyless", Type: "ollama", URL: "http://x/v1"})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("models", func() {
		It("serves a model only with rates, and allows the guard on OpenRouter only", func(ctx SpecContext) {
			p := create(ctx, "openrouter", "")
			m, err := svc.CreateModel(ctx, org, actor, user, catalog.ModelInput{ProviderID: p.ID, Name: "deepseek/flash", Guard: true})
			Expect(err).NotTo(HaveOccurred())
			Expect(m.Rates).To(BeNil())
			rows, err := store.New(pool).ListServedCatalog(ctx, org)
			Expect(err).NotTo(HaveOccurred())
			Expect(rows).To(HaveLen(1))
			Expect(rows[0].InputRate.Valid).To(BeFalse(), "the reconciler does not serve a model without a price")

			m, err = svc.UpdateModel(ctx, org, actor, m.ID, catalog.ModelPatch{Rates: &catalog.Rates{Input: "0.30", Output: "1.20"}})
			Expect(err).NotTo(HaveOccurred())
			Expect(m.Rates).To(Equal(&catalog.Rates{Input: "0.30", Output: "1.20"}))

			ollama, err := svc.CreateProvider(ctx, org, actor, user, catalog.ProviderInput{Name: "ollama", Type: "ollama", URL: "http://x/v1"})
			Expect(err).NotTo(HaveOccurred())
			_, err = svc.CreateModel(ctx, org, actor, user, catalog.ModelInput{ProviderID: ollama.ID, Name: "q", Guard: true})
			var inv *catalog.InvalidError
			Expect(errors.As(err, &inv)).To(BeTrue())
		})

		It("keeps model names unique per org across providers", func(ctx SpecContext) {
			a, b := create(ctx, "a", ""), create(ctx, "b", "")
			_, err := svc.CreateModel(ctx, org, actor, user, catalog.ModelInput{ProviderID: a.ID, Name: "m"})
			Expect(err).NotTo(HaveOccurred())
			_, err = svc.CreateModel(ctx, org, actor, user, catalog.ModelInput{ProviderID: b.ID, Name: "m"})
			Expect(err).To(MatchError(catalog.ErrConflict))
		})

		It("rejects a rate that is not a decimal", func(ctx SpecContext) {
			p := create(ctx, "a", "")
			_, err := svc.CreateModel(ctx, org, actor, user, catalog.ModelInput{ProviderID: p.ID, Name: "m", Rates: &catalog.Rates{Input: "1e3", Output: "1"}})
			var inv *catalog.InvalidError
			Expect(errors.As(err, &inv)).To(BeTrue())
		})

		It("does not find another org's provider or model", func(ctx SpecContext) {
			p := create(ctx, "a", "")
			other, otherUser, _ := pgtest.NewOrg(ctx, pool)
			_, err := svc.GetProvider(ctx, other, p.ID)
			Expect(err).To(MatchError(catalog.ErrNotFound))
			_, err = svc.CreateModel(ctx, other, audit.User(otherUser), otherUser, catalog.ModelInput{ProviderID: p.ID, Name: "m"})
			Expect(err).To(MatchError(catalog.ErrNotFound))
			Expect(svc.DeleteProvider(ctx, other, audit.User(otherUser), p.ID)).To(MatchError(catalog.ErrNotFound))
		})
	})

	Describe("declared by a Secret", func() {
		declared := func(version string, models ...catalog.DeclaredModel) catalog.DeclaredProvider {
			return catalog.DeclaredProvider{
				SecretName: "openrouter-secret", Version: version, Name: "openrouter", Type: "openrouter",
				URL: "https://openrouter.ai/api/v1", APIKey: "sk-declared", Models: models,
			}
		}
		flash := catalog.DeclaredModel{Name: "deepseek/flash", Guard: true, Rates: &catalog.Rates{Input: "0.30", Output: "1.20"}}

		It("is read-only here, audited as the Secret, and applied once per version", func(ctx SpecContext) {
			changed, err := svc.ApplyDeclaredProvider(ctx, org, declared("v1", flash))
			Expect(err).NotTo(HaveOccurred())
			Expect(changed).To(BeTrue())
			changed, err = svc.ApplyDeclaredProvider(ctx, org, declared("v1", flash))
			Expect(err).NotTo(HaveOccurred())
			Expect(changed).To(BeFalse())

			list, err := svc.ListProviders(ctx, org)
			Expect(err).NotTo(HaveOccurred())
			Expect(list).To(HaveLen(1))
			Expect(list[0].Source).To(Equal("secret"))
			Expect(list[0].ManagedBySecret).To(Equal("openrouter-secret"))

			newURL := "https://elsewhere/v1"
			_, err = svc.UpdateProvider(ctx, org, actor, list[0].ID, catalog.ProviderPatch{URL: &newURL})
			Expect(err).To(MatchError(catalog.ErrReadOnly))
			Expect(err.Error()).To(ContainSubstring("openrouter-secret"))
			Expect(svc.DeleteProvider(ctx, org, actor, list[0].ID)).To(MatchError(catalog.ErrReadOnly))
			models, err := svc.ListModels(ctx, org)
			Expect(err).NotTo(HaveOccurred())
			Expect(models).To(HaveLen(1))
			_, err = svc.UpdateModel(ctx, org, actor, models[0].ID, catalog.ModelPatch{ClearRates: true})
			Expect(err).To(MatchError(catalog.ErrReadOnly))
			_, err = svc.CreateModel(ctx, org, actor, user, catalog.ModelInput{ProviderID: list[0].ID, Name: "extra"})
			Expect(err).To(MatchError(catalog.ErrReadOnly))

			ev := events(ctx, "provider.declare")
			Expect(ev).To(HaveLen(1))
			Expect(ev[0].ActorKind).To(Equal("secret"))
			Expect(ev[0].ActorID).To(Equal("openrouter-secret"))
			Expect(string(ev[0].Summary)).NotTo(ContainSubstring("sk-declared"))
		})

		It("wins a name collision: the UI entry is disabled with a reason, and back when the Secret goes", func(ctx SpecContext) {
			ui := create(ctx, "openrouter", "sk-ui")
			uiModel, err := svc.CreateModel(ctx, org, actor, user, catalog.ModelInput{ProviderID: ui.ID, Name: "deepseek/flash", Rates: &catalog.Rates{Input: "9", Output: "9"}})
			Expect(err).NotTo(HaveOccurred())

			_, err = svc.ApplyDeclaredProvider(ctx, org, declared("v1", flash))
			Expect(err).NotTo(HaveOccurred())

			got, err := svc.GetProvider(ctx, org, ui.ID)
			Expect(err).NotTo(HaveOccurred())
			Expect(got.DisabledReason).To(Equal(catalog.ReasonShadowedProvider))
			gm, err := svc.GetModel(ctx, org, uiModel.ID)
			Expect(err).NotTo(HaveOccurred())
			Expect(gm.DisabledReason).NotTo(BeEmpty())
			served, err := store.New(pool).ListServedCatalog(ctx, org)
			Expect(err).NotTo(HaveOccurred())
			Expect(served).To(HaveLen(1))
			Expect(served[0].InputRate.String).To(Equal("0.30"), "only the declared model is served")

			Expect(svc.RemoveDeclared(ctx, org, "openrouter-secret")).To(Succeed())
			got, err = svc.GetProvider(ctx, org, ui.ID)
			Expect(err).NotTo(HaveOccurred())
			Expect(got.DisabledReason).To(BeEmpty())
			gm, err = svc.GetModel(ctx, org, uiModel.ID)
			Expect(err).NotTo(HaveOccurred())
			Expect(gm.DisabledReason).To(BeEmpty())
			list, _ := svc.ListProviders(ctx, org)
			Expect(list).To(HaveLen(1))
			Expect(events(ctx, "provider.undeclare")).To(HaveLen(1))
		})

		It("updates models with the Secret and drops the ones it no longer lists", func(ctx SpecContext) {
			_, err := svc.ApplyDeclaredProvider(ctx, org, declared("v1", flash, catalog.DeclaredModel{Name: "old"}))
			Expect(err).NotTo(HaveOccurred())
			cheaper := flash
			cheaper.Rates = &catalog.Rates{Input: "0.10", Output: "0.20"}
			_, err = svc.ApplyDeclaredProvider(ctx, org, declared("v2", cheaper))
			Expect(err).NotTo(HaveOccurred())
			models, _ := svc.ListModels(ctx, org)
			Expect(models).To(HaveLen(1))
			Expect(models[0].Rates.Input).To(Equal("0.10"))
		})

		It("refuses a provider name another Secret declares, and an invalid model, changing nothing", func(ctx SpecContext) {
			_, err := svc.ApplyDeclaredProvider(ctx, org, declared("v1", flash))
			Expect(err).NotTo(HaveOccurred())
			other := declared("v1")
			other.SecretName = "second"
			_, err = svc.ApplyDeclaredProvider(ctx, org, other)
			Expect(err).To(MatchError(ContainSubstring("already declared by the Secret openrouter-secret")))

			bad := declared("v3", catalog.DeclaredModel{Name: "m", Guard: true})
			bad.Type = "ollama"
			_, err = svc.ApplyDeclaredProvider(ctx, org, bad)
			var inv *catalog.InvalidError
			Expect(errors.As(err, &inv)).To(BeTrue())
			list, _ := svc.ListProviders(ctx, org)
			Expect(list[0].Type).To(Equal("openrouter"))
		})
	})
})

func mustUUID(s string) pgtype.UUID {
	var id pgtype.UUID
	Expect(id.Scan(s)).To(Succeed())
	return id
}
