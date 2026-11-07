// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/agentd"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/flavor"
	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/thinking"
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
			// The format such a model gets by default (ADR 0069): the one table decides it.
			format := modelformat.Default(r.Entry.Provider, name)
			mode := prices.CacheModeFor(r.Entry.Provider, name, format, r.Price)
			Expect(mode).NotTo(Equal(prices.CacheUnknown),
				"%s (%s/%s) has a cache_read price but no cache mode: add its provider format to cacheModeOf in internal/prices/cache.go (ADR 0068)",
				r.Entry.ID, r.Entry.Provider, name)
			marker := mode.Marker()
			cmd, err := agentd.LaunchCommand(store.AgentFlavorOpencode, agentd.Endpoints{
				ModelBaseURL: "http://gw.test/v1", Model: name, MCPURL: "http://gw.test/mcp", Format: format, CacheMarker: marker,
				Retry: agentd.DefaultRetryPolicy(),
			})
			Expect(err).NotTo(HaveOccurred(), r.Entry.ID)
			switch mode {
			case prices.CacheNative:
				// The harness places the markers: no switch, and the entry for the format.
				Expect(cmd).NotTo(ContainElement(HavePrefix("--cache-")), "%s places its own markers", r.Entry.ID)
				Expect(cmd).To(ContainElements("--model-format", string(format)), r.Entry.ID)
			case prices.CacheAuto:
				Expect(cmd).NotTo(ContainElement(HavePrefix("--cache-")), "%s caches by itself", r.Entry.ID)
			default:
				Expect(cmd).To(ContainElement("--cache-"+marker), "%s needs %s markers", r.Entry.ID, mode)
			}
		}
	})

	// ADR 0069 decision 8: a direct Anthropic model runs on the Messages route with the harness
	// placing the cache markers, and OpenCode gets its own entry for it.
	It("runs every direct Anthropic model in the catalog natively, on the anthropic entry", func() {
		n := 0
		for _, r := range entries {
			if r.Entry.Provider != "anthropic" {
				continue
			}
			n++
			name := strings.TrimSuffix(r.Entry.Match.Glob, "*")
			format := modelformat.Default(r.Entry.Provider, name)
			Expect(prices.CacheModeFor(r.Entry.Provider, name, format, r.Price)).To(Equal(prices.CacheNative),
				"%s is a direct Anthropic model: it must run natively in Messages format with a cache_read price (ADR 0069)", r.Entry.ID)
			Expect(agentd.ModelOption(store.AgentFlavorOpencode, format, name)).To(Equal("koinon-anthropic/"+name), r.Entry.ID)
		}
		Expect(n).To(BeNumerically(">", 0), "the default price list has no direct Anthropic entry")
	})

	DescribeTable("maps provider formats to cache modes",
		func(provider, model string, format modelformat.Format, want prices.CacheMode) {
			p := pricingWithCacheRead()
			Expect(prices.CacheModeFor(provider, model, format, p)).To(Equal(want))
		},
		Entry("direct anthropic: Messages, the harness places the markers", "anthropic", "claude-haiku-5-5", modelformat.Anthropic, prices.CacheNative),
		Entry("openrouter anthropic an admin kept on chat completions: cache_control markers", "openrouter", "anthropic/claude-haiku-5.5", modelformat.OpenAI, prices.CacheAnthropic),
		Entry("openrouter anthropic in Messages format (the default since old issue 242)", "openrouter", "anthropic/claude-haiku-5.5", modelformat.Anthropic, prices.CacheNative),
		Entry("openrouter qwen", "openrouter", "qwen/qwen4", modelformat.OpenAI, prices.CacheAlibaba),
		Entry("openai", "openai", "gpt-6-luna", modelformat.OpenAI, prices.CacheAuto),
		Entry("openrouter deepseek", "openrouter", "deepseek/deepseek-v4.1-flash", modelformat.OpenAI, prices.CacheAuto),
		Entry("an unmapped format", "openrouter", "mistral/large", modelformat.OpenAI, prices.CacheUnknown),
		Entry("an unmapped provider", "newprovider", "m", modelformat.OpenAI, prices.CacheUnknown),
	)

	It("leaves a model without a cache_read price alone", func() {
		Expect(prices.CacheModeFor("anthropic", "claude-haiku-5-5", modelformat.Anthropic, pricing.Price{})).To(Equal(prices.CacheNone))
	})

	It("counts only an unmapped model as uncached", func() {
		Expect(prices.CacheUnknown.Uncached()).To(BeTrue())
		Expect(prices.CacheNative.Uncached()).To(BeFalse())
		Expect(prices.CacheNative.Marker()).To(BeEmpty())
	})

	It("launches a direct Anthropic model on the anthropic entry", func() {
		cmd, err := agentd.LaunchCommand(store.AgentFlavorOpencode, agentd.Endpoints{
			ModelBaseURL: "http://gw.test/v1", Model: "claude-haiku-5-5", MCPURL: "http://gw.test/mcp", Format: modelformat.Anthropic,
			Retry: agentd.DefaultRetryPolicy(),
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(cmd).To(Equal([]string{
			"/usr/local/bin/koinon-opencode", "--model-base-url", "http://gw.test/v1", "--model", "claude-haiku-5-5",
			"--mcp-url", "http://gw.test/mcp",
			"--model-tries", "3", "--model-retry-backoff-ms", "1000", "--model-max-retry-after-ms", "60000",
			"--model-format", "anthropic",
		}))
		Expect(agentd.ModelOption(store.AgentFlavorOpencode, modelformat.Anthropic, "claude-haiku-5-5")).To(Equal("koinon-anthropic/claude-haiku-5-5"))
	})

	It("passes the thinking mode Koinon decided, and nothing when none was", func() {
		for _, mode := range thinking.Modes() {
			cmd, err := agentd.LaunchCommand(store.AgentFlavorOpencode, agentd.Endpoints{
				ModelBaseURL: "http://gw.test/v1", Model: "claude-haiku-5-5", MCPURL: "http://gw.test/mcp", Format: modelformat.Anthropic, Thinking: mode,
				Retry: agentd.DefaultRetryPolicy(),
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(cmd[len(cmd)-2:]).To(Equal([]string{"--thinking", string(mode)}))
		}
	})

	DescribeTable("selects the entry of the model's format",
		func(format modelformat.Format, want string) {
			Expect(agentd.ModelOption(store.AgentFlavorOpencode, format, "m")).To(Equal(want))
		},
		Entry("openai", modelformat.OpenAI, "koinon/m"),
		Entry("no format is chat completions", modelformat.Format(""), "koinon/m"),
		Entry("anthropic", modelformat.Anthropic, "koinon-anthropic/m"),
		Entry("a format the flavor has no entry for", modelformat.Format("gemini"), ""),
	)

	It("covers every known format with an entry in every flavor", func() {
		for _, d := range flavor.All() {
			for _, f := range modelformat.All() {
				Expect(d.ModelEntries).To(HaveKey(f), "flavor %s has no provider entry for the %s format", d.Name, f)
			}
		}
	})

	It("refuses a format the flavor has no entry for", func() {
		_, err := agentd.LaunchCommand(store.AgentFlavorOpencode, agentd.Endpoints{Format: "gemini", Retry: agentd.DefaultRetryPolicy()})
		Expect(err).To(MatchError(ContainSubstring("no provider entry")))
	})

	It("passes the run ID as --session-id, and nothing without one", func() {
		e := agentd.Endpoints{ModelBaseURL: "http://gw.test/v1", Model: "m", MCPURL: "http://gw.test/mcp", SessionID: "run-1", Retry: agentd.DefaultRetryPolicy()}
		cmd, err := agentd.LaunchCommand(store.AgentFlavorOpencode, e)
		Expect(err).NotTo(HaveOccurred())
		Expect(cmd[len(cmd)-2:]).To(Equal([]string{"--session-id", "run-1"}))
		e.SessionID = ""
		cmd, err = agentd.LaunchCommand(store.AgentFlavorOpencode, e)
		Expect(err).NotTo(HaveOccurred())
		Expect(cmd).NotTo(ContainElement("--session-id"))
	})

	It("refuses a marker the flavor has no switch for", func() {
		_, err := agentd.LaunchCommand(store.AgentFlavorOpencode, agentd.Endpoints{CacheMarker: "gemini", Retry: agentd.DefaultRetryPolicy()})
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
