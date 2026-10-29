// SPDX-License-Identifier: Apache-2.0

// Package thinking is the one table that knows whether a model can think and how (ADR 0069
// amendment 2026-10-29, "thinking is on unless the agent turns it off").
//
// Koinon decides whether a run's model thinks; no harness does. A harness that guesses from a model
// ID (OpenCode turns adaptive thinking on for every ID that parses as Claude 5.1 or later) sends a
// request body the gateway has no way to vouch for, so three things hang off this table and nowhere
// else:
//
//   - the model's [Mode], the way it thinks (None for a model that does not),
//   - the anthropic-beta values the gateway pins for that mode (Capability.Betas), and
//   - the harness setting derived from the mode and the agent's own setting ([Capability.Effective]).
//
// A new mode is one row in the table, one value in each flavor's entry script and, when it needs a
// beta, the value in the row. Callers never switch on model names; they call this package.
package thinking

import (
	"fmt"
	"slices"
	"strings"

	"github.com/stateracloud/koinon/internal/modelformat"
)

// Mode is the way a model thinks.
type Mode string

// The modes Koinon knows. The strings reach the flavors' entry scripts (--thinking), so a new one
// needs a case there.
const (
	// None is a model that does not think, or one whose thinking Koinon has not wired.
	None Mode = "none"
	// Adaptive is Anthropic's adaptive thinking on the Messages API: the model decides how much to
	// think. The harness sends thinking {"type": "adaptive"} and the gateway pins the beta that
	// goes with the body (Capability.Betas).
	Adaptive Mode = "adaptive"
)

// Known reports whether m is a mode Koinon knows.
func Known(m Mode) bool { return m == None || m == Adaptive }

// BindingBeta is the beta of thinking.block_binding, the body field OpenCode adds to adaptive
// thinking. Anthropic refuses the field without the beta: "thinking.adaptive.block_binding: Extra
// inputs are not permitted".
const BindingBeta = "thinking-binding-controls-2026-08-01"

// Capability is what the table says about one model.
type Capability struct {
	Mode Mode
	// Betas are the anthropic-beta values the gateway sets on every request to the model, whether
	// or not an agent has thinking on: a beta that no field uses changes nothing, and the pin
	// depends on the model alone. Empty when the model needs none; the gateway then drops the
	// client's anthropic-beta header (ADR 0069).
	Betas []string
}

// Supported reports whether the model can think.
func (c Capability) Supported() bool { return c.Mode != None }

// AnthropicBeta is the value of the anthropic-beta header the gateway sets, "" for none.
func (c Capability) AnthropicBeta() string { return strings.Join(c.Betas, ",") }

// Effective is the mode a run uses: the model's mode when it supports thinking and the setting is
// on, None otherwise.
func (c Capability) Effective(s Setting) Mode {
	if c.Supported() && s != Off {
		return c.Mode
	}
	return None
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

// row is one family of models that think. The first row that matches wins.
type row struct {
	// Provider limits the row to one provider type; empty matches every type.
	Provider string
	// Prefix is matched against the start of the normalized model name (see normalize).
	Prefix string
	Mode   Mode
	Betas  []string
}

// rows is the table, most specific rows first. Names that match no row do not think.
//
// The families are the ones the price list ships and the live checks covered. A Claude model that is
// not listed runs without thinking until a row is added: that is deliberate, because a beta the
// model does not know is an error and a field it does not take is a refusal (ADR 0069 amendment).
var rows = anthropicRows()

// claudeFamilies are the Claude families that take adaptive thinking, as normalized prefixes.
var claudeFamilies = []string{"claude-fable-5-", "claude-opus-5-", "claude-sonnet-5-", "claude-haiku-5-"}

func anthropicRows() []row {
	var out []row
	for _, f := range claudeFamilies {
		// The Anthropic API takes the beta. OpenRouter's Messages route took the same body without
		// it in the live check of 2026-10-29 (issue #242); no beta is pinned there, so nothing
		// unverified is sent.
		out = append(out,
			row{Provider: modelformat.ProviderAnthropic, Prefix: f, Mode: Adaptive, Betas: []string{BindingBeta}},
			row{Provider: modelformat.ProviderOpenRouter, Prefix: f, Mode: Adaptive},
		)
	}
	return out
}

// normalize lowercases a model name, drops OpenRouter's vendor prefix and writes the version with
// dashes, so claude-haiku-5-5 and anthropic/claude-haiku-5.5 are one name.
func normalize(model string) string {
	n := strings.ToLower(strings.TrimSpace(model))
	n = strings.TrimPrefix(n, "anthropic/")
	return strings.ReplaceAll(n, ".", "-")
}

// Of returns what the table says about a model of a provider type that speaks format. Adaptive
// thinking is a Messages feature, so a model on chat completions does not think through Koinon,
// whatever its name: the harness has no thinking block to send there.
func Of(providerType, model string, format modelformat.Format) Capability {
	if format != modelformat.Anthropic {
		return Capability{Mode: None}
	}
	name := normalize(model)
	for _, r := range rows {
		if (r.Provider == "" || r.Provider == providerType) && strings.HasPrefix(name, r.Prefix) {
			return Capability{Mode: r.Mode, Betas: slices.Clone(r.Betas)}
		}
	}
	return Capability{Mode: None}
}
