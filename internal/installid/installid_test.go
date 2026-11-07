// SPDX-License-Identifier: Apache-2.0

package installid_test

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/installid"
	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

var _ = Describe("the install ID", func() {
	open := func(ctx SpecContext, dsn string) *pgxpool.Pool {
		GinkgoHelper()
		pool, err := pgxpool.New(ctx, dsn)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(pool.Close)
		return pool
	}

	It("is a UUID a migration writes once, different for every database it migrates", func(ctx SpecContext) {
		a, b := open(ctx, pgtest.Run(ctx)), open(ctx, pgtest.Run(ctx))
		for _, p := range []*pgxpool.Pool{a, b} {
			_, err := installid.Read(ctx, p)
			Expect(err).To(MatchError(installid.ErrNotMigrated))
			pgtest.Migrate(ctx, p.Config().ConnString())
		}
		idA, err := installid.Read(ctx, a)
		Expect(err).NotTo(HaveOccurred())
		Expect(secretstore.ValidInstallID(idA)).To(BeTrue(), idA)
		Expect(installid.Read(ctx, a)).To(Equal(idA), "it does not change")
		Expect(installid.Read(ctx, b)).NotTo(Equal(idA), "another database is another install")

		By("migrating again changes nothing")
		pgtest.Migrate(ctx, a.Config().ConnString())
		Expect(installid.Read(ctx, a)).To(Equal(idA))
	})

	It("cannot be changed, deleted or truncated, nor a second one added", func(ctx SpecContext) {
		pool := pgtest.Start(ctx)
		id, err := installid.Read(ctx, pool)
		Expect(err).NotTo(HaveOccurred())
		for _, stmt := range []string{
			`UPDATE install_identity SET install_id = gen_random_uuid()`,
			`DELETE FROM install_identity`,
			`TRUNCATE install_identity`,
			`INSERT INTO install_identity (singleton, install_id) VALUES (true, gen_random_uuid())`,
			`INSERT INTO install_identity (singleton, install_id) VALUES (false, gen_random_uuid())`,
		} {
			_, err := pool.Exec(ctx, stmt)
			Expect(err).To(HaveOccurred(), stmt)
		}
		Expect(installid.Read(ctx, pool)).To(Equal(id))
	})

	It("is read by a source once and kept; a read before the migration is not kept", func(ctx SpecContext) {
		dsn := pgtest.Run(ctx)
		pool := open(ctx, dsn)
		src := installid.Source(pool)
		_, err := src(ctx)
		Expect(err).To(MatchError(installid.ErrNotMigrated))

		pgtest.Migrate(ctx, dsn)
		id, err := src(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(installid.Read(ctx, pool)).To(Equal(id))

		pool.Close()
		Expect(src(ctx)).To(Equal(id), "kept: the database is not asked again")
	})

	Describe("the bound-only marker (old issue 615)", func() {
		It("is not set after the migrations, is set once, and keeps its first setting", func(ctx SpecContext) {
			pool := pgtest.Start(ctx)
			b, err := installid.ReadBoundOnly(ctx, pool)
			Expect(err).NotTo(HaveOccurred())
			Expect(b.Set).To(BeFalse())

			set, err := installid.SetBoundOnly(ctx, pool, secretstore.BoundOnlyFirstInstall)
			Expect(err).NotTo(HaveOccurred())
			Expect(set.Set).To(BeTrue())
			Expect(set.How).To(Equal(secretstore.BoundOnlyFirstInstall))
			Expect(set.At).NotTo(BeZero())

			By("setting it again changes nothing: the app has no way to change or clear it")
			again, err := installid.Marker{DB: pool}.SetBoundOnly(ctx, secretstore.BoundOnlyRewrap)
			Expect(err).NotTo(HaveOccurred())
			Expect(again).To(Equal(set))
		})

		It("cannot be cleared or changed in the database", func(ctx SpecContext) {
			pool := pgtest.Start(ctx)
			set, err := installid.SetBoundOnly(ctx, pool, secretstore.BoundOnlyRewrap)
			Expect(err).NotTo(HaveOccurred())
			for _, stmt := range []string{
				`DELETE FROM kek_bound_only`,
				`TRUNCATE kek_bound_only`,
				`UPDATE kek_bound_only SET set_by = 'first-install'`,
				`UPDATE kek_bound_only SET set_at = now() - interval '1 day'`,
				`INSERT INTO kek_bound_only (singleton, set_by) VALUES (false, 'rewrap-unbound')`,
				`INSERT INTO kek_bound_only (set_by) VALUES ('cleared')`,
			} {
				_, err := pool.Exec(ctx, stmt)
				Expect(err).To(HaveOccurred(), stmt)
			}
			Expect(installid.ReadBoundOnly(ctx, pool)).To(Equal(set))
		})

		It("is read by a source on every call until it is set, then kept", func(ctx SpecContext) {
			dsn := pgtest.Run(ctx)
			pool := open(ctx, dsn)
			src := installid.BoundOnlySource(pool)
			_, err := src(ctx)
			Expect(err).To(MatchError(installid.ErrNotMigrated), "fail closed before the migration")

			pgtest.Migrate(ctx, dsn)
			b, err := src(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(b.Set).To(BeFalse())

			By("a process that read it unset sees it set on its next load")
			set, err := installid.SetBoundOnly(ctx, pool, secretstore.BoundOnlyRewrap)
			Expect(err).NotTo(HaveOccurred())
			Expect(src(ctx)).To(Equal(set))

			pool.Close()
			Expect(src(ctx)).To(Equal(set), "kept: it is never cleared")
		})
	})

	It("is waited for while the database is not migrated, as the key-admin Job does on a new install", func(ctx SpecContext) {
		dsn := pgtest.Run(ctx)
		pool := open(ctx, dsn)
		type result struct {
			id  string
			err error
		}
		got := make(chan result, 1)
		go func() {
			defer GinkgoRecover()
			id, err := installid.Wait(ctx, pool, 20*time.Millisecond)
			got <- result{id, err}
		}()
		Consistently(got).WithTimeout(200 * time.Millisecond).ShouldNot(Receive())
		pgtest.Migrate(ctx, dsn)
		var r result
		Eventually(got).WithTimeout(30 * time.Second).Should(Receive(&r))
		Expect(r.err).NotTo(HaveOccurred())
		Expect(installid.Read(ctx, pool)).To(Equal(r.id))

		By("it gives up when its context ends")
		unmigrated := open(ctx, pgtest.Run(ctx))
		_, err := installid.Read(ctx, unmigrated)
		Expect(err).To(MatchError(installid.ErrNotMigrated))
		short, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		defer cancel()
		// The deadline may end the wait between reads or in one: either way the error says so.
		id, err := installid.Wait(short, unmigrated, 20*time.Millisecond)
		Expect(err).To(MatchError(context.DeadlineExceeded))
		Expect(id).To(BeEmpty())
	})
})
