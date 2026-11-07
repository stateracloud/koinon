// SPDX-License-Identifier: Apache-2.0

// Package providertype holds what Koinon knows about a kind of model provider (the provider's
// type: "anthropic", "openrouter", "openai", ...) beyond its wire format (package modelformat).
//
// A trait is an explicit, cited claim about the provider, set per type in the table below. A type
// without a row (a custom or unknown provider) takes the conservative default of every trait: the
// answer that never under-counts spend (ADR 0056, amendment 2026-10-29). A new trait needs a row
// for each type it is known for and a conservative default here.
package providertype

// Traits are the facts about one provider type.
type Traits struct {
	// BillsRejectedRequests is whether the provider may bill a request it refused with an HTTP 4xx
	// that carries no usage (an invalid request, a content flag). False only where the provider
	// documents that it does not; the ledger then charges such a request nothing.
	BillsRejectedRequests bool
}

// Types of providers Koinon has a row for. They are the values of providers.type.
const (
	Anthropic  = "anthropic"
	OpenRouter = "openrouter"
	OpenAI     = "openai"
)

// conservative is what a type without a row gets.
var conservative = Traits{BillsRejectedRequests: true}

// traits has one row per provider type that documents its behaviour. Each row names the page that
// describes the provider's errors and the issue that recorded the billing claim; re-read the page
// when changing a row.
var traits = map[string]Traits{
	// https://docs.anthropic.com/en/api/errors; billing claim: old issue 241.
	Anthropic: {BillsRejectedRequests: false},
	// https://openrouter.ai/docs/api-reference/errors (a moderation or guardrail 403 is returned
	// before a provider is reached); billing claim: old issue 241.
	OpenRouter: {BillsRejectedRequests: false},
	// https://platform.openai.com/docs/guides/error-codes; billing claim: old issue 241.
	OpenAI: {BillsRejectedRequests: false},
}

// For returns the traits of a provider type. An unknown type gets the conservative traits.
func For(providerType string) Traits {
	if t, ok := traits[providerType]; ok {
		return t
	}
	return conservative
}

// Known reports whether the type has a row.
func Known(providerType string) bool {
	_, ok := traits[providerType]
	return ok
}
