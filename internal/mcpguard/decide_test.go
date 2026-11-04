// SPDX-License-Identifier: Apache-2.0

package mcpguard_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/leakguard"
	"github.com/stateracloud/koinon/internal/mcpguard"
	"github.com/stateracloud/koinon/internal/sinkclass"
)

var _ = Describe("the decision table (ADR 0091 decision 5)", func() {
	clean := leakguard.Decision{Allow: true, Reason: leakguard.ReasonPass}
	taint := leakguard.Decision{Allow: false, Reason: leakguard.ReasonTaint}
	broken := leakguard.Decision{Allow: false, Reason: leakguard.ReasonBudgetWork, Detail: "too much"}
	urlNo := leakguard.Decision{Allow: false, Reason: "url_shape", Detail: "no"}

	DescribeTable("decides", func(f mcpguard.Facts, action mcpguard.Action, reason string) {
		v := mcpguard.Decide(f)
		Expect(v.Action).To(Equal(action))
		Expect(v.Reason).To(Equal(reason))
	},
		Entry("none passes, even with both legs and a match", mcpguard.Facts{Class: sinkclass.None, BothLegs: true, Taint: taint}, mcpguard.Pass, mcpguard.ReasonInternal),
		Entry("internal passes, even with both legs and a match", mcpguard.Facts{Class: sinkclass.Internal, BothLegs: true, Taint: taint, OverCap: true}, mcpguard.Pass, mcpguard.ReasonInternal),
		Entry("external-low, clean", mcpguard.Facts{Class: sinkclass.ExternalLow, Taint: clean}, mcpguard.Pass, mcpguard.ReasonClean),
		Entry("external-low, the URL rule", mcpguard.Facts{Class: sinkclass.ExternalLow, Taint: clean, URLChecked: true, URL: urlNo}, mcpguard.Refuse, mcpguard.ReasonURLRule),
		Entry("external-low, over the cap without both legs", mcpguard.Facts{Class: sinkclass.ExternalLow, Taint: clean, OverCap: true}, mcpguard.Pass, mcpguard.ReasonClean),
		Entry("external-low, both legs, over the cap", mcpguard.Facts{Class: sinkclass.ExternalLow, Taint: clean, BothLegs: true, OverCap: true}, mcpguard.Refuse, mcpguard.ReasonByteCap),
		Entry("external-low, both legs, within the cap", mcpguard.Facts{Class: sinkclass.ExternalLow, Taint: clean, BothLegs: true}, mcpguard.Pass, mcpguard.ReasonClean),
		Entry("external-low, a match", mcpguard.Facts{Class: sinkclass.ExternalLow, Taint: taint}, mcpguard.NeedsDecision, mcpguard.ReasonTaint),
		Entry("external-high, clean", mcpguard.Facts{Class: sinkclass.ExternalHigh, Taint: clean}, mcpguard.Pass, mcpguard.ReasonClean),
		Entry("external-high, a match", mcpguard.Facts{Class: sinkclass.ExternalHigh, Taint: taint}, mcpguard.NeedsDecision, mcpguard.ReasonTaint),
		Entry("external-high, both legs", mcpguard.Facts{Class: sinkclass.ExternalHigh, Taint: clean, BothLegs: true}, mcpguard.NeedsDecision, mcpguard.ReasonBothLegs),
		Entry("an unknown class is external-high", mcpguard.Facts{Class: "", Taint: clean, BothLegs: true}, mcpguard.NeedsDecision, mcpguard.ReasonBothLegs),
		Entry("a check that could not finish refuses", mcpguard.Facts{Class: sinkclass.ExternalHigh, Taint: broken}, mcpguard.Refuse, mcpguard.ReasonCheck),
	)

	It("puts both legs and a match on one decision", func() {
		v := mcpguard.Decide(mcpguard.Facts{Class: sinkclass.ExternalHigh, Taint: taint, BothLegs: true})
		Expect(v.Action).To(Equal(mcpguard.NeedsDecision))
		Expect(v.Reasons).To(ConsistOf(mcpguard.ReasonBothLegs, mcpguard.ReasonTaint))
	})

	It("answers before agentgateway's fixed processor timeout", func() {
		Expect(mcpguard.Deadline).To(BeNumerically(">=", leakguard.DefaultLimits().MaxDuration+mcpguard.LookupBudget))
		Expect(mcpguard.Deadline).To(BeNumerically("<", mcpguard.GatewayProcessorTimeout))
	})
})
