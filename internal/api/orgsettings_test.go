// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	connect "connectrpc.com/connect/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"github.com/stateracloud/koinon/internal/db/store"
	orgsettingsv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/orgsettings/v1alpha1"
)

var _ = Describe("org settings: retention", func() {
	var (
		e     *env
		org   string
		owner *client
	)
	BeforeEach(func(ctx SpecContext) {
		e = newEnv(ctx, envOptions{})
		org, owner = e.newTeamOrg(ctx)
	})

	It("lets only admins and owners read or change the retentions and the approver rule", func(ctx SpecContext) {
		member := e.userIn(ctx, org, store.OrgRoleMember)
		admin := e.userIn(ctx, org, store.OrgRoleAdmin)
		calls := map[string]func(c *client) error{
			"GetRunContentRetention": func(c *client) error {
				_, err := c.orgsettings.GetRunContentRetention(ctx, &orgsettingsv1alpha1.GetRunContentRetentionRequest{OrgId: org})
				return err
			},
			"SetRunContentRetention": func(c *client) error {
				_, err := c.orgsettings.SetRunContentRetention(ctx, &orgsettingsv1alpha1.SetRunContentRetentionRequest{OrgId: org, Days: 7})
				return err
			},
			"GetHomeBackupRetention": func(c *client) error {
				_, err := c.orgsettings.GetHomeBackupRetention(ctx, &orgsettingsv1alpha1.GetHomeBackupRetentionRequest{OrgId: org})
				return err
			},
			"SetHomeBackupRetention": func(c *client) error {
				_, err := c.orgsettings.SetHomeBackupRetention(ctx, &orgsettingsv1alpha1.SetHomeBackupRetentionRequest{OrgId: org, Days: 14})
				return err
			},
			"GetApproverNotRequester": func(c *client) error {
				_, err := c.orgsettings.GetApproverNotRequester(ctx, &orgsettingsv1alpha1.GetApproverNotRequesterRequest{OrgId: org})
				return err
			},
			"GetRunLimits": func(c *client) error {
				_, err := c.orgsettings.GetRunLimits(ctx, &orgsettingsv1alpha1.GetRunLimitsRequest{OrgId: org})
				return err
			},
			"SetRunLimits": func(c *client) error {
				_, err := c.orgsettings.SetRunLimits(ctx, &orgsettingsv1alpha1.SetRunLimitsRequest{OrgId: org, MaxConcurrentRuns: proto.Int32(12)})
				return err
			},
			"SetApproverNotRequester": func(c *client) error {
				_, err := c.orgsettings.SetApproverNotRequester(ctx, &orgsettingsv1alpha1.SetApproverNotRequesterRequest{OrgId: org, On: true})
				return err
			},
		}
		for name, call := range calls {
			Expect(codeOf(call(member))).To(Equal(connect.CodePermissionDenied), name+" refuses a member")
		}
		r, err := owner.orgsettings.GetHomeBackupRetention(ctx, &orgsettingsv1alpha1.GetHomeBackupRetentionRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		Expect(r.GetRetention().GetDays()).To(Equal(int32(7)), "a member's Set changed nothing")
		rc, err := owner.orgsettings.GetRunContentRetention(ctx, &orgsettingsv1alpha1.GetRunContentRetentionRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		Expect(rc.GetRetention().GetDays()).To(Equal(int32(30)), "a member's Set changed nothing")
		rl, err := owner.orgsettings.GetRunLimits(ctx, &orgsettingsv1alpha1.GetRunLimitsRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		Expect(rl.GetLimits().GetMaxConcurrentRuns().GetValue()).To(Equal(int32(20)), "a member's Set changed nothing")
		an, err := owner.orgsettings.GetApproverNotRequester(ctx, &orgsettingsv1alpha1.GetApproverNotRequesterRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		Expect(an.GetOn()).To(BeFalse(), "a member's Set changed nothing")

		for name, call := range calls {
			Expect(call(admin)).To(Succeed(), name+" serves an admin")
			Expect(call(owner)).To(Succeed(), name+" serves an owner")
		}
		an, err = owner.orgsettings.GetApproverNotRequester(ctx, &orgsettingsv1alpha1.GetApproverNotRequesterRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		Expect(an.GetOn()).To(BeTrue(), "an admin's Set turned the rule on")
	})
})

var _ = Describe("org settings: run limits", func() {
	var (
		e     *env
		org   string
		owner *client
	)
	BeforeEach(func(ctx SpecContext) {
		e = newEnv(ctx, envOptions{})
		org, owner = e.newTeamOrg(ctx)
	})

	audits := func(ctx SpecContext) int {
		GinkgoHelper()
		return count(ctx, e, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'org.run_limits.set' AND actor_kind = 'user'`, org)
	}

	It("shows each limit with its default and bounds, changes only the limits that are set, returns one to the default with 0 and audits each change", func(ctx SpecContext) {
		got, err := owner.orgsettings.GetRunLimits(ctx, &orgsettingsv1alpha1.GetRunLimitsRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		l := got.GetLimits()
		Expect(l.GetMaxConcurrentRuns()).To(HaveField("Value", int32(20)))
		Expect(l.GetMaxConcurrentRuns()).To(HaveField("DefaultValue", int32(20)))
		Expect(l.GetMaxConcurrentRuns()).To(HaveField("Min", int32(1)))
		Expect(l.GetMaxConcurrentRuns()).To(HaveField("Max", int32(10000)))
		Expect(l.GetTriggerConcurrency().GetValue()).To(Equal(int32(4)))
		Expect(l.GetTriggerRatePerMinute().GetValue()).To(Equal(int32(60)))
		Expect(l.GetSandboxCreationsPerMinute().GetValue()).To(Equal(int32(10)))

		set, err := owner.orgsettings.SetRunLimits(ctx, &orgsettingsv1alpha1.SetRunLimitsRequest{OrgId: org, MaxConcurrentRuns: proto.Int32(12), TriggerRatePerMinute: proto.Int32(30)})
		Expect(err).NotTo(HaveOccurred())
		Expect(set.GetLimits().GetMaxConcurrentRuns().GetValue()).To(Equal(int32(12)))
		Expect(set.GetLimits().GetTriggerRatePerMinute().GetValue()).To(Equal(int32(30)))
		Expect(set.GetLimits().GetTriggerConcurrency().GetValue()).To(Equal(int32(4)), "a limit that is not set stays")
		Expect(audits(ctx)).To(Equal(1))

		set, err = owner.orgsettings.SetRunLimits(ctx, &orgsettingsv1alpha1.SetRunLimitsRequest{OrgId: org, MaxConcurrentRuns: proto.Int32(0)})
		Expect(err).NotTo(HaveOccurred())
		Expect(set.GetLimits().GetMaxConcurrentRuns().GetValue()).To(Equal(int32(20)), "0 returns the default")
		Expect(set.GetLimits().GetTriggerRatePerMinute().GetValue()).To(Equal(int32(30)), "a limit that is not set stays")
		Expect(audits(ctx)).To(Equal(2))

		_, err = owner.orgsettings.SetRunLimits(ctx, &orgsettingsv1alpha1.SetRunLimitsRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		Expect(audits(ctx)).To(Equal(2), "an empty change is not audited")
	})

	It("refuses a limit out of its bounds and changes nothing", func(ctx SpecContext) {
		_, err := owner.orgsettings.SetRunLimits(ctx, &orgsettingsv1alpha1.SetRunLimitsRequest{OrgId: org, MaxConcurrentRuns: proto.Int32(12), TriggerConcurrency: proto.Int32(1001)})
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
		got, err := owner.orgsettings.GetRunLimits(ctx, &orgsettingsv1alpha1.GetRunLimitsRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.GetLimits().GetMaxConcurrentRuns().GetValue()).To(Equal(int32(20)))
		Expect(audits(ctx)).To(BeZero())
	})
})
