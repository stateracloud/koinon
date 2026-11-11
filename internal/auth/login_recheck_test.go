// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

// The password is fake and obviously so.
const fakeLoginPassword = "koinon_test_not-a-real-password-login"

// A login checks the password outside a transaction and creates its session in one; a password reset
// in between must not leave a session that the reset did not revoke (PR #560 review).
// These specs skip without Docker (pgtest requires it); CI runs them.
var _ = Describe("password login against a concurrent reset", func() {
	var (
		pool     *pgxpool.Pool
		verified store.LocalCredential
	)

	BeforeEach(func(ctx SpecContext) {
		pool = pgtest.Start(ctx)
		pw := filepath.Join(GinkgoT().TempDir(), "password")
		Expect(os.WriteFile(pw, []byte(fakeLoginPassword+"\n"), 0o600)).To(Succeed())
		_, err := Bootstrap(ctx, pool, BootstrapConfig{Username: "admin", PasswordFile: pw, LoginEnabled: true}, time.Now())
		Expect(err).NotTo(HaveOccurred())
		verified, err = store.New(pool).GetLocalCredentialByUsername(ctx, "admin")
		Expect(err).NotTo(HaveOccurred())
	})

	recheck := func(ctx SpecContext) error {
		return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error { return recheckCredential(ctx, store.New(tx), verified) })
	}

	It("lets an unchanged credential through", func(ctx SpecContext) {
		Expect(recheck(ctx)).To(Succeed())
	})

	It("refuses the session when the password changed after the check", func(ctx SpecContext) {
		hash, err := HashPasswordContext(ctx, "koinon_test_not-a-real-password-new")
		Expect(err).NotTo(HaveOccurred())
		Expect(store.New(pool).SetLocalPassword(ctx, store.SetLocalPasswordParams{
			UserID: verified.UserID, PasswordHash: hash, MustChangePassword: true,
			PasswordChangedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		})).To(Succeed())
		Expect(recheck(ctx)).To(MatchError(errCredentialChanged))
	})

	It("holds a reset back until the login's session transaction ends", func(ctx SpecContext) {
		tx, err := pool.Begin(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = tx.Rollback(ctx) }()
		Expect(recheckCredential(ctx, store.New(tx), verified)).To(Succeed())

		// The reset's UPDATE waits on the share lock; with a short lock_timeout it fails instead.
		err = pgx.BeginFunc(ctx, pool, func(r pgx.Tx) error {
			if _, err := r.Exec(ctx, `SET LOCAL lock_timeout = '200ms'`); err != nil {
				return err
			}
			_, err := r.Exec(ctx, `UPDATE local_credentials SET must_change_password = true WHERE user_id = $1`, verified.UserID)
			return err
		})
		var pgErr *pgconn.PgError
		Expect(errors.As(err, &pgErr)).To(BeTrue(), "the reset fails on the lock: %v", err)
		Expect(pgErr.Code).To(Equal("55P03"), "lock_not_available")
	})
})
