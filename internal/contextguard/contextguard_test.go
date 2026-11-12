// SPDX-License-Identifier: Apache-2.0

package contextguard_test

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/contextguard"
)

var _ = Describe("contextguard", func() {
	It("caps an input at 80% of the window, and assumes a default window when the catalog has none", func() {
		Expect(contextguard.Cap(100_000)).To(BeEquivalentTo(80_000))
		Expect(contextguard.Cap(0)).To(BeEquivalentTo(160_000))
		Expect(contextguard.Cap(-1)).To(BeEquivalentTo(160_000))
	})

	It("lets inputs under the cap pass, and names the ones over it", func() {
		// 2.3 bytes per token: 100_000 tokens are 230_000 bytes.
		small := strings.Repeat("a", 100_000)
		edge := strings.Repeat("a", 229_990) // just under 100_000 tokens, the cap of a 125_000 window
		big := strings.Repeat("a", 230_100)
		Expect(contextguard.Check([]string{small, edge}, 125_000)).To(BeEmpty())
		over := contextguard.Check([]string{small, big, edge, big}, 125_000)
		Expect(over).To(HaveLen(2))
		Expect(over[0].Index).To(Equal(1))
		Expect(over[1].Index).To(Equal(3))
		Expect(over[0].Tokens).To(BeNumerically(">", contextguard.Cap(125_000)))
	})

	It("passes every input of an empty list and an empty input", func() {
		Expect(contextguard.Check(nil, 0)).To(BeEmpty())
		Expect(contextguard.Check([]string{""}, 0)).To(BeEmpty())
	})
})
