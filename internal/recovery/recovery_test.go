// SPDX-License-Identifier: Apache-2.0

package recovery_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/auth"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/recovery"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
	"github.com/stateracloud/koinon/internal/tokens"
)

// The password is fake and obviously so.
const fakeOldPassword = "koinon_test_not-a-real-password-old"

// The requester stands for the Kubernetes identity the request handler passes as the actor.
var requester = audit.System("koinon-reconciler:recovery-test")

// These specs skip without Docker (pgtest requires it); CI runs them.
var _ = Describe("platform admin recovery actions (issue #512, ADR 0110 decision 6)", func() {
	var pool *pgxpool.Pool

	BeforeEach(func(ctx SpecContext) {
		pool = pgtest.Start(ctx)
		pw := filepath.Join(GinkgoT().TempDir(), "password")
		Expect(os.WriteFile(pw, []byte(fakeOldPassword+"\n"), 0o600)).To(Succeed())
		_, err := auth.Bootstrap(ctx, pool, auth.BootstrapConfig{Username: "admin", PasswordFile: pw, LoginEnabled: true}, time.Now())
		Expect(err).NotTo(HaveOccurred())
	})

	userID := func(ctx context.Context) (id string) {
		GinkgoHelper()
		Expect(pool.QueryRow(ctx, `SELECT user_id::text FROM local_credentials WHERE username = 'admin'`).Scan(&id)).To(Succeed())
		return id
	}

	reset := func(ctx context.Context, username string) (res recovery.ResetResult, err error) {
		_, err = audit.InTx(ctx, pool, func(q *store.Queries, b audit.Batch) error {
			res, err = recovery.ResetPassword(ctx, q, b, requester, username, time.Now())
			return err
		})
		return res, err
	}
	mint := func(ctx context.Context, svc *tokens.Service, p recovery.MintTokenParams) (c tokens.Created, err error) {
		_, err = audit.InTx(ctx, pool, func(q *store.Queries, b audit.Batch) error {
			c, err = recovery.MintAdminToken(ctx, svc, q, b, requester, p)
			return err
		})
		return c, err
	}

	Describe("ResetPassword", func() {
		It("sets a one-time password, requires a change, ends the sessions and audits it", func(ctx SpecContext) {
			uid := userID(ctx)
			var oldHash string
			Expect(pool.QueryRow(ctx, `UPDATE local_credentials SET must_change_password = false WHERE user_id = $1 RETURNING password_hash`, uid).Scan(&oldHash)).To(Succeed())
			for range 2 {
				_, err := pool.Exec(ctx, `INSERT INTO sessions (token_hash, user_id, auth_method, created_at, last_seen_at, idle_expires_at, expires_at)
					VALUES (sha256(gen_random_uuid()::text::bytea), $1, 'password', now(), now(), now() + interval '1 hour', now() + interval '8 hours')`, uid)
				Expect(err).NotTo(HaveOccurred())
			}

			res, err := reset(ctx, "admin")
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RevokedSessions).To(BeEquivalentTo(2))
			Expect(res.Password).NotTo(BeEmpty())
			Expect(res.Password).NotTo(Equal(fakeOldPassword))

			var hash string
			var must bool
			Expect(pool.QueryRow(ctx, `SELECT password_hash, must_change_password FROM local_credentials WHERE user_id = $1`, uid).Scan(&hash, &must)).To(Succeed())
			Expect(must).To(BeTrue())
			Expect(hash).NotTo(Equal(oldHash))
			ok, err := auth.VerifyPassword(hash, res.Password)
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeTrue())
			var live int
			Expect(pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE user_id = $1 AND revoked_at IS NULL`, uid).Scan(&live)).To(Succeed())
			Expect(live).To(BeZero())

			var kind, id, raw string
			Expect(pool.QueryRow(ctx, `SELECT actor_kind, actor_id, summary::text FROM audit_events WHERE action = 'auth.password.reset' AND target_id = $1`, uid).
				Scan(&kind, &id, &raw)).To(Succeed())
			Expect([]string{kind, id}).To(Equal([]string{requester.Kind, requester.ID}))
			Expect(raw).NotTo(ContainSubstring(res.Password))
			Expect(raw).NotTo(ContainSubstring(hash))
		})

		It("rolls back with the caller's transaction", func(ctx SpecContext) {
			uid := userID(ctx)
			var oldHash string
			Expect(pool.QueryRow(ctx, `SELECT password_hash FROM local_credentials WHERE user_id = $1`, uid).Scan(&oldHash)).To(Succeed())
			_, err := audit.InTx(ctx, pool, func(q *store.Queries, b audit.Batch) error {
				_, err := recovery.ResetPassword(ctx, q, b, requester, "admin", time.Now())
				Expect(err).NotTo(HaveOccurred())
				return errors.New("a later step of the request failed")
			})
			Expect(err).To(HaveOccurred())
			var hash string
			var n int
			Expect(pool.QueryRow(ctx, `SELECT password_hash FROM local_credentials WHERE user_id = $1`, uid).Scan(&hash)).To(Succeed())
			Expect(hash).To(Equal(oldHash))
			Expect(pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action = 'auth.password.reset'`).Scan(&n)).To(Succeed())
			Expect(n).To(BeZero())
		})

		It("gives a different password every time", func(ctx SpecContext) {
			a, err := reset(ctx, "admin")
			Expect(err).NotTo(HaveOccurred())
			b, err := reset(ctx, "admin")
			Expect(err).NotTo(HaveOccurred())
			Expect(a.Password).NotTo(Equal(b.Password))
		})

		It("refuses an unknown user and writes nothing", func(ctx SpecContext) {
			_, err := reset(ctx, "nobody")
			Expect(err).To(MatchError(recovery.ErrNoLocalUser))
			_, err = reset(ctx, "")
			Expect(err).To(HaveOccurred())
			var n int
			Expect(pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action = 'auth.password.reset'`).Scan(&n)).To(Succeed())
			Expect(n).To(BeZero())
		})
	})

	Describe("MintAdminToken", func() {
		It("mints a hashed token for a platform admin, audited, and refuses a longer life", func(ctx SpecContext) {
			uid := userID(ctx)
			svc := tokens.New(pool, nil)
			created, err := mint(ctx, svc, recovery.MintTokenParams{
				Username: "admin", Org: "default", Name: "restore-drill", TTL: 2 * time.Hour,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(created.Secret).To(HavePrefix("koinon_"))

			var hash []byte
			var creator string
			Expect(pool.QueryRow(ctx, `SELECT token_hash, created_by::text FROM api_tokens WHERE name = 'restore-drill'`).Scan(&hash, &creator)).To(Succeed())
			Expect(creator).To(Equal(uid))
			Expect(string(hash)).NotTo(ContainSubstring(created.Secret), "only the hash is stored")
			var kind, actor, raw string
			Expect(pool.QueryRow(ctx, `SELECT actor_kind, actor_id, summary::text FROM audit_events WHERE action = 'token.create' AND target_id = $1`, created.ID).Scan(&kind, &actor, &raw)).To(Succeed())
			Expect([]string{kind, actor}).To(Equal([]string{requester.Kind, requester.ID}))
			Expect(raw).NotTo(ContainSubstring(created.Secret))

			_, err = mint(ctx, svc, recovery.MintTokenParams{
				Username: "admin", Org: "default", Name: "too-long", TTL: recovery.MaxTokenTTL + time.Hour,
			})
			Expect(err).To(MatchError(ContainSubstring("at most")))
		})

		It("refuses a user who is not a platform admin, an unknown user and an unknown org", func(ctx SpecContext) {
			svc := tokens.New(pool, nil)
			_, err := pool.Exec(ctx, `DELETE FROM platform_admins WHERE user_id = $1`, userID(ctx))
			Expect(err).NotTo(HaveOccurred())
			_, err = mint(ctx, svc, recovery.MintTokenParams{Username: "admin", Org: "default", Name: "n"})
			Expect(err).To(MatchError(recovery.ErrNotPlatformAdmin))
			_, err = mint(ctx, svc, recovery.MintTokenParams{Username: "nobody", Org: "default", Name: "n"})
			Expect(err).To(MatchError(recovery.ErrNoLocalUser))
			_, err = pool.Exec(ctx, `INSERT INTO platform_admins (user_id, source) SELECT user_id, 'bootstrap' FROM local_credentials WHERE username = 'admin'`)
			Expect(err).NotTo(HaveOccurred())
			_, err = mint(ctx, svc, recovery.MintTokenParams{Username: "admin", Org: "no-such-org", Name: "n"})
			Expect(err).To(MatchError(recovery.ErrNoOrg))
			_, err = mint(ctx, svc, recovery.MintTokenParams{Username: "admin", Org: "0190f2a4-0000-7000-8000-000000000000", Name: "n"})
			Expect(err).To(MatchError(recovery.ErrNoOrg), "an unknown org ID")

			var orgID string
			Expect(pool.QueryRow(ctx, `SELECT id::text FROM orgs WHERE slug = 'default'`).Scan(&orgID)).To(Succeed())
			_, err = mint(ctx, svc, recovery.MintTokenParams{Username: "admin", Org: orgID, Name: "by-id"})
			Expect(err).NotTo(HaveOccurred(), "a live org ID")
			_, err = pool.Exec(ctx, `UPDATE orgs SET deleted_at = now() WHERE id = $1`, orgID)
			Expect(err).NotTo(HaveOccurred())
			_, err = mint(ctx, svc, recovery.MintTokenParams{Username: "admin", Org: orgID, Name: "n"})
			Expect(err).To(MatchError(recovery.ErrNoOrg), "a deleted org ID")
			var n int
			Expect(pool.QueryRow(ctx, `SELECT count(*) FROM api_tokens WHERE name = 'n'`).Scan(&n)).To(Succeed())
			Expect(n).To(BeZero())
		})

		It("stops working, with its children, when the user loses the platform admin grant, though the user owns the org", func(ctx SpecContext) {
			uid := userID(ctx)
			svc := tokens.New(pool, nil)
			var role string
			Expect(pool.QueryRow(ctx, `SELECT m.role::text FROM org_memberships m JOIN orgs o ON o.id = m.org_id WHERE o.slug = 'default' AND m.user_id = $1`, uid).Scan(&role)).To(Succeed())
			Expect(role).To(Equal("owner"), "the bootstrap admin owns the default org")

			created, err := mint(ctx, svc, recovery.MintTokenParams{Username: "admin", Org: "default", Name: "break-glass"})
			Expect(err).NotTo(HaveOccurred())
			p, err := svc.Authenticate(ctx, created.Secret)
			Expect(err).NotTo(HaveOccurred())
			Expect(p.RequiresPlatformAdmin).To(BeTrue())
			child, err := svc.Create(ctx, p.Org, requester, tokens.Creator{User: p.CreatedBy, Role: p.Role, Parent: &p}, "child", store.OrgRoleOwner, 0)
			Expect(err).NotTo(HaveOccurred())
			// An ordinary owner token of the same user keeps the owner rule.
			plain, err := svc.Create(ctx, p.Org, requester, tokens.Creator{User: p.CreatedBy, Role: store.OrgRoleOwner}, "plain", store.OrgRoleOwner, 0)
			Expect(err).NotTo(HaveOccurred())

			_, err = pool.Exec(ctx, `DELETE FROM platform_admins WHERE user_id = $1`, uid)
			Expect(err).NotTo(HaveOccurred())
			var n int
			Expect(pool.QueryRow(ctx, `SELECT count(*) FROM platform_admins WHERE user_id = $1`, uid).Scan(&n)).To(Succeed())
			Expect(n).To(BeZero())
			for i, secret := range []string{created.Secret, child.Secret} {
				_, err = svc.Authenticate(ctx, secret)
				var ae *tokens.AuthError
				Expect(errors.As(err, &ae)).To(BeTrue(), "token %d", i)
				Expect(ae.Reason).To(Equal("its creator is no longer a platform admin"))
			}
			_, err = svc.Authenticate(ctx, plain.Secret)
			Expect(err).NotTo(HaveOccurred(), "an ordinary owner token still works for an owner")

			_, err = pool.Exec(ctx, `INSERT INTO platform_admins (user_id, source) VALUES ($1, 'bootstrap')`, uid)
			Expect(err).NotTo(HaveOccurred())
			_, err = svc.Authenticate(ctx, created.Secret)
			Expect(err).NotTo(HaveOccurred(), "the grant back makes it work again")
		})
	})
})
