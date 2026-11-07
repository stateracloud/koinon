// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"net/http"

	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/stateracloud/koinon/internal/db/store"
	agentv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/agent/v1alpha1"
	approvalv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/approval/v1alpha1"
	auditv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/audit/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/audit/v1alpha1/auditv1alpha1connect"
)

func auditClient(c *client) auditv1alpha1connect.AuditServiceClient {
	return auditv1alpha1connect.NewAuditServiceClient(connect.NewClient(connecthttp.NewTransport(c.b.http, c.b.e.srv.URL)))
}

func actionsOf(res *auditv1alpha1.ListResponse) []string {
	var out []string
	for _, e := range res.GetEvents() {
		out = append(out, e.GetAction())
	}
	return out
}

var _ = Describe("AuditService and the audit coverage", func() {
	var (
		e     *env
		org   string
		owner *client
	)
	BeforeEach(func(ctx SpecContext) {
		e = newEnv(ctx, envOptions{})
		org, owner = e.newTeamOrg(ctx)
	})

	It("audits creating and archiving an agent, and approval decisions", func(ctx SpecContext) {
		f := e.newOrgFixture(ctx)
		req := e.pendingApproval(ctx, f)
		admin := e.userIn(ctx, f.org, store.OrgRoleAdmin)
		_, err := admin.approvals.Deny(ctx, &approvalv1alpha1.DenyRequest{OrgId: f.org, ApprovalId: req.ID.String(), Note: "no"})
		Expect(err).NotTo(HaveOccurred())
		_, err = admin.agents.ArchiveAgent(ctx, &agentv1alpha1.ArchiveAgentRequest{OrgId: f.org, AgentId: f.agent.GetId()})
		Expect(err).NotTo(HaveOccurred())

		res, err := auditClient(admin).List(ctx, &auditv1alpha1.ListRequest{OrgId: f.org})
		Expect(err).NotTo(HaveOccurred())
		Expect(actionsOf(res)).To(ContainElements("agent.create", "agent.archive", "approval.deny", "agent.activate"))
		byAction := map[string]*auditv1alpha1.AuditEvent{}
		for _, ev := range res.GetEvents() {
			byAction[ev.GetAction()] = ev
		}
		Expect(byAction["approval.deny"].GetActorKind()).To(Equal("user"))
		Expect(byAction["approval.deny"].GetActorId()).To(Equal(admin.userID))
		Expect(byAction["approval.deny"].GetTargetId()).To(Equal(req.ID.String()))
		Expect(byAction["approval.deny"].GetSummary().AsMap()["after"]).To(HaveKeyWithValue("note", "no"))
		Expect(byAction["agent.archive"].GetActorId()).To(Equal(admin.userID))
		Expect(byAction["agent.archive"].GetTargetId()).To(Equal(f.agent.GetId()))
		Expect(byAction["agent.create"].GetSummary().AsMap()["after"]).To(HaveKey("displayName"))
		Expect(byAction["agent.create"].GetChainSeq()).To(BeNumerically(">", 0))
		Expect(byAction["agent.create"].GetHash()).To(HaveLen(64))
	})

	It("audits logins and logouts in the user's personal org, without the password", func(ctx SpecContext) {
		var personal string
		Expect(e.pool.QueryRow(ctx, `SELECT personal_org_id FROM users WHERE id = $1`, owner.userID).Scan(&personal)).To(Succeed())
		res, err := auditClient(owner).List(ctx, &auditv1alpha1.ListRequest{OrgId: personal, ActionPrefix: "auth."})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetEvents()).To(HaveLen(1))
		login := res.GetEvents()[0]
		Expect(login.GetAction()).To(Equal("auth.login"))
		Expect(login.GetActorId()).To(Equal(owner.userID))
		Expect(login.GetSummary().AsMap()).To(HaveKeyWithValue("after", map[string]any{"method": "password"}))
		Expect(login.GetSummary().String()).NotTo(ContainSubstring(fixturePassword))

		out := owner.b.post(ctx, "/auth/logout", nil)
		Expect(out.StatusCode).To(Equal(http.StatusNoContent))
		Expect(count(ctx, e, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'auth.logout' AND actor_id = $2`, personal, owner.userID)).To(Equal(1))

		By("a second logout without a live session records nothing")
		owner.b.post(ctx, "/auth/logout", nil)
		Expect(count(ctx, e, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'auth.logout'`, personal)).To(Equal(1))
	})

	It("lets org admins and owners read their org's events, and nobody else", func(ctx SpecContext) {
		owner.draftAgent(ctx, org)
		admin := e.userIn(ctx, org, store.OrgRoleAdmin)
		member := e.userIn(ctx, org, store.OrgRoleMember)
		other, otherOwner := e.newTeamOrg(ctx)
		otherOwner.draftAgent(ctx, other)

		for _, c := range []*client{owner, admin} {
			res, err := auditClient(c).List(ctx, &auditv1alpha1.ListRequest{OrgId: org})
			Expect(err).NotTo(HaveOccurred())
			Expect(actionsOf(res)).To(ContainElement("agent.create"))
			for _, ev := range res.GetEvents() {
				Expect(ev.GetOrgId()).To(Equal(org))
			}
		}
		_, err := auditClient(member).List(ctx, &auditv1alpha1.ListRequest{OrgId: org})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))

		By("another org's admin gets NotFound, as for every org-scoped call")
		_, err = auditClient(otherOwner).List(ctx, &auditv1alpha1.ListRequest{OrgId: org})
		Expect(codeOf(err)).To(Equal(connect.CodeNotFound))
		_, err = auditClient(admin).List(ctx, &auditv1alpha1.ListRequest{OrgId: other})
		Expect(codeOf(err)).To(Equal(connect.CodeNotFound))

		By("naming no org needs a platform admin")
		_, err = auditClient(owner).List(ctx, &auditv1alpha1.ListRequest{})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
		_, err = auditClient(admin).List(ctx, &auditv1alpha1.ListRequest{ActionPrefix: "agent."})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))

		By("a bad filter is InvalidArgument")
		_, err = auditClient(admin).List(ctx, &auditv1alpha1.ListRequest{OrgId: org, ActionPrefix: "A B"})
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
		_, err = auditClient(admin).List(ctx, &auditv1alpha1.ListRequest{OrgId: org, PageToken: "###"})
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
	})

	It("lets a platform admin read any org or every org", func(ctx SpecContext) {
		owner.draftAgent(ctx, org)
		other, otherOwner := e.newTeamOrg(ctx)
		otherOwner.draftAgent(ctx, other)
		pa := e.userIn(ctx, org, store.OrgRoleMember)
		_, err := e.pool.Exec(ctx, `INSERT INTO platform_admins (user_id) VALUES ($1)`, pa.userID)
		Expect(err).NotTo(HaveOccurred())

		res, err := auditClient(pa).List(ctx, &auditv1alpha1.ListRequest{OrgId: other, ActionPrefix: "agent.create"})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetEvents()).To(HaveLen(1))
		Expect(res.GetEvents()[0].GetOrgId()).To(Equal(other))

		all, err := auditClient(pa).List(ctx, &auditv1alpha1.ListRequest{ActionPrefix: "agent.create"})
		Expect(err).NotTo(HaveOccurred())
		orgs := map[string]bool{}
		for _, ev := range all.GetEvents() {
			orgs[ev.GetOrgId()] = true
		}
		Expect(orgs).To(HaveKey(org))
		Expect(orgs).To(HaveKey(other))
	})

	It("serves the install chain to platform admins only", func(ctx SpecContext) {
		admin := e.userIn(ctx, org, store.OrgRoleAdmin)
		pa := e.userIn(ctx, org, store.OrgRoleMember)
		_, err := e.pool.Exec(ctx, `INSERT INTO platform_admins (user_id) VALUES ($1)`, pa.userID)
		Expect(err).NotTo(HaveOccurred())
		_, err = e.pool.Exec(ctx, `INSERT INTO install_audit_events (actor_kind, actor_id, action, target_kind, target_id, summary)
			VALUES ('system', 'koinon-test:spec', 'platform_price.set', 'platform_price', 'x', '{}')`)
		Expect(err).NotTo(HaveOccurred())

		res, err := auditClient(pa).List(ctx, &auditv1alpha1.ListRequest{Scope: auditv1alpha1.AuditScope_AUDIT_SCOPE_INSTALL, ActionPrefix: "platform_price."})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetEvents()).NotTo(BeEmpty())
		for _, ev := range res.GetEvents() {
			Expect(ev.GetOrgId()).To(BeEmpty())
			Expect(ev.GetChainSeq()).To(BeNumerically(">", 0))
			Expect(ev.GetHash()).To(HaveLen(64))
		}

		By("an org admin and an owner get PermissionDenied, with or without an org")
		for _, c := range []*client{admin, owner} {
			_, err = auditClient(c).List(ctx, &auditv1alpha1.ListRequest{Scope: auditv1alpha1.AuditScope_AUDIT_SCOPE_INSTALL})
			Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
			_, err = auditClient(c).List(ctx, &auditv1alpha1.ListRequest{OrgId: org, Scope: auditv1alpha1.AuditScope_AUDIT_SCOPE_INSTALL})
			Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
		}

		By("an org id with the install scope is a bad request even for a platform admin")
		_, err = auditClient(pa).List(ctx, &auditv1alpha1.ListRequest{OrgId: org, Scope: auditv1alpha1.AuditScope_AUDIT_SCOPE_INSTALL})
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))

		By("the org scope never shows install events")
		all, err := auditClient(pa).List(ctx, &auditv1alpha1.ListRequest{Scope: auditv1alpha1.AuditScope_AUDIT_SCOPE_ORG, ActionPrefix: "platform_price."})
		Expect(err).NotTo(HaveOccurred())
		Expect(all.GetEvents()).To(BeEmpty())
	})

	It("filters and pages", func(ctx SpecContext) {
		admin := e.userIn(ctx, org, store.OrgRoleAdmin)
		for range 4 {
			owner.draftAgent(ctx, org)
		}
		ac := auditClient(admin)
		first, err := ac.List(ctx, &auditv1alpha1.ListRequest{OrgId: org, ActionPrefix: "agent.create", PageSize: 3})
		Expect(err).NotTo(HaveOccurred())
		Expect(first.GetEvents()).To(HaveLen(3))
		Expect(first.GetNextPageToken()).NotTo(BeEmpty())
		second, err := ac.List(ctx, &auditv1alpha1.ListRequest{OrgId: org, ActionPrefix: "agent.create", PageSize: 3, PageToken: first.GetNextPageToken()})
		Expect(err).NotTo(HaveOccurred())
		Expect(second.GetEvents()).To(HaveLen(1))
		Expect(second.GetNextPageToken()).To(BeEmpty())
		Expect(second.GetEvents()[0].GetId()).NotTo(Equal(first.GetEvents()[2].GetId()))

		byActor, err := ac.List(ctx, &auditv1alpha1.ListRequest{OrgId: org, ActorKind: "user", ActorId: owner.userID, ActionPrefix: "agent."})
		Expect(err).NotTo(HaveOccurred())
		Expect(byActor.GetEvents()).To(HaveLen(4))
		none, err := ac.List(ctx, &auditv1alpha1.ListRequest{OrgId: org, ActorKind: "user", ActorId: admin.userID, ActionPrefix: "agent."})
		Expect(err).NotTo(HaveOccurred())
		Expect(none.GetEvents()).To(BeEmpty())

		future, err := ac.List(ctx, &auditv1alpha1.ListRequest{OrgId: org, Since: timestamppb.New(first.GetEvents()[0].GetCreatedAt().AsTime().Add(1 << 40))})
		Expect(err).NotTo(HaveOccurred())
		Expect(future.GetEvents()).To(BeEmpty())
	})
})
