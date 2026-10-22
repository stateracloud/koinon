// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"crypto/sha256"
	"encoding/json"
	"net/http"

	connect "connectrpc.com/connect/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/procoduck/koinon/internal/auth"
	authv1alpha1 "github.com/procoduck/koinon/internal/gen/koinon/auth/v1alpha1"
)

var _ = Describe("bootstrap password login", func() {
	var e *env

	BeforeEach(func(ctx SpecContext) {
		e = newEnv(ctx, envOptions{})
	})

	It("rejects wrong passwords and unknown users alike", func(ctx SpecContext) {
		b := e.browser()
		for _, creds := range [][2]string{{"admin", "wrong-password"}, {"nobody", bootstrapPassword}, {"admin", ""}} {
			res := b.login(ctx, creds[0], creds[1])
			Expect(res.StatusCode).To(Equal(http.StatusUnauthorized))
			Expect(res.Cookies()).To(BeEmpty())
		}
	})

	It("sets an HTTP-only, Secure, SameSite=Lax host cookie holding a random token", func(ctx SpecContext) {
		b := e.browser()
		res := b.login(ctx, "admin", bootstrapPassword)
		Expect(res.StatusCode).To(Equal(http.StatusOK))
		var body map[string]bool
		Expect(json.NewDecoder(res.Body).Decode(&body)).To(Succeed())
		Expect(body).To(Equal(map[string]bool{"passwordChangeRequired": true}))

		Expect(res.Cookies()).To(HaveLen(1))
		c := res.Cookies()[0]
		Expect(c.Name).To(Equal("__Host-koinon_session"))
		Expect(c.HttpOnly).To(BeTrue())
		Expect(c.Secure).To(BeTrue())
		Expect(c.SameSite).To(Equal(http.SameSiteLaxMode))
		Expect(c.Path).To(Equal("/"))
		Expect(c.Domain).To(BeEmpty())
		Expect(len(c.Value)).To(BeNumerically(">=", 43)) // 32 random bytes

		By("storing only the token's SHA-256")
		var stored []byte
		Expect(e.pool.QueryRow(ctx, `SELECT token_hash FROM sessions`).Scan(&stored)).To(Succeed())
		sum := sha256.Sum256([]byte(c.Value))
		Expect(stored).To(Equal(sum[:]))
	})

	It("forces a password change before any other call", func(ctx SpecContext) {
		b := e.browser()
		Expect(b.login(ctx, "admin", bootstrapPassword).StatusCode).To(Equal(http.StatusOK))

		who, err := b.auth().WhoAmI(ctx, &authv1alpha1.WhoAmIRequest{})
		Expect(err).NotTo(HaveOccurred())
		Expect(who.GetPasswordChangeRequired()).To(BeTrue())
		Expect(who.GetPlatformAdmin()).To(BeTrue())
		Expect(who.GetAuthMethod()).To(Equal(authv1alpha1.AuthMethod_AUTH_METHOD_PASSWORD))
		Expect(who.GetMemberships()).To(ContainElement(SatisfyAll(
			HaveField("OrgSlug", "default"),
			HaveField("Role", authv1alpha1.OrgRole_ORG_ROLE_OWNER),
		)))

		Expect(codeOf(b.probe(ctx))).To(Equal(connect.CodePermissionDenied))
	})

	It("changes the password, lifts the gate and ends the other sessions", func(ctx SpecContext) {
		other := e.browser()
		Expect(other.login(ctx, "admin", bootstrapPassword).StatusCode).To(Equal(http.StatusOK))
		b := e.browser()
		Expect(b.login(ctx, "admin", bootstrapPassword).StatusCode).To(Equal(http.StatusOK))
		client := b.auth()

		By("validating the request")
		_, err := client.ChangePassword(ctx, &authv1alpha1.ChangePasswordRequest{
			CurrentPassword: bootstrapPassword, NewPassword: "short",
		})
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
		_, err = client.ChangePassword(ctx, &authv1alpha1.ChangePasswordRequest{
			CurrentPassword: bootstrapPassword, NewPassword: bootstrapPassword,
		})
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
		_, err = client.ChangePassword(ctx, &authv1alpha1.ChangePasswordRequest{
			CurrentPassword: "not-the-password", NewPassword: newPassword,
		})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))

		By("changing it")
		_, err = client.ChangePassword(ctx, &authv1alpha1.ChangePasswordRequest{
			CurrentPassword: bootstrapPassword, NewPassword: newPassword,
		})
		Expect(err).NotTo(HaveOccurred())

		who, err := client.WhoAmI(ctx, &authv1alpha1.WhoAmIRequest{})
		Expect(err).NotTo(HaveOccurred())
		Expect(who.GetPasswordChangeRequired()).To(BeFalse())
		Expect(b.probe(ctx)).To(Succeed())

		_, err = other.auth().WhoAmI(ctx, &authv1alpha1.WhoAmIRequest{})
		Expect(codeOf(err)).To(Equal(connect.CodeUnauthenticated))

		By("logging in with the new password only")
		Expect(e.browser().login(ctx, "admin", bootstrapPassword).StatusCode).To(Equal(http.StatusUnauthorized))
		res := e.browser().login(ctx, "admin", newPassword)
		Expect(res.StatusCode).To(Equal(http.StatusOK))
		var body map[string]bool
		Expect(json.NewDecoder(res.Body).Decode(&body)).To(Succeed())
		Expect(body["passwordChangeRequired"]).To(BeFalse())
	})

	It("accepts a form post too", func(ctx SpecContext) {
		b := e.browser()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.srv.URL+"/auth/login",
			stringsReader("username=admin&password="+bootstrapPassword))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		res, err := b.http.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = res.Body.Close() }()
		Expect(res.StatusCode).To(Equal(http.StatusOK))
		Expect(b.sessionToken()).NotTo(BeEmpty())
	})

	It("refuses ChangePassword for OIDC users", func(ctx SpecContext) {
		oe := newEnv(ctx, envOptions{oidc: true})
		b := oe.browser()
		oe.issuer.SetUser(oidcUser("carol"))
		b.oidcLogin(ctx, "/")
		_, err := b.auth().ChangePassword(ctx, &authv1alpha1.ChangePasswordRequest{
			CurrentPassword: "whatever-it-is", NewPassword: newPassword,
		})
		Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition))
	})

	It("requires a session for every RPC", func(ctx SpecContext) {
		b := e.browser()
		_, err := b.auth().WhoAmI(ctx, &authv1alpha1.WhoAmIRequest{})
		Expect(codeOf(err)).To(Equal(connect.CodeUnauthenticated))
		Expect(codeOf(b.probe(ctx))).To(Equal(connect.CodeUnauthenticated))
		b.setSessionToken("forged-token")
		Expect(codeOf(b.probe(ctx))).To(Equal(connect.CodeUnauthenticated))
		Expect(auth.SessionCookieName).To(HavePrefix("__Host-"))
	})
})
