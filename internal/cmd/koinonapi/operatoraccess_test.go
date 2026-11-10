// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/auth"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

// The password strings are fake and obviously so.
const (
	fakeOldPassword = "koinon_test_not-a-real-password-old"
	fakeNewPassword = "koinon_test_not-a-real-password-new"
)

var _ = Describe("koinon-api users and tokens (break-glass, issue #512)", func() {
	runIn := func(stdin string, args ...string) (stdout, stderr string, err error) {
		root := NewRootCommand()
		root.SilenceUsage, root.SilenceErrors = true, true
		var out, errOut bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&errOut)
		root.SetIn(strings.NewReader(stdin))
		root.SetArgs(args)
		err = root.ExecuteContext(context.Background())
		return out.String(), errOut.String(), err
	}

	Describe("flags", func() {
		BeforeEach(func() {
			GinkgoT().Setenv("DATABASE_URL", "")
			GinkgoT().Setenv("KOINON_DATABASE_URL", "")
		})

		It("reset-password needs the database, the username and a password before it connects", func() {
			_, _, err := runIn("", "users", "reset-password", "--username", "admin")
			Expect(err).To(MatchError(ContainSubstring("--database-url is required")))
			const db = "postgres://koinon_test:not-a-real-token@127.0.0.1:1/koinon"
			_, _, err = runIn("", "users", "reset-password", "--database-url", db)
			Expect(err).To(MatchError(ContainSubstring("--username is required")))
			_, _, err = runIn("", "users", "reset-password", "--database-url", db, "--username", "admin")
			Expect(err).To(MatchError(ContainSubstring("no password on stdin")))
		})

		It("reads a password from stdin without its line break, and never from an argument", func() {
			p, err := readNewPassword(strings.NewReader(fakeNewPassword+"\n"), "")
			Expect(err).NotTo(HaveOccurred())
			Expect(p).To(Equal(fakeNewPassword))
			_, _, err = runIn("", "users", "reset-password", "--username", "admin", "--password", fakeNewPassword)
			Expect(err).To(MatchError(ContainSubstring("unknown flag")))
			_, err = readNewPassword(strings.NewReader(strings.Repeat("x", maxPasswordStdin+1)), "")
			Expect(err).To(MatchError(ContainSubstring("too long")))
		})

		It("tokens create refuses a missing field and a role above owner", func() {
			const db = "postgres://koinon_test:not-a-real-token@127.0.0.1:1/koinon"
			_, _, err := runIn("", "tokens", "create", "--database-url", db, "--org", "default")
			Expect(err).To(MatchError(ContainSubstring("--org, --as-user and --name are required")))
			_, _, err = runIn("", "tokens", "create", "--database-url", db, "--org", "default", "--as-user", "admin", "--name", "n", "--role", "member")
			Expect(err).To(MatchError(ContainSubstring("--role must be admin or owner")))
		})
	})

	// These specs skip without Docker (pgtest requires it); CI runs them.
	Context("against Postgres", func() {
		var (
			pool *pgxpool.Pool
			dsn  string
		)
		BeforeEach(func(ctx SpecContext) {
			pool = pgtest.Start(ctx)
			dsn = pool.Config().ConnString()
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

		It("resets the password, requires a change, ends the sessions and audits it", func(ctx SpecContext) {
			uid := userID(ctx)
			var oldHash string
			Expect(pool.QueryRow(ctx, `UPDATE local_credentials SET must_change_password = false WHERE user_id = $1 RETURNING password_hash`, uid).Scan(&oldHash)).To(Succeed())
			for range 2 {
				_, err := pool.Exec(ctx, `INSERT INTO sessions (token_hash, user_id, auth_method, created_at, last_seen_at, idle_expires_at, expires_at)
					VALUES (sha256(gen_random_uuid()::text::bytea), $1, 'password', now(), now(), now() + interval '1 hour', now() + interval '8 hours')`, uid)
				Expect(err).NotTo(HaveOccurred())
			}

			out, _, err := runIn(fakeNewPassword+"\n", "users", "reset-password", "--database-url", dsn, "--username", "admin")
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring("2 session(s) ended"))
			Expect(out).NotTo(ContainSubstring(fakeNewPassword))

			var hash string
			var must bool
			Expect(pool.QueryRow(ctx, `SELECT password_hash, must_change_password FROM local_credentials WHERE user_id = $1`, uid).Scan(&hash, &must)).To(Succeed())
			Expect(must).To(BeTrue())
			Expect(hash).NotTo(Equal(oldHash))
			ok, err := auth.VerifyPassword(hash, fakeNewPassword)
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeTrue())
			var live int
			Expect(pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE user_id = $1 AND revoked_at IS NULL`, uid).Scan(&live)).To(Succeed())
			Expect(live).To(BeZero())

			var kind, id, raw string
			Expect(pool.QueryRow(ctx, `SELECT actor_kind, actor_id, summary::text FROM audit_events WHERE action = 'auth.password.reset' AND target_id = $1`, uid).
				Scan(&kind, &id, &raw)).To(Succeed())
			Expect(kind).To(Equal("cli"))
			Expect(id).To(Equal("users reset-password"))
			Expect(raw).NotTo(ContainSubstring(fakeNewPassword))
			Expect(raw).NotTo(ContainSubstring(hash))
		})

		It("re-applies a password file, and refuses an unknown user and a short password", func(ctx SpecContext) {
			pw := filepath.Join(GinkgoT().TempDir(), "secret")
			Expect(os.WriteFile(pw, []byte(fakeNewPassword), 0o600)).To(Succeed())
			_, _, err := runIn("", "users", "reset-password", "--database-url", dsn, "--username", "admin", "--password-file", pw)
			Expect(err).NotTo(HaveOccurred())
			var hash string
			Expect(pool.QueryRow(ctx, `SELECT password_hash FROM local_credentials WHERE username = 'admin'`).Scan(&hash)).To(Succeed())
			Expect(auth.VerifyPassword(hash, fakeNewPassword)).To(BeTrue())

			_, _, err = runIn(fakeNewPassword, "users", "reset-password", "--database-url", dsn, "--username", "nobody")
			Expect(err).To(MatchError(auth.ErrNoLocalUser))
			_, _, err = runIn("short", "users", "reset-password", "--database-url", dsn, "--username", "admin")
			Expect(err).To(MatchError(ContainSubstring("at least")))
		})

		It("creates an admin token for a platform admin, audited, and revokes it", func(ctx SpecContext) {
			uid := userID(ctx)
			out, errOut, err := runIn("", "tokens", "create", "--database-url", dsn, "--org", "default", "--as-user", "admin",
				"--name", "restore-drill", "--role", "admin", "--expires-in", "24h")
			Expect(err).NotTo(HaveOccurred())
			secret := strings.TrimSpace(out)
			Expect(secret).To(HavePrefix("koinon_"))
			Expect(errOut).NotTo(ContainSubstring(secret))

			var hash []byte
			var creator, id string
			Expect(pool.QueryRow(ctx, `SELECT token_hash, created_by::text, id::text FROM api_tokens WHERE name = 'restore-drill'`).Scan(&hash, &creator, &id)).To(Succeed())
			Expect(creator).To(Equal(uid))
			Expect(string(hash)).NotTo(ContainSubstring(secret), "only the hash is stored")
			var kind, actor string
			Expect(pool.QueryRow(ctx, `SELECT actor_kind, actor_id FROM audit_events WHERE action = 'token.create' AND target_id = $1`, id).Scan(&kind, &actor)).To(Succeed())
			Expect([]string{kind, actor}).To(Equal([]string{"cli", "tokens create"}))

			_, _, err = runIn("", "tokens", "revoke", "--database-url", dsn, "--org", "default", "--id", id)
			Expect(err).NotTo(HaveOccurred())
			var revoked bool
			Expect(pool.QueryRow(ctx, `SELECT revoked_at IS NOT NULL FROM api_tokens WHERE id = $1`, id).Scan(&revoked)).To(Succeed())
			Expect(revoked).To(BeTrue())
		})
	})
})
