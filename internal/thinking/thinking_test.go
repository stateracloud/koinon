// SPDX-License-Identifier: Apache-2.0

package thinking_test

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/internal/thinking"
	"github.com/stateracloud/koinon/pricing"
)

var _ = Describe("Of", func() {
	DescribeTable("the catalog capability of a model",
		func(provider, model string, mode thinking.Mode, beta string) {
			c := thinking.Of(provider, model, modelformat.Anthropic)
			Expect(c.Mode).To(Equal(mode))
			Expect(c.AnthropicBeta()).To(Equal(beta))
		},
		Entry("direct Haiku", "anthropic", "claude-haiku-5-5", thinking.Adaptive, thinking.BindingBeta),
		Entry("direct Opus", "anthropic", "claude-opus-5-5", thinking.Adaptive, thinking.BindingBeta),
		Entry("direct Fable", "anthropic", "claude-fable-5-1", thinking.Adaptive, thinking.BindingBeta),
		Entry("OpenRouter Haiku, dotted and vendor-prefixed", "openrouter", "anthropic/claude-haiku-5.5", thinking.Adaptive, ""),
		Entry("OpenRouter, case is ignored", "openrouter", "Anthropic/Claude-Sonnet-5.5", thinking.Adaptive, ""),
		Entry("an older Claude", "anthropic", "claude-sonnet-4-5", thinking.None, ""),
		Entry("a model of another family", "openrouter", "openai/gpt-6-luna", thinking.None, ""),
		Entry("a local model", "ollama", "llama3", thinking.None, ""),
		Entry("a Claude name on a type that does not serve Claude", "ollama", "claude-haiku-5-5", thinking.None, ""),
	)

	It("returns a copy of the betas", func() {
		c := thinking.Of("anthropic", "claude-haiku-5-5", modelformat.Anthropic)
		c.Betas[0] = "changed"
		Expect(thinking.Of("anthropic", "claude-haiku-5-5", modelformat.Anthropic).Betas).To(Equal([]string{thinking.BindingBeta}))
	})

	It("does not think on chat completions, whatever the name", func() {
		Expect(thinking.Of("openrouter", "anthropic/claude-haiku-5.5", modelformat.OpenAI).Mode).To(Equal(thinking.None))
		Expect(thinking.Of("anthropic", "claude-haiku-5-5", "").Mode).To(Equal(thinking.None))
	})

	It("lists only known modes", func() {
		for _, m := range []string{"claude-haiku-5-5", "claude-opus-5-5", "gpt", ""} {
			Expect(thinking.Known(thinking.Of("anthropic", m, modelformat.Anthropic).Mode)).To(BeTrue(), m)
		}
	})
})

var _ = Describe("Effective", func() {
	It("thinks only when the model can and the setting is on", func() {
		can := thinking.Of("anthropic", "claude-haiku-5-5", modelformat.Anthropic)
		cannot := thinking.Of("anthropic", "claude-sonnet-4-5", modelformat.Anthropic)
		Expect(can.Effective(thinking.On)).To(Equal(thinking.Adaptive))
		Expect(can.Effective("")).To(Equal(thinking.Adaptive))
		Expect(can.Effective(thinking.Off)).To(Equal(thinking.None))
		Expect(cannot.Effective(thinking.On)).To(Equal(thinking.None))
		Expect(cannot.Supported()).To(BeFalse())
	})
})

var _ = Describe("Choose", func() {
	It("is on unless somebody set it off, and the agent beats the version", func() {
		Expect(thinking.Choose("", "")).To(Equal(thinking.On))
		Expect(thinking.Choose("", thinking.Off)).To(Equal(thinking.Off))
		Expect(thinking.Choose(thinking.On, thinking.Off)).To(Equal(thinking.On))
		Expect(thinking.Choose(thinking.Off, thinking.On)).To(Equal(thinking.Off))
		Expect(thinking.Choose("bogus", thinking.Off)).To(Equal(thinking.Off))
	})
})

var _ = Describe("ParseSetting", func() {
	It("accepts on, off and not set, and refuses anything else", func() {
		for in, want := range map[string]thinking.Setting{"": "", " on ": thinking.On, "off": thinking.Off} {
			got, err := thinking.ParseSetting(in)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(want))
		}
		_, err := thinking.ParseSetting("high")
		Expect(err).To(MatchError(ContainSubstring(`"on" or "off"`)))
	})
})

// A model that can think bills thinking as output the response may not show, so its price entry
// says so: that is what flags an aborted response's output as unmeasured (ADR 0056) and what the
// run's reserve is held for. Haiku 5.5 lacked the flag until thinking went on by default.
var _ = Describe("the default price list", func() {
	It("marks every model the catalog says can think as billing hidden thinking", func() {
		_, entries := pricing.Default()
		checked := 0
		for _, r := range entries {
			glob := r.Entry.Match.Glob
			if glob == "" || strings.ContainsAny(glob, "*?[") {
				continue
			}
			if thinking.Of(r.Entry.Provider, glob, modelformat.Anthropic).Supported() {
				checked++
				Expect(r.Entry.HiddenThinking).To(BeTrue(), "%s (%s) can think", r.Entry.ID, glob)
			}
		}
		Expect(checked).To(BeNumerically(">=", 8), "the price list ships four Claude models on two providers")
	})
})
