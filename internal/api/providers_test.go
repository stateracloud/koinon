// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	connect "connectrpc.com/connect/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	authv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/auth/v1alpha1"
	providerv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/provider/v1alpha1"
	tokenv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/token/v1alpha1"
)

var _ = Describe("ProviderService", func() {
	var (
		e     *env
		org   string
		owner *client
	)
	BeforeEach(func(ctx SpecContext) {
		e = newEnv(ctx, envOptions{})
		org, owner = e.newTeamOrg(ctx)
	})

	rates := &providerv1alpha1.Rates{Input: "0.30", Output: "1.20"}
	create := func(ctx SpecContext, c *client, name, typ, url, key string) (*providerv1alpha1.CreateProviderResponse, error) {
		return c.providers.CreateProvider(ctx, &providerv1alpha1.CreateProviderRequest{OrgId: org, Name: name, Type: typ, Url: url, ApiKey: key})
	}

	It("warns when a new model has a cache_read price but no prompt-cache mode (ADR 0068)", func(ctx SpecContext) {
		prov, err := create(ctx, owner, "warned", "openrouter", "https://openrouter.ai/api/v1", "")
		Expect(err).NotTo(HaveOccurred())
		pid := prov.GetProvider().GetId()
		_, err = e.pool.Exec(ctx, `INSERT INTO price_overrides (org_id, name, entry, valid_from, created_by)
			VALUES ($1, 'gem', '{"provider":"openrouter","match":{"glob":"google/*"},"validFrom":"2020-01-01","prices":{"input":"1","output":"2","cacheRead":"0.1"},"tokenizer":"bytes","ratio":"3.0","maxOutput":10}', now() - interval '1 hour', 'test')`, org)
		Expect(err).NotTo(HaveOccurred())
		mod, err := owner.providers.CreateModel(ctx, &providerv1alpha1.CreateModelRequest{OrgId: org, ProviderId: pid, Name: "google/gemini-x"})
		Expect(err).NotTo(HaveOccurred())
		Expect(mod.GetWarnings()).To(ConsistOf(And(ContainSubstring("google/gemini-x"), ContainSubstring("without caching"))))
		other, err := owner.providers.CreateModel(ctx, &providerv1alpha1.CreateModelRequest{OrgId: org, ProviderId: pid, Name: "anthropic/claude-haiku-5.5"})
		Expect(err).NotTo(HaveOccurred())
		Expect(other.GetWarnings()).To(BeEmpty())
	})

	It("is for admins and owners only, on all eight RPCs", func(ctx SpecContext) {
		prov, err := create(ctx, owner, "existing", "openrouter", "https://openrouter.ai/api/v1", "")
		Expect(err).NotTo(HaveOccurred())
		pid := prov.GetProvider().GetId()
		mod, err := owner.providers.CreateModel(ctx, &providerv1alpha1.CreateModelRequest{OrgId: org, ProviderId: pid, Name: "m"})
		Expect(err).NotTo(HaveOccurred())
		mid := mod.GetModel().GetId()
		calls := map[string]func(c *client) error{
			"ListProviders": func(c *client) error {
				_, err := c.providers.ListProviders(ctx, &providerv1alpha1.ListProvidersRequest{OrgId: org})
				return err
			},
			"CreateProvider": func(c *client) error {
				_, err := create(ctx, c, "x", "openai", "https://api.example.com/v1", "")
				return err
			},
			"UpdateProvider": func(c *client) error {
				_, err := c.providers.UpdateProvider(ctx, &providerv1alpha1.UpdateProviderRequest{OrgId: org, ProviderId: pid, Url: new("https://example.com/v1")})
				return err
			},
			"DeleteProvider": func(c *client) error {
				_, err := c.providers.DeleteProvider(ctx, &providerv1alpha1.DeleteProviderRequest{OrgId: org, ProviderId: pid})
				return err
			},
			"ListModels": func(c *client) error {
				_, err := c.providers.ListModels(ctx, &providerv1alpha1.ListModelsRequest{OrgId: org})
				return err
			},
			"CreateModel": func(c *client) error {
				_, err := c.providers.CreateModel(ctx, &providerv1alpha1.CreateModelRequest{OrgId: org, ProviderId: pid, Name: "n"})
				return err
			},
			"UpdateModel": func(c *client) error {
				_, err := c.providers.UpdateModel(ctx, &providerv1alpha1.UpdateModelRequest{OrgId: org, ModelId: mid, Guard: new(false)})
				return err
			},
			"DeleteModel": func(c *client) error {
				_, err := c.providers.DeleteModel(ctx, &providerv1alpha1.DeleteModelRequest{OrgId: org, ModelId: mid})
				return err
			},
		}
		Expect(calls).To(HaveLen(8))
		for _, role := range []store.OrgRole{store.OrgRoleMember, store.OrgRoleViewer} {
			c := e.userIn(ctx, org, role)
			for name, call := range calls {
				Expect(codeOf(call(c))).To(Equal(connect.CodePermissionDenied), string(role)+" "+name)
			}
		}
		admin := e.userIn(ctx, org, store.OrgRoleAdmin)
		for _, name := range []string{"ListProviders", "ListModels", "CreateProvider", "CreateModel", "UpdateProvider", "UpdateModel", "DeleteModel", "DeleteProvider"} {
			Expect(calls[name](admin)).To(Succeed(), name)
		}
	})

	It("does not reveal an org to someone outside it", func(ctx SpecContext) {
		_, other := e.newTeamOrg(ctx)
		_, err := other.providers.ListProviders(ctx, &providerv1alpha1.ListProvidersRequest{OrgId: org})
		Expect(codeOf(err)).To(Equal(connect.CodeNotFound))
	})

	It("creates, updates and deletes a provider; the key is write-only and the changes are audited", func(ctx SpecContext) {
		res, err := create(ctx, owner, "openrouter", "openrouter", "https://openrouter.ai/api/v1", "sk-or-very-secret")
		Expect(err).NotTo(HaveOccurred())
		p := res.GetProvider()
		Expect(p.GetHasKey()).To(BeTrue())
		Expect(p.GetSource()).To(Equal("ui"))
		Expect(mustJSON(res)).NotTo(ContainSubstring("sk-or-very-secret"))

		list, err := owner.providers.ListProviders(ctx, &providerv1alpha1.ListProvidersRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		Expect(list.GetProviders()).To(HaveLen(1))
		Expect(mustJSON(list)).NotTo(ContainSubstring("sk-or-very-secret"))

		upd, err := owner.providers.UpdateProvider(ctx, &providerv1alpha1.UpdateProviderRequest{
			OrgId: org, ProviderId: p.GetId(), ApiKey: new("sk-or-replacement"),
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(upd.GetProvider().GetHasKey()).To(BeTrue())
		upd, err = owner.providers.UpdateProvider(ctx, &providerv1alpha1.UpdateProviderRequest{OrgId: org, ProviderId: p.GetId(), ApiKey: new("")})
		Expect(err).NotTo(HaveOccurred())
		Expect(upd.GetProvider().GetHasKey()).To(BeFalse())

		_, err = owner.providers.DeleteProvider(ctx, &providerv1alpha1.DeleteProviderRequest{OrgId: org, ProviderId: p.GetId()})
		Expect(err).NotTo(HaveOccurred())
		_, err = owner.providers.DeleteProvider(ctx, &providerv1alpha1.DeleteProviderRequest{OrgId: org, ProviderId: p.GetId()})
		Expect(codeOf(err)).To(Equal(connect.CodeNotFound))

		var actions []string
		rows, err := e.pool.Query(ctx, `SELECT action FROM audit_events WHERE org_id = $1 AND action LIKE 'provider.%' ORDER BY id`, org)
		Expect(err).NotTo(HaveOccurred())
		defer rows.Close()
		for rows.Next() {
			var a string
			Expect(rows.Scan(&a)).To(Succeed())
			actions = append(actions, a)
		}
		Expect(actions).To(Equal([]string{"provider.create", "provider.update", "provider.update", "provider.delete"}))
		var leaked int
		Expect(e.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND summary::text LIKE '%sk-or-%'`, org).Scan(&leaked)).To(Succeed())
		Expect(leaked).To(BeZero())
	})

	It("sets, defaults, overrides and validates a model's format (ADR 0069)", func(ctx SpecContext) {
		or, err := create(ctx, owner, "openrouter", "openrouter", "https://openrouter.ai/api/v1", "k")
		Expect(err).NotTo(HaveOccurred())
		oa, err := create(ctx, owner, "openai", "openai", "https://api.openai.com/v1", "k")
		Expect(err).NotTo(HaveOccurred())
		ids := func(fs []*providerv1alpha1.ModelFormat) (out []string) {
			for _, f := range fs {
				Expect(f.GetLabel()).NotTo(BeEmpty())
				out = append(out, f.GetId())
			}
			return out
		}
		Expect(ids(or.GetProvider().GetFormats())).To(Equal([]string{"openai", "anthropic"}))
		Expect(ids(oa.GetProvider().GetFormats())).To(Equal([]string{"openai"}))
		Expect(or.GetProvider().GetFormats()[1].GetLabel()).To(Equal("Anthropic Messages"))
		orID, oaID := or.GetProvider().GetId(), oa.GetProvider().GetId()

		// The default follows the table: OpenRouter's Claude models speak Messages (old issue 242).
		m, err := owner.providers.CreateModel(ctx, &providerv1alpha1.CreateModelRequest{OrgId: org, ProviderId: orID, Name: "anthropic/claude-haiku-5.5"})
		Expect(err).NotTo(HaveOccurred())
		Expect(m.GetModel().GetFormat()).To(Equal("anthropic"))
		Expect(m.GetModel().GetFormatOverridden()).To(BeFalse())

		fm, err := owner.providers.ModelFormats(ctx, &providerv1alpha1.ModelFormatsRequest{OrgId: org, ProviderId: orID, Name: "claude-haiku"})
		Expect(err).NotTo(HaveOccurred())
		Expect(fm.GetDefaultFormat()).To(Equal("anthropic"), "OpenRouter claude-* follows the family preference")
		ti, err := owner.providers.ProviderTypeInfo(ctx, &providerv1alpha1.ProviderTypeInfoRequest{OrgId: org, Type: "anthropic"})
		Expect(err).NotTo(HaveOccurred())
		Expect(ti.GetTakesWorkspaceId()).To(BeTrue())
		Expect(ti.GetFormats()).To(HaveLen(1))
		ti, err = owner.providers.ProviderTypeInfo(ctx, &providerv1alpha1.ProviderTypeInfoRequest{OrgId: org, Type: "openrouter"})
		Expect(err).NotTo(HaveOccurred())
		Expect(ti.GetTakesWorkspaceId()).To(BeFalse())
		Expect(ti.GetFormats()).To(HaveLen(2))

		// An override within what the provider offers is stored and marked.
		upd, err := owner.providers.UpdateModel(ctx, &providerv1alpha1.UpdateModelRequest{OrgId: org, ModelId: m.GetModel().GetId(), Format: new("openai")})
		Expect(err).NotTo(HaveOccurred())
		Expect(upd.GetModel().GetFormat()).To(Equal("openai"))
		Expect(upd.GetModel().GetFormatOverridden()).To(BeTrue())
		// An empty format goes back to the default.
		upd, err = owner.providers.UpdateModel(ctx, &providerv1alpha1.UpdateModelRequest{OrgId: org, ModelId: m.GetModel().GetId(), Format: new("")})
		Expect(err).NotTo(HaveOccurred())
		Expect(upd.GetModel().GetFormat()).To(Equal("anthropic"))
		Expect(upd.GetModel().GetFormatOverridden()).To(BeFalse())

		// Not offered, and unknown, are refused.
		_, err = owner.providers.CreateModel(ctx, &providerv1alpha1.CreateModelRequest{OrgId: org, ProviderId: oaID, Name: "gpt", Format: "anthropic"})
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
		Expect(err).To(MatchError(ContainSubstring("does not offer")))
		_, err = owner.providers.CreateModel(ctx, &providerv1alpha1.CreateModelRequest{OrgId: org, ProviderId: orID, Name: "gem", Format: "gemini"})
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
		Expect(err).To(MatchError(ContainSubstring("unknown model format")))
	})

	It("takes an Anthropic provider's workspace ID, for that type only", func(ctx SpecContext) {
		_, err := owner.providers.CreateProvider(ctx, &providerv1alpha1.CreateProviderRequest{
			OrgId: org, Name: "oa", Type: "openai", Url: "https://api.openai.com/v1", AnthropicWorkspaceId: "wrkspc_01Abc",
		})
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
		_, err = owner.providers.CreateProvider(ctx, &providerv1alpha1.CreateProviderRequest{
			OrgId: org, Name: "bad", Type: "anthropic", Url: "https://api.anthropic.com/v1", AnthropicWorkspaceId: "workspace-1",
		})
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
		res, err := owner.providers.CreateProvider(ctx, &providerv1alpha1.CreateProviderRequest{
			OrgId: org, Name: "anthropic", Type: "anthropic", Url: "https://api.anthropic.com/v1", AnthropicWorkspaceId: "wrkspc_01Abc",
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetProvider().GetAnthropicWorkspaceId()).To(Equal("wrkspc_01Abc"))
		Expect(res.GetProvider().GetFormats()).To(HaveLen(1))
		m, err := owner.providers.CreateModel(ctx, &providerv1alpha1.CreateModelRequest{OrgId: org, ProviderId: res.GetProvider().GetId(), Name: "claude-haiku-4-5"})
		Expect(err).NotTo(HaveOccurred())
		Expect(m.GetModel().GetFormat()).To(Equal("anthropic"))
		// Changing the type away needs the workspace ID gone first.
		_, err = owner.providers.UpdateProvider(ctx, &providerv1alpha1.UpdateProviderRequest{OrgId: org, ProviderId: res.GetProvider().GetId(), Type: new("openai")})
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
		upd, err := owner.providers.UpdateProvider(ctx, &providerv1alpha1.UpdateProviderRequest{OrgId: org, ProviderId: res.GetProvider().GetId(), AnthropicWorkspaceId: new("")})
		Expect(err).NotTo(HaveOccurred())
		Expect(upd.GetProvider().GetAnthropicWorkspaceId()).To(BeEmpty())
		// With it gone the type may change, and the model follows the new type's default.
		upd, err = owner.providers.UpdateProvider(ctx, &providerv1alpha1.UpdateProviderRequest{OrgId: org, ProviderId: res.GetProvider().GetId(), Type: new("openai")})
		Expect(err).NotTo(HaveOccurred())
		Expect(upd.GetProvider().GetFormats()).To(HaveLen(1))
		Expect(upd.GetProvider().GetFormats()[0].GetId()).To(Equal("openai"))
		list, err := owner.providers.ListModels(ctx, &providerv1alpha1.ListModelsRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		Expect(list.GetModels()[0].GetFormat()).To(Equal("openai"))
	})

	It("refuses upstream addresses the policy denies, and says why", func(ctx SpecContext) {
		for _, u := range []string{
			"http://127.0.0.1:11434/v1", "http://169.254.169.254/latest", "http://10.0.0.5/v1", "http://localhost/v1",
			"http://ollama/v1", "http://koinon-api.koinon.svc/v1", "http://2130706433/v1", "http://[::ffff:7f00:1]/v1",
		} {
			_, err := create(ctx, owner, "bad", "openai", u, "")
			Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument), u)
			Expect(err.Error()).NotTo(BeEmpty())
		}
		_, err := create(ctx, owner, "bad", "openai", "https://user:pw@api.example.com/v1", "")
		Expect(err).To(MatchError(ContainSubstring("credentials")))
		_, err = create(ctx, owner, "Bad Name", "openai", "https://api.example.com/v1", "")
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
		_, err = owner.providers.UpdateProvider(ctx, &providerv1alpha1.UpdateProviderRequest{OrgId: org, ProviderId: "nope", Url: new("http://127.0.0.1/v1")})
		Expect(codeOf(err)).To(Equal(connect.CodeNotFound))
	})

	It("refuses a second provider of the same name", func(ctx SpecContext) {
		_, err := create(ctx, owner, "dup", "openai", "https://api.example.com/v1", "")
		Expect(err).NotTo(HaveOccurred())
		_, err = create(ctx, owner, "dup", "openai", "https://api.example.com/v1", "")
		Expect(codeOf(err)).To(Equal(connect.CodeAlreadyExists))
	})

	It("manages models: guard only on OpenRouter, rates refused, org-scoped lists", func(ctx SpecContext) {
		or, err := create(ctx, owner, "openrouter", "openrouter", "https://openrouter.ai/api/v1", "k")
		Expect(err).NotTo(HaveOccurred())
		oa, err := create(ctx, owner, "openai", "openai", "https://api.openai.com/v1", "k")
		Expect(err).NotTo(HaveOccurred())

		m, err := owner.providers.CreateModel(ctx, &providerv1alpha1.CreateModelRequest{
			OrgId: org, ProviderId: or.GetProvider().GetId(), Name: "deepseek/deepseek-v4.1-flash", Guard: true,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(m.GetModel().GetGuard()).To(BeTrue())

		_, err = owner.providers.CreateModel(ctx, &providerv1alpha1.CreateModelRequest{
			OrgId: org, ProviderId: oa.GetProvider().GetId(), Name: "gpt", Guard: true,
		})
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
		Expect(err).To(MatchError(ContainSubstring("routing guard")))
		_, err = owner.providers.CreateModel(ctx, &providerv1alpha1.CreateModelRequest{
			OrgId: org, ProviderId: oa.GetProvider().GetId(), Name: "gpt", Rates: rates, //nolint:staticcheck // deprecated, refused
		})
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
		Expect(err).To(MatchError(ContainSubstring("prices")))

		upd, err := owner.providers.UpdateModel(ctx, &providerv1alpha1.UpdateModelRequest{
			OrgId: org, ModelId: m.GetModel().GetId(), Guard: new(false),
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(upd.GetModel().GetGuard()).To(BeFalse())
		_, err = owner.providers.UpdateModel(ctx, &providerv1alpha1.UpdateModelRequest{OrgId: org, ModelId: m.GetModel().GetId(), Rates: rates}) //nolint:staticcheck // deprecated, refused
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
		_, err = owner.providers.UpdateModel(ctx, &providerv1alpha1.UpdateModelRequest{OrgId: org, ModelId: m.GetModel().GetId(), ClearRates: true}) //nolint:staticcheck // deprecated, refused
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))

		list, err := owner.providers.ListModels(ctx, &providerv1alpha1.ListModelsRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		Expect(list.GetModels()).To(HaveLen(1))

		By("another org sees none of it")
		otherOrg, other := e.newTeamOrg(ctx)
		none, err := other.providers.ListModels(ctx, &providerv1alpha1.ListModelsRequest{OrgId: otherOrg})
		Expect(err).NotTo(HaveOccurred())
		Expect(none.GetModels()).To(BeEmpty())
		_, err = other.providers.DeleteModel(ctx, &providerv1alpha1.DeleteModelRequest{OrgId: otherOrg, ModelId: m.GetModel().GetId()})
		Expect(codeOf(err)).To(Equal(connect.CodeNotFound))

		_, err = owner.providers.DeleteModel(ctx, &providerv1alpha1.DeleteModelRequest{OrgId: org, ModelId: m.GetModel().GetId()})
		Expect(err).NotTo(HaveOccurred())
	})

	It("keeps providers and models declared by a Secret read-only, and lets a removed one go", func(ctx SpecContext) {
		var pid, mid string
		Expect(e.pool.QueryRow(ctx, `INSERT INTO providers (org_id, id, name, type, url, source, managed_by_secret, declared_version)
			VALUES ($1, gen_random_uuid(), 'declared', 'openrouter', 'https://openrouter.ai/api/v1', 'secret', 'my-secret', 'v1') RETURNING id`, org).Scan(&pid)).To(Succeed())
		Expect(e.pool.QueryRow(ctx, `INSERT INTO models (org_id, id, provider_id, name, guard, source, managed_by_secret)
			VALUES ($1, gen_random_uuid(), $2, 'm', false, 'secret', 'my-secret') RETURNING id`, org, pid).Scan(&mid)).To(Succeed())

		list, err := owner.providers.ListProviders(ctx, &providerv1alpha1.ListProvidersRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		Expect(list.GetProviders()[0].GetSource()).To(Equal("secret"))
		Expect(list.GetProviders()[0].GetManagedBySecret()).To(Equal("my-secret"))

		_, err = owner.providers.UpdateProvider(ctx, &providerv1alpha1.UpdateProviderRequest{OrgId: org, ProviderId: pid, Url: new("https://example.com/v1")})
		Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition))
		Expect(err).To(MatchError(ContainSubstring("my-secret")))
		_, err = owner.providers.DeleteProvider(ctx, &providerv1alpha1.DeleteProviderRequest{OrgId: org, ProviderId: pid})
		Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition))
		_, err = owner.providers.UpdateModel(ctx, &providerv1alpha1.UpdateModelRequest{OrgId: org, ModelId: mid, Guard: new(false)})
		Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition))
		_, err = owner.providers.DeleteModel(ctx, &providerv1alpha1.DeleteModelRequest{OrgId: org, ModelId: mid})
		Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition))
		_, err = owner.providers.CreateModel(ctx, &providerv1alpha1.CreateModelRequest{OrgId: org, ProviderId: pid, Name: "n"})
		Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition))

		By("once its Secret is gone it is marked removed and may be deleted")
		_, err = e.pool.Exec(ctx, `UPDATE providers SET removed_at = now(), disabled_reason = 'gone' WHERE org_id = $1 AND id = $2`, org, pid)
		Expect(err).NotTo(HaveOccurred())
		list, err = owner.providers.ListProviders(ctx, &providerv1alpha1.ListProvidersRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		Expect(list.GetProviders()[0].GetRemoved()).To(BeTrue())
		Expect(list.GetProviders()[0].GetDisabledReason()).To(Equal("gone"))
		_, err = owner.providers.DeleteProvider(ctx, &providerv1alpha1.DeleteProviderRequest{OrgId: org, ProviderId: pid})
		Expect(err).NotTo(HaveOccurred())
	})

	It("tells tokens apart by who made them, and the token it came from", func(ctx SpecContext) {
		root, err := owner.tokens.CreateToken(ctx, &tokenv1alpha1.CreateTokenRequest{OrgId: org, Name: "root", Role: authv1alpha1.OrgRole_ORG_ROLE_ADMIN})
		Expect(err).NotTo(HaveOccurred())
		Expect(root.GetToken().GetCreatedBy()).To(Equal(owner.userID))
		Expect(root.GetToken().GetCreatedByName()).NotTo(BeEmpty())
		Expect(root.GetToken().GetParentTokenId()).To(BeEmpty())
		list, err := owner.tokens.ListTokens(ctx, &tokenv1alpha1.ListTokensRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		Expect(list.GetTokens()[0].GetCreatedByName()).To(Equal(root.GetToken().GetCreatedByName()))
	})
})
