// SPDX-License-Identifier: Apache-2.0

package orgsettings_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/orgsettings"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

var _ = Describe("SandboxWaitCap", func() {
	It("is the default without a row or without a value, and round-trips through Set and Get", func(ctx SpecContext) {
		q := store.New(pool)
		org, _, _ := pgtest.NewOrg(ctx, pool)

		s, err := orgsettings.Get(ctx, q, org, orgsettings.Defaults())
		Expect(err).NotTo(HaveOccurred())
		Expect(s.SandboxWaitCap).To(Equal(orgsettings.DefaultSandboxWaitCap))
		Expect(s.SandboxWaitCap).To(Equal(time.Hour))

		By("a row that sets another value keeps the cap at the default")
		queueWait := 5 * time.Minute
		Expect(orgsettings.Set(ctx, q, org, orgsettings.Override{QueueWait: &queueWait})).To(Succeed())
		s, err = orgsettings.Get(ctx, q, org, orgsettings.Defaults())
		Expect(err).NotTo(HaveOccurred())
		Expect(s.SandboxWaitCap).To(Equal(orgsettings.DefaultSandboxWaitCap))

		By("the cap set by the org is read back")
		capd := 10 * time.Minute
		Expect(orgsettings.Set(ctx, q, org, orgsettings.Override{SandboxWaitCap: &capd})).To(Succeed())
		s, err = orgsettings.Get(ctx, q, org, orgsettings.Defaults())
		Expect(err).NotTo(HaveOccurred())
		Expect(s.SandboxWaitCap).To(Equal(capd))

		By("clearing the override returns the default")
		Expect(orgsettings.Set(ctx, q, org, orgsettings.Override{})).To(Succeed())
		s, err = orgsettings.Get(ctx, q, org, orgsettings.Defaults())
		Expect(err).NotTo(HaveOccurred())
		Expect(s.SandboxWaitCap).To(Equal(orgsettings.DefaultSandboxWaitCap))
	})
})
