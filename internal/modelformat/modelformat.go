// SPDX-License-Identifier: Apache-2.0

// Package modelformat is the one table that knows which wire format a model speaks (ADR 0069).
//
// The format belongs to the model, not the provider: OpenRouter serves Claude and other families
// through one provider, and each wants its own API. Three things live here and nowhere else:
//
//   - the formats each provider type offers (Offered),
//   - the family preference, matched on the model name (preferences), and
//   - the default rule that combines them (Default).
//
// A new format is one row in formats, plus rows in offers and preferences where it applies.
// Callers never switch on provider types or model names to decide a format; they call this package.
package modelformat

import (
	"fmt"
	"slices"
	"strings"
)

// Format is a model's wire format. The value set is open: each new one needs a row here, a gateway
// renderer and a harness entry (ADR 0069 decision 2).
type Format string

// The formats Koinon knows. The strings are stored in models.format; the migration's CHECK lists
// the same values.
const (
	// OpenAI is chat completions.
	OpenAI Format = "openai"
	// Anthropic is the Messages API.
	Anthropic Format = "anthropic"
)

// row describes one known format.
type row struct {
	ID Format
	// Label is how the format reads to a person.
	Label string
}

// formats is every known format, in display order.
var formats = []row{
	{OpenAI, "OpenAI chat completions"},
	{Anthropic, "Anthropic Messages"},
}

// Provider types.
const (
	ProviderAnthropic  = "anthropic"
	ProviderOpenRouter = "openrouter"
)

// defaultOffer is what a provider type not listed in offers offers.
var defaultOffer = []Format{OpenAI}

// offers lists the formats a provider type serves, preferred first: the first is the fallback
// default when a model's family preference is not offered.
var offers = map[string][]Format{
	ProviderAnthropic:  {Anthropic},
	ProviderOpenRouter: {OpenAI, Anthropic},
}

// preference is one row of the family preference. The first row that matches wins.
type preference struct {
	// Provider limits the row to one provider type; empty matches every type.
	Provider string
	// Prefix is matched against the start of the model name, case-insensitively.
	Prefix string
	Format Format
}

// preferences is the family preference, most specific rows first. Names that match no row prefer
// OpenAI (fallbackPreference).
var preferences = []preference{
	{Prefix: "claude-", Format: Anthropic},
	{Prefix: "anthropic/", Format: Anthropic},
}

const fallbackPreference = OpenAI

// Known reports whether f is a format Koinon knows.
func Known(f Format) bool {
	return slices.ContainsFunc(formats, func(x row) bool { return x.ID == f })
}

// All returns every known format, in display order.
func All() []Format {
	out := make([]Format, len(formats))
	for i, f := range formats {
		out[i] = f.ID
	}
	return out
}

// Label is how a format reads to a person; an unknown format reads as itself.
func Label(f Format) string {
	for _, x := range formats {
		if x.ID == f {
			return x.Label
		}
	}
	return string(f)
}

// Offered returns the formats a provider of this type serves, preferred first. The slice is a copy.
func Offered(providerType string) []Format {
	if o, ok := offers[providerType]; ok {
		return slices.Clone(o)
	}
	return slices.Clone(defaultOffer)
}

// Offers reports whether a provider of this type serves the format.
func Offers(providerType string, f Format) bool {
	return slices.Contains(Offered(providerType), f)
}

// Preferred returns the format a model's family prefers on a provider type, whether or not the
// provider offers it.
func Preferred(providerType, model string) Format {
	name := strings.ToLower(model)
	for _, p := range preferences {
		if (p.Provider == "" || p.Provider == providerType) && strings.HasPrefix(name, p.Prefix) {
			return p.Format
		}
	}
	return fallbackPreference
}

// Default returns the format a model gets unless an admin overrides it: the family's preference when
// the provider offers it, otherwise the provider's first offered format.
func Default(providerType, model string) Format {
	if pref := Preferred(providerType, model); Offers(providerType, pref) {
		return pref
	}
	return Offered(providerType)[0]
}

// Validate refuses a format that is unknown or that the provider type does not offer. The message is
// safe to show.
func Validate(providerType string, f Format) error {
	if !Known(f) {
		return fmt.Errorf("unknown model format %q; known formats are %s", f, join(All()))
	}
	if !Offers(providerType, f) {
		return fmt.Errorf("a %s provider does not offer the %s format; it offers %s", providerType, f, join(Offered(providerType)))
	}
	return nil
}

// Resolve returns the format a model is stored with and whether that is an override. override is
// the admin's choice, "" for the default. An override is validated against the provider type.
func Resolve(providerType, model, override string) (Format, bool, error) {
	if override == "" {
		return Default(providerType, model), false, nil
	}
	f := Format(override)
	if err := Validate(providerType, f); err != nil {
		return "", false, err
	}
	return f, true, nil
}

func join(fs []Format) string {
	s := make([]string, len(fs))
	for i, f := range fs {
		s[i] = string(f)
	}
	return strings.Join(s, ", ")
}
