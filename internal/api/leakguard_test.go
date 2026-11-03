// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	connect "connectrpc.com/connect/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	leakguardv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/leakguard/v1alpha1"
	mcpserverv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/mcpserver/v1alpha1"
)

// The leak guard's data from the UI (ADR 0084, #350): McpServerService.SetServerClass and
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
		Expect(plain.GetServer().GetDataClass()).To(Equal([]string{"untrusted_input", "private_data", "outbound"}))

		fetch, err := owner.mcps.CreateServer(ctx, &mcpserverv1alpha1.CreateServerRequest{
			OrgId: org, Name: "fetcher", Url: "https://mcp.example.com/mcp",
			Class: &mcpserverv1alpha1.ServerClass{Kind: "fetch", DataClass: []string{"outbound", "untrusted_input"}},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fetch.GetServer().GetKind()).To(Equal("fetch"))
		Expect(fetch.GetServer().GetDataClass()).To(Equal([]string{"untrusted_input", "outbound"}))

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

	It("starts at 24 characters and an empty host list, and manages both", func(ctx SpecContext) {
		res, err := owner.leakguard.GetLeakGuard(ctx, &leakguardv1alpha1.GetLeakGuardRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetLeakGuard().GetMatchThreshold()).To(BeEquivalentTo(24))
		Expect(res.GetLeakGuard().GetDefaultMatchThreshold()).To(BeEquivalentTo(24))
		Expect(res.GetLeakGuard().GetTrustedHosts()).To(BeEmpty())

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
		_, err = member.mcps.SetServerClass(ctx, &mcpserverv1alpha1.SetServerClassRequest{
			OrgId: org, ServerId: srv.GetServer().GetId(), Class: &mcpserverv1alpha1.ServerClass{Kind: "fetch"},
		})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))

		res, err := owner.leakguard.GetLeakGuard(ctx, &leakguardv1alpha1.GetLeakGuardRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetLeakGuard().GetTrustedHosts()).To(BeEmpty())
	})
})
