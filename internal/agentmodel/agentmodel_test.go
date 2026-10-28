// SPDX-License-Identifier: Apache-2.0

package agentmodel_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/agentmodel"
)

var _ = DescribeTable("Choose: the agent's setting, then the version's model, then the org default",
	func(setting, version, platform, want string, src agentmodel.Source) {
		got, from := agentmodel.Choose(setting, version, platform)
		Expect(got).To(Equal(want))
		Expect(from).To(Equal(src))
	},
	Entry("the agent's setting wins", "a", "v", "p", "a", agentmodel.SourceAgent),
	Entry("the version's model without a setting", "", "v", "p", "v", agentmodel.SourceVersion),
	Entry("'default' is no setting", "default", "v", "", "v", agentmodel.SourceVersion),
	Entry("a version that says default falls through", "", "default", "p", "p", agentmodel.SourceOrgDefault),
	Entry("padded names are trimmed", " a ", "", "", "a", agentmodel.SourceAgent),
	Entry("nothing names a model: the caller takes the org's first served one", "", "default", "", "", agentmodel.SourceNone),
)

var _ = Describe("UnusableError", func() {
	It("gives the run's failure reason, never a fallback", func() {
		Expect((&agentmodel.UnusableError{Model: "m", Reason: "not_served"}).RunReason()).To(Equal(`the agent's model "m" is not served in this org; pick another on the agent page`))
		Expect((&agentmodel.UnusableError{Model: "m", Reason: "unpriced"}).RunReason()).To(ContainSubstring("has no price"))
	})
})
