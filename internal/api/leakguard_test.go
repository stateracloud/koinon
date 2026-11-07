// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"encoding/json"

	connect "connectrpc.com/connect/v2"
	guuid "github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	leakguardv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/leakguard/v1alpha1"
	mcpserverv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/mcpserver/v1alpha1"
	"github.com/stateracloud/koinon/internal/mcpservers"
)

// The leak guard's data from the UI (ADR 0084, old issue 350): McpServerService.SetServerClass and
// LeakGuardService call the same services as the admin API.
var _ = Describe("the leak guard settings in the UI API", func() {
	var (
		e     *env
		org   string
		owner *client
	)
	BeforeEach(func(ctx SpecContext) {
		e = newEnv(ctx, envOptions{})
		org, owner = e.newTeamOrg(ctx)
	})

	audited := func(ctx SpecContext, action string) int {
		GinkgoHelper()
		var n int
		Expect(e.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = $2`, org, action).Scan(&n)).To(Succeed())
		return n
	}

	It("classifies a server, from create and later, and audits the change", func(ctx SpecContext) {
		plain, err := owner.mcps.CreateServer(ctx, &mcpserverv1alpha1.CreateServerRequest{OrgId: org, Name: "plain", Url: "https://mcp.example.com/mcp"})
		Expect(err).NotTo(HaveOccurred())
		Expect(plain.GetServer().GetKind()).To(Equal("other"))
		Expect(plain.GetServer().GetDataClass()).To(Equal([]string{"untrusted_input", "private_data"}))

		fetch, err := owner.mcps.CreateServer(ctx, &mcpserverv1alpha1.CreateServerRequest{
			OrgId: org, Name: "fetcher", Url: "https://mcp.example.com/mcp",
			Class: &mcpserverv1alpha1.ServerClass{Kind: "fetch", DataClass: []string{"untrusted_input"}},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fetch.GetServer().GetKind()).To(Equal("fetch"))
		Expect(fetch.GetServer().GetDataClass()).To(Equal([]string{"untrusted_input"}))

		set, err := owner.mcps.SetServerClass(ctx, &mcpserverv1alpha1.SetServerClassRequest{
			OrgId: org, ServerId: plain.GetServer().GetId(), Class: &mcpserverv1alpha1.ServerClass{Kind: "search"},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(set.GetServer().GetKind()).To(Equal("search"))
		Expect(set.GetServer().GetDataClass()).To(BeEmpty(), "a class with no legs carries none")
		Expect(audited(ctx, "mcp_server.data_class.set")).To(Equal(1))

		list, err := owner.mcps.ListServers(ctx, &mcpserverv1alpha1.ListServersRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		kinds := map[string]string{}
		for _, s := range list.GetServers() {
			kinds[s.GetName()] = s.GetKind()
		}
		Expect(kinds).To(Equal(map[string]string{"plain": "search", "fetcher": "fetch"}))
	})

	It("refuses an unknown kind or leg", func(ctx SpecContext) {
		srv, err := owner.mcps.CreateServer(ctx, &mcpserverv1alpha1.CreateServerRequest{OrgId: org, Name: "plain", Url: "https://mcp.example.com/mcp"})
		Expect(err).NotTo(HaveOccurred())
		for _, c := range []*mcpserverv1alpha1.ServerClass{{Kind: "browser"}, {Kind: "other", DataClass: []string{"secrets"}}, nil} {
			_, err = owner.mcps.SetServerClass(ctx, &mcpserverv1alpha1.SetServerClassRequest{OrgId: org, ServerId: srv.GetServer().GetId(), Class: c})
			Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
		}
		Expect(audited(ctx, "mcp_server.data_class.set")).To(BeZero())
	})

	It("declares the sink class of a tool, audits it, and puts it back when the server's URL changes", func(ctx SpecContext) {
		srv, err := owner.mcps.CreateServer(ctx, &mcpserverv1alpha1.CreateServerRequest{OrgId: org, Name: "forge", Url: "https://mcp.example.com/mcp"})
		Expect(err).NotTo(HaveOccurred())
		sid := srv.GetServer().GetId()
		var pgid pgtype.UUID
		Expect(pgid.Scan(sid)).To(Succeed())
		var orgID pgtype.UUID
		Expect(orgID.Scan(org)).To(Succeed())
		_, err = mcpservers.New(e.pool, nil, nil).SyncTools(ctx, orgID, pgid, []mcpservers.ListedTool{
			{Name: "create_issue", Description: "Create an issue", InputSchema: json.RawMessage(`{"type":"object"}`)},
		})
		Expect(err).NotTo(HaveOccurred())

		tools, err := owner.mcps.ListTools(ctx, &mcpserverv1alpha1.ListToolsRequest{OrgId: org, ServerId: sid})
		Expect(err).NotTo(HaveOccurred())
		Expect(tools.GetTools()).To(HaveLen(1))
		Expect(tools.GetTools()[0].GetSink().GetSinkClass()).To(Equal("external-high"))
		Expect(tools.GetTools()[0].GetSink().GetDerivation()).To(Equal("fixed"))

		hash := tools.GetTools()[0].GetContentHash()
		yes := true
		set, err := owner.mcps.SetToolSink(ctx, &mcpserverv1alpha1.SetToolSinkRequest{
			OrgId: org, ServerId: sid, Tool: "create_issue", ContentHash: hash,
			Sink: &mcpserverv1alpha1.ToolSink{SinkClass: "external-high", Derivation: "code_host", AddressArgs: []string{"owner", "repo"}, ResultUntrustedInput: &yes},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(set.GetTool().GetSink().GetDerivation()).To(Equal("code_host"))
		Expect(set.GetTool().GetSink().GetAddressArgs()).To(Equal([]string{"owner", "repo"}))
		Expect(set.GetTool().GetSink().GetResultUntrustedInput()).To(BeTrue())
		Expect(set.GetTool().GetSink().ResultPrivateData).To(BeNil())
		Expect(audited(ctx, "mcp_tool.declaration.set")).To(Equal(1))

		for _, bad := range []*mcpserverv1alpha1.ToolSink{
			nil,
			{SinkClass: "everywhere", Derivation: "fixed"},
			{SinkClass: "none", Derivation: "oracle"},
			{SinkClass: "external-high", Derivation: "fetch"},
		} {
			_, err = owner.mcps.SetToolSink(ctx, &mcpserverv1alpha1.SetToolSinkRequest{OrgId: org, ServerId: sid, Tool: "create_issue", ContentHash: hash, Sink: bad})
			Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument), "%v", bad)
		}
		_, err = owner.mcps.SetToolSink(ctx, &mcpserverv1alpha1.SetToolSinkRequest{
			OrgId: org, ServerId: sid, Tool: "no_such", ContentHash: hash,
			Sink: &mcpserverv1alpha1.ToolSink{SinkClass: "none", Derivation: "fixed"},
		})
		Expect(codeOf(err)).To(Equal(connect.CodeNotFound))

		By("a call without the reviewed hash is refused")
		_, err = owner.mcps.SetToolSink(ctx, &mcpserverv1alpha1.SetToolSinkRequest{OrgId: org, ServerId: sid, Tool: "create_issue", Sink: &mcpserverv1alpha1.ToolSink{SinkClass: "none", Derivation: "fixed"}})
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))

		_, err = owner.mcps.UpdateServer(ctx, &mcpserverv1alpha1.UpdateServerRequest{OrgId: org, ServerId: sid, Url: new("https://other.example.com/mcp")})
		Expect(err).NotTo(HaveOccurred())
		tools, err = owner.mcps.ListTools(ctx, &mcpserverv1alpha1.ListToolsRequest{OrgId: org, ServerId: sid})
		Expect(err).NotTo(HaveOccurred())
		Expect(tools.GetTools()[0].GetSink().GetDerivation()).To(Equal("fixed"))
		Expect(tools.GetTools()[0].GetSink().GetAddressArgs()).To(BeEmpty())
		Expect(audited(ctx, "mcp_tool.declaration.reset")).To(Equal(1))
	})

	It("answers Aborted and keeps the tool as it was when the server republished it after the review", func(ctx SpecContext) {
		srv, err := owner.mcps.CreateServer(ctx, &mcpserverv1alpha1.CreateServerRequest{OrgId: org, Name: "forge", Url: "https://mcp.example.com/mcp"})
		Expect(err).NotTo(HaveOccurred())
		sid := srv.GetServer().GetId()
		var pgid, orgID pgtype.UUID
		Expect(pgid.Scan(sid)).To(Succeed())
		Expect(orgID.Scan(org)).To(Succeed())
		sync := func(description string) {
			GinkgoHelper()
			_, err := mcpservers.New(e.pool, nil, nil).SyncTools(ctx, orgID, pgid, []mcpservers.ListedTool{
				{Name: "create_issue", Description: description, InputSchema: json.RawMessage(`{"type":"object"}`)},
			})
			Expect(err).NotTo(HaveOccurred())
		}
		sync("Create an issue")
		tools, err := owner.mcps.ListTools(ctx, &mcpserverv1alpha1.ListToolsRequest{OrgId: org, ServerId: sid})
		Expect(err).NotTo(HaveOccurred())
		reviewed := tools.GetTools()[0].GetContentHash()
		none := &mcpserverv1alpha1.ToolSink{SinkClass: "none", Derivation: "fixed"}

		_, err = owner.mcps.SetToolSink(ctx, &mcpserverv1alpha1.SetToolSinkRequest{OrgId: org, ServerId: sid, Tool: "create_issue", Sink: none})
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument), "a call without the reviewed hash")

		sync("Create an issue and email everyone")
		_, err = owner.mcps.SetToolSink(ctx, &mcpserverv1alpha1.SetToolSinkRequest{OrgId: org, ServerId: sid, Tool: "create_issue", ContentHash: reviewed, Sink: none})
		Expect(codeOf(err)).To(Equal(connect.CodeAborted))
		Expect(audited(ctx, "mcp_tool.declaration.set")).To(BeZero())
		tools, err = owner.mcps.ListTools(ctx, &mcpserverv1alpha1.ListToolsRequest{OrgId: org, ServerId: sid})
		Expect(err).NotTo(HaveOccurred())
		Expect(tools.GetTools()[0].GetSink().GetSinkClass()).To(Equal("external-high"))
		Expect(tools.GetTools()[0].GetContentHash()).NotTo(Equal(reviewed))
	})

	It("links a server to one of the org's connections and refuses another", func(ctx SpecContext) {
		srv, err := owner.mcps.CreateServer(ctx, &mcpserverv1alpha1.CreateServerRequest{OrgId: org, Name: "forge", Url: "https://mcp.example.com/mcp"})
		Expect(err).NotTo(HaveOccurred())
		Expect(srv.GetServer().GetCodeHostConnectionId()).To(BeEmpty())
		var orgID pgtype.UUID
		Expect(orgID.Scan(org)).To(Succeed())
		conn := pgtype.UUID{Bytes: guuid.New(), Valid: true}
		_, err = store.New(e.pool).CreateConnection(ctx, store.CreateConnectionParams{
			// A GitHub Enterprise Server at the server's host, so the server may act through it (#117).
			OrgID: orgID, ID: conn, Connector: "github", DisplayName: "gh", Config: []byte(`{"baseUrl":"https://mcp.example.com"}`),
			SecretName: pgtype.Text{String: "conn-" + conn.String(), Valid: true},
		})
		Expect(err).NotTo(HaveOccurred())
		gitea := pgtype.UUID{Bytes: guuid.New(), Valid: true}
		_, err = store.New(e.pool).CreateConnection(ctx, store.CreateConnectionParams{
			OrgID: orgID, ID: gitea, Connector: "gitea", DisplayName: "forge", Config: []byte(`{"baseUrl":"https://git.example.com"}`),
			SecretName: pgtype.Text{String: "conn-" + gitea.String(), Valid: true},
		})
		Expect(err).NotTo(HaveOccurred())

		_, err = owner.mcps.SetServerClass(ctx, &mcpserverv1alpha1.SetServerClassRequest{
			OrgId: org, ServerId: srv.GetServer().GetId(), Class: &mcpserverv1alpha1.ServerClass{Kind: "other", CodeHostConnectionId: gitea.String()},
		})
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument), "a server at another host than the connection's (#117)")

		set, err := owner.mcps.SetServerClass(ctx, &mcpserverv1alpha1.SetServerClassRequest{
			OrgId: org, ServerId: srv.GetServer().GetId(), Class: &mcpserverv1alpha1.ServerClass{Kind: "other", DataClass: []string{"untrusted_input"}, CodeHostConnectionId: conn.String()},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(set.GetServer().GetCodeHostConnectionId()).To(Equal(conn.String()))

		_, err = owner.mcps.SetServerClass(ctx, &mcpserverv1alpha1.SetServerClassRequest{
			OrgId: org, ServerId: srv.GetServer().GetId(), Class: &mcpserverv1alpha1.ServerClass{Kind: "other", CodeHostConnectionId: guuid.NewString()},
		})
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
	})

	It("starts at 24 characters and an empty host list, and manages both", func(ctx SpecContext) {
		res, err := owner.leakguard.GetLeakGuard(ctx, &leakguardv1alpha1.GetLeakGuardRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetLeakGuard().GetMatchThreshold()).To(BeEquivalentTo(24))
		Expect(res.GetLeakGuard().GetDefaultMatchThreshold()).To(BeEquivalentTo(24))
		Expect(res.GetLeakGuard().GetTrustedHosts()).To(BeEmpty())
		Expect(res.GetLeakGuard().GetCodeHostScope().GetRepositories()).To(BeZero())
		Expect(res.GetLeakGuard().GetCodeHostScope().GetVisibilityMaxAgeSeconds()).To(BeEquivalentTo(3600))

		set, err := owner.leakguard.SetMatchThreshold(ctx, &leakguardv1alpha1.SetMatchThresholdRequest{OrgId: org, MatchThreshold: 48})
		Expect(err).NotTo(HaveOccurred())
		Expect(set.GetLeakGuard().GetMatchThreshold()).To(BeEquivalentTo(48))
		for _, bad := range []int32{1, 11, 257, -3} {
			_, err = owner.leakguard.SetMatchThreshold(ctx, &leakguardv1alpha1.SetMatchThresholdRequest{OrgId: org, MatchThreshold: bad})
			Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument), "%d", bad)
		}
		set, err = owner.leakguard.SetMatchThreshold(ctx, &leakguardv1alpha1.SetMatchThresholdRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		Expect(set.GetLeakGuard().GetMatchThreshold()).To(BeEquivalentTo(24), "0 returns the default")

		add, err := owner.leakguard.AddTrustedHost(ctx, &leakguardv1alpha1.AddTrustedHostRequest{OrgId: org, Host: "Docs.Example.com"})
		Expect(err).NotTo(HaveOccurred())
		Expect(add.GetLeakGuard().GetTrustedHosts()).To(Equal([]string{"docs.example.com"}))
		_, err = owner.leakguard.AddTrustedHost(ctx, &leakguardv1alpha1.AddTrustedHostRequest{OrgId: org, Host: "https://x.example.com/a"})
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))

		rm, err := owner.leakguard.RemoveTrustedHost(ctx, &leakguardv1alpha1.RemoveTrustedHostRequest{OrgId: org, Host: "docs.example.com"})
		Expect(err).NotTo(HaveOccurred())
		Expect(rm.GetLeakGuard().GetTrustedHosts()).To(BeEmpty())
		_, err = owner.leakguard.RemoveTrustedHost(ctx, &leakguardv1alpha1.RemoveTrustedHostRequest{OrgId: org, Host: "docs.example.com"})
		Expect(codeOf(err)).To(Equal(connect.CodeNotFound))

		Expect(audited(ctx, "org.leak_guard.threshold.set")).To(Equal(2))
		Expect(audited(ctx, "org.fetch_trusted_host.add")).To(Equal(1))
		Expect(audited(ctx, "org.fetch_trusted_host.remove")).To(Equal(1))
	})

	It("is for admins and owners only", func(ctx SpecContext) {
		member := e.userIn(ctx, org, store.OrgRoleMember)
		srv, err := owner.mcps.CreateServer(ctx, &mcpserverv1alpha1.CreateServerRequest{OrgId: org, Name: "plain", Url: "https://mcp.example.com/mcp"})
		Expect(err).NotTo(HaveOccurred())

		_, err = member.leakguard.GetLeakGuard(ctx, &leakguardv1alpha1.GetLeakGuardRequest{OrgId: org})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
		_, err = member.leakguard.SetMatchThreshold(ctx, &leakguardv1alpha1.SetMatchThresholdRequest{OrgId: org, MatchThreshold: 30})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
		_, err = member.leakguard.AddTrustedHost(ctx, &leakguardv1alpha1.AddTrustedHostRequest{OrgId: org, Host: "docs.example.com"})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
		_, err = member.leakguard.RemoveTrustedHost(ctx, &leakguardv1alpha1.RemoveTrustedHostRequest{OrgId: org, Host: "docs.example.com"})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
		_, err = member.mcps.SetToolSink(ctx, &mcpserverv1alpha1.SetToolSinkRequest{
			OrgId: org, ServerId: srv.GetServer().GetId(), Tool: "x", ContentHash: "h",
			Sink: &mcpserverv1alpha1.ToolSink{SinkClass: "none", Derivation: "fixed"},
		})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
		_, err = member.mcps.SetServerClass(ctx, &mcpserverv1alpha1.SetServerClassRequest{
			OrgId: org, ServerId: srv.GetServer().GetId(), Class: &mcpserverv1alpha1.ServerClass{Kind: "fetch"},
		})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))

		res, err := owner.leakguard.GetLeakGuard(ctx, &leakguardv1alpha1.GetLeakGuardRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetLeakGuard().GetTrustedHosts()).To(BeEmpty())
	})
})
