// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"bytes"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

var _ = Describe("koinon-api audit verify", func() {
	It("walks the install chain by default, alone with --install, and reports a break", func(ctx SpecContext) {
		var pool *pgxpool.Pool = pgtest.Start(ctx)
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
