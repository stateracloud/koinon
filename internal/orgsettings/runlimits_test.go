// SPDX-License-Identifier: Apache-2.0

package orgsettings_test

import (
	"sync"
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

	It("sets the sandbox creation rate, keeps it when another limit changes, and audits it (issue #27)", func(ctx SpecContext) {
		org, _, _ := pgtest.NewOrg(ctx, pool)
		five, three := 5, 3
		Expect(svc.SetRunLimits(ctx, org, actor, orgsettings.RunLimitsPatch{SandboxCreationsPerMinute: &five})).To(Succeed())
		got, _ := svc.RunLimits(ctx, org)
		Expect(got.SandboxCreationsPerMinute).To(Equal(5))
		Expect(svc.SetRunLimits(ctx, org, actor, orgsettings.RunLimitsPatch{MaxConcurrentRuns: &three})).To(Succeed())
		got, _ = svc.RunLimits(ctx, org)
		Expect(got.SandboxCreationsPerMinute).To(Equal(5), "a change to another limit keeps it")
		var after int
		Expect(pool.QueryRow(ctx, `SELECT (summary->'after'->>'sandbox_creations_per_minute')::int FROM audit_events WHERE org_id = $1 AND action = 'org.run_limits.set' ORDER BY id LIMIT 1`, org).Scan(&after)).To(Succeed())
		Expect(after).To(Equal(5))
		zero, over := 0, 10001
		Expect(svc.SetRunLimits(ctx, org, actor, orgsettings.RunLimitsPatch{SandboxCreationsPerMinute: &over})).NotTo(Succeed())
		Expect(svc.SetRunLimits(ctx, org, actor, orgsettings.RunLimitsPatch{SandboxCreationsPerMinute: &zero})).To(Succeed())
		got, _ = svc.RunLimits(ctx, org)
		Expect(got.SandboxCreationsPerMinute).To(Equal(10), "0 returns it to the default")
	})

	It("defaults, changes only the patched fields, resets with 0, and audits real changes", func(ctx SpecContext) {
		org, _, _ := pgtest.NewOrg(ctx, pool)
		got, err := svc.RunLimits(ctx, org)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(orgsettings.RunLimits{MaxConcurrentRuns: 20, TriggerConcurrency: 4, TriggerRatePerMinute: 60, SandboxCreationsPerMinute: 10}))

		// An unrelated override stays when a limit changes.
		idle := 3 * time.Minute
		Expect(orgsettings.Set(ctx, store.New(pool), org, orgsettings.Override{IdleTimeout: &idle})).To(Succeed())

		two, rate := 2, 5
		Expect(svc.SetRunLimits(ctx, org, actor, orgsettings.RunLimitsPatch{MaxConcurrentRuns: &two, TriggerRatePerMinute: &rate})).To(Succeed())
		got, _ = svc.RunLimits(ctx, org)
		Expect(got).To(Equal(orgsettings.RunLimits{MaxConcurrentRuns: 2, TriggerConcurrency: 4, TriggerRatePerMinute: 5, SandboxCreationsPerMinute: 10}))
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
		Expect(got).To(Equal(orgsettings.RunLimits{MaxConcurrentRuns: 20, TriggerConcurrency: 4, TriggerRatePerMinute: 5, SandboxCreationsPerMinute: 10}))
		Expect(events(ctx, org)).To(Equal(2))
	})

	It("audits an explicit value equal to the default, which changes the stored row from NULL", func(ctx SpecContext) {
		org, _, _ := pgtest.NewOrg(ctx, pool)
		def := orgsettings.DefaultMaxConcurrentRuns
		Expect(svc.SetRunLimits(ctx, org, actor, orgsettings.RunLimitsPatch{MaxConcurrentRuns: &def})).To(Succeed())
		Expect(events(ctx, org)).To(Equal(1))
		By("setting it again changes nothing")
		Expect(svc.SetRunLimits(ctx, org, actor, orgsettings.RunLimitsPatch{MaxConcurrentRuns: &def})).To(Succeed())
		Expect(events(ctx, org)).To(Equal(1))
	})

	It("audits the right before when two admins set at once", func(ctx SpecContext) {
		org, _, _ := pgtest.NewOrg(ctx, pool)
		var wg sync.WaitGroup
		for _, n := range []int{3, 4} {
			wg.Add(1)
			go func() {
				defer GinkgoRecover()
				defer wg.Done()
				Expect(svc.SetRunLimits(ctx, org, actor, orgsettings.RunLimitsPatch{MaxConcurrentRuns: &n})).To(Succeed())
			}()
		}
		wg.Wait()
		// The second writer's before is the first writer's after, never the default both would have read.
		var firstAfter, secondBefore int
		Expect(pool.QueryRow(ctx, `SELECT (summary->'after'->>'max_concurrent_runs')::int FROM audit_events WHERE org_id = $1 AND action = 'org.run_limits.set' ORDER BY id LIMIT 1`, org).Scan(&firstAfter)).To(Succeed())
		Expect(pool.QueryRow(ctx, `SELECT (summary->'before'->>'max_concurrent_runs')::int FROM audit_events WHERE org_id = $1 AND action = 'org.run_limits.set' ORDER BY id DESC LIMIT 1`, org).Scan(&secondBefore)).To(Succeed())
		Expect(secondBefore).To(Equal(firstAfter))
	})

	It("audits NULL to an explicit value equal to the default as a change, with the stored values", func(ctx SpecContext) {
		org, _, _ := pgtest.NewOrg(ctx, pool)
		def := orgsettings.DefaultMaxConcurrentRuns
		Expect(svc.SetRunLimits(ctx, org, actor, orgsettings.RunLimitsPatch{MaxConcurrentRuns: &def})).To(Succeed())
		var beforeNull bool
		var after int
		Expect(pool.QueryRow(ctx, `SELECT summary->'before'->'max_concurrent_runs' = 'null'::jsonb, (summary->'after'->>'max_concurrent_runs')::int FROM audit_events WHERE org_id = $1 AND action = 'org.run_limits.set'`, org).Scan(&beforeNull, &after)).To(Succeed())
		Expect(beforeNull).To(BeTrue())
		Expect(after).To(Equal(def))
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
