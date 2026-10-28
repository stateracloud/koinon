// SPDX-License-Identifier: Apache-2.0

package orgsettings_test

import (
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/durable"
	"github.com/stateracloud/koinon/internal/orgsettings"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

var _ = Describe("QueueLimits", func() {
	It("reads an org's trigger limits and falls back to the defaults", func(ctx SpecContext) {
		q := store.New(pool)
		limits := orgsettings.QueueLimits(q, orgsettings.Defaults())
		org, _, _ := pgtest.NewOrg(ctx, pool)
		orgID := uuidString(org)

		l, err := limits(ctx, orgID)
		Expect(err).NotTo(HaveOccurred())
		Expect(l).To(Equal(orgsettings.QueueDefaults()))
		Expect(l).To(Equal(durable.OrgLimits{Concurrency: 4, RatePerWindow: 60}))

		conc, rate := 9, 120
		Expect(orgsettings.Set(ctx, q, org, orgsettings.Override{TriggerConcurrency: &conc, TriggerRatePerMinute: &rate})).To(Succeed())
		l, err = limits(ctx, orgID)
		Expect(err).NotTo(HaveOccurred())
		Expect(l).To(Equal(durable.OrgLimits{Concurrency: 9, RatePerWindow: 120}))

		Expect(orgsettings.Set(ctx, q, org, orgsettings.Override{})).To(Succeed())
		l, err = limits(ctx, orgID)
		Expect(err).NotTo(HaveOccurred())
		Expect(l).To(Equal(orgsettings.QueueDefaults()))
	})

	It("rejects an org ID that is not a UUID", func(ctx SpecContext) {
		_, err := orgsettings.QueueLimits(store.New(pool), orgsettings.Defaults())(ctx, "nope")
		Expect(err).To(HaveOccurred())
	})
})

func uuidString(u pgtype.UUID) string {
	v, err := u.Value()
	Expect(err).NotTo(HaveOccurred())
	return v.(string)
}
