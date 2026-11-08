// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
	"github.com/stateracloud/koinon/internal/testing/testenv"
)

var _ = Describe("koinon-api audit verify", func() {
	It("walks the install chain by default, alone with --install, and reports a break", func(ctx SpecContext) {
		pool := pgtest.Start(ctx)
		for range 3 {
			_, err := audit.InTx(ctx, pool, func(_ *store.Queries, b audit.Batch) error {
				b.Install(audit.InstallEvent{
					Actor: audit.System("koinon-test:spec"), Action: "platform_price.set", TargetKind: "platform_price", TargetID: "cli",
				})
				return nil
			})
			Expect(err).NotTo(HaveOccurred())
		}

		By("an intact chain: the install line comes first, both modes exit zero")
		var out bytes.Buffer
		Expect(verifyAudit(ctx, pool, "", false, &out)).To(Succeed())
		Expect(out.String()).To(HavePrefix("ok      install chain: 3 events"))
		out.Reset()
		Expect(verifyAudit(ctx, pool, "", true, &out)).To(Succeed())
		Expect(out.String()).To(Equal("ok      install chain: 3 events\n"))

		By("--install and --org together are refused")
		Expect(verifyAudit(ctx, pool, "00000000-0000-0000-0000-000000000001", true, &out)).To(MatchError(ContainSubstring("exclusive")))

		By("a tampered install row breaks the default run and --install, and an --org run does not look at it")
		tx, err := pool.Begin(ctx)
		Expect(err).NotTo(HaveOccurred())
		_, err = tx.Exec(ctx, `SET LOCAL session_replication_role = replica`)
		Expect(err).NotTo(HaveOccurred())
		// audit-lint:allow a tamper test: forges a row past the triggers so "audit verify" has a break to report
		_, err = tx.Exec(ctx, `UPDATE install_audit_events SET target_id = 'forged' WHERE chain_seq = 2`)
		Expect(err).NotTo(HaveOccurred())
		Expect(tx.Commit(ctx)).To(Succeed())

		out.Reset()
		Expect(verifyAudit(ctx, pool, "", false, &out)).To(MatchError(audit.ErrBroken))
		Expect(out.String()).To(ContainSubstring("BROKEN  install chain: first broken link at chain_seq 2"))
		out.Reset()
		Expect(verifyAudit(ctx, pool, "", true, &out)).To(MatchError(audit.ErrBroken))
		Expect(out.String()).To(ContainSubstring("chain_seq 2"))
	}, SpecTimeout(3*time.Minute))
})

// lockedBuffer is a bytes.Buffer that the retry goroutine and the spec can use together.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

var _ = Describe("startup install audit writes before the writer role is granted", func() {
	denied := fmt.Errorf("audit: record install event x: %w: permission denied for table install_audit_events", audit.ErrInstallWriteDenied)

	It("retries a refused write until it is recorded, and stops with the server", func(ctx SpecContext) {
		calls := 0
		installWriteLoop(ctx, "probe", "", time.Millisecond, func(context.Context) error {
			calls++
			if calls < 3 {
				return denied
			}
			return nil
		})
		Expect(calls).To(Equal(3))

		stopped, cancel := context.WithCancel(ctx)
		cancel()
		installWriteLoop(stopped, "probe", "", time.Hour, func(context.Context) error {
			Fail("no write after the server stopped")
			return nil
		})
	})

	It("starts the one retry only for a refused install write (or any error when asked), and leaves other errors to the caller", func(ctx SpecContext) {
		bg, cancel := context.WithCancel(ctx)
		defer cancel()
		landed := make(chan struct{})
		write := func(context.Context) error { close(landed); return nil }

		Expect(retryInstallWrite(bg, cancel, installRetry{Goroutine: "probe", What: "probe"}, time.Millisecond, nil, write)).To(Succeed(), "nothing to retry")

		other := errors.New("not a refusal")
		Expect(retryInstallWrite(bg, cancel, installRetry{Goroutine: "probe", What: "probe"}, time.Millisecond, other, write)).To(MatchError(other))
		Consistently(landed, "50ms").ShouldNot(BeClosed(), "another error does not start a retry")

		Expect(retryInstallWrite(bg, cancel, installRetry{Goroutine: "probe", What: "probe"}, time.Millisecond, denied, write)).To(Succeed())
		Eventually(landed).Should(BeClosed())
	})

	It("logs a refused write once for the same state: the first denial, then the landing", func(ctx SpecContext) {
		var logs lockedBuffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
		defer slog.SetDefault(prev)

		bg, cancel := context.WithCancel(ctx)
		defer cancel()
		calls := 0
		// The sequence is denied (the run that has just happened), denied (the first retry), then success.
		Expect(retryInstallWrite(bg, cancel, installRetry{Goroutine: "probe", What: "probe"}, time.Millisecond, denied, func(context.Context) error {
			calls++
			if calls < 2 {
				return denied
			}
			return nil
		})).To(Succeed())
		Eventually(logs.String).Should(ContainSubstring("recorded after a retry"))

		lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
		Expect(lines).To(HaveLen(2), "one line for the first denial, one for the landing: %v", lines)
		Expect(lines[0]).To(ContainSubstring("the probe is not recorded yet:"))
		Expect(lines[1]).To(ContainSubstring("recorded after a retry"))
		Expect(logs.String()).NotTo(ContainSubstring("still not recorded"))
	})

	It("retries any error when asked to, as the key-set check does", func(ctx SpecContext) {
		bg, cancel := context.WithCancel(ctx)
		defer cancel()
		landed := make(chan struct{})
		Expect(retryInstallWrite(bg, cancel, installRetry{Goroutine: "probe", What: "probe", AnyError: true}, time.Millisecond,
			errors.New("a hiccup"), func(context.Context) error { close(landed); return nil })).To(Succeed())
		Eventually(landed).Should(BeClosed())
	})

	It("does not let a hung first attempt hold the start, and still retries in the background", func(ctx SpecContext) {
		bg, cancel := context.WithCancel(ctx)
		defer cancel()
		calls := 0
		landed := make(chan struct{})
		returned := make(chan error, 1)
		go func() {
			returned <- startInstallWrite(bg, cancel, installRetry{Goroutine: "probe", What: "probe", AnyError: true}, time.Millisecond, 50*time.Millisecond,
				func(c context.Context) error {
					calls++
					if calls == 1 {
						<-c.Done() // a database that does not answer: only the first attempt's bound ends it
						return c.Err()
					}
					close(landed)
					return nil
				})
		}()
		Eventually(returned, "2s").Should(Receive(BeNil()), "control returns within about the first-attempt timeout")
		Eventually(landed).Should(BeClosed(), "the background retry still runs")
	})

	It("waits in the grant-only migrate for the role, and gives up with the wait", func(ctx SpecContext) {
		calls := 0
		Expect(grantInstallAuditWriter(ctx, time.Minute, time.Millisecond, func(context.Context) (bool, error) {
			calls++
			return calls == 3, nil
		})).To(Succeed())
		Expect(calls).To(Equal(3))

		short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
		defer cancel()
		Expect(grantInstallAuditWriter(short, 20*time.Millisecond, time.Millisecond, func(context.Context) (bool, error) {
			return false, nil
		})).To(MatchError(ContainSubstring("did not appear within 20ms")))

		Expect(grantInstallAuditWriter(ctx, time.Minute, time.Millisecond, func(context.Context) (bool, error) {
			return false, errors.New("permission denied for table install_audit_events")
		})).To(MatchError(ContainSubstring("grant the install audit writer")), "a grant that fails is not waited out")
	})

	// An external database: nothing creates the role after the migrate Job, so the chart passes
	// --require-install-writer and the Job fails with the remedy (the cnpg upgrade only warns). The
	// check runs before any migration, so a failed Job leaves the schema as it was.
	It("fails the migration without the role when it is required, and only warns otherwise", func(ctx SpecContext) {
		testenv.Require(testenv.Docker)
		dsn, stop, err := pgtest.StartServer(ctx)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(stop)
		conn, err := pgx.Connect(ctx, dsn)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx SpecContext) { _ = conn.Close(ctx) })
		untouched := func() {
			GinkgoHelper()
			var goose, tables bool
			Expect(conn.QueryRow(ctx, `SELECT to_regclass('goose_db_version') IS NOT NULL, to_regclass('install_audit_events') IS NOT NULL`).
				Scan(&goose, &tables)).To(Succeed())
			Expect(goose).To(BeFalse(), "no migration ran")
			Expect(tables).To(BeFalse(), "no migration ran")
		}
		exec := func(stmt string) {
			GinkgoHelper()
			_, err := conn.Exec(ctx, stmt)
			Expect(err).NotTo(HaveOccurred(), stmt)
		}
		required := migrateOptions{requireWriter: true}

		err = migrate(ctx, dsn, time.Minute, required)
		Expect(err).To(MatchError(ContainSubstring("the role koinon_install_audit_writer does not exist")))
		Expect(err).To(MatchError(ContainSubstring("docs/ops/secret-keys.md")))
		Expect(err).To(MatchError(ContainSubstring("no migration was applied")))
		untouched()

		By("without --require-install-writer nothing is checked: the cnpg pre-upgrade run goes on")
		Expect(migrate(ctx, dsn, time.Minute, migrateOptions{})).To(Succeed())

		By("the operator created the role; membership is koinon-api's own check, not the migration's")
		exec(`CREATE ROLE koinon_install_audit_writer NOLOGIN`)
		Expect(migrate(ctx, dsn, time.Minute, required)).To(Succeed())
	}, SpecTimeout(3*time.Minute))
})
