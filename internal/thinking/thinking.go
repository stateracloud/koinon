// SPDX-License-Identifier: Apache-2.0

// Package thinking is the one table that knows whether a model thinks, whether that can be turned
// off, and what goes on the wire for each (ADR 0069 amendments 2026-10-29 and 2026-10-31, old issue 316).
//
// Koinon decides whether a run's model thinks; no harness does, and no provider default does
// either. A harness that guesses from a model ID (OpenCode turns adaptive thinking on for every ID
// that parses as Claude 5.1 or later) sends a request body the gateway has no way to vouch for, and
// a provider default differs per route (direct Anthropic hides Claude 5.x thinking unless asked,
// OpenRouter summarizes it). So the table has one row per catalog model and route, and every row
// names the exact form for thinking on and for thinking off, or says the model cannot stop
// thinking. Three things hang off it and nowhere else:
//
//   - whether the model thinks never, optionally or always ([Thinks]),
//   - the form a run sends, from the row and the agent's setting ([Capability.Effective]), which the
//     flavor's entry script renders, and
//   - the anthropic-beta values the gateway pins for the model (Capability.Betas).
//
// A new model is one row. A new form is one [Mode], one case in each flavor's entry script and,
// when it needs a beta, the value in the row. Callers never switch on model names; they call this
// package.
package thinking

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/stateracloud/koinon/internal/modelformat"
)

// Mode is the form a run's request takes for thinking: what the harness puts in the body.
type Mode string

// The modes Koinon knows. The strings reach the flavors' entry scripts (--thinking), so a new one
// needs a case there.
const (
	// None sends nothing: the model does not think, or Koinon has not wired its thinking.
	None Mode = "none"
	// Default sends nothing because the model's own default is to think: chat-completions models
	// with thinking on.
	Default Mode = "default"
	// AdaptiveSummarized is Claude's adaptive thinking on the Messages API with the thinking shown
	// as a summary: thinking {"type": "adaptive", "display": "summarized"}. Direct Anthropic
	// defaults Claude 5.x to display "omitted", which bills the thinking and returns empty blocks.
	AdaptiveSummarized Mode = "adaptive-summarized"
	// Disabled turns a Claude model's thinking off on the Messages API: thinking {"type":
	// "disabled"}. Only a model whose row has it as its off form takes it; the others refuse it.
	Disabled Mode = "disabled"
	// EffortNone turns a chat-completions model's reasoning off on OpenRouter: reasoning {"effort":
	// "none"}. Only a model whose row has it as its off form takes it.
	EffortNone Mode = "effort-none"
)

var modes = []Mode{None, Default, AdaptiveSummarized, Disabled, EffortNone}

// Known reports whether m is a mode Koinon knows.
func Known(m Mode) bool { return slices.Contains(modes, m) }

// Modes returns every mode Koinon knows.
func Modes() []Mode { return slices.Clone(modes) }

// Format is the wire format the mode's body field belongs to, "" when it sends nothing and so fits
// either.
func (m Mode) Format() modelformat.Format {
	switch m {
	case AdaptiveSummarized, Disabled:
		return modelformat.Anthropic
	case EffortNone:
		return modelformat.OpenAI
	}
	return ""
}

// Thinks is whether a model thinks.
type Thinks string

const (
	// Never is a model that does not think through Koinon. The zero value reads the same.
	Never Thinks = "never"
	// Optional is a model whose thinking can really be turned off.
	Optional Thinks = "optional"
	// Always is a model that thinks whatever the request says: it refuses the off forms, or has
	// none (Claude Sonnet 5.5's between_tools only drops the thinking before the first tool call, so
	// it is not off).
	Always Thinks = "always"
)

// Display is how the model's thinking comes back. It is what the table records; the request only
// carries it for Claude on Messages (the summarized display of AdaptiveSummarized).
type Display string

const (
	// DisplaySummarized: Koinon asks for a summary of the thinking (Claude on Messages).
	DisplaySummarized Display = "summarized"
	// DisplayVisible: the provider returns the reasoning text by itself (OpenRouter chat completions).
	DisplayVisible Display = "visible"
	// DisplayHidden: the provider returns no reasoning text (OpenAI chat completions).
	DisplayHidden Display = "hidden"
)

// UsageSplit is where the provider reports how many of the output tokens were thinking.
type UsageSplit string

const (
	// SplitAnthropic is usage.output_tokens_details.thinking_tokens of the Messages API (on the last
	// message_delta when streaming). agentgateway v1.7.0 does not read it, so the gateway logs no
	// split for these models: the ledger records it as unknown (spend_entries.reasoning_known false)
	// unless an OpenRouter settlement reports one, and the thinking stays billed inside the output
	// count, which agentgateway takes from the final message_delta (#78).
	SplitAnthropic UsageSplit = "anthropic-thinking-tokens"
	// SplitCompletions is usage.completion_tokens_details.reasoning_tokens of chat completions, which
	// agentgateway reads into the ledger.
	SplitCompletions UsageSplit = "completions-reasoning-tokens"
)

// AlwaysThinksMsg is the refusal of "off" for a model that always thinks, and what a UI shows in
// place of an off choice.
const AlwaysThinksMsg = "this model always thinks"

// OffOverriddenMsg says that an agent's stored "off" does not apply to its model, which always
// thinks: the run thinks, and the setting is kept for a model that can stop.
const OffOverriddenMsg = "this model always thinks; your off setting doesn't apply"

// BindingBeta is the beta of thinking.block_binding, the body field OpenCode adds to adaptive
// thinking when its own heuristic decides. Anthropic refuses the field without the beta. No row pins
// it since the explicit on form (old issue 316) leaves block_binding out; it stays named because the strict
// fake and the gateway specs use it as the example of a beta-only field.
const BindingBeta = "thinking-binding-controls-2026-08-01"

// Capability is what the table says about one model on one route.
type Capability struct {
	Thinks Thinks
	// On is the form for thinking on; None when the model does not think.
	On Mode
	// Off is the form for thinking off; "" when the model cannot stop thinking (Always) or does not
	// think.
	Off     Mode
	Display Display
	// Betas are the anthropic-beta values the gateway sets on every request to the model, whether
	// or not an agent has thinking on: the pin depends on the model alone. Empty when the model
	// needs none; the gateway then drops the client's anthropic-beta header (ADR 0069).
	Betas []string
	// UsageSplit is where the provider reports the thinking part of the output tokens.
	UsageSplit UsageSplit
}

// Supported reports whether the model thinks at all.
func (c Capability) Supported() bool { return c.Thinks == Optional || c.Thinks == Always }

// CanTurnOff reports whether the model's thinking can really be turned off.
func (c Capability) CanTurnOff() bool { return c.Thinks == Optional }

// AlwaysThinks reports whether the model thinks whatever the request says.
func (c Capability) AlwaysThinks() bool { return c.Thinks == Always }

// AnthropicBeta is the value of the anthropic-beta header the gateway sets, "" for none.
func (c Capability) AnthropicBeta() string { return strings.Join(c.Betas, ",") }

// Effective is the form a run sends for the setting: None for a model that does not think, the off
// form when the setting is off and the model can stop, and the on form otherwise. A stored "off" on
// a model that always thinks runs with thinking on ([Capability.OffOverridden]); the run never fails
// for it.
func (c Capability) Effective(s Setting) Mode {
	switch {
	case !c.Supported():
		return None
	case s == Off && c.CanTurnOff():
		return c.Off
	}
	return c.On
}

// OffOverridden reports whether the setting is off and the model thinks anyway.
func (c Capability) OffOverridden(s Setting) bool { return s == Off && c.AlwaysThinks() }

// Validate checks that the capability is one the table could hold: known values, and an off form
// exactly when the model can stop thinking. The zero value is a model that does not think.
func (c Capability) Validate() error {
	var errs []error
	for _, m := range []Mode{c.On, c.Off} {
		if m != "" && !Known(m) {
			errs = append(errs, fmt.Errorf("unknown thinking mode %q", m))
		}
	}
	switch c.Thinks {
	case "", Never:
		if (c.On != "" && c.On != None) || c.Off != "" {
			errs = append(errs, errors.New("a model that does not think has no thinking form"))
		}
	case Optional:
		if c.Off == "" || c.Off == None || c.Off == Default || c.On == "" || c.On == None {
			errs = append(errs, errors.New("a model whose thinking can be turned off needs an on and an off form"))
		}
	case Always:
		if c.Off != "" || c.On == "" || c.On == None {
			errs = append(errs, errors.New("a model that always thinks has an on form and no off form"))
		}
	default:
		errs = append(errs, fmt.Errorf("unknown thinking capability %q", c.Thinks))
	}
	return errors.Join(errs...)
}

// Setting is an agent's choice: thinking on or off. The zero value is on, the default.
type Setting string

// The settings. The strings are stored in agents.thinking and templates.
const (
	On  Setting = "on"
	Off Setting = "off"
)

// ValidSetting reports whether s is "on" or "off".
func ValidSetting(s string) bool { return s == string(On) || s == string(Off) }

// ParseSetting reads a stored or requested setting. "" is "not set" (ok with the zero Setting).
func ParseSetting(s string) (Setting, error) {
	switch s = strings.TrimSpace(s); s {
	case "":
		return "", nil
	case string(On), string(Off):
		return Setting(s), nil
	}
	return "", fmt.Errorf("thinking must be %q or %q, not %q", On, Off, s)
}

// Choose applies the precedence to the agent's own setting and the version's (a template's
// spec.model.thinking): the first that is set, else On. Thinking is on unless somebody turned it off.
func Choose(agent, version Setting) Setting {
	for _, s := range []Setting{agent, version} {
		if ValidSetting(string(s)) {
			return s
		}
	}
	return On
}

// row is one model family on one route. The first row that matches wins.
type row struct {
	// Provider is the provider type the row is for.
	Provider string
	// Format is the wire format the row is for; empty matches every format.
	Format modelformat.Format
	// Prefix is matched against the start of the normalized model name (see normalize); empty
	// matches every name.
	Prefix string
	Capability
}

// The forms per kind of row. Sources, 2026-10-31 (deep dive G4 of old issue 316): Anthropic's adaptive
// thinking, effort and display pages (platform.claude.com/docs/en/build-with-claude/), OpenRouter's
// reasoning-tokens guide and models API (reasoning.mandatory, supported efforts), models.dev
// reasoning_options, OpenAI's reasoning guide; the rc.9 home-lab runs for what each route reported.
var (
	// claudeOptional: Haiku 5.5 takes thinking {"type": "disabled"} on Anthropic and on OpenRouter's
	// Messages route (which maps it to reasoning.enabled false).
	claudeOptional = Capability{Thinks: Optional, On: AdaptiveSummarized, Off: Disabled, Display: DisplaySummarized, UsageSplit: SplitAnthropic}
	// claudeAlways: Sonnet, Opus and Fable 5.x refuse "disabled" (400) on Anthropic and are
	// reasoning.mandatory on OpenRouter.
	claudeAlways = Capability{Thinks: Always, On: AdaptiveSummarized, Display: DisplaySummarized, UsageSplit: SplitAnthropic}
	// orOptional: OpenRouter chat-completions models that think by default and take reasoning
	// {"effort": "none"} (rc.9: Luna and DeepSeek reported 5k-49k reasoning tokens with nothing sent).
	orOptional = Capability{Thinks: Optional, On: Default, Off: EffortNone, Display: DisplayVisible, UsageSplit: SplitCompletions}
	// orAlways: reasoning.mandatory on OpenRouter; "none" is refused.
	orAlways = Capability{Thinks: Always, On: Default, Display: DisplayVisible, UsageSplit: SplitCompletions}
	// openaiAlways: OpenAI's own chat completions think by default and return no reasoning text.
	openaiAlways = Capability{Thinks: Always, On: Default, Display: DisplayHidden, UsageSplit: SplitCompletions}
	never        = Capability{Thinks: Never, On: None}
)

// claudeFamilies are the Claude families of the price list, as normalized prefixes, with whether
// they can stop thinking.
var claudeFamilies = []struct {
	prefix string
	c      Capability
}{
	{"claude-haiku-5-", claudeOptional},
	{"claude-sonnet-5-", claudeAlways},
	{"claude-opus-5-", claudeAlways},
	{"claude-fable-5-", claudeAlways},
}

// rows is the table, most specific rows first. A name that matches no row does not think: a Claude
// model that is not listed, or a Claude model on chat completions, runs without a thinking field
// until a row is added, because a field the model does not take is a refusal (ADR 0069 amendment).
var rows = func() []row {
	var out []row
	for _, f := range claudeFamilies {
		// No beta on either route: the on form has no beta-only field. OpenRouter's Messages route
		// took adaptive thinking without one in the live check of 2026-10-29 (old issue 242).
		out = append(out,
			row{Provider: modelformat.ProviderAnthropic, Format: modelformat.Anthropic, Prefix: f.prefix, Capability: f.c},
			row{Provider: modelformat.ProviderOpenRouter, Format: modelformat.Anthropic, Prefix: f.prefix, Capability: f.c},
		)
	}
	return append(out,
		row{Provider: modelformat.ProviderOpenRouter, Format: modelformat.OpenAI, Prefix: "openai/gpt-6-luna", Capability: orOptional},
		row{Provider: modelformat.ProviderOpenRouter, Format: modelformat.OpenAI, Prefix: "deepseek/deepseek-v4-1-flash", Capability: orOptional},
		row{Provider: modelformat.ProviderOpenRouter, Format: modelformat.OpenAI, Prefix: "openai/gpt-6-astra", Capability: orAlways},
		row{Provider: "openai", Format: modelformat.OpenAI, Prefix: "gpt-6-astra", Capability: openaiAlways},
		// Luna on OpenAI's own API documents reasoning_effort "none", but no live check has covered
		// it and no flavor sends that field: until one does, Koinon offers no off here.
		row{Provider: "openai", Format: modelformat.OpenAI, Prefix: "gpt-6-luna", Capability: openaiAlways},
		// Local models: thinking depends on the model and is not wired.
		row{Provider: "ollama", Capability: never},
	)
}()

// normalize lowercases a model name, drops OpenRouter's anthropic/ vendor prefix and writes the
// version with dashes, so claude-haiku-5-5 and anthropic/claude-haiku-5.5 are one name.
func normalize(model string) string {
	n := strings.ToLower(strings.TrimSpace(model))
	n = strings.TrimPrefix(n, "anthropic/")
	return strings.ReplaceAll(n, ".", "-")
}

// Lookup returns the row for a model of a provider type that speaks format, and whether a row
// matched.
func Lookup(providerType, model string, format modelformat.Format) (Capability, bool) {
	name := normalize(model)
	for _, r := range rows {
		if r.Provider == providerType && (r.Format == "" || r.Format == format) && strings.HasPrefix(name, r.Prefix) {
			c := r.Capability
			c.Betas = slices.Clone(c.Betas)
			return c, true
		}
	}
	return Capability{Thinks: Never, On: None}, false
}

// Of returns what the table says about a model of a provider type that speaks format. A model no
// row matches does not think.
func Of(providerType, model string, format modelformat.Format) Capability {
	c, _ := Lookup(providerType, model, format)
	return c
}
