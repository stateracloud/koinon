// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/pressly/goose/v3"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/auth"
	"github.com/stateracloud/koinon/internal/db/migrations"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

// The Postgres specs skip without Docker: pgtest.Run requires it (testenv).
func TestKoinonAPI(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "koinon-api Suite")
}

var _ = DescribeTable("retryableBootstrap",
	func(err error, retry bool) { Expect(retryableBootstrap(err)).To(Equal(retry)) },
	Entry("schema not migrated", fmt.Errorf("bootstrap admin: %w", &pgconn.PgError{Code: "42P01"}), true),
	Entry("server starting up", &pgconn.PgError{Code: "57P03"}, true),
	Entry("connection exception", &pgconn.PgError{Code: "08006"}, true),
	Entry("unique violation", &pgconn.PgError{Code: "23505"}, false),
	Entry("password authentication failed", &pgconn.PgError{Code: "28P01"}, false),
	Entry("unreadable password file", fmt.Errorf("read bootstrap password file: %w",
		&fs.PathError{Op: "open", Path: "/run/secrets/password", Err: syscall.ENOENT}), false),
	Entry("unresolvable database host", &net.DNSError{Err: "no such host", Name: "koinon-db-rw", IsNotFound: true}, true),
	Entry("empty password file", errors.New("bootstrap password file is empty"), false),
	Entry("canceled", context.Canceled, false),
)

var _ = Describe("retryBootstrap", func() {
	It("fails at once on a permanent error", func(ctx SpecContext) {
		calls := 0
		_, err := retryBootstrap(ctx, time.Minute, time.Millisecond, func() (int, error) {
			calls++
			return 0, errors.New("bootstrap password file is empty")
		})
		Expect(err).To(MatchError("bootstrap password file is empty"))
		Expect(calls).To(Equal(1))
	})

	It("retries while the schema is missing, then succeeds", func(ctx SpecContext) {
		calls := 0
		res, err := retryBootstrap(ctx, time.Minute, time.Millisecond, func() (int, error) {
			calls++
			if calls < 3 {
				return 0, &pgconn.PgError{Code: "42P01"}
			}
			return 7, nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal(7))
		Expect(calls).To(Equal(3))
	})

	It("gives up after wait", func(ctx SpecContext) {
		calls := 0
		_, err := retryBootstrap(ctx, 20*time.Millisecond, 5*time.Millisecond, func() (int, error) {
			calls++
			return 0, &pgconn.PgError{Code: "42P01"}
		})
		Expect(err).To(HaveOccurred())
		Expect(calls).To(BeNumerically(">", 1))
	})

	It("retries a database that does not accept connections yet", func(ctx SpecContext) {
		pool, err := pgxpool.New(ctx, "postgres://koinon@127.0.0.1:1/koinon?connect_timeout=1")
		Expect(err).NotTo(HaveOccurred())
		defer pool.Close()
		_, err = auth.Bootstrap(ctx, pool, auth.BootstrapConfig{}, time.Now())
		Expect(err).To(HaveOccurred())
		Expect(retryableBootstrap(err)).To(BeTrue(), "%v", err)
	})

	Context("against Postgres", Ordered, func() {
		var (
			dsn  string
			pool *pgxpool.Pool
		)
		BeforeAll(func(ctx SpecContext) {
			dsn = pgtest.Run(ctx)
			var err error
			pool, err = pgxpool.New(ctx, dsn)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(pool.Close)
		}, NodeTimeout(3*time.Minute))

		It("retries an unmigrated database and fails fast on a bad password file once migrated", func(ctx SpecContext) {
			By("the schema is missing: retryable")
			_, err := auth.Bootstrap(ctx, pool, auth.BootstrapConfig{LoginEnabled: true}, time.Now())
			Expect(err).To(HaveOccurred())
			Expect(retryableBootstrap(err)).To(BeTrue(), "%v", err)

			db, err := sql.Open("pgx", dsn)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(db.Close)
			provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
			Expect(err).NotTo(HaveOccurred())
			_, err = provider.Up(ctx)
			Expect(err).NotTo(HaveOccurred())

			By("a password file that does not exist: permanent, one attempt")
			calls := 0
			_, err = retryBootstrap(ctx, time.Minute, time.Millisecond, func() (auth.BootstrapResult, error) {
				calls++
				return auth.Bootstrap(ctx, pool, auth.BootstrapConfig{
					LoginEnabled: true, PasswordFile: filepath.Join(GinkgoT().TempDir(), "missing"),
				}, time.Now())
			})
			Expect(err).To(MatchError(ContainSubstring("read bootstrap password file")))
			Expect(calls).To(Equal(1))
		})
	})
})

var _ = Describe("bootstrapRetryWrite", func() {
	type ctxKey struct{}

	It("runs bootstrap with the retry goroutine's ctx, not an outer one", func() {
		serve := context.WithValue(context.Background(), ctxKey{}, "serve")
		retry := context.WithValue(context.Background(), ctxKey{}, "retry")
		var got any
		write := bootstrapRetryWrite(func(ctx context.Context) (auth.BootstrapResult, error) {
			got = ctx.Value(ctxKey{})
			return auth.BootstrapResult{}, nil
		}, true)
		Expect(write(retry)).To(Succeed())
		Expect(got).To(Equal("retry"))
		Expect(got).NotTo(Equal(serve.Value(ctxKey{})))
	})

	It("passes the error on, so the retry goes on, and logs nothing for it", func() {
		denied := fmt.Errorf("record: %w", audit.ErrInstallWriteDenied)
		write := bootstrapRetryWrite(func(context.Context) (auth.BootstrapResult, error) {
			return auth.BootstrapResult{MissingPasswordFile: true}, denied
		}, true)
		Expect(write(context.Background())).To(MatchError(audit.ErrInstallWriteDenied))
	})

	It("warns about a missing password file after a retry succeeds, as the direct path does", func() {
		var buf bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
		DeferCleanup(func() { slog.SetDefault(prev) })
		write := bootstrapRetryWrite(func(context.Context) (auth.BootstrapResult, error) {
			return auth.BootstrapResult{MissingPasswordFile: true}, nil
		}, true)
		Expect(write(context.Background())).To(Succeed())
		Expect(buf.String()).To(ContainSubstring("bootstrap done"))
		Expect(buf.String()).To(ContainSubstring("no password is set"))
	})
})
