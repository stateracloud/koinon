// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	connect "connectrpc.com/connect/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

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

	It("lets only admins and owners read or change the run content and home backup retention", func(ctx SpecContext) {
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

		for name, call := range calls {
			Expect(call(admin)).To(Succeed(), name+" serves an admin")
			Expect(call(owner)).To(Succeed(), name+" serves an owner")
		}
	})
})
