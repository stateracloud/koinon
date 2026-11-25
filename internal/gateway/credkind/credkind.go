// SPDX-License-Identifier: Apache-2.0

// Package credkind is the one list of the credential kinds that koinon-gateway sends upstream, and of how the
// gateway's credential scrub covers each kind (#1221, ADR 0097 decision 12).
//
// Every code path that opens a credential names its kind with a [Kind] constant of this package and gives the
// value to the scrub with that kind. The scrub reads the rule of the kind from [Rules]. A kind that is not in the
// list is scrubbed by value, as [ByValue]: a new kind is covered by default. A spec of this package parses the
// source tree and fails when a [Kind] constant has no rule, so a new kind is a decision, not an accident.
//
// The list holds outbound credentials: values the gateway attaches to a request it sends. Inbound credentials (a
// gateway key, an actor token) are not in it. The caller already holds them, and the trace scrubber matches
// their shapes (internal/tracing/scrub).
package credkind

// Kind names a credential kind. Its value is a fixed name: the audit row and the span attribute carry it, never a
// token. Declare each kind as a constant in this package and give it a rule in [Rules].
type Kind string

// The credential kinds. A kind that ships later is listed now with its rule, so that its opener only names it.
const (
	// None is a call that sends no credential.
	None Kind = "none"
	// Error is a call whose credential could not be read. Nothing is sent.
	Error Kind = "error"
	// MCPStatic is the static header value of an org's MCP server (ADR 0097). An org owns it.
	MCPStatic Kind = "static"
	// MCPSignIn is the OAuth access token of an MCP sign-in (ADR 0096). An org owns it.
	MCPSignIn Kind = "signin"
	// Passport is the agent passport, a signed JWT for one run, audience and scope (ADR 0073). An agent owns it.
	Passport Kind = "passport"
	// EgressBearer is a bearer credential of an egress route (ADR 0120). An agent owns it.
	EgressBearer Kind = "egress_bearer"
	// EgressBasic is a Basic credential of an egress route (ADR 0120). An agent owns it.
	EgressBasic Kind = "egress_basic"
	// ProviderKey is the API key of a model provider on the model path (ADR 0097). An org owns it.
	ProviderKey Kind = "provider_key"
)

// Scrub is how the credential scrub covers a kind.
type Scrub uint8

// The scrub classes.
const (
	// ByValue records each opened value in the value set of its org: the scrub removes the exact value, the token
	// after an auth scheme word and their encoded forms. It is the default for a kind that is not in the list.
	ByValue Scrub = iota + 1
	// ByShape does not record the value. The scrub removes every text that has the fixed shape of the kind, in
	// every org. It is for a kind with many short-lived values that would push other values out of the capped
	// value sets. A value that does not have the shape is recorded by value.
	ByShape
	// NoValue is a kind that carries no value. A value given with such a kind is recorded by value all the same.
	NoValue
)

// String returns the name of the class.
func (s Scrub) String() string {
	switch s {
	case ByValue:
		return "by-value"
	case ByShape:
		return "by-shape"
	case NoValue:
		return "no-value"
	}
	return "unknown"
}

// Rule is the scrub rule of one kind.
type Rule struct {
	Kind  Kind
	Scrub Scrub
	// Why says why the kind has this class. A spec requires it for each class other than ByValue.
	Why string
}

var rules = []Rule{
	{Kind: None, Scrub: NoValue, Why: "the call sends no credential"},
	{Kind: Error, Scrub: NoValue, Why: "the credential could not be read, so nothing is sent"},
	{Kind: MCPStatic, Scrub: ByValue},
	{Kind: MCPSignIn, Scrub: ByValue},
	{Kind: Passport, Scrub: ByShape, Why: "one value per run, audience and scope with a life of minutes would " +
		"push org values out of the capped value set; the fixed JWT typ " + PassportType + " is matched instead"},
	{Kind: EgressBearer, Scrub: ByValue},
	{Kind: EgressBasic, Scrub: ByValue},
	{Kind: ProviderKey, Scrub: ByValue},
}

// Rules returns a copy of the list.
func Rules() []Rule { return append([]Rule(nil), rules...) }

// Lookup returns the rule of k. ok is false when k is not in the list; the rule is then [ByValue], so an unlisted
// kind is scrubbed by value.
func Lookup(k Kind) (r Rule, ok bool) {
	for _, r := range rules {
		if r.Kind == k {
			return r, true
		}
	}
	return Rule{Kind: k, Scrub: ByValue}, false
}
