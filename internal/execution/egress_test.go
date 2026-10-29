// SPDX-License-Identifier: Apache-2.0

package execution_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/execution"
)

var _ = Describe("CheckEgressHost", func() {
	DescribeTable("refuses patterns",
		func(host string) {
			err := execution.CheckEgressHost(host)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("DNS exfiltration"))
		},
		Entry("leading label", "*.example.com"),
		Entry("bare star", "*"),
		Entry("double star", "**"),
		Entry("inner star", "api.*.example.com"),
		Entry("partial label", "git*.example.com"),
		Entry("question mark", "a?.example.com"),
		Entry("class", "[ab].example.com"),
		Entry("braces", "{a,b}.example.com"),
	)
	DescribeTable("accepts exact names and addresses",
		func(host string) { Expect(execution.CheckEgressHost(host)).To(Succeed()) },
		Entry("name", "api.github.com"),
		Entry("service", "agentgateway.koinon.svc"),
		Entry("ip", "10.0.0.1"),
	)
})
