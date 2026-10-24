// SPDX-License-Identifier: Apache-2.0

package ledger_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/ledger"
)

var _ = Describe("FormatUSD", func() {
	DescribeTable("trims a numeric",
		func(in, want string) { Expect(ledger.FormatUSD(in)).To(Equal(want)) },
		Entry("zero", "0", "0.00"),
		Entry("trailing zeros", "0.0020000000", "0.002"),
		Entry("whole", "3.0000000000", "3.00"),
		Entry("cents", "12.5", "12.50"),
	)
})
