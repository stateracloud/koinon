// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"context"
	"slices"

	"connectrpc.com/connect/v2"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	authv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/auth/v1alpha1"
	budgetv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/budget/v1alpha1"
	orgv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/org/v1alpha1"
	platformv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/platform/v1alpha1"
)

// makePlatformAdmin grants c the platform admin role for the rest of the spec. The principal is
// read per request, so the session sees it at once.
func makePlatformAdmin(ctx context.Context, e *env, c *client) {
	_, err := e.pool.Exec(ctx, `INSERT INTO platform_admins (user_id) VALUES ($1)`, c.userID)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func(ctx SpecContext) {
		_, _ = e.pool.Exec(ctx, `DELETE FROM platform_admins WHERE user_id = $1`, c.userID)
	})
}

// listAllOrgs pages through ListOrgs with page size n and returns every org.
func listAllOrgs(ctx context.Context, c *client, includePersonal bool, n int32) []*platformv1alpha1.OrgSummary {
	var out []*platformv1alpha1.OrgSummary
	token := ""
	for range 1000 {
		res, err := c.platform.ListOrgs(ctx, &platformv1alpha1.ListOrgsRequest{IncludePersonal: includePersonal, PageSize: n, PageToken: token})
		Expect(err).NotTo(HaveOccurred())
		Expect(len(res.GetOrgs())).To(BeNumerically("<=", n))
		out = append(out, res.GetOrgs()...)
		token = res.GetNextPageToken()
		if token == "" {
			return out
		}
	}
	Fail("ListOrgs did not end")
	return nil
}

func orgByID(orgs []*platformv1alpha1.OrgSummary, id string) *platformv1alpha1.OrgSummary {
	i := slices.IndexFunc(orgs, func(o *platformv1alpha1.OrgSummary) bool { return o.GetOrgId() == id })
	if i < 0 {
		return nil
	}
	return orgs[i]
}

var _ = Describe("the platform console org reads (PlatformService)", func() {
	var (
		e            *env
		own, other   string
		admin, owner *client
	)
	BeforeEach(func(ctx SpecContext) {
		e = newEnv(ctx, envOptions{})
		own, admin = e.newTeamOrg(ctx)
		makePlatformAdmin(ctx, e, admin)
		other, owner = e.newTeamOrg(ctx)
	})

	It("refuses ListOrgs and GetOrg to every org role that is not a platform admin", func(ctx SpecContext) {
		for _, role := range []store.OrgRole{store.OrgRoleOwner, store.OrgRoleAdmin, store.OrgRoleMember, store.OrgRoleViewer} {
			c := e.userIn(ctx, other, role)
			_, err := c.platform.ListOrgs(ctx, &platformv1alpha1.ListOrgsRequest{})
			Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied), "ListOrgs as %s", role)
			reason, _ := reasonOf(err)
			Expect(reason).To(Equal("PLATFORM_ADMIN_REQUIRED"))
			// Its own org too: GetOrg is the console's read, not an org read.
			for _, org := range []string{other, own} {
				_, err = c.platform.GetOrg(ctx, &platformv1alpha1.GetOrgRequest{OrgId: org})
				Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied), "GetOrg as %s", role)
			}
		}
		// A refusal does not tell whether an org exists.
		_, err := owner.platform.GetOrg(ctx, &platformv1alpha1.GetOrgRequest{OrgId: "00000000-0000-0000-0000-000000000000"})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
	})

	It("lists team orgs with counts, budget and spend, and leaves out personal orgs by default", func(ctx SpecContext) {
		a := owner.activeAgent(ctx, other)
		_ = e.userIn(ctx, other, store.OrgRoleViewer)
		_, err := budgetClient(owner).SetMonthlyCap(ctx, &budgetv1alpha1.SetMonthlyCapRequest{OrgId: other, MonthlyCapUsd: "100"})
		Expect(err).NotTo(HaveOccurred())
		var orgID pgtype.UUID
		Expect(orgID.Scan(other)).To(Succeed())
		_, err = e.pool.Exec(ctx, `INSERT INTO spend_entries (org_id, request_id, agent_id, key_id, model, input_tokens, output_tokens, occurred_at, cost_usd, priced, estimated)
			VALUES ($1, 'req-console', $2, 'k', 'm', 1, 1, now(), 2.5, true, false)`, orgID, a.GetId())
		Expect(err).NotTo(HaveOccurred())

		orgs := listAllOrgs(ctx, admin, false, 2)
		for _, o := range orgs {
			Expect(o.GetKind()).To(Equal(orgv1alpha1.OrgKind_ORG_KIND_TEAM), o.GetSlug())
		}
		slugs := make([]string, 0, len(orgs))
		for _, o := range orgs {
			slugs = append(slugs, o.GetSlug())
		}
		Expect(slices.IsSorted(slugs)).To(BeTrue())
		Expect(slices.Compact(slices.Clone(slugs))).To(HaveLen(len(slugs)), "a page repeated an org")

		got := orgByID(orgs, other)
		Expect(got).NotTo(BeNil())
		Expect(got.GetMemberCount()).To(Equal(int32(2)))
		Expect(got.GetAgentCount()).To(Equal(int32(1)))
		Expect(got.GetSpentThisMonthUsd()).To(Equal("2.50"))
		Expect(got.GetMonthlyCapUsd()).To(Equal("100.00"))
		Expect(got.GetPlatformCeilingUsd()).To(BeEmpty())
		Expect(orgByID(orgs, own)).NotTo(BeNil())

		// The personal org of the admin shows only with include_personal.
		var personal string
		Expect(e.pool.QueryRow(ctx, `SELECT personal_org_id::text FROM users WHERE id = $1`, admin.userID).Scan(&personal)).To(Succeed())
		Expect(orgByID(orgs, personal)).To(BeNil())
		withPersonal := listAllOrgs(ctx, admin, true, 200)
		p := orgByID(withPersonal, personal)
		Expect(p).NotTo(BeNil())
		Expect(p.GetKind()).To(Equal(orgv1alpha1.OrgKind_ORG_KIND_PERSONAL))
	})

	It("reads an org the platform admin is not a member of", func(ctx SpecContext) {
		res, err := admin.platform.GetOrg(ctx, &platformv1alpha1.GetOrgRequest{OrgId: other})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetOrg().GetOrgId()).To(Equal(other))
		Expect(res.GetOrg().GetMemberCount()).To(Equal(int32(1)))
		Expect(res.GetOrg().GetSpentThisMonthUsd()).To(Equal("0.00"))
		Expect(res.GetMonthStart()).NotTo(BeNil())

		_, err = admin.platform.GetOrg(ctx, &platformv1alpha1.GetOrgRequest{OrgId: "00000000-0000-0000-0000-000000000000"})
		Expect(codeOf(err)).To(Equal(connect.CodeNotFound))
		_, err = admin.platform.GetOrg(ctx, &platformv1alpha1.GetOrgRequest{OrgId: "not-a-uuid"})
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
		_, err = admin.platform.ListOrgs(ctx, &platformv1alpha1.ListOrgsRequest{PageToken: "!!"})
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))

		_, err = e.pool.Exec(ctx, `UPDATE orgs SET deleted_at = now() WHERE id = $1`, other)
		Expect(err).NotTo(HaveOccurred())
		_, err = admin.platform.GetOrg(ctx, &platformv1alpha1.GetOrgRequest{OrgId: other})
		Expect(codeOf(err)).To(Equal(connect.CodeNotFound), "a deleted org is not read")
		Expect(orgByID(listAllOrgs(ctx, admin, true, 200), other)).To(BeNil(), "a deleted org is not listed")
	})
})

var _ = Describe("the members read (OrgService.ListMembers)", func() {
	var (
		e            *env
		org, foreign string
		owner        *client
	)
	BeforeEach(func(ctx SpecContext) {
		e = newEnv(ctx, envOptions{})
		org, owner = e.newTeamOrg(ctx)
		foreign, _ = e.newTeamOrg(ctx)
	})

	It("lists only the org's members with their effective role, in pages, to every role", func(ctx SpecContext) {
		viewer := e.userIn(ctx, org, store.OrgRoleViewer)
		admin := e.userIn(ctx, org, store.OrgRoleAdmin)
		outsider := e.userIn(ctx, foreign, store.OrgRoleOwner)
		want := map[string]authv1alpha1.OrgRole{
			owner.userID:  authv1alpha1.OrgRole_ORG_ROLE_OWNER,
			viewer.userID: authv1alpha1.OrgRole_ORG_ROLE_VIEWER,
			admin.userID:  authv1alpha1.OrgRole_ORG_ROLE_ADMIN,
		}
		for _, c := range []*client{viewer, admin, owner} {
			got := map[string]authv1alpha1.OrgRole{}
			token := ""
			for range 10 {
				res, err := c.orgs.ListMembers(ctx, &orgv1alpha1.ListMembersRequest{OrgId: org, PageSize: 1, PageToken: token})
				Expect(err).NotTo(HaveOccurred())
				Expect(res.GetMembers()).To(HaveLen(1))
				for _, m := range res.GetMembers() {
					Expect(got).NotTo(HaveKey(m.GetUserId()), "a page repeated a member")
					Expect(m.GetDisplayName()).NotTo(BeEmpty())
					got[m.GetUserId()] = m.GetRole()
				}
				if token = res.GetNextPageToken(); token == "" {
					break
				}
			}
			Expect(got).To(Equal(want))
			Expect(got).NotTo(HaveKey(outsider.userID))
		}

		_, err := viewer.orgs.ListMembers(ctx, &orgv1alpha1.ListMembersRequest{OrgId: org, PageToken: "bm90LWpzb24"})
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
	})

	It("answers NotFound to a user of another org, and leaves out deleted users", func(ctx SpecContext) {
		outsider := e.userIn(ctx, foreign, store.OrgRoleOwner)
		_, err := outsider.orgs.ListMembers(ctx, &orgv1alpha1.ListMembersRequest{OrgId: org})
		Expect(codeOf(err)).To(Equal(connect.CodeNotFound))

		gone := e.userIn(ctx, org, store.OrgRoleMember)
		_, err = e.pool.Exec(ctx, `UPDATE users SET deleted_at = now() WHERE id = $1`, gone.userID)
		Expect(err).NotTo(HaveOccurred())
		res, err := owner.orgs.ListMembers(ctx, &orgv1alpha1.ListMembersRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetMembers()).To(HaveLen(1))
		Expect(res.GetMembers()[0].GetUserId()).To(Equal(owner.userID))
	})

	It("applies the org's identity mode, as WhoAmI does", func(ctx SpecContext) {
		member := e.userIn(ctx, org, store.OrgRoleMember)
		_, err := e.pool.Exec(ctx, `UPDATE orgs SET identity_mode = 'idp' WHERE id = $1`, org)
		Expect(err).NotTo(HaveOccurred())
		res, err := owner.orgs.ListMembers(ctx, &orgv1alpha1.ListMembersRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		ids := []string{}
		for _, m := range res.GetMembers() {
			ids = append(ids, m.GetUserId())
		}
		// In an idp org a manual row counts only for the owner (ADR 0004).
		Expect(ids).To(ConsistOf(owner.userID))
		Expect(ids).NotTo(ContainElement(member.userID))
	})

	It("lets a platform admin who is not a member read the members", func(ctx SpecContext) {
		pa := e.userIn(ctx, foreign, store.OrgRoleMember)
		makePlatformAdmin(ctx, e, pa)
		res, err := pa.orgs.ListMembers(ctx, &orgv1alpha1.ListMembersRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetMembers()).To(HaveLen(1))
		Expect(res.GetMembers()[0].GetUserId()).To(Equal(owner.userID))

		// An org that does not exist or is deleted is NotFound, as for GetOrg, not an empty list.
		_, err = pa.orgs.ListMembers(ctx, &orgv1alpha1.ListMembersRequest{OrgId: "00000000-0000-0000-0000-000000000000"})
		Expect(codeOf(err)).To(Equal(connect.CodeNotFound))
		_, err = e.pool.Exec(ctx, `UPDATE orgs SET deleted_at = now() WHERE id = $1`, org)
		Expect(err).NotTo(HaveOccurred())
		_, err = pa.orgs.ListMembers(ctx, &orgv1alpha1.ListMembersRequest{OrgId: org})
		Expect(codeOf(err)).To(Equal(connect.CodeNotFound))
	})
})
