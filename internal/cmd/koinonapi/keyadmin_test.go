// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/spf13/cobra"

	"github.com/stateracloud/koinon/internal/keyscli"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

var _ = Describe("the key-admin command", func() {
	It("parses --activate-if-no-active next to --add-key", func() {
		var got keyAdminOptions
		cmd := newKeyAdminCommand()
		cmd.RunE = func(cmd *cobra.Command, _ []string) error {
			var err error
			got.addID, err = cmd.Flags().GetString("add-key")
			Expect(err).NotTo(HaveOccurred())
			got.activateIfNoActive, err = cmd.Flags().GetBool("activate-if-no-active")
			return err
		}
		cmd.SetArgs([]string{
			"--kek-secret=koinon-kek", "--namespace=koinon", "--database-url=postgres://example.invalid/koinon",
			"--add-key=kek-20261106-a1b2", "--activate-if-no-active",
		})
		Expect(cmd.Execute()).To(Succeed())
		Expect(got.addID).To(Equal("kek-20261106-a1b2"))
		Expect(got.activateIfNoActive).To(BeTrue())
	})

	It("leaves recovery off unless asked", func() {
		cmd := newKeyAdminCommand()
		f := cmd.Flags().Lookup("activate-if-no-active")
		Expect(f).NotTo(BeNil())
		Expect(f.DefValue).To(Equal("false"))
	})

	It("fails closed on a database that is not migrated, where a fresh install may carry on", func(ctx SpecContext) {
		dsn := pgtest.Run(ctx) // empty: nothing migrated
		pool, err := pgxpool.New(ctx, dsn)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(pool.Close)

		found, err := anyEnvelopeYet(ctx, pool)
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeFalse(), "a table that does not exist holds no values")

		used, err := kekIDUsedYet(ctx, pool, "kek-20251201-eeee")
		Expect(err).To(MatchError(ContainSubstring("rerun this command once the migrations have finished")))
		Expect(used).To(BeFalse())
	})

	// The probe every KEK add path runs: envelopes, then the audit chain. A retired ID has no envelope.
	It("refuses an ID the audit chain recorded, and accepts a new one", func(ctx SpecContext) {
		dsn := pgtest.Run(ctx)
		pgtest.Migrate(ctx, dsn)
		pool, err := pgxpool.New(ctx, dsn)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(pool.Close)

		used, err := kekIDUsedYet(ctx, pool, "kek-20261023-ab12")
		Expect(err).NotTo(HaveOccurred())
		Expect(used).To(BeFalse())

		auditor := &keyscli.Auditor{Pool: pool, Authoritative: true}
		Expect(auditor.Record(ctx, keyscli.Event{Command: "retire", Action: keyscli.ActionRetire, TargetKind: "kek", TargetID: "kek-20261023-ab12"})).To(Succeed())
		used, err = kekIDUsedYet(ctx, pool, "kek-20261023-ab12")
		Expect(err).To(MatchError(keyscli.ErrIDRetired))
		Expect(used).To(BeTrue())

		used, err = kekIDUsedYet(ctx, pool, "kek-20261023-cd34")
		Expect(err).NotTo(HaveOccurred())
		Expect(used).To(BeFalse())
	})
})
