// SPDX-License-Identifier: Apache-2.0

package recovery_test

import (
	"context"
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

			res, err := recovery.ResetPassword(ctx, pool, requester, "admin", time.Now())
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

		It("gives a different password every time", func(ctx SpecContext) {
			a, err := recovery.ResetPassword(ctx, pool, requester, "admin", time.Now())
			Expect(err).NotTo(HaveOccurred())
			b, err := recovery.ResetPassword(ctx, pool, requester, "admin", time.Now())
			Expect(err).NotTo(HaveOccurred())
			Expect(a.Password).NotTo(Equal(b.Password))
		})

		It("refuses an unknown user and writes nothing", func(ctx SpecContext) {
			_, err := recovery.ResetPassword(ctx, pool, requester, "nobody", time.Now())
			Expect(err).To(MatchError(recovery.ErrNoLocalUser))
			_, err = recovery.ResetPassword(ctx, pool, requester, "", time.Now())
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
			q := store.New(pool)
			created, err := recovery.MintAdminToken(ctx, svc, q, requester, recovery.MintTokenParams{
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

			_, err = recovery.MintAdminToken(ctx, svc, q, requester, recovery.MintTokenParams{
				Username: "admin", Org: "default", Name: "too-long", TTL: recovery.MaxTokenTTL + time.Hour,
			})
			Expect(err).To(MatchError(ContainSubstring("at most")))
		})

		It("refuses a user who is not a platform admin, an unknown user and an unknown org", func(ctx SpecContext) {
			svc := tokens.New(pool, nil)
			q := store.New(pool)
			_, err := pool.Exec(ctx, `DELETE FROM platform_admins WHERE user_id = $1`, userID(ctx))
			Expect(err).NotTo(HaveOccurred())
			_, err = recovery.MintAdminToken(ctx, svc, q, requester, recovery.MintTokenParams{Username: "admin", Org: "default", Name: "n"})
			Expect(err).To(MatchError(recovery.ErrNotPlatformAdmin))
			_, err = recovery.MintAdminToken(ctx, svc, q, requester, recovery.MintTokenParams{Username: "nobody", Org: "default", Name: "n"})
			Expect(err).To(MatchError(recovery.ErrNoLocalUser))
			_, err = pool.Exec(ctx, `INSERT INTO platform_admins (user_id, source) SELECT user_id, 'bootstrap' FROM local_credentials WHERE username = 'admin'`)
			Expect(err).NotTo(HaveOccurred())
			_, err = recovery.MintAdminToken(ctx, svc, q, requester, recovery.MintTokenParams{Username: "admin", Org: "no-such-org", Name: "n"})
			Expect(err).To(MatchError(recovery.ErrNoOrg))
		})
	})
})
