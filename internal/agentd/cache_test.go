// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/agentd"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/pricing"
)

// ADR 0068: every model the default price list gives a cache_read price runs with caching on.
var _ = Describe("prompt caching of the default price list", func() {
	_, entries := pricing.Default()

	It("lists models with cache prices", func() {
		n := 0
		for _, r := range entries {
			if r.Price.Base.CacheRead != nil {
				n++
			}
		}
		Expect(n).To(BeNumerically(">", 0))
	})

	It("starts the harness with caching on for every model with a cache_read price", func() {
		for _, r := range entries {
			if r.Price.Base.CacheRead == nil {
				continue
			}
			name := strings.TrimSuffix(r.Entry.Match.Glob, "*")
			mode := prices.CacheModeFor(r.Entry.Provider, name, r.Price)
			Expect(mode).NotTo(Equal(prices.CacheUnknown),
				"%s (%s/%s) has a cache_read price but no cache mode: add its provider format to cacheModeOf in internal/prices/cache.go (ADR 0068)",
				r.Entry.ID, r.Entry.Provider, name)
			marker := mode.Marker()
			cmd, err := agentd.LaunchCommand(store.AgentFlavorOpencode, agentd.Endpoints{
				ModelBaseURL: "http://gw.test/v1", Model: name, MCPURL: "http://gw.test/mcp", CacheMarker: marker,
			})
			Expect(err).NotTo(HaveOccurred(), r.Entry.ID)
			switch mode {
			case prices.CacheAuto:
				Expect(cmd).NotTo(ContainElement(HavePrefix("--cache-")), "%s caches by itself", r.Entry.ID)
			default:
				Expect(cmd).To(ContainElement("--cache-"+marker), "%s needs %s markers", r.Entry.ID, mode)
			}
		}
	})

	DescribeTable("maps provider formats to cache modes",
		func(provider, model string, want prices.CacheMode) {
			p := pricingWithCacheRead()
			Expect(prices.CacheModeFor(provider, model, p)).To(Equal(want))
		},
		Entry("anthropic", "anthropic", "claude-haiku-5-5", prices.CacheAnthropic),
		Entry("openrouter anthropic", "openrouter", "anthropic/claude-haiku-5.5", prices.CacheAnthropic),
		Entry("openrouter qwen", "openrouter", "qwen/qwen4", prices.CacheAlibaba),
		Entry("openai", "openai", "gpt-6-luna", prices.CacheAuto),
		Entry("openrouter deepseek", "openrouter", "deepseek/deepseek-v4.1-flash", prices.CacheAuto),
		Entry("an unmapped format", "openrouter", "mistral/large", prices.CacheUnknown),
		Entry("an unmapped provider", "newprovider", "m", prices.CacheUnknown),
	)

	It("leaves a model without a cache_read price alone", func() {
		Expect(prices.CacheModeFor("anthropic", "claude-haiku-5-5", pricing.Price{})).To(Equal(prices.CacheNone))
	})

	It("refuses a marker the flavor has no switch for", func() {
		_, err := agentd.LaunchCommand(store.AgentFlavorOpencode, agentd.Endpoints{CacheMarker: "gemini"})
		Expect(err).To(MatchError(ContainSubstring("cannot send")))
	})
})

func pricingWithCacheRead() pricing.Price {
	_, entries := pricing.Default()
	for _, r := range entries {
		if r.Price.Base.CacheRead != nil {
			return r.Price
		}
	}
	panic("no entry with a cache_read price")
}
