// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/orgsettings"
)

// Per-org sandbox creation rate (issue #27, ADR 0066 amendment 2026-11-12).
var _ = Describe("per-org sandbox creation rate", func() {
	It("defaults to 10 a minute, holds the rest, leaves another org alone and frees slots as the minute passes", func(ctx SpecContext) {
		a := newFixture(ctx)
		b := newFixture(ctx)
		rate, err := orgsettings.Get(ctx, q, a.org, orgsettings.Defaults())
		Expect(err).NotTo(HaveOccurred())
		Expect(rate.SandboxCreationsPerMinute).To(Equal(10))

		// A burst of 25 agents' creations at once, from 5 concurrent workers.
		agents := make([]store.Agent, 25)
		for i := range agents {
			agents[i], _ = newAgent(ctx, a.org, a.user)
		}
		granted := make(chan int, len(agents))
		for _, ag := range agents {
			go func() {
				defer GinkgoRecover()
				ok, _, err := orgsettings.ReserveSandboxCreation(ctx, pool, a.org, ag.ID, orgsettings.CreationCreate, rate.SandboxCreationsPerMinute)
				Expect(err).NotTo(HaveOccurred())
				if ok {
					granted <- 1
				} else {
					granted <- 0
				}
			}()
		}
		total := 0
		for range agents {
			total += <-granted
		}
		Expect(total).To(Equal(10), "exactly the rate is granted under concurrency; the other 15 wait, none fails")

		ok, wait, err := orgsettings.ReserveSandboxCreation(ctx, pool, a.org, agents[0].ID, orgsettings.CreationStart, 10)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse())
		Expect(wait).To(BeNumerically(">", 0))
		Expect(wait).To(BeNumerically("<=", time.Minute))

		By("another org is unaffected")
		ok, _, err = orgsettings.ReserveSandboxCreation(ctx, pool, b.org, b.agent.ID, orgsettings.CreationCreate, 10)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())

		By("a raised per-org rate admits more at once")
		ok, _, err = orgsettings.ReserveSandboxCreation(ctx, pool, a.org, agents[0].ID, orgsettings.CreationStart, 11)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())

		By("slots free as the minute passes")
		_, err = pool.Exec(ctx, `UPDATE sandbox_creations SET created_at = now() - interval '61 seconds' WHERE org_id = $1`, a.org)
		Expect(err).NotTo(HaveOccurred())
		ok, _, err = orgsettings.ReserveSandboxCreation(ctx, pool, a.org, agents[0].ID, orgsettings.CreationStart, 10)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
	})

	It("is a column next to max_concurrent_runs and is bounded", func(ctx SpecContext) {
		f := newFixture(ctx)
		n := 3
		Expect(orgsettings.Set(ctx, q, f.org, orgsettings.Override{SandboxCreationsPerMinute: &n})).To(Succeed())
		got, err := orgsettings.Get(ctx, q, f.org, orgsettings.Defaults())
		Expect(err).NotTo(HaveOccurred())
		Expect(got.SandboxCreationsPerMinute).To(Equal(3))
		bad := 0
		Expect(orgsettings.Set(ctx, q, f.org, orgsettings.Override{SandboxCreationsPerMinute: &bad})).NotTo(Succeed(), "the table refuses 0")
	})
})
