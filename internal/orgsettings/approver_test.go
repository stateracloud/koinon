// SPDX-License-Identifier: Apache-2.0

package orgsettings_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/orgsettings"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

var _ = Describe("the rule the approver must not be the requester (#1175)", func() {
	var svc *orgsettings.Service
	BeforeEach(func() { svc = orgsettings.NewService(pool) })

	It("is off by default, turns on and off, and is audited only when it changes", func(ctx SpecContext) {
		org, _, _ := pgtest.NewOrg(ctx, pool)
		on, err := svc.ApproverNotRequester(ctx, org)
		Expect(err).NotTo(HaveOccurred())
		Expect(on).To(BeFalse())
		Expect(orgsettings.Defaults().ApproverNotRequester).To(BeFalse())

		thirty := 30
		Expect(svc.SetHomeBackupRetention(ctx, org, audit.System("koinon-test:spec"), &thirty)).To(Succeed())
		Expect(svc.SetApproverNotRequester(ctx, org, audit.System("koinon-test:spec"), false)).To(Succeed(), "unchanged: no audit entry")
		Expect(svc.SetApproverNotRequester(ctx, org, audit.System("koinon-test:spec"), true)).To(Succeed())
		Expect(svc.SetApproverNotRequester(ctx, org, audit.System("koinon-test:spec"), true)).To(Succeed(), "unchanged: no audit entry")
		on, err = svc.ApproverNotRequester(ctx, org)
		Expect(err).NotTo(HaveOccurred())
		Expect(on).To(BeTrue())
		days, _ := svc.HomeBackupRetentionDays(ctx, org)
		Expect(days).To(Equal(30), "the other settings stay")

		Expect(svc.SetApproverNotRequester(ctx, org, audit.System("koinon-test:spec"), false)).To(Succeed())
		on, _ = svc.ApproverNotRequester(ctx, org)
		Expect(on).To(BeFalse())

		rows, err := pool.Query(ctx, `SELECT summary->'before'->>'on', summary->'after'->>'on', target_kind FROM audit_events
			WHERE org_id = $1 AND action = 'org.approver_not_requester.set' ORDER BY chain_seq`, org)
		Expect(err).NotTo(HaveOccurred())
		var changes [][3]string
		for rows.Next() {
			var b, a, k string
			Expect(rows.Scan(&b, &a, &k)).To(Succeed())
			changes = append(changes, [3]string{b, a, k})
		}
		Expect(rows.Err()).NotTo(HaveOccurred())
		Expect(changes).To(Equal([][3]string{{"false", "true", "org"}, {"true", "false", "org"}}))
	})
})
