// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	connect "connectrpc.com/connect/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	providerv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/provider/v1alpha1"
)

// The org default model from the UI (ADR 0086): ProviderService.GetDefaultModel and SetDefaultModel
// call the same agentmodel.Service as PUT /admin/v1/default-model.
var _ = Describe("the org's default model in ProviderService", func() {
	var (
		e     *env
		org   string
		owner *client
	)
	BeforeEach(func(ctx SpecContext) {
		e = newEnv(ctx, envOptions{})
		org, owner = e.newTeamOrg(ctx)
	})

	It("lists the priced models, sets, changes and clears the default, and audits each change", func(ctx SpecContext) {
		res, err := owner.providers.GetDefaultModel(ctx, &providerv1alpha1.GetDefaultModelRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetModel()).To(BeEmpty())
		Expect(res.GetAvailable()).To(BeEmpty())

		serveModel(ctx, e, org, "vendor/a-model", true)
		serveModel(ctx, e, org, "vendor/b-model", true)
		serveModel(ctx, e, org, "vendor/unpriced", false)
		res, err = owner.providers.GetDefaultModel(ctx, &providerv1alpha1.GetDefaultModelRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetModel()).To(Equal("vendor/a-model"))
		Expect(res.GetAvailable()).To(HaveLen(2), "an unpriced model is not offered")

		set, err := owner.providers.SetDefaultModel(ctx, &providerv1alpha1.SetDefaultModelRequest{OrgId: org, Model: "vendor/b-model"})
		Expect(err).NotTo(HaveOccurred())
		Expect(set.GetModel()).To(Equal("vendor/b-model"))
		for _, bad := range []string{"vendor/ghost", "vendor/unpriced"} {
			_, err = owner.providers.SetDefaultModel(ctx, &providerv1alpha1.SetDefaultModelRequest{OrgId: org, Model: bad})
			Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument), bad)
		}
		set, err = owner.providers.SetDefaultModel(ctx, &providerv1alpha1.SetDefaultModelRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		Expect(set.GetModel()).To(BeEmpty())

		var n int
		Expect(e.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'org.default_model.set'
			AND summary->'before'->>'model' = 'vendor/a-model' AND summary->'after'->>'model' = 'vendor/b-model'`, org).Scan(&n)).To(Succeed())
		Expect(n).To(Equal(1))
	})

	It("is for admins and owners only", func(ctx SpecContext) {
		member := e.userIn(ctx, org, store.OrgRoleMember)
		_, err := member.providers.GetDefaultModel(ctx, &providerv1alpha1.GetDefaultModelRequest{OrgId: org})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
		_, err = member.providers.SetDefaultModel(ctx, &providerv1alpha1.SetDefaultModelRequest{OrgId: org})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
	})
})
