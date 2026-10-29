// SPDX-License-Identifier: Apache-2.0

// Package errbody redacts and caps the text of a failed model request so that it is safe to show
// and to store (ADR 0056, amendment 2026-10-29).
//
// The text comes from the harness: agentd already receives the error a harness reports for a failed
// turn, and shows it to the person with a label that says so. The gateway does not supply an error
// body. agentgateway v1.7.0 records the body of EVERY response on a listener, up to 32 MiB on an LLM
// route and held for the whole stream, as soon as one access-log or tracer expression mentions
// response.body or response.bodyPrefix (see the ADR 0056 amendment); Koinon's gateway therefore
// reads no response body at all.
//
// A harness's text can echo secrets (a rejected key, a bearer token) and fragments of the prompt.
// Redaction removes what looks like a credential and the cap bounds the rest; it does not remove
// every word that came from the prompt.
package errbody

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// ExcerptMax is the most characters of a harness error that is shown.
const ExcerptMax = 512

// Redacted replaces everything a pattern matches.
const Redacted = "[redacted]"

// Pattern is one redaction: what looks like a credential, in RE2 syntax.
type Pattern struct {
	Name, Regex string
}

// Patterns are applied in order. They are what looks like a credential: a bearer token, a Koinon
// gateway key, a provider key (sk-..., including OpenRouter's sk-or-v1-...), a credential-like
// JSON field with its value, and any long unbroken token.
var Patterns = []Pattern{
	{"bearer", `(?i)bearer\s+[A-Za-z0-9._~+/=-]{8,}`},
	{"koinon-key", `kgw[_][A-Za-z0-9_-]{8,}`},
	{"provider-key", `\b(?:sk|pk|rk|gsk|xai)-[A-Za-z0-9_-]{8,}`},
	{"credential-field", `(?i)"?(?:api[_-]?key|authorization|access[_-]?token|secret|password)"?\s*[:=]\s*"?[^",\s}]+"?`},
	{"long-token", `\b[A-Za-z0-9_-]{40,}\b`},
}

var compiled = func() []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(Patterns))
	for i, p := range Patterns {
		out[i] = regexp.MustCompile(p.Regex)
	}
	return out
}()

// Redact replaces everything in s that matches a pattern.
func Redact(s string) string {
	for _, re := range compiled {
		s = re.ReplaceAllString(s, Redacted)
	}
	return s
}

var space = regexp.MustCompile(`\s+`)

// Excerpt returns the one-line form of an error text: whitespace collapsed, credentials redacted,
// at most ExcerptMax characters.
func Excerpt(s string) string {
	return capRunes(Redact(space.ReplaceAllString(strings.TrimSpace(s), " ")), ExcerptMax)
}

// capRunes cuts s to at most n characters (not bytes) and marks the cut.
func capRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}
