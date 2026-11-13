// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"crypto/sha256"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	connect "connectrpc.com/connect/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/auth"
	authv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/auth/v1alpha1"
	"github.com/stateracloud/koinon/internal/installbinding"
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

	// ADR 0110 decision 3: a session another install issued is refused on a restored copy and never
	// comes back; a new login on the copy works.
	It("refuses a session another install issued, and accepts a new login", func(ctx SpecContext) {
		var cp *installbinding.Binding
		e := newEnv(ctx, envOptions{binding: func(p *pgxpool.Pool) *installbinding.Binding {
			quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
			_, err := installbinding.New(p, "hash-source", false, quiet).Claim(ctx, installbinding.Targets{})
			Expect(err).NotTo(HaveOccurred())
			cp = installbinding.New(p, "hash-copy", false, quiet)
			st, err := cp.Claim(ctx, installbinding.Targets{})
			Expect(err).NotTo(HaveOccurred())
			Expect(st).To(Equal(installbinding.StateCopy))
			return cp
		}})
		b := e.browser()
		Expect(b.login(ctx, "admin", bootstrapPassword).StatusCode).To(Equal(http.StatusOK))
		_, err := b.auth().WhoAmI(ctx, &authv1alpha1.WhoAmIRequest{})
		Expect(err).NotTo(HaveOccurred(), "a session made on the copy carries the copy's mark")
		var mark string
		Expect(e.pool.QueryRow(ctx, `SELECT install_mark FROM sessions`).Scan(&mark)).To(Succeed())
		Expect(mark).To(Equal("hash-copy"))

		By("the session came with the dump of the source")
		_, err = e.pool.Exec(ctx, `UPDATE sessions SET install_mark = 'hash-source'`)
		Expect(err).NotTo(HaveOccurred())
		_, err = e.pool.Exec(ctx, `UPDATE sessions SET last_seen_at = last_seen_at - interval '1 hour'`)
		Expect(err).NotTo(HaveOccurred())
		var seenBefore, idleBefore time.Time
		Expect(e.pool.QueryRow(ctx, `SELECT last_seen_at, idle_expires_at FROM sessions`).Scan(&seenBefore, &idleBefore)).To(Succeed())
		_, err = b.auth().WhoAmI(ctx, &authv1alpha1.WhoAmIRequest{})
		Expect(codeOf(err)).To(Equal(connect.CodeUnauthenticated))

		By("a refused session is not touched: its idle deadline does not move")
		var seenAfter, idleAfter time.Time
		Expect(e.pool.QueryRow(ctx, `SELECT last_seen_at, idle_expires_at FROM sessions`).Scan(&seenAfter, &idleAfter)).To(Succeed())
		Expect(seenAfter).To(Equal(seenBefore))
		Expect(idleAfter).To(Equal(idleBefore))

		By("a legacy session without a mark is refused on a copy too")
		_, err = e.pool.Exec(ctx, `UPDATE sessions SET install_mark = NULL`)
		Expect(err).NotTo(HaveOccurred())
		_, err = b.auth().WhoAmI(ctx, &authv1alpha1.WhoAmIRequest{})
		Expect(codeOf(err)).To(Equal(connect.CodeUnauthenticated))

		By("a new login works")
		b2 := e.browser()
		Expect(b2.login(ctx, "admin", bootstrapPassword).StatusCode).To(Equal(http.StatusOK))
		_, err = b2.auth().WhoAmI(ctx, &authv1alpha1.WhoAmIRequest{})
		Expect(err).NotTo(HaveOccurred())

		By("adopt-restore removes the refused session instead of re-enabling it, and keeps the new one")
		res, err := cp.Adopt(ctx, audit.System("test-recovery"), installbinding.Targets{})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RevokedSessions).To(Equal(1))
		_, err = b.auth().WhoAmI(ctx, &authv1alpha1.WhoAmIRequest{})
		Expect(codeOf(err)).To(Equal(connect.CodeUnauthenticated))
		_, err = b2.auth().WhoAmI(ctx, &authv1alpha1.WhoAmIRequest{})
		Expect(err).NotTo(HaveOccurred())
	})

	// ADR 0110 decision 1: until the release step has run, an unheld koinon-api serves no session of the
	// dump, also on an in-place restore where the marks are the same, and no sign-in makes a new one.
	It("refuses every session and sign-in while the release step has not run (the gate fails closed)", func(ctx SpecContext) {
		var live *installbinding.Binding
		e := newEnv(ctx, envOptions{binding: func(p *pgxpool.Pool) *installbinding.Binding {
			live = installbinding.New(p, "hash-live", false, slog.New(slog.NewTextHandler(io.Discard, nil)))
			_, err := live.Claim(ctx, installbinding.Targets{})
			Expect(err).NotTo(HaveOccurred())
			return live
		}})
		b := e.browser()
		Expect(b.login(ctx, "admin", bootstrapPassword).StatusCode).To(Equal(http.StatusOK))
		_, err := b.auth().WhoAmI(ctx, &authv1alpha1.WhoAmIRequest{})
		Expect(err).NotTo(HaveOccurred(), "a session of this install, same mark")

		By("the release step could not run (its Kubernetes List failed)")
		live.HoldSessions(true)
		_, err = b.auth().WhoAmI(ctx, &authv1alpha1.WhoAmIRequest{})
		Expect(codeOf(err)).To(Equal(connect.CodeUnauthenticated), "a session with this install's mark is refused too")
		Expect(e.browser().login(ctx, "admin", bootstrapPassword).StatusCode).To(Equal(http.StatusServiceUnavailable))
		var n int
		Expect(e.pool.QueryRow(ctx, `SELECT count(*) FROM sessions`).Scan(&n)).To(Succeed())
		Expect(n).To(Equal(1), "the refused sign-in made no session")

		By("the release step ran")
		live.HoldSessions(false)
		_, err = b.auth().WhoAmI(ctx, &authv1alpha1.WhoAmIRequest{})
		Expect(err).NotTo(HaveOccurred())
	})

	It("drops Secure and the __Host- prefix when the public URL is plain http", func(ctx SpecContext) {
		e := newEnv(ctx, envOptions{plainHTTP: true})
		Expect(e.srv.URL).To(HavePrefix("http://"))
		b := e.browser()
		res := b.login(ctx, "admin", bootstrapPassword)
		Expect(res.StatusCode).To(Equal(http.StatusOK))
		Expect(res.Cookies()).To(HaveLen(1))
		c := res.Cookies()[0]
		Expect(c.Name).To(Equal(auth.InsecureSessionCookieName))
		Expect(c.Name).NotTo(HavePrefix("__Host-"))
		Expect(c.Secure).To(BeFalse())
		Expect(c.HttpOnly).To(BeTrue())
		Expect(c.SameSite).To(Equal(http.SameSiteLaxMode))

		By("keeping the session over plain HTTP, as a browser would")
		who, err := b.auth().WhoAmI(ctx, &authv1alpha1.WhoAmIRequest{})
		Expect(err).NotTo(HaveOccurred())
		Expect(who.GetPlatformAdmin()).To(BeTrue())

		By("logging out with the same cookie")
		res = b.post(ctx, "/auth/logout", nil)
		Expect(res.StatusCode).To(Equal(http.StatusNoContent))
		Expect(res.Cookies()).To(ContainElement(SatisfyAll(
			HaveField("Name", auth.InsecureSessionCookieName), HaveField("MaxAge", -1), HaveField("Secure", false))))
		Expect(b.sessionToken()).To(BeEmpty())
	})

	It("rejects a public URL that is neither http nor https", func(ctx SpecContext) {
		_, err := auth.New(newPool(ctx), auth.Config{PublicURL: "ftp://koinon.example.com"})
		Expect(err).To(MatchError(ContainSubstring("scheme must be http or https")))
	})

	It("logs in with the configured bootstrap admin username", func(ctx SpecContext) {
		e := newEnv(ctx, envOptions{username: "root"})
		Expect(e.browser().login(ctx, "admin", bootstrapPassword).StatusCode).To(Equal(http.StatusUnauthorized))
		Expect(e.browser().login(ctx, "root", bootstrapPassword).StatusCode).To(Equal(http.StatusOK))

		By("renaming the login on a restart with another username")
		e2 := newEnvOn(ctx, e, envOptions{username: "ops"})
		Expect(e2.bootstrp).To(Equal(auth.BootstrapResult{}))
		Expect(e2.browser().login(ctx, "root", bootstrapPassword).StatusCode).To(Equal(http.StatusUnauthorized))
		Expect(e2.browser().login(ctx, "ops", bootstrapPassword).StatusCode).To(Equal(http.StatusOK))
		Expect(count(ctx, e2, `SELECT count(*) FROM users`)).To(Equal(1))
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
