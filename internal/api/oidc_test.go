// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	connect "connectrpc.com/connect/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/auth"
	"github.com/stateracloud/koinon/internal/auth/oidctest"
	"github.com/stateracloud/koinon/internal/db/store"
	authv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/auth/v1alpha1"
)

func stringsReader(s string) io.Reader { return strings.NewReader(s) }

func oidcUser(sub string, groups ...string) oidctest.User {
	return oidctest.User{Subject: sub, Email: sub + "@example.test", Name: strings.ToUpper(sub[:1]) + sub[1:], Groups: groups}
}

// oidcLogin runs the browser through start, the issuer and the callback, and
// returns the final response (the page the user lands on).
func (b *browser) oidcLogin(ctx context.Context, returnTo string) *http.Response {
	u := b.e.srv.URL + "/auth/oidc/start"
	if returnTo != "" {
		u += "?return_to=" + url.QueryEscape(returnTo)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	Expect(err).NotTo(HaveOccurred())
	res, err := b.http.Do(req)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(res.Body.Close)
	return res
}

func membershipsOf(who *authv1alpha1.WhoAmIResponse) map[string]authv1alpha1.OrgRole {
	out := map[string]authv1alpha1.OrgRole{}
	for _, m := range who.GetMemberships() {
		out[m.GetOrgSlug()] = m.GetRole()
	}
	return out
}

// teamOrg creates a team org owned by the bootstrap admin, with one group
// mapping from the test issuer.
func teamOrg(ctx context.Context, e *env, slug string, mode store.IdentityMode, group string, role store.OrgRole) {
	tx, err := e.pool.Begin(ctx)
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = tx.Rollback(ctx) }()
	var orgID string
	Expect(tx.QueryRow(ctx, `INSERT INTO orgs (slug, display_name, kind, identity_mode) VALUES ($1, $1, 'team', $2) RETURNING id`,
		slug, mode).Scan(&orgID)).To(Succeed())
	_, err = tx.Exec(ctx, `INSERT INTO org_memberships (org_id, user_id, source, role)
		SELECT $1, id, 'manual', 'owner' FROM users WHERE oidc_issuer = $2`, orgID, auth.LocalIssuer)
	Expect(err).NotTo(HaveOccurred())
	_, err = tx.Exec(ctx, `INSERT INTO org_group_mappings (org_id, oidc_issuer, group_name, role) VALUES ($1, $2, $3, $4)`,
		orgID, e.issuer.URL(), group, role)
	Expect(err).NotTo(HaveOccurred())
	Expect(tx.Commit(ctx)).To(Succeed())
}

var _ = Describe("OIDC login", func() {
	var e *env

	BeforeEach(func(ctx SpecContext) {
		e = newEnv(ctx, envOptions{oidc: true, groupMappings: []auth.GroupMapping{
			{Group: "koinon-admins", Role: store.OrgRoleAdmin},
			{Group: "staff", Role: store.OrgRoleMember},
		}})
	})

	It("logs in with code + PKCE, creates the user and maps groups to roles", func(ctx SpecContext) {
		teamOrg(ctx, e, "platform-team", store.IdentityModePlatform, "staff", store.OrgRoleAdmin)
		teamOrg(ctx, e, "idp-team", store.IdentityModeIdp, "eng", store.OrgRoleViewer)
		e.issuer.SetUser(oidcUser("alice", "koinon-admins", "staff", "eng"))

		b := e.browser()
		res := b.oidcLogin(ctx, "/board?ticket=1")
		Expect(res.Request.URL.Host).To(Equal(strings.TrimPrefix(e.srv.URL, "https://")))
		Expect(res.Request.URL.RequestURI()).To(Equal("/board?ticket=1"))
		Expect(b.sessionToken()).NotTo(BeEmpty())

		By("sending an S256 challenge and a nonce to the IdP")
		Expect(e.issuer.LastRequest.Get("code_challenge_method")).To(Equal("S256"))
		Expect(e.issuer.LastRequest.Get("nonce")).NotTo(BeEmpty())
		Expect(e.issuer.LastRequest.Get("scope")).To(Equal("openid email profile"))

		who, err := b.auth().WhoAmI(ctx, &authv1alpha1.WhoAmIRequest{})
		Expect(err).NotTo(HaveOccurred())
		Expect(who.GetAuthMethod()).To(Equal(authv1alpha1.AuthMethod_AUTH_METHOD_OIDC))
		Expect(who.GetPlatformAdmin()).To(BeFalse())
		Expect(who.GetPasswordChangeRequired()).To(BeFalse())
		Expect(who.GetUser().GetEmail()).To(Equal("alice@example.test"))
		Expect(who.GetUser().GetDisplayName()).To(Equal("Alice"))
		roles := membershipsOf(who)
		// Highest mapped role wins in the hybrid default org; the platform-mode
		// org ignores IdP groups; the idp-mode org uses them.
		Expect(roles).To(HaveKeyWithValue("default", authv1alpha1.OrgRole_ORG_ROLE_ADMIN))
		Expect(roles).To(HaveKeyWithValue("idp-team", authv1alpha1.OrgRole_ORG_ROLE_VIEWER))
		Expect(roles).NotTo(HaveKey("platform-team"))
		Expect(roles).To(HaveLen(3)) // plus the personal org
		var personal int
		for _, m := range who.GetMemberships() {
			if m.GetOrgId() == who.GetUser().GetPersonalOrgId() {
				Expect(m.GetRole()).To(Equal(authv1alpha1.OrgRole_ORG_ROLE_OWNER))
				personal++
			}
		}
		Expect(personal).To(Equal(1))
	})

	It("upserts by issuer and subject and resyncs groups at every login", func(ctx SpecContext) {
		e.issuer.SetUser(oidcUser("bob", "koinon-admins"))
		first := e.browser()
		first.oidcLogin(ctx, "")
		who1, err := first.auth().WhoAmI(ctx, &authv1alpha1.WhoAmIRequest{})
		Expect(err).NotTo(HaveOccurred())
		Expect(membershipsOf(who1)).To(HaveKeyWithValue("default", authv1alpha1.OrgRole_ORG_ROLE_ADMIN))

		u := oidcUser("bob", "staff")
		u.Email = "robert@example.test"
		e.issuer.SetUser(u)
		second := e.browser()
		second.oidcLogin(ctx, "")
		who2, err := second.auth().WhoAmI(ctx, &authv1alpha1.WhoAmIRequest{})
		Expect(err).NotTo(HaveOccurred())
		Expect(who2.GetUser().GetId()).To(Equal(who1.GetUser().GetId()))
		Expect(who2.GetUser().GetEmail()).To(Equal("robert@example.test"))
		Expect(membershipsOf(who2)).To(HaveKeyWithValue("default", authv1alpha1.OrgRole_ORG_ROLE_MEMBER))

		e.issuer.SetUser(oidcUser("bob"))
		third := e.browser()
		third.oidcLogin(ctx, "")
		who3, err := third.auth().WhoAmI(ctx, &authv1alpha1.WhoAmIRequest{})
		Expect(err).NotTo(HaveOccurred())
		Expect(membershipsOf(who3)).NotTo(HaveKey("default"))
		Expect(count(ctx, e, `SELECT count(*) FROM users WHERE oidc_subject = 'bob'`)).To(Equal(1))
	})

	It("drops IdP roles when a mapping is removed from the configuration", func(ctx SpecContext) {
		e.issuer.SetUser(oidcUser("dana", "staff"))
		b := e.browser()
		b.oidcLogin(ctx, "")
		e.opts.groupMappings = []auth.GroupMapping{{Group: "koinon-admins", Role: store.OrgRoleAdmin}}
		e.bootstrap(ctx)
		who, err := b.auth().WhoAmI(ctx, &authv1alpha1.WhoAmIRequest{})
		Expect(err).NotTo(HaveOccurred())
		Expect(membershipsOf(who)).NotTo(HaveKey("default"))
	})

	It("keeps redirects on this site", func(ctx SpecContext) {
		e.issuer.SetUser(oidcUser("erin"))
		for _, target := range []string{"//evil.example/x", "https://evil.example/", "/\\evil.example", "board"} {
			res := e.browser().oidcLogin(ctx, target)
			Expect(res.Request.URL.Host).To(Equal(strings.TrimPrefix(e.srv.URL, "https://")), target)
			Expect(res.Request.URL.Path).To(Equal("/"), target)
		}
	})

	It("rejects a callback whose state does not match this browser", func(ctx SpecContext) {
		e.issuer.SetUser(oidcUser("mallory"))
		attacker := e.browser()
		attacker.http.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
			if req.URL.Path == "/auth/oidc/callback" {
				return http.ErrUseLastResponse
			}
			return nil
		}
		res := attacker.oidcLogin(ctx, "")
		Expect(res.StatusCode).To(Equal(http.StatusFound))
		callback := res.Header.Get("Location")

		// The victim's browser has no state cookie for this flow.
		victim := e.browser()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, callback, nil)
		Expect(err).NotTo(HaveOccurred())
		vres, err := victim.http.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = vres.Body.Close() }()
		Expect(vres.StatusCode).To(Equal(http.StatusBadRequest))
		Expect(victim.sessionToken()).To(BeEmpty())
		Expect(e.issuer.Exchanges).To(Equal(0))
	})

	It("rejects an ID token with the wrong nonce", func(ctx SpecContext) {
		e.issuer.SetUser(oidcUser("oscar"))
		e.issuer.SetWrongNonce(true)
		b := e.browser()
		res := b.oidcLogin(ctx, "")
		Expect(res.StatusCode).To(Equal(http.StatusUnauthorized))
		Expect(b.sessionToken()).To(BeEmpty())
		Expect(count(ctx, e, `SELECT count(*) FROM users WHERE oidc_subject = 'oscar'`)).To(Equal(0))
	})

	It("rejects an expired login flow", func(ctx SpecContext) {
		e.issuer.SetUser(oidcUser("pat"))
		b := e.browser()
		b.http.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
			if req.URL.Path == "/auth/oidc/callback" {
				return http.ErrUseLastResponse
			}
			return nil
		}
		callback := b.oidcLogin(ctx, "").Header.Get("Location")
		e.clock.Advance(11 * time.Minute)
		b.http.CheckRedirect = nil
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, callback, nil)
		Expect(err).NotTo(HaveOccurred())
		res, err := b.http.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = res.Body.Close() }()
		Expect(res.StatusCode).To(Equal(http.StatusBadRequest))
	})

	It("is not offered when OIDC is not configured", func(ctx SpecContext) {
		plain := newEnv(ctx, envOptions{})
		res := plain.browser().oidcLogin(ctx, "")
		Expect(res.StatusCode).To(Equal(http.StatusNotFound))
	})

	It("rejects a disabled user", func(ctx SpecContext) {
		e.issuer.SetUser(oidcUser("quinn"))
		b := e.browser()
		b.oidcLogin(ctx, "")
		_, err := e.pool.Exec(ctx, `UPDATE users SET deleted_at = now() WHERE oidc_subject = 'quinn'`)
		Expect(err).NotTo(HaveOccurred())
		_, err = b.auth().WhoAmI(ctx, &authv1alpha1.WhoAmIRequest{})
		Expect(codeOf(err)).To(Equal(connect.CodeUnauthenticated))
		Expect(e.browser().oidcLogin(ctx, "").StatusCode).To(Equal(http.StatusForbidden))
	})
})

var _ = Describe("OIDC platform admin groups", func() {
	var e *env

	isAdmin := func(ctx context.Context, subject string) bool {
		return count(ctx, e, `SELECT count(*) FROM platform_admins pa JOIN users u ON u.id = pa.user_id WHERE u.oidc_subject = $1`, subject) == 1
	}
	// events counts the install chain's events for a subject's user, by the sync's named actor
	// (ADR 0100): a platform admin grant belongs to no org, so nothing lands in a personal org.
	events := func(ctx context.Context, subject, action string) int {
		return count(ctx, e, `SELECT count(*) FROM install_audit_events i JOIN users u ON u.id::text = i.target_id
			WHERE i.action = $1 AND i.actor_kind = 'system' AND i.actor_id = 'koinon-api:oidc-sync' AND u.oidc_subject = $2`, action, subject)
	}

	It("grants on login, revokes when the group is gone, and audits both", func(ctx SpecContext) {
		e = newEnv(ctx, envOptions{oidc: true, platformAdminGroups: []string{"koinon-platform", "other"}})
		e.issuer.SetUser(oidcUser("pia", "koinon-platform"))
		b := e.browser()
		b.oidcLogin(ctx, "")
		who, err := b.auth().WhoAmI(ctx, &authv1alpha1.WhoAmIRequest{})
		Expect(err).NotTo(HaveOccurred())
		Expect(who.GetPlatformAdmin()).To(BeTrue())
		Expect(isAdmin(ctx, "pia")).To(BeTrue())
		Expect(events(ctx, "pia", "platform_admin.grant")).To(Equal(1))

		By("writing the install event before the login event, which points at it (ADR 0100 decision 1.7)")
		Expect(count(ctx, e, `SELECT count(*) FROM audit_events a
			JOIN users u ON u.personal_org_id = a.org_id
			JOIN install_audit_events i ON i.id::text = a.summary->>'install_event_id'
			WHERE u.oidc_subject = 'pia' AND a.action = 'auth.login' AND i.action = 'platform_admin.grant' AND i.created_at <= a.created_at`)).To(Equal(1))

		By("keeping the grant, without a second event, while the group stays")
		e.browser().oidcLogin(ctx, "")
		Expect(events(ctx, "pia", "platform_admin.grant")).To(Equal(1))

		By("revoking when the user loses every listed group")
		e.issuer.SetUser(oidcUser("pia", "staff"))
		again := e.browser()
		again.oidcLogin(ctx, "")
		Expect(isAdmin(ctx, "pia")).To(BeFalse())
		who, err = again.auth().WhoAmI(ctx, &authv1alpha1.WhoAmIRequest{})
		Expect(err).NotTo(HaveOccurred())
		Expect(who.GetPlatformAdmin()).To(BeFalse())
		Expect(events(ctx, "pia", "platform_admin.revoke")).To(Equal(1))

		By("granting again when the group returns")
		e.issuer.SetUser(oidcUser("pia", "other"))
		e.browser().oidcLogin(ctx, "")
		Expect(isAdmin(ctx, "pia")).To(BeTrue())
		Expect(events(ctx, "pia", "platform_admin.grant")).To(Equal(2))

		By("writing none of it into the user's personal org")
		Expect(count(ctx, e, `SELECT count(*) FROM audit_events a JOIN users u ON u.personal_org_id = a.org_id
			WHERE u.oidc_subject = 'pia' AND a.action LIKE 'platform_admin.%'`)).To(BeZero())
	})

	It("leaves the bootstrap admin and other platform admins alone", func(ctx SpecContext) {
		e = newEnv(ctx, envOptions{oidc: true, platformAdminGroups: []string{"koinon-platform"}})
		e.issuer.SetUser(oidcUser("sam", "staff"))
		e.browser().oidcLogin(ctx, "")
		_, err := e.pool.Exec(ctx, `INSERT INTO platform_admins (user_id, source) SELECT id, 'bootstrap' FROM users WHERE oidc_subject = 'sam'`)
		Expect(err).NotTo(HaveOccurred())

		e.browser().oidcLogin(ctx, "")
		Expect(isAdmin(ctx, "sam")).To(BeTrue(), "a grant from another source is not revoked by the login")
		Expect(count(ctx, e, `SELECT count(*) FROM platform_admins pa JOIN users u ON u.id = pa.user_id WHERE u.oidc_issuer = $1`, auth.LocalIssuer)).To(Equal(1))
		Expect(events(ctx, "sam", "platform_admin.revoke")).To(Equal(0))

		By("not turning another source's grant into a group grant")
		e.issuer.SetUser(oidcUser("sam", "koinon-platform"))
		e.browser().oidcLogin(ctx, "")
		e.issuer.SetUser(oidcUser("sam"))
		e.browser().oidcLogin(ctx, "")
		Expect(isAdmin(ctx, "sam")).To(BeTrue())
		Expect(events(ctx, "sam", "platform_admin.grant")).To(Equal(0))
	})

	It("changes nothing when the setting is empty", func(ctx SpecContext) {
		e = newEnv(ctx, envOptions{oidc: true})
		e.issuer.SetUser(oidcUser("tom", "koinon-platform"))
		e.browser().oidcLogin(ctx, "")
		Expect(isAdmin(ctx, "tom")).To(BeFalse())
		_, err := e.pool.Exec(ctx, `INSERT INTO platform_admins (user_id, source) SELECT id, 'oidc_group' FROM users WHERE oidc_subject = 'tom'`)
		Expect(err).NotTo(HaveOccurred())
		e.issuer.SetUser(oidcUser("tom"))
		e.browser().oidcLogin(ctx, "")
		Expect(isAdmin(ctx, "tom")).To(BeTrue(), "an empty setting does not revoke")
		Expect(events(ctx, "tom", "platform_admin.revoke")).To(Equal(0))
	})
})
