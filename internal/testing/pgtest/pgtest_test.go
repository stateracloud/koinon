// SPDX-License-Identifier: Apache-2.0

package pgtest_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

func TestPgtest(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "pgtest")
}

func show(ctx context.Context, pool *pgxpool.Pool, setting string) string {
	var v string
	Expect(pool.QueryRow(ctx, "SELECT current_setting($1)", setting).Scan(&v)).To(Succeed())
	return v
}

var _ = Describe("pgtest", Ordered, func() {
	var (
		fast, durable *pgxpool.Pool
		tmpl          *pgtest.Template
	)
	BeforeAll(func(ctx SpecContext) {
		fast = pgtest.Start(ctx)
		durable = pgtest.Start(ctx, pgtest.Durable())
		tmpl = pgtest.StartTemplate(ctx)
	}, NodeTimeout(3*time.Minute))

	It("starts fast by default: fsync, synchronous_commit and full_page_writes off", func(ctx SpecContext) {
		Expect(show(ctx, fast, "fsync")).To(Equal("off"))
		Expect(show(ctx, fast, "synchronous_commit")).To(Equal("off"))
		Expect(show(ctx, fast, "full_page_writes")).To(Equal("off"))
	})

	It("keeps production durability settings with Durable", func(ctx SpecContext) {
		Expect(show(ctx, durable, "fsync")).To(Equal("on"))
		Expect(show(ctx, durable, "synchronous_commit")).To(Equal("on"))
		Expect(show(ctx, durable, "full_page_writes")).To(Equal("on"))
	})

	It("migrates the schema", func(ctx SpecContext) {
		var n int
		Expect(fast.QueryRow(ctx, "SELECT count(*) FROM orgs").Scan(&n)).To(Succeed())
	})

	It("gives every NewDB its own migrated copy of the template", func(ctx SpecContext) {
		a, b := tmpl.NewDB(ctx), tmpl.NewDB(ctx)
		pgtest.NewOrg(ctx, a)
		var na, nb int
		Expect(a.QueryRow(ctx, "SELECT count(*) FROM orgs").Scan(&na)).To(Succeed())
		Expect(b.QueryRow(ctx, "SELECT count(*) FROM orgs").Scan(&nb)).To(Succeed())
		Expect(na).To(Equal(1))
		Expect(nb).To(BeZero())
	})
})
