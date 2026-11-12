// SPDX-License-Identifier: Apache-2.0

package reconciler_test

import (
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution/openshell"
	"github.com/stateracloud/koinon/internal/execution/openshell/fakegateway"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/reconciler"
	"github.com/stateracloud/koinon/pricing"
	"github.com/stateracloud/koinon/templates"
)

const toolsURL = "http://koinon-api-tools.koinon.svc:8081/mcp"

var _ = Describe("ToolGrants", func() {
	servers := map[string]string{"github": toolsURL}
	grant := func(preset templates.Preset, scopes ...string) agentconfig.Config {
		return agentconfig.Config{Grants: []templates.Grant{{Connector: "github", Scopes: scopes, Preset: preset}}}
	}

	It("allows the tools of the grant's scopes", func() {
		Expect(reconciler.ToolGrants(grant(templates.PresetStandard, "repo.read", "pull_request.create", "issue.comment"), servers)).To(Equal(
			[]gateway.ToolGrant{{Target: "github", Tools: []string{"comment_on_issue", "create_pull_request", "get_approval_status"}}}))
		Expect(reconciler.ToolGrants(grant(templates.PresetStandard, "repo.read", "pull_request.create"), servers)).To(Equal(
			[]gateway.ToolGrant{{Target: "github", Tools: []string{"create_pull_request", "get_approval_status"}}}))
	})

	It("allows nothing the preset denies, or without a tool server", func() {
		Expect(reconciler.ToolGrants(grant(templates.PresetReadOnly, "repo.read", "pull_request.create"), servers)).To(BeEmpty())
		Expect(reconciler.ToolGrants(grant(templates.PresetStandard, "repo.read"), servers)).To(BeEmpty())
		Expect(reconciler.ToolGrants(grant(templates.PresetStandard, "pull_request.create"), nil)).To(BeEmpty())
	})
})

var _ = Describe("ToolGrants for a Gitea grant", func() {
	It("offers the code-host tools when the tool server is registered for gitea", func() {
		cfg := agentconfig.Config{Grants: []templates.Grant{{Connector: "gitea", Scopes: []string{"repo.read", "pull_request.create", "issue.comment"}, Preset: templates.PresetStandard}}}
		Expect(reconciler.ToolGrants(cfg, map[string]string{"gitea": toolsURL})).To(Equal(
			[]gateway.ToolGrant{{Target: "gitea", Tools: []string{"comment_on_issue", "create_pull_request", "get_approval_status"}}}))
	})
})

var _ = Describe("ToolGrants for Koinon's own tools", func() {
	It("offers ask_user and cancel_task to every agent when the koinon tool server is configured, grant or not", func() {
		servers := map[string]string{gateway.KoinonTarget: "http://koinon-api-tools.koinon.svc:8081/mcp-koinon"}
		ask := gateway.ToolGrant{Target: "koinon", Tools: []string{"ask_user", "cancel_task"}}
		Expect(reconciler.ToolGrants(agentconfig.Config{}, servers)).To(Equal([]gateway.ToolGrant{ask}))

		both := map[string]string{"github": toolsURL, gateway.KoinonTarget: servers[gateway.KoinonTarget]}
		cfg := agentconfig.Config{Grants: []templates.Grant{{Connector: "github", Scopes: []string{"repo.read", "pull_request.create"}, Preset: templates.PresetStandard}}}
		Expect(reconciler.ToolGrants(cfg, both)).To(Equal([]gateway.ToolGrant{
			ask,
			{Target: "github", Tools: []string{"create_pull_request", "get_approval_status"}},
		}))
	})
	It("offers nothing without the server", func() {
		Expect(reconciler.ToolGrants(agentconfig.Config{}, map[string]string{"github": toolsURL})).To(BeEmpty())
	})
})

var _ = Describe("Reconciler with the GitHub tool server", func() {
	It("routes the tool server and allows each active agent its granted tools", func(ctx SpecContext) {
		osg := fakegateway.New()
		conn, err := osg.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = conn.Close(); osg.Stop() })
		mg := &fakeModelGateway{}
		cfg := reconciler.Config{
			Prices:      prices.NewResolver(pricing.Embedded()),
			Gateway:     reconciler.Endpoint{Host: "koinon-gateway.koinon.svc", Port: 8080},
			ToolServers: map[string]string{"github": toolsURL},
			Harnesses: map[string]reconciler.Harness{
				harness.OpenCode: {Image: image, Binaries: reconciler.DefaultBinaries[harness.OpenCode]},
			},
			Logger: slog.New(slog.DiscardHandler),
		}
		r, err := reconciler.New(pool, openshell.New(conn), mg, reconciler.KubeSecrets{Client: fake.NewClientset(), Namespace: namespace}, cfg)
		Expect(err).NotTo(HaveOccurred())

		org, user := newOrg(ctx)
		withGrant := newAgentWithConfig(ctx, org, user, `{"instructions":"help","grants":[`+
			`{"connector":"github","scopes":["repo.read","pull_request.create","issue.comment"],"preset":"standard"}]}`)
		readOnly := newAgentWithConfig(ctx, org, user, `{"instructions":"help","grants":[`+
			`{"connector":"github","scopes":["repo.read","pull_request.create"],"preset":"read-only"}]}`)
		Expect(r.ReconcileAll(ctx)).To(Succeed())

		state := mg.last()
		Expect(state.MCP).To(Equal([]gateway.MCPTarget{{Name: "github", URL: toolsURL}}))
		ref := func(a store.Agent) gateway.AgentRef {
			o, err := q.GetAgent(ctx, store.GetAgentParams{OrgID: org, ID: a.ID})
			Expect(err).NotTo(HaveOccurred())
			return gateway.AgentRef{Org: state.Keys[0].Agent.Org, Name: o.ID.String()}
		}
		Expect(state.Tools).To(Equal(map[gateway.AgentRef][]gateway.ToolGrant{
			ref(withGrant): {{Target: "github", Tools: []string{"comment_on_issue", "create_pull_request", "get_approval_status"}}},
		}))
		Expect(state.Tools).NotTo(HaveKey(ref(readOnly)))

		By("the sandbox may reach agentgateway's MCP route")
		ws := orgWorkspace(ctx, org)
		var paths []string
		for _, rule := range osg.Profile(ws, "koinon-agentgateway-opencode").GetEndpoints()[0].GetRules() {
			paths = append(paths, rule.GetAllow().GetMethod()+" "+rule.GetAllow().GetPath())
		}
		Expect(paths).To(ConsistOf("POST /v1/chat/completions", "POST /v1/messages", "POST /mcp", "GET /mcp", "DELETE /mcp"),
			"both model routes and the MCP routes, and nothing else (/v1/messages/count_tokens stays closed)")
	})
})

func orgWorkspace(ctx SpecContext, org pgtype.UUID) string {
	row, err := q.GetOrgWorkspace(ctx, org)
	Expect(err).NotTo(HaveOccurred())
	return row.Workspace
}
