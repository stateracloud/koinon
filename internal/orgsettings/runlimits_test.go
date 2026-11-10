// SPDX-License-Identifier: Apache-2.0

package orgsettings_test

import (
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/orgsettings"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

var _ = Describe("Org run limits (issue #462)", func() {
	var svc *orgsettings.Service
	actor := audit.CLI("org-limits set")
	BeforeEach(func() { svc = orgsettings.NewService(pool) })

	events := func(ctx SpecContext, org pgtype.UUID) int {
		GinkgoHelper()
		var n int
		Expect(pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'org.run_limits.set'`, org).Scan(&n)).To(Succeed())
		return n
	}

	It("defaults, changes only the patched fields, resets with 0, and audits real changes", func(ctx SpecContext) {
		org, _, _ := pgtest.NewOrg(ctx, pool)
		got, err := svc.RunLimits(ctx, org)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(orgsettings.RunLimits{MaxConcurrentRuns: 20, TriggerConcurrency: 4, TriggerRatePerMinute: 60}))

		// An unrelated override stays when a limit changes.
		idle := 3 * time.Minute
		Expect(orgsettings.Set(ctx, store.New(pool), org, orgsettings.Override{IdleTimeout: &idle})).To(Succeed())

		two, rate := 2, 5
		Expect(svc.SetRunLimits(ctx, org, actor, orgsettings.RunLimitsPatch{MaxConcurrentRuns: &two, TriggerRatePerMinute: &rate})).To(Succeed())
		got, _ = svc.RunLimits(ctx, org)
		Expect(got).To(Equal(orgsettings.RunLimits{MaxConcurrentRuns: 2, TriggerConcurrency: 4, TriggerRatePerMinute: 5}))
		st, err := orgsettings.Get(ctx, store.New(pool), org, orgsettings.Defaults())
		Expect(err).NotTo(HaveOccurred())
		Expect(st.IdleTimeout).To(Equal(idle), "the other overrides stay")
		Expect(events(ctx, org)).To(Equal(1))

		By("a patch that changes nothing writes no audit event")
		Expect(svc.SetRunLimits(ctx, org, actor, orgsettings.RunLimitsPatch{MaxConcurrentRuns: &two})).To(Succeed())
		Expect(svc.SetRunLimits(ctx, org, actor, orgsettings.RunLimitsPatch{})).To(Succeed())
		Expect(events(ctx, org)).To(Equal(1))

		By("0 returns a field to the platform default and nil keeps the others")
		zero := 0
		Expect(svc.SetRunLimits(ctx, org, actor, orgsettings.RunLimitsPatch{MaxConcurrentRuns: &zero})).To(Succeed())
		got, _ = svc.RunLimits(ctx, org)
		Expect(got).To(Equal(orgsettings.RunLimits{MaxConcurrentRuns: 20, TriggerConcurrency: 4, TriggerRatePerMinute: 5}))
		Expect(events(ctx, org)).To(Equal(2))
	})

	It("refuses values outside the table's bounds and an org that does not exist", func(ctx SpecContext) {
		org, _, _ := pgtest.NewOrg(ctx, pool)
		for _, p := range []orgsettings.RunLimitsPatch{
			{MaxConcurrentRuns: ptr(-1)},
			{MaxConcurrentRuns: ptr(10001)},
			{TriggerConcurrency: ptr(1001)},
			{TriggerRatePerMinute: ptr(100001)},
		} {
			Expect(svc.SetRunLimits(ctx, org, actor, p)).To(MatchError(ContainSubstring("must be")))
		}
		Expect(svc.SetRunLimits(ctx, pgtype.UUID{Bytes: [16]byte{9, 9, 9}, Valid: true}, actor,
			orgsettings.RunLimitsPatch{MaxConcurrentRuns: ptr(3)})).To(MatchError(orgsettings.ErrNotFound))
	})
})

func ptr(n int) *int { return &n }
