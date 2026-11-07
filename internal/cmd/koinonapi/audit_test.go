// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
		q := store.New(pool)
		for range 3 {
			Expect(audit.RecordInstallErr(ctx, q, audit.InstallEvent{
				Actor: audit.System("koinon-test:spec"), Action: "platform_price.set", TargetKind: "platform_price", TargetID: "cli",
			})).To(Succeed())
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

var _ = Describe("startup install audit writes before the writer role is granted", func() {
	denied := fmt.Errorf("audit: record install event x: %w: permission denied for table install_audit_events", audit.ErrInstallWriteDenied)

	It("retries a refused write until it is recorded, and stops with the server", func(ctx SpecContext) {
		calls := 0
		retryInstallWrite(ctx, "probe", time.Millisecond, func(context.Context) error {
			calls++
			if calls < 3 {
				return denied
			}
			return nil
		})
		Expect(calls).To(Equal(3))

		stopped, cancel := context.WithCancel(ctx)
		cancel()
		retryInstallWrite(stopped, "probe", time.Hour, func(context.Context) error {
			Fail("no write after the server stopped")
			return nil
		})
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
