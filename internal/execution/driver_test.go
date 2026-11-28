// SPDX-License-Identifier: Apache-2.0

package execution_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/execution"
)

var _ = Describe("ParseDriver", func() {
	DescribeTable("reads execution.driver",
		func(in string, want execution.DriverName, ok bool) {
			got, err := execution.ParseDriver(in)
			if !ok {
				Expect(err).To(HaveOccurred())
				return
			}
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(want))
		},
		Entry("unset is Substrate (#1093)", "", execution.DriverSubstrate, true),
		Entry("openshell", "openshell", execution.DriverOpenShell, true),
		Entry("substrate", "substrate", execution.DriverSubstrate, true),
		Entry("an unknown name is refused", "kata", execution.DriverName(""), false),
		Entry("case matters", "Substrate", execution.DriverName(""), false),
	)

	It("has Substrate as the default since #1093", func() {
		Expect(execution.DefaultDriver).To(Equal(execution.DriverSubstrate))
		Expect(execution.DriverOpenShell.HasEgress()).To(BeTrue())
		Expect(execution.DriverSubstrate.HasEgress()).To(BeFalse())
	})
})
