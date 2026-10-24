// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"time"

	connect "connectrpc.com/connect/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/stateracloud/koinon/internal/db/store"
	authv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/auth/v1alpha1"
	tokenv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/token/v1alpha1"
	"github.com/stateracloud/koinon/internal/tokens"
)

var _ = Describe("TokenService", func() {
	var (
		e     *env
		org   string
		owner *client
	)
	BeforeEach(func(ctx SpecContext) {
		e = newEnv(ctx, envOptions{})
		org, owner = e.newTeamOrg(ctx)
	})

	create := func(ctx SpecContext, c *client, role authv1alpha1.OrgRole, ttl time.Duration) (*tokenv1alpha1.CreateTokenResponse, error) {
		req := &tokenv1alpha1.CreateTokenRequest{OrgId: org, Name: "ci", Role: role}
		if ttl > 0 {
			req.ExpiresIn = durationpb.New(ttl)
		}
		return c.tokens.CreateToken(ctx, req)
	}

	It("creates a token for an admin, shows the secret once, and lists it without it", func(ctx SpecContext) {
		admin := e.userIn(ctx, org, store.OrgRoleAdmin)
		res, err := create(ctx, admin, authv1alpha1.OrgRole_ORG_ROLE_ADMIN, 0)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetSecret()).To(HavePrefix(tokens.Prefix))
		Expect(res.GetToken().GetExpiresAt().AsTime().Sub(res.GetToken().GetCreatedAt().AsTime())).To(BeNumerically("~", 30*24*time.Hour, time.Second))

		list, err := admin.tokens.ListTokens(ctx, &tokenv1alpha1.ListTokensRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		Expect(list.GetTokens()).To(HaveLen(1))
		Expect(mustJSON(list)).NotTo(ContainSubstring(res.GetSecret()))

		By("the new token authenticates against the token service")
		p, err := tokens.New(e.pool, nil).Authenticate(ctx, res.GetSecret())
		Expect(err).NotTo(HaveOccurred())
		Expect(p.Role).To(Equal(store.OrgRoleAdmin))

		By("the creation is audited with the creating user")
		var actor, summary string
		Expect(e.pool.QueryRow(ctx, `SELECT actor_kind, summary::text FROM audit_events WHERE org_id = $1 AND action = 'token.create'`, org).Scan(&actor, &summary)).To(Succeed())
		Expect(actor).To(Equal("user"))
		Expect(summary).NotTo(ContainSubstring(res.GetSecret()))
	})

	It("never gives a token more than its creator's role, and no viewer or member tokens", func(ctx SpecContext) {
		admin := e.userIn(ctx, org, store.OrgRoleAdmin)
		_, err := create(ctx, admin, authv1alpha1.OrgRole_ORG_ROLE_OWNER, 0)
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
		for _, r := range []authv1alpha1.OrgRole{authv1alpha1.OrgRole_ORG_ROLE_MEMBER, authv1alpha1.OrgRole_ORG_ROLE_VIEWER, authv1alpha1.OrgRole_ORG_ROLE_UNSPECIFIED} {
			_, err = create(ctx, owner, r, 0)
			Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument), r.String())
		}
		_, err = create(ctx, owner, authv1alpha1.OrgRole_ORG_ROLE_OWNER, 0)
		Expect(err).NotTo(HaveOccurred())
	})

	It("limits the lifetime to 90 days", func(ctx SpecContext) {
		_, err := create(ctx, owner, authv1alpha1.OrgRole_ORG_ROLE_ADMIN, 91*24*time.Hour)
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
		_, err = create(ctx, owner, authv1alpha1.OrgRole_ORG_ROLE_ADMIN, 90*24*time.Hour)
		Expect(err).NotTo(HaveOccurred())
	})

	It("is for admins and owners only", func(ctx SpecContext) {
		for _, role := range []store.OrgRole{store.OrgRoleMember, store.OrgRoleViewer} {
			c := e.userIn(ctx, org, role)
			_, err := create(ctx, c, authv1alpha1.OrgRole_ORG_ROLE_ADMIN, 0)
			Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied), string(role))
			_, err = c.tokens.ListTokens(ctx, &tokenv1alpha1.ListTokensRequest{OrgId: org})
			Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied), string(role))
		}
	})

	It("revokes a token, and a revoked token no longer authenticates", func(ctx SpecContext) {
		res, err := create(ctx, owner, authv1alpha1.OrgRole_ORG_ROLE_ADMIN, 0)
		Expect(err).NotTo(HaveOccurred())
		rev, err := owner.tokens.RevokeToken(ctx, &tokenv1alpha1.RevokeTokenRequest{OrgId: org, TokenId: res.GetToken().GetId()})
		Expect(err).NotTo(HaveOccurred())
		Expect(rev.GetToken().GetRevokedAt()).NotTo(BeNil())
		_, err = tokens.New(e.pool, nil).Authenticate(ctx, res.GetSecret())
		Expect(err).To(MatchError(tokens.ErrInvalid))
		_, err = owner.tokens.RevokeToken(ctx, &tokenv1alpha1.RevokeTokenRequest{OrgId: org, TokenId: "not-a-uuid"})
		Expect(codeOf(err)).To(Equal(connect.CodeNotFound))
	})
})
