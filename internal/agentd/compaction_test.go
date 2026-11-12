// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/agentd"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/pricing"
)

// The compaction point is per model, from the catalog's first context tier, with OpenCode's 170k
// when the catalog has none (ADR 0070 amendment 2026-11-11, issue 97).
var _ = Describe("compaction point", func() {
	price := func(over ...int64) pricing.Price {
		spec := pricing.Spec{Prices: pricing.Wire{Input: "0.1", Output: "0.5"}}
		for _, o := range over {
			spec.Tiers = append(spec.Tiers, pricing.WireTier{ContextOver: o, Prices: pricing.Wire{Input: "0.5", Output: "2.5"}})
		}
		p, err := spec.Parse()
		Expect(err).NotTo(HaveOccurred())
		return p
	}

	It("is the first tier boundary of a model with tiers (Haiku 5.5 bills over 100k at 5x)", func() {
		Expect(agentd.CompactionTokens(price(100_000))).To(Equal(100_000))
		Expect(agentd.CompactionTokens(price(100_000, 500_000))).To(Equal(100_000))
	})

	It("is 170k for a model without tiers", func() {
		Expect(agentd.DefaultCompactionTokens).To(Equal(170_000))
		Expect(agentd.CompactionTokens(price())).To(Equal(170_000))
	})

	It("never exceeds 170k when the first tier starts higher", func() {
		Expect(agentd.CompactionTokens(price(272_000))).To(Equal(170_000))
	})

	It("is a launch switch only when set", func() {
		e := agentd.Endpoints{ModelBaseURL: "http://gw.test/v1", Model: "m-1", MCPURL: "http://gw.test/mcp", Retry: agentd.DefaultRetryPolicy()}
		cmd, err := agentd.LaunchCommand(store.AgentFlavorOpencode, e)
		Expect(err).NotTo(HaveOccurred())
		Expect(cmd).NotTo(ContainElement("--compaction-tokens"))
		e.CompactionTokens = 100_000
		cmd, err = agentd.LaunchCommand(store.AgentFlavorOpencode, e)
		Expect(err).NotTo(HaveOccurred())
		Expect(cmd).To(ContainElements("--compaction-tokens", "100000"))
	})
})
