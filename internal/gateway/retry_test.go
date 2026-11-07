// SPDX-License-Identifier: Apache-2.0

package gateway_test

import (
	"net/http"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/gateway"
)

var _ = Describe("the do-not-retry marker (old issue 342)", func() {
	It("reads the header the harnesses honour", func() {
		Expect(gateway.DoNotRetryHeader).To(Equal("x-should-retry"))
		Expect(gateway.DoNotRetryValue).To(Equal("false"))
	})

	DescribeTable("DoNotRetry",
		func(h http.Header, want bool) { Expect(gateway.DoNotRetry(h)).To(Equal(want)) },
		Entry("marked", http.Header{"X-Should-Retry": {"false"}}, true),
		Entry("case and space do not matter", http.Header{"X-Should-Retry": {" False "}}, true),
		Entry("explicit retry", http.Header{"X-Should-Retry": {"true"}}, false),
		Entry("absent", http.Header{}, false),
		Entry("nil", nil, false),
	)

	It("is a CEL expression that reads no body", func() {
		Expect(gateway.DoNotRetryCEL).NotTo(ContainSubstring("response.body"))
		Expect(gateway.DoNotRetryCEL).NotTo(ContainSubstring("bodyPrefix"))
		Expect(gateway.DoNotRetryCEL).To(ContainSubstring(`"false"`))
	})
})
