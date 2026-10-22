// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"context"
	"net/http"
	"os"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/procoduck/koinon/internal/auth"
	"github.com/procoduck/koinon/internal/db/store"
	authv1alpha1 "github.com/procoduck/koinon/internal/gen/koinon/auth/v1alpha1"
)

func count(ctx context.Context, e *env, query string, args ...any) int {
	var n int
	Expect(e.pool.QueryRow(ctx, query, args...).Scan(&n)).To(Succeed())
	return n
}

var _ = Describe("bootstrap", func() {
	It("creates the default org and a platform admin who owns it", func(ctx SpecContext) {
		e := newEnv(ctx, envOptions{})
		Expect(e.bootstrp).To(Equal(auth.BootstrapResult{CreatedAdmin: true, CreatedOrg: true, CreatedCredential: true}))

		Expect(count(ctx, e, `SELECT count(*) FROM orgs WHERE slug = 'default' AND kind = 'team' AND identity_mode = 'hybrid'`)).To(Equal(1))
		Expect(count(ctx, e, `SELECT count(*) FROM platform_admins pa JOIN users u ON u.id = pa.user_id
			WHERE u.oidc_issuer = $1 AND u.oidc_subject = 'admin' AND pa.granted_by IS NULL`, auth.LocalIssuer)).To(Equal(1))
		Expect(count(ctx, e, `SELECT count(*) FROM org_memberships om JOIN orgs o ON o.id = om.org_id
			WHERE o.slug = 'default' AND om.source = 'manual' AND om.role = 'owner'`)).To(Equal(1))
	})

	It("stores the one-time password only as an argon2id hash", func(ctx SpecContext) {
		e := newEnv(ctx, envOptions{})
		var hash string
		var mustChange bool
		Expect(e.pool.QueryRow(ctx, `SELECT password_hash, must_change_password FROM local_credentials`).
			Scan(&hash, &mustChange)).To(Succeed())
		Expect(hash).To(HavePrefix("$argon2id$v=19$m=65536,t=3,p=4$"))
		Expect(hash).NotTo(ContainSubstring(bootstrapPassword))
		Expect(mustChange).To(BeTrue())
	})

	It("is idempotent and does not re-read the password file", func(ctx SpecContext) {
		e := newEnv(ctx, envOptions{})
		Expect(os.WriteFile(e.pwFile, []byte("a-rotated-secret-value"), 0o600)).To(Succeed())

		Expect(e.bootstrap(ctx)).To(Equal(auth.BootstrapResult{}))
		Expect(count(ctx, e, `SELECT count(*) FROM orgs WHERE kind = 'team'`)).To(Equal(1))
		Expect(count(ctx, e, `SELECT count(*) FROM users`)).To(Equal(1))
		Expect(count(ctx, e, `SELECT count(*) FROM platform_admins`)).To(Equal(1))

		b := e.browser()
		Expect(b.login(ctx, "admin", bootstrapPassword).StatusCode).To(Equal(http.StatusOK))
	})

	It("is safe to run concurrently from several replicas", func(ctx SpecContext) {
		pool := newPool(ctx)
		pw := GinkgoT().TempDir() + "/pw"
		Expect(os.WriteFile(pw, []byte(bootstrapPassword), 0o600)).To(Succeed())

		var wg sync.WaitGroup
		errs := make(chan error, 4)
		for range 4 {
			wg.Go(func() {
				_, err := auth.Bootstrap(ctx, pool, auth.BootstrapConfig{PasswordFile: pw, LoginEnabled: true}, timeNow())
				errs <- err
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			Expect(err).NotTo(HaveOccurred())
		}
		var orgs, users, creds int
		Expect(pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM orgs WHERE kind = 'team'),
			(SELECT count(*) FROM users), (SELECT count(*) FROM local_credentials)`).Scan(&orgs, &users, &creds)).To(Succeed())
		Expect([]int{orgs, users, creds}).To(Equal([]int{1, 1, 1}))
	})

	It("starts without a password file but leaves password login unusable", func(ctx SpecContext) {
		pool := newPool(ctx)
		res, err := auth.Bootstrap(ctx, pool, auth.BootstrapConfig{LoginEnabled: true}, timeNow())
		Expect(err).NotTo(HaveOccurred())
		Expect(res.MissingPasswordFile).To(BeTrue())
		Expect(res.CreatedCredential).To(BeFalse())
	})

	It("fails when the configured password file is missing or empty", func(ctx SpecContext) {
		pool := newPool(ctx)
		_, err := auth.Bootstrap(ctx, pool, auth.BootstrapConfig{
			LoginEnabled: true, PasswordFile: GinkgoT().TempDir() + "/missing",
		}, timeNow())
		Expect(err).To(MatchError(ContainSubstring("read bootstrap password file")))

		empty := GinkgoT().TempDir() + "/empty"
		Expect(os.WriteFile(empty, []byte("\n"), 0o600)).To(Succeed())
		_, err = auth.Bootstrap(ctx, pool, auth.BootstrapConfig{LoginEnabled: true, PasswordFile: empty}, timeNow())
		Expect(err).To(MatchError(ContainSubstring("empty")))
	})

	It("reconciles the default org's group mappings for the OIDC issuer", func(ctx SpecContext) {
		e := newEnv(ctx, envOptions{oidc: true, groupMappings: []auth.GroupMapping{
			{Group: "koinon-admins", Role: store.OrgRoleAdmin},
			{Group: "staff", Role: store.OrgRoleMember},
		}})
		Expect(count(ctx, e, `SELECT count(*) FROM org_group_mappings`)).To(Equal(2))

		e.opts.groupMappings = []auth.GroupMapping{{Group: "staff", Role: store.OrgRoleViewer}}
		e.bootstrap(ctx)
		var group, role string
		Expect(e.pool.QueryRow(ctx, `SELECT group_name, role::text FROM org_group_mappings`).Scan(&group, &role)).To(Succeed())
		Expect([]string{group, role}).To(Equal([]string{"staff", "viewer"}))
	})

	Describe("with bootstrap login disabled", func() {
		It("refuses password login and ends existing bootstrap sessions", func(ctx SpecContext) {
			e := newEnv(ctx, envOptions{})
			b := e.browser()
			Expect(b.login(ctx, "admin", bootstrapPassword).StatusCode).To(Equal(http.StatusOK))
			token := b.sessionToken()

			off := newEnvOn(ctx, e, envOptions{bootstrapLoginDisabled: true})
			Expect(off.bootstrp.RevokedSessions).To(Equal(int64(1)))

			b2 := off.browser()
			Expect(b2.login(ctx, "admin", bootstrapPassword).StatusCode).To(Equal(http.StatusNotFound))
			b2.setSessionToken(token)
			_, err := b2.auth().WhoAmI(ctx, &authv1alpha1.WhoAmIRequest{})
			Expect(codeOf(err).String()).To(Equal("unauthenticated"))
		})
	})
})
