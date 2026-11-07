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
	// The truth table of old issue 316 (deep dive G4, 2026-10-31), one entry per catalog model and route.
	DescribeTable("the catalog capability of a model",
		func(provider, model string, format modelformat.Format, thinks thinking.Thinks, on, off thinking.Mode) {
			c := thinking.Of(provider, model, format)
			Expect(c.Thinks).To(Equal(thinks))
			Expect(c.On).To(Equal(on))
			Expect(c.Off).To(Equal(off))
			Expect(c.Validate()).To(Succeed())
			Expect(c.AnthropicBeta()).To(BeEmpty(), "no on or off form has a beta-only field")
		},
		Entry("direct Haiku 5.5", "anthropic", "claude-haiku-5-5", modelformat.Anthropic, thinking.Optional, thinking.AdaptiveSummarized, thinking.Disabled),
		Entry("OpenRouter Haiku 5.5, dotted and vendor-prefixed", "openrouter", "anthropic/claude-haiku-5.5", modelformat.Anthropic, thinking.Optional, thinking.AdaptiveSummarized, thinking.Disabled),
		Entry("direct Sonnet 5.5: between_tools is not off", "anthropic", "claude-sonnet-5-5", modelformat.Anthropic, thinking.Always, thinking.AdaptiveSummarized, thinking.Mode("")),
		Entry("OpenRouter Sonnet 5.5, case is ignored", "openrouter", "Anthropic/Claude-Sonnet-5.5", modelformat.Anthropic, thinking.Always, thinking.AdaptiveSummarized, thinking.Mode("")),
		Entry("direct Opus 5.5", "anthropic", "claude-opus-5-5", modelformat.Anthropic, thinking.Always, thinking.AdaptiveSummarized, thinking.Mode("")),
		Entry("OpenRouter Opus 5.5", "openrouter", "anthropic/claude-opus-5.5", modelformat.Anthropic, thinking.Always, thinking.AdaptiveSummarized, thinking.Mode("")),
		Entry("direct Fable 5.1", "anthropic", "claude-fable-5-1", modelformat.Anthropic, thinking.Always, thinking.AdaptiveSummarized, thinking.Mode("")),
		Entry("OpenRouter Fable 5.1", "openrouter", "anthropic/claude-fable-5.1", modelformat.Anthropic, thinking.Always, thinking.AdaptiveSummarized, thinking.Mode("")),
		Entry("OpenRouter GPT-6 Luna", "openrouter", "openai/gpt-6-luna", modelformat.OpenAI, thinking.Optional, thinking.Default, thinking.EffortNone),
		Entry("OpenRouter DeepSeek v4.1 flash", "openrouter", "deepseek/deepseek-v4.1-flash", modelformat.OpenAI, thinking.Optional, thinking.Default, thinking.EffortNone),
		Entry("OpenRouter GPT-6 Astra", "openrouter", "openai/gpt-6-astra", modelformat.OpenAI, thinking.Always, thinking.Default, thinking.Mode("")),
		Entry("OpenAI GPT-6 Astra", "openai", "gpt-6-astra", modelformat.OpenAI, thinking.Always, thinking.Default, thinking.Mode("")),
		Entry("OpenAI GPT-6 Luna: no verified off form", "openai", "gpt-6-luna", modelformat.OpenAI, thinking.Always, thinking.Default, thinking.Mode("")),
		Entry("a local model", "ollama", "llama3", modelformat.OpenAI, thinking.Never, thinking.None, thinking.Mode("")),
		Entry("an older Claude", "anthropic", "claude-sonnet-4-5", modelformat.Anthropic, thinking.Never, thinking.None, thinking.Mode("")),
		Entry("a Claude name on a type that does not serve Claude", "ollama", "claude-haiku-5-5", modelformat.Anthropic, thinking.Never, thinking.None, thinking.Mode("")),
		Entry("a Claude model on chat completions has no thinking field to send", "openrouter", "anthropic/claude-haiku-5.5", modelformat.OpenAI, thinking.Never, thinking.None, thinking.Mode("")),
	)

	It("records where each route reports the thinking split and how thinking comes back", func() {
		claude := thinking.Of("anthropic", "claude-haiku-5-5", modelformat.Anthropic)
		Expect(claude.UsageSplit).To(Equal(thinking.SplitAnthropic))
		Expect(claude.Display).To(Equal(thinking.DisplaySummarized))
		luna := thinking.Of("openrouter", "openai/gpt-6-luna", modelformat.OpenAI)
		Expect(luna.UsageSplit).To(Equal(thinking.SplitCompletions))
		Expect(luna.Display).To(Equal(thinking.DisplayVisible))
	})

	It("lists only known modes, and each mode on the format it belongs to", func() {
		for _, format := range modelformat.All() {
			for _, m := range []string{"claude-haiku-5-5", "claude-opus-5-5", "openai/gpt-6-luna", "gpt", ""} {
				for _, p := range []string{"anthropic", "openrouter", "openai"} {
					c := thinking.Of(p, m, format)
					for _, mode := range []thinking.Mode{c.On, c.Off} {
						if mode == "" {
							continue
						}
						Expect(thinking.Known(mode)).To(BeTrue(), m)
						if f := mode.Format(); f != "" {
							Expect(f).To(Equal(format), "%s %s: %s", p, m, mode)
						}
					}
				}
			}
		}
	})
})

var _ = Describe("Effective", func() {
	haiku := thinking.Of("anthropic", "claude-haiku-5-5", modelformat.Anthropic)
	opus := thinking.Of("anthropic", "claude-opus-5-5", modelformat.Anthropic)
	luna := thinking.Of("openrouter", "openai/gpt-6-luna", modelformat.OpenAI)
	astra := thinking.Of("openrouter", "openai/gpt-6-astra", modelformat.OpenAI)
	cannot := thinking.Of("anthropic", "claude-sonnet-4-5", modelformat.Anthropic)

	It("sends the explicit on form, and the explicit off form where the model can stop", func() {
		Expect(haiku.Effective(thinking.On)).To(Equal(thinking.AdaptiveSummarized))
		Expect(haiku.Effective("")).To(Equal(thinking.AdaptiveSummarized))
		Expect(haiku.Effective(thinking.Off)).To(Equal(thinking.Disabled))
		Expect(luna.Effective(thinking.On)).To(Equal(thinking.Default))
		Expect(luna.Effective(thinking.Off)).To(Equal(thinking.EffortNone))
	})

	It("runs a stored off with thinking on for a model that always thinks, and says so", func() {
		Expect(opus.Effective(thinking.Off)).To(Equal(thinking.AdaptiveSummarized))
		Expect(opus.OffOverridden(thinking.Off)).To(BeTrue())
		Expect(opus.OffOverridden(thinking.On)).To(BeFalse())
		Expect(astra.Effective(thinking.Off)).To(Equal(thinking.Default))
		Expect(haiku.OffOverridden(thinking.Off)).To(BeFalse())
	})

	It("sends nothing for a model that does not think", func() {
		Expect(cannot.Effective(thinking.On)).To(Equal(thinking.None))
		Expect(cannot.Effective(thinking.Off)).To(Equal(thinking.None))
		Expect(cannot.Supported()).To(BeFalse())
		Expect(cannot.OffOverridden(thinking.Off)).To(BeFalse())
		Expect(thinking.Capability{}.Effective(thinking.On)).To(Equal(thinking.None), "the zero value does not think")
	})
})

var _ = Describe("Capability.Validate", func() {
	It("accepts the zero value and refuses rows that contradict themselves", func() {
		Expect(thinking.Capability{}.Validate()).To(Succeed())
		Expect(thinking.Capability{Thinks: thinking.Always, On: thinking.AdaptiveSummarized, Off: thinking.Disabled}.Validate()).To(MatchError(ContainSubstring("no off form")))
		Expect(thinking.Capability{Thinks: thinking.Optional, On: thinking.AdaptiveSummarized}.Validate()).To(MatchError(ContainSubstring("an on and an off form")))
		Expect(thinking.Capability{Thinks: thinking.Never, On: thinking.AdaptiveSummarized}.Validate()).To(MatchError(ContainSubstring("does not think")))
		Expect(thinking.Capability{Thinks: thinking.Always, On: "extreme"}.Validate()).To(MatchError(ContainSubstring(`thinking mode "extreme"`)))
		Expect(thinking.Capability{Thinks: "sometimes"}.Validate()).To(MatchError(ContainSubstring(`"sometimes"`)))
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

var _ = Describe("the default price list", func() {
	// sampleName is a model name a price entry's glob matches.
	sampleName := func(glob string) string {
		if glob == "*" {
			return "llama3"
		}
		return glob
	}

	It("has a thinking row for every entry, on the format the entry's model defaults to", func() {
		_, entries := pricing.Default()
		Expect(entries).NotTo(BeEmpty())
		for _, r := range entries {
			glob := r.Entry.Match.Glob
			Expect(glob == "*" || !strings.ContainsAny(glob, "*?[")).To(BeTrue(), "%s: a new kind of glob needs a sample name here", r.Entry.ID)
			name := sampleName(glob)
			c, ok := thinking.Lookup(r.Entry.Provider, name, modelformat.Default(r.Entry.Provider, name))
			Expect(ok).To(BeTrue(), "%s (%s on %s) has no row in internal/thinking", r.Entry.ID, glob, r.Entry.Provider)
			Expect(c.Validate()).To(Succeed(), r.Entry.ID)
		}
	})

	// A model that thinks without showing it bills thinking as output the response may not show, so
	// its price entry says so: that is what flags an aborted response's output as unmeasured (ADR
	// 0056) and what the run's reserve is held for. A model whose provider returns the reasoning text
	// (DeepSeek on OpenRouter) needs no flag.
	It("marks every model that thinks without returning the text as billing hidden thinking", func() {
		_, entries := pricing.Default()
		checked := 0
		for _, r := range entries {
			glob := r.Entry.Match.Glob
			if glob == "" || strings.ContainsAny(glob, "*?[") {
				continue
			}
			c := thinking.Of(r.Entry.Provider, glob, modelformat.Default(r.Entry.Provider, glob))
			if c.Supported() && c.Display != thinking.DisplayVisible {
				checked++
				Expect(r.Entry.HiddenThinking).To(BeTrue(), "%s (%s) thinks", r.Entry.ID, glob)
			}
		}
		Expect(checked).To(BeNumerically(">=", 10), "four Claude models on two providers and two GPT-6 models on OpenAI")
	})
})
