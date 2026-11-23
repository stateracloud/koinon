// SPDX-License-Identifier: Apache-2.0

package mcpservers_test

import (
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/mcpservers"
)

var _ = Describe("the prober wake payload (#1173)", func() {
	It("reads back the org and server it names", func() {
		var org, server pgtype.UUID
		Expect(org.Scan("0190a3f2-0000-7000-8000-000000000001")).To(Succeed())
		Expect(server.Scan("0190a3f2-0000-7000-8000-000000000002")).To(Succeed())
		gotOrg, gotServer, ok := mcpservers.ParseProbeWakePayload(mcpservers.ProbeWakePayload(org, server))
		Expect(ok).To(BeTrue())
		Expect(gotOrg).To(Equal(org))
		Expect(gotServer).To(Equal(server))
	})

	DescribeTable("refuses a payload that names no org and server",
		func(p string) {
			_, _, ok := mcpservers.ParseProbeWakePayload(p)
			Expect(ok).To(BeFalse())
		},
		Entry("empty", ""),
		Entry("no separator", "0190a3f2-0000-7000-8000-000000000001"),
		Entry("not a UUID", "org/server"),
		Entry("one side empty", "0190a3f2-0000-7000-8000-000000000001/"),
		Entry("a third part", "0190a3f2-0000-7000-8000-000000000001/0190a3f2-0000-7000-8000-000000000002/x"),
	)
})
