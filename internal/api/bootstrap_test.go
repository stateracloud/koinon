// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sync"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/auth"
	"github.com/stateracloud/koinon/internal/db/store"
	authv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/auth/v1alpha1"
)

var probeSeq atomic.Int64

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

	Describe("audit (ADR 0100)", func() {
		install := func(ctx context.Context, e *env, action string) int {
			return count(ctx, e, `SELECT count(*) FROM install_audit_events WHERE action = $1 AND actor_kind = 'system' AND actor_id = 'koinon-api:bootstrap'`, action)
		}
		orgEvents := func(ctx context.Context, e *env, action string) int {
			return count(ctx, e, `SELECT count(*) FROM audit_events a JOIN orgs o ON o.id = a.org_id
				WHERE o.slug = 'default' AND a.action = $1 AND a.actor_kind = 'system' AND a.actor_id = 'koinon-api:bootstrap'`, action)
		}

		It("writes platform_admin.bootstrap on the first start, targeting the default org, and nothing in a personal org", func(ctx SpecContext) {
			e := newEnv(ctx, envOptions{})
			Expect(install(ctx, e, "platform_admin.bootstrap")).To(Equal(1))
			Expect(count(ctx, e, `SELECT count(*) FROM install_audit_events i JOIN orgs o ON o.id::text = i.target_id
				WHERE i.action = 'platform_admin.bootstrap' AND o.slug = 'default'`)).To(Equal(1))
			Expect(install(ctx, e, "platform_admin.grant")).To(BeZero(), "the creation is the one event of the first grant")
			Expect(count(ctx, e, `SELECT count(*) FROM audit_events a JOIN orgs o ON o.id = a.org_id WHERE o.kind = 'personal' AND a.action LIKE 'platform_admin.%'`)).To(BeZero())
		})

		It("writes nothing on a restart with the grant present, and platform_admin.grant when it was removed", func(ctx SpecContext) {
			e := newEnv(ctx, envOptions{})
			total := func() int {
				return count(ctx, e, `SELECT (SELECT count(*) FROM install_audit_events) + (SELECT count(*) FROM audit_events)`)
			}
			before := total()
			e.bootstrap(ctx)
			Expect(total()).To(Equal(before), "a restart changes nothing, so it records nothing")

			_, err := e.pool.Exec(ctx, `DELETE FROM platform_admins`)
			Expect(err).NotTo(HaveOccurred())
			e.bootstrap(ctx)
			Expect(install(ctx, e, "platform_admin.grant")).To(Equal(1))
			Expect(install(ctx, e, "platform_admin.bootstrap")).To(Equal(1), "the admin was not created again")
			e.bootstrap(ctx)
			Expect(install(ctx, e, "platform_admin.grant")).To(Equal(1))
		})

		It("records a change to the default org in its chain, only when it changes something", func(ctx SpecContext) {
			e := newEnv(ctx, envOptions{oidc: true, groupMappings: []auth.GroupMapping{{Group: "staff", Role: store.OrgRoleMember}}})
			Expect(orgEvents(ctx, e, "org.group_mappings.set")).To(Equal(1), "the first start adds the mapping")
			e.bootstrap(ctx)
			Expect(orgEvents(ctx, e, "org.group_mappings.set")).To(Equal(1))

			e.opts.groupMappings = []auth.GroupMapping{{Group: "staff", Role: store.OrgRoleViewer}}
			e.opts.defaultOrgMode = store.IdentityModeIdp
			e.bootstrap(ctx)
			Expect(orgEvents(ctx, e, "org.group_mappings.set")).To(Equal(2))
			Expect(orgEvents(ctx, e, "org.identity_mode.set")).To(Equal(1))
			e.bootstrap(ctx)
			Expect(orgEvents(ctx, e, "org.group_mappings.set")).To(Equal(2))
			Expect(orgEvents(ctx, e, "org.identity_mode.set")).To(Equal(1))
			Expect(count(ctx, e, `SELECT count(*) FROM audit_events WHERE action = 'org.identity_mode.set' AND NOT (summary ? 'install_event_id')`)).To(Equal(1),
				"no install event caused a later change")
		})

		It("points the default org's events back at the install event of the same start", func(ctx SpecContext) {
			e := newEnv(ctx, envOptions{oidc: true, groupMappings: []auth.GroupMapping{{Group: "staff", Role: store.OrgRoleMember}}})
			Expect(count(ctx, e, `SELECT count(*) FROM audit_events a JOIN install_audit_events i ON i.id::text = a.summary->>'install_event_id'
				WHERE a.action = 'org.group_mappings.set' AND i.action = 'platform_admin.bootstrap'`)).To(Equal(1))
		})

		It("records the revocation of bootstrap sessions when bootstrap login is switched off", func(ctx SpecContext) {
			e := newEnv(ctx, envOptions{})
			b := e.browser()
			Expect(b.login(ctx, "admin", bootstrapPassword).StatusCode).To(Equal(http.StatusOK))
			off := newEnvOn(ctx, e, envOptions{bootstrapLoginDisabled: true})
			Expect(orgEvents(ctx, off, "org.bootstrap_sessions.revoked")).To(Equal(1))
			off.bootstrap(ctx)
			Expect(orgEvents(ctx, off, "org.bootstrap_sessions.revoked")).To(Equal(1), "nothing left to revoke")
		})

		It("revokes bootstrap sessions while the install write is denied, and writes the revocation event once (ADR 0100 decision 3)", func(ctx SpecContext) {
			e := newEnv(ctx, envOptions{})
			b := e.browser()
			Expect(b.login(ctx, "admin", bootstrapPassword).StatusCode).To(Equal(http.StatusOK))

			By("a removed platform admin grant makes the next start want an install event, which the role cannot write yet")
			_, err := e.pool.Exec(ctx, `DELETE FROM platform_admins`)
			Expect(err).NotTo(HaveOccurred())
			role := fmt.Sprintf("bootstrap_probe_%d", probeSeq.Add(1))
			for _, stmt := range []string{
				`CREATE ROLE ` + role + ` NOLOGIN`,
				`GRANT USAGE ON SCHEMA public TO ` + role,
				`GRANT ALL ON ALL TABLES IN SCHEMA public TO ` + role,
				`GRANT ALL ON ALL SEQUENCES IN SCHEMA public TO ` + role,
				`REVOKE ALL ON install_audit_events, install_audit_chain_head FROM ` + role,
			} {
				_, err := e.pool.Exec(ctx, stmt)
				Expect(err).NotTo(HaveOccurred(), stmt)
			}
			cfg := e.pool.Config().Copy()
			prev := cfg.AfterConnect
			cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
				if prev != nil {
					if err := prev(ctx, c); err != nil {
						return err
					}
				}
				_, err := c.Exec(ctx, `SET ROLE `+role)
				return err
			}
			restricted, err := pgxpool.NewWithConfig(ctx, cfg)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(restricted.Close)

			res, err := auth.Bootstrap(ctx, restricted, auth.BootstrapConfig{}, timeNow())
			Expect(err).To(MatchError(audit.ErrInstallWriteDenied))
			Expect(res.RevokedSessions).To(Equal(int64(1)), "the revocation does not wait for the install writer")
			Expect(count(ctx, e, `SELECT count(*) FROM sessions WHERE auth_method = 'password' AND revoked_at IS NULL`)).To(BeZero())
			Expect(orgEvents(ctx, e, "org.bootstrap_sessions.revoked")).To(Equal(1))
			Expect(count(ctx, e, `SELECT count(*) FROM audit_events WHERE action = 'org.bootstrap_sessions.revoked' AND NOT (summary ? 'install_event_id')`)).To(Equal(1),
				"no install event exists to point at yet")
			Expect(count(ctx, e, `SELECT count(*) FROM platform_admins`)).To(BeZero(), "the rest of the transaction rolled back")
			Expect(install(ctx, e, "platform_admin.grant")).To(BeZero())

			By("the grant arrives and the retry succeeds without a second revocation event")
			_, err = e.pool.Exec(ctx, `GRANT ALL ON ALL TABLES IN SCHEMA public TO `+role)
			Expect(err).NotTo(HaveOccurred())
			res, err = auth.Bootstrap(ctx, restricted, auth.BootstrapConfig{}, timeNow())
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RevokedSessions).To(BeZero())
			Expect(install(ctx, e, "platform_admin.grant")).To(Equal(1))
			Expect(count(ctx, e, `SELECT count(*) FROM platform_admins`)).To(Equal(1))
			Expect(orgEvents(ctx, e, "org.bootstrap_sessions.revoked")).To(Equal(1), "written once")
		})
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
