// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"context"

	connect "connectrpc.com/connect/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/stateracloud/koinon/internal/db/store"
	agentv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/agent/v1alpha1"
	templatev1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/template/v1alpha1"
)

// Org templates (#1271, ADR 0126 section 5): "save as template" writes an org-owned template; the
// shipped templates stay platform data.
var _ = Describe("org templates", func() {
	var (
		e    *env
		a, b orgFixture
	)
	scopes := func(res *templatev1alpha1.ListTemplatesResponse) map[string]templatev1alpha1.TemplateScope {
		out := map[string]templatev1alpha1.TemplateScope{}
		for _, t := range res.GetTemplates() {
			out[t.GetSlug()] = t.GetScope()
		}
		return out
	}
	BeforeEach(func(ctx SpecContext) {
		e = newEnv(ctx, envOptions{})
		a, b = e.newOrgFixture(ctx), e.newOrgFixture(ctx)
	})

	save := func(ctx context.Context, c *client, f orgFixture, name string) (*templatev1alpha1.Template, error) {
		res, err := c.templates.CreateTemplateFromAgent(ctx, &templatev1alpha1.CreateTemplateFromAgentRequest{
			OrgId: f.org, AgentId: f.agent.GetId(), DisplayName: name, Description: "saved in a test",
		})
		return res.GetTemplate(), err
	}

	It("saves an agent as an org template that the org lists with its scope", func(ctx SpecContext) {
		t, err := save(ctx, a.owner, a, "My Repo Helper")
		Expect(err).NotTo(HaveOccurred())
		Expect(t.GetScope()).To(Equal(templatev1alpha1.TemplateScope_TEMPLATE_SCOPE_ORG))
		Expect(t.GetOrgId()).To(Equal(a.org))
		Expect(t.GetSlug()).To(Equal("my-repo-helper"))
		Expect(t.GetInstructions()).NotTo(BeEmpty())

		res, err := a.owner.templates.ListTemplates(ctx, &templatev1alpha1.ListTemplatesRequest{OrgId: a.org})
		Expect(err).NotTo(HaveOccurred())
		got := scopes(res)
		Expect(got).To(HaveKeyWithValue("my-repo-helper", templatev1alpha1.TemplateScope_TEMPLATE_SCOPE_ORG))
		Expect(got).To(HaveKeyWithValue("repo-helper", templatev1alpha1.TemplateScope_TEMPLATE_SCOPE_PLATFORM))
	})

	It("keeps a template of one org from every other org", func(ctx SpecContext) {
		t, err := save(ctx, a.owner, a, "Secret Recipe")
		Expect(err).NotTo(HaveOccurred())

		// B lists its own org: platform templates only.
		res, err := b.owner.templates.ListTemplates(ctx, &templatev1alpha1.ListTemplatesRequest{OrgId: b.org})
		Expect(err).NotTo(HaveOccurred())
		Expect(scopes(res)).NotTo(HaveKey("secret-recipe"))
		Expect(protojson.Format(res)).NotTo(ContainSubstring(t.GetId()))
		// No org named: platform templates only, whoever the caller is.
		res, err = a.owner.templates.ListTemplates(ctx, &templatev1alpha1.ListTemplatesRequest{})
		Expect(err).NotTo(HaveOccurred())
		Expect(scopes(res)).NotTo(HaveKey("secret-recipe"))
		// B cannot list A's templates.
		_, err = b.owner.templates.ListTemplates(ctx, &templatev1alpha1.ListTemplatesRequest{OrgId: a.org})
		Expect(codeOf(err)).To(BeElementOf(connect.CodeNotFound, connect.CodePermissionDenied))
		// B cannot make an agent from A's template, and learns nothing about it.
		_, err = b.owner.agents.CreateAgentFromTemplate(ctx, &agentv1alpha1.CreateAgentFromTemplateRequest{
			OrgId: b.org, TemplateId: t.GetId(), DisplayName: "stolen",
		})
		Expect(codeOf(err)).To(Equal(connect.CodeNotFound))
		// The same slug is free in B.
		_, err = save(ctx, b.owner, b, "Secret Recipe")
		Expect(err).NotTo(HaveOccurred())
	})

	It("lets the org make an agent from its template", func(ctx SpecContext) {
		t, err := save(ctx, a.owner, a, "Again")
		Expect(err).NotTo(HaveOccurred())
		res, err := a.owner.agents.CreateAgentFromTemplate(ctx, &agentv1alpha1.CreateAgentFromTemplateRequest{
			OrgId: a.org, TemplateId: t.GetId(), DisplayName: "second", Grants: e.repoGrant(ctx, a.org, "acme/widgets"),
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetAgent().GetTemplateId()).To(Equal(t.GetId()))
	})

	It("refuses a viewer, an unknown agent, a bad slug and a slug the org has", func(ctx SpecContext) {
		viewer := e.userIn(ctx, a.org, store.OrgRoleViewer)
		_, err := save(ctx, viewer, a, "Nope")
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))

		member := e.userIn(ctx, a.org, store.OrgRoleMember)
		_, err = save(ctx, member, a, "Member Made")
		Expect(err).NotTo(HaveOccurred())

		_, err = a.owner.templates.CreateTemplateFromAgent(ctx, &templatev1alpha1.CreateTemplateFromAgentRequest{
			OrgId: a.org, AgentId: b.agent.GetId(), DisplayName: "Other org agent",
		})
		Expect(codeOf(err)).To(Equal(connect.CodeNotFound))

		_, err = a.owner.templates.CreateTemplateFromAgent(ctx, &templatev1alpha1.CreateTemplateFromAgentRequest{
			OrgId: a.org, AgentId: a.agent.GetId(), DisplayName: "Bad", Slug: "Not A Slug",
		})
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))

		_, err = save(ctx, a.owner, a, "Member Made")
		Expect(codeOf(err)).To(Equal(connect.CodeAlreadyExists))
	})

	It("refuses the slug of a platform template", func(ctx SpecContext) {
		_, err := save(ctx, a.owner, a, "Repo Helper")
		Expect(codeOf(err)).To(Equal(connect.CodeAlreadyExists))
	})

	It("keeps the shipped templates platform data", func(ctx SpecContext) {
		res, err := a.owner.templates.ListTemplates(ctx, &templatev1alpha1.ListTemplatesRequest{})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetTemplates()).NotTo(BeEmpty())
		for _, t := range res.GetTemplates() {
			Expect(t.GetScope()).To(Equal(templatev1alpha1.TemplateScope_TEMPLATE_SCOPE_PLATFORM), t.GetSlug())
			Expect(t.GetOrgId()).To(BeEmpty())
		}
		var owned int
		Expect(e.pool.QueryRow(ctx, `SELECT count(*) FROM templates WHERE org_id IS NULL`).Scan(&owned)).To(Succeed())
		Expect(owned).To(BeNumerically(">=", len(res.GetTemplates())))
	})

	It("leaves the agent's bindings out of the template", func(ctx SpecContext) {
		t, err := save(ctx, a.owner, a, "No Bindings")
		Expect(err).NotTo(HaveOccurred())
		for _, g := range t.GetGrants() {
			Expect(g.GetConnectionId()).To(BeEmpty())
			Expect(g.GetRepositories()).To(BeEmpty())
		}
	})
})
