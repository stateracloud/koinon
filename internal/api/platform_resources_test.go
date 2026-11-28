// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"context"

	"connectrpc.com/connect/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	mcpserverv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/mcpserver/v1alpha1"
	orgv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/org/v1alpha1"
	platformv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/platform/v1alpha1"
	providerv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/provider/v1alpha1"
	templatev1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/template/v1alpha1"
	"github.com/stateracloud/koinon/internal/secretstore/secretstoretest"
)

// resourceOrg is a team org with one provider, one org template and one MCP server.
type resourceOrg struct {
	org, provider, template, server string
	owner                           *client
}

func (e *env) newResourceOrg(ctx context.Context) resourceOrg {
	var r resourceOrg
	r.org, r.owner = e.newTeamOrg(ctx)
	prov, err := r.owner.providers.CreateProvider(ctx, &providerv1alpha1.CreateProviderRequest{
		OrgId: r.org, Name: "router", Type: "openrouter", Url: "https://openrouter.ai/api/v1", ApiKey: "koinon_test_not-a-real-token",
	})
	Expect(err).NotTo(HaveOccurred())
	r.provider = prov.GetProvider().GetId()
	agent := r.owner.activeAgent(ctx, r.org)
	slug := "tpl-" + r.org
	tpl, err := r.owner.templates.CreateTemplateFromAgent(ctx, &templatev1alpha1.CreateTemplateFromAgentRequest{
		OrgId: r.org, AgentId: agent.GetId(), DisplayName: slug, Slug: slug,
	})
	Expect(err).NotTo(HaveOccurred())
	r.template = tpl.GetTemplate().GetId()
	srv, err := r.owner.mcps.CreateServer(ctx, &mcpserverv1alpha1.CreateServerRequest{
		OrgId: r.org, Name: "search", Url: "https://mcp.example.com/mcp", CredentialValue: "koinon_test_not-a-real-token",
	})
	Expect(err).NotTo(HaveOccurred())
	r.server = srv.GetServer().GetId()
	return r
}

var _ = Describe("the platform console lists of org resources (PlatformResourceService)", func() {
	var (
		e     *env
		a, b  resourceOrg
		admin *client
	)
	BeforeEach(func(ctx SpecContext) {
		e = newEnv(ctx, envOptions{keys: secretstoretest.Keyring()})
		a = e.newResourceOrg(ctx)
		b = e.newResourceOrg(ctx)
		_, admin = e.newTeamOrg(ctx)
		makePlatformAdmin(ctx, e, admin)
	})

	It("refuses every call to every org role that is not a platform admin, also for the caller's own org", func(ctx SpecContext) {
		for _, role := range []store.OrgRole{store.OrgRoleOwner, store.OrgRoleAdmin, store.OrgRoleMember, store.OrgRoleViewer} {
			c := e.userIn(ctx, a.org, role)
			for _, org := range []string{"", a.org} {
				_, err := c.platformResources.ListProviders(ctx, &platformv1alpha1.ListProvidersRequest{OrgId: org})
				Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied), "ListProviders as %s", role)
				reason, _ := reasonOf(err)
				Expect(reason).To(Equal("PLATFORM_ADMIN_REQUIRED"))
				_, err = c.platformResources.ListTemplates(ctx, &platformv1alpha1.ListTemplatesRequest{OrgId: org})
				Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied), "ListTemplates as %s", role)
				_, err = c.platformResources.ListMcpServers(ctx, &platformv1alpha1.ListMcpServersRequest{OrgId: org})
				Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied), "ListMcpServers as %s", role)
			}
		}
	})

	It("lists the rows of one org with its org, for an org the platform admin is not a member of", func(ctx SpecContext) {
		provs, err := admin.platformResources.ListProviders(ctx, &platformv1alpha1.ListProvidersRequest{OrgId: a.org})
		Expect(err).NotTo(HaveOccurred())
		Expect(provs.GetNextPageToken()).To(BeEmpty())
		Expect(provs.GetProviders()).To(HaveLen(1))
		p := provs.GetProviders()[0]
		Expect(p.GetProvider().GetId()).To(Equal(a.provider))
		Expect(p.GetProvider().GetHasKey()).To(BeTrue())
		Expect(p.GetOrg().GetOrgId()).To(Equal(a.org))
		Expect(p.GetOrg().GetSlug()).NotTo(BeEmpty())
		Expect(p.GetOrg().GetKind()).To(Equal(orgv1alpha1.OrgKind_ORG_KIND_TEAM))

		tpls, err := admin.platformResources.ListTemplates(ctx, &platformv1alpha1.ListTemplatesRequest{OrgId: a.org})
		Expect(err).NotTo(HaveOccurred())
		Expect(tpls.GetTemplates()).To(HaveLen(1), "no platform templates with org_id")
		Expect(tpls.GetTemplates()[0].GetTemplate().GetId()).To(Equal(a.template))
		Expect(tpls.GetTemplates()[0].GetOrg().GetOrgId()).To(Equal(a.org))

		srvs, err := admin.platformResources.ListMcpServers(ctx, &platformv1alpha1.ListMcpServersRequest{OrgId: b.org})
		Expect(err).NotTo(HaveOccurred())
		Expect(srvs.GetServers()).To(HaveLen(1))
		Expect(srvs.GetServers()[0].GetServer().GetId()).To(Equal(b.server))
		Expect(srvs.GetServers()[0].GetServer().GetHasCredential()).To(BeTrue())
		Expect(srvs.GetServers()[0].GetOrg().GetOrgId()).To(Equal(b.org))
	})

	It("pages through every org and puts the platform templates on the first page only", func(ctx SpecContext) {
		platform, err := admin.templates.ListTemplates(ctx, &templatev1alpha1.ListTemplatesRequest{})
		Expect(err).NotTo(HaveOccurred())
		var platformIDs []string
		for _, t := range platform.GetTemplates() {
			platformIDs = append(platformIDs, t.GetId())
		}

		providerOrg := map[string]string{}
		templateOrg := map[string]string{}
		serverOrg := map[string]string{}
		var withoutOrg []string
		token := ""
		for page := 0; ; page++ {
			Expect(page).To(BeNumerically("<", 1000), "the list did not end")
			provs, err := admin.platformResources.ListProviders(ctx, &platformv1alpha1.ListProvidersRequest{PageSize: 1, PageToken: token})
			Expect(err).NotTo(HaveOccurred())
			for _, p := range provs.GetProviders() {
				providerOrg[p.GetProvider().GetId()] = p.GetOrg().GetOrgId()
			}
			tpls, err := admin.platformResources.ListTemplates(ctx, &platformv1alpha1.ListTemplatesRequest{PageSize: 1, PageToken: token})
			Expect(err).NotTo(HaveOccurred())
			for _, t := range tpls.GetTemplates() {
				if t.GetOrg() == nil {
					Expect(page).To(BeZero(), "a platform template after the first page")
					withoutOrg = append(withoutOrg, t.GetTemplate().GetId())
					continue
				}
				templateOrg[t.GetTemplate().GetId()] = t.GetOrg().GetOrgId()
			}
			srvs, err := admin.platformResources.ListMcpServers(ctx, &platformv1alpha1.ListMcpServersRequest{PageSize: 1, PageToken: token})
			Expect(err).NotTo(HaveOccurred())
			for _, s := range srvs.GetServers() {
				serverOrg[s.GetServer().GetId()] = s.GetOrg().GetOrgId()
			}
			token = provs.GetNextPageToken()
			if token == "" {
				break
			}
		}
		Expect(withoutOrg).To(ConsistOf(platformIDs))
		for _, r := range []resourceOrg{a, b} {
			Expect(providerOrg).To(HaveKeyWithValue(r.provider, r.org))
			Expect(templateOrg).To(HaveKeyWithValue(r.template, r.org))
			Expect(serverOrg).To(HaveKeyWithValue(r.server, r.org))
		}
	})

	It("leaves out personal orgs unless asked, and deleted orgs always", func(ctx SpecContext) {
		// a becomes a personal org and b is deleted. The SQL stands in for the flows that make them, so the spec
		// keeps the rows of newResourceOrg.
		_, err := e.pool.Exec(ctx, `UPDATE orgs SET kind = 'personal' WHERE id = $1`, a.org)
		Expect(err).NotTo(HaveOccurred())
		_, err = e.pool.Exec(ctx, `UPDATE orgs SET deleted_at = now() WHERE id = $1`, b.org)
		Expect(err).NotTo(HaveOccurred())
		providerOrgs := func(req *platformv1alpha1.ListProvidersRequest) []string {
			req.PageSize = 100
			res, err := admin.platformResources.ListProviders(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.GetNextPageToken()).To(BeEmpty())
			var orgs []string
			for _, p := range res.GetProviders() {
				orgs = append(orgs, p.GetOrg().GetOrgId())
			}
			return orgs
		}

		all := providerOrgs(&platformv1alpha1.ListProvidersRequest{})
		Expect(all).NotTo(ContainElement(a.org), "personal org without include_personal")
		Expect(all).NotTo(ContainElement(b.org), "deleted org")
		withPersonal := providerOrgs(&platformv1alpha1.ListProvidersRequest{IncludePersonal: true})
		Expect(withPersonal).To(ContainElement(a.org))
		Expect(withPersonal).NotTo(ContainElement(b.org))

		res, err := admin.platformResources.ListProviders(ctx, &platformv1alpha1.ListProvidersRequest{OrgId: a.org})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetProviders()).To(HaveLen(1))
		Expect(res.GetProviders()[0].GetOrg().GetKind()).To(Equal(orgv1alpha1.OrgKind_ORG_KIND_PERSONAL))
		_, err = admin.platformResources.ListProviders(ctx, &platformv1alpha1.ListProvidersRequest{OrgId: b.org})
		Expect(codeOf(err)).To(Equal(connect.CodeNotFound))
	})

	It("refuses a bad page token and an org that does not exist", func(ctx SpecContext) {
		_, err := admin.platformResources.ListProviders(ctx, &platformv1alpha1.ListProvidersRequest{PageToken: "!"})
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
		_, err = admin.platformResources.ListMcpServers(ctx, &platformv1alpha1.ListMcpServersRequest{OrgId: "00000000-0000-0000-0000-000000000001"})
		Expect(codeOf(err)).To(Equal(connect.CodeNotFound))
		_, err = admin.platformResources.ListTemplates(ctx, &platformv1alpha1.ListTemplatesRequest{OrgId: "not-a-uuid"})
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
	})
})
