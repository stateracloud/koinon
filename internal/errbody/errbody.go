// SPDX-License-Identifier: Apache-2.0

// Package errbody turns the body of a failed model response into a short, redacted line that is
// safe to log and to store (ADR 0056, amendment 2026-10-29).
//
// A provider's error body can echo secrets (a rejected key, a bearer token) and fragments of the
// prompt (OpenRouter's moderation errors quote the flagged input). Koinon keeps neither beyond a
// short, capped excerpt, and applies the same redaction in two places:
//
//   - at the gateway, in the CEL expression of the access-log field koinon.error_body
//     ([GatewayExpr]), which keeps only the known error fields of a JSON body (never a field
//     that quotes the prompt), so the gateway's own log and the OTLP export carry no prompt text
//     and secrets are replaced;
//   - in the spend ledger, which runs [Summarize] on the exported field before it stores anything.
//     It reads the provider's error message and reasons from the known JSON shapes, drops
//     everything else (metadata such as flagged_input or raw), redacts again and caps the result
//     at [ExcerptMax] characters.
//
// Redaction is a best effort on top of caps: it removes what looks like a credential, not every
// word that came from the prompt. The cap bounds the rest.
package errbody

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	// GatewayCap is the most characters of a response body the gateway puts in its access log.
	GatewayCap = 2048
	// ExcerptMax is the most characters of an error the ledger stores.
	ExcerptMax = 512
	// MessageMax caps the provider's message inside the excerpt.
	messageMax = 400
)

// Redacted replaces everything a pattern matches. The patterns have no capture groups and the
// replacement no references: agentgateway's config loader expands "$" in a config file as an
// environment variable, so no expression Koinon renders may contain one.
const Redacted = "[redacted]"

// Pattern is one redaction: what looks like a credential, in RE2 syntax (valid in Go and in the
// gateway's Rust regex engine).
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

// GatewayExpr is the CEL expression of the access-log field koinon.error_body: for a response with
// an error status, a sanitized body with the patterns applied; for any other response it fails,
// which leaves the field out of the record. It reads the body prefix the gateway records for
// logging only, as the stream passes; nothing is buffered before the response is forwarded
// (agentgateway schema/cel-functions.md, "Body Variables").
//
// The expression never copies the body. For a JSON body it builds a new object from the known
// error fields only, in the shape [Summarize] reads ({"error": {type, code, message,
// metadata: {reasons, provider_name}}}), so a field that carries the prompt (OpenRouter's
// metadata.flagged_input, raw) never reaches the gateway's log or the OTLP export (ADR 0074
// decision 6). The first alternative that resolves wins (coalesce):
//
//  1. {"error": {"message": ...}} (OpenAI, OpenRouter, Anthropic), with the optional fields;
//  2. {"error": "text"};
//  3. {"message": "text"};
//  4. any other JSON, or a JSON body that does not parse as a whole (cut at the prefix limit):
//     a fixed placeholder, so an unknown shape can not carry a prompt either;
//  5. a body that is not JSON: its first [GatewayCap] characters.
func GatewayExpr() string {
	capTo := func(expr string, n int) string {
		return `(` + expr + `).with(m, size(m) > ` + fmt.Sprint(n) + ` ? m.substring(0, ` + fmt.Sprint(n) + `) : m)`
	}
	const body = `response.bodyPrefix`
	// A missing or non-string field is an error here, which moves the coalesce to the next shape.
	msg := func(expr string) string { return capTo(expr+` + ""`, messageMax) }
	structured := `json(` + body + `).error.with(e, toJson({"error": {` +
		`"type": coalesce(e.type + "", ""), ` +
		`"code": coalesce(e.code, ""), ` +
		`"message": ` + capTo(`e.message + ""`, messageMax) + `, ` +
		`"metadata": {"reasons": coalesce(e.metadata.reasons, []), "provider_name": coalesce(e.metadata.provider_name + "", "")}}}))`
	errorText := `toJson({"error": {"message": ` + msg(`json(`+body+`).error`) + `}})`
	topMessage := `toJson({"error": {"message": ` + msg(`json(`+body+`).message`) + `}})`
	otherJSON := `json(` + body + `).with(j, "[error body of an unrecognised shape]")`
	text := `string(` + body + `).with(b, b.trim().startsWith("{") || b.trim().startsWith("[") ? "[error body that is not complete JSON]" : ` + capTo(`b`, GatewayCap) + `)`
	e := `coalesce(` + strings.Join([]string{structured, errorText, topMessage, otherJSON, text}, ", ") + `)`
	for _, p := range Patterns {
		e += `.regexReplaceAll(` + celString(p.Regex) + `, ` + celString(Redacted) + `)`
	}
	return `response.code >= 400 ? ` + e + ` : fail("not an error response")`
}

func celString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}

var space = regexp.MustCompile(`\s+`)

// Summarize returns the one-line excerpt of an error body that the ledger stores: at most
// ExcerptMax characters, redacted, without prompt fragments from known fields. An empty body gives
// an empty excerpt.
func Summarize(body string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}
	if line, ok := fromJSON(body); ok {
		return capRunes(Redact(line), ExcerptMax)
	}
	return capRunes(Redact(space.ReplaceAllString(body, " ")), ExcerptMax)
}

// envelope covers the error shapes of the providers Koinon talks to:
//
//	OpenAI, OpenRouter  {"error": {"message", "type", "code", "metadata": {"reasons", "provider_name"}}}
//	Anthropic           {"type": "error", "error": {"type", "message"}}
//	plain               {"error": "text"} or {"message": "text"}
type envelope struct {
	Error   json.RawMessage `json:"error"`
	Message string          `json:"message"`
	Type    string          `json:"type"`
}

type detail struct {
	Message  string          `json:"message"`
	Type     string          `json:"type"`
	Code     json.RawMessage `json:"code"`
	Metadata struct {
		Reasons  []string `json:"reasons"`
		Provider string   `json:"provider_name"`
	} `json:"metadata"`
}

// fromJSON builds "type/code: message (reasons: ...; provider: ...)" from the known fields.
func fromJSON(body string) (string, bool) {
	var env envelope
	if json.Unmarshal([]byte(body), &env) != nil {
		return "", false
	}
	var d detail
	var text string
	if len(env.Error) > 0 {
		if json.Unmarshal(env.Error, &d) != nil {
			if json.Unmarshal(env.Error, &text) != nil {
				return "", false
			}
			d.Message = text
		}
	} else {
		d.Message = env.Message
	}
	if d.Message == "" {
		return "", false
	}
	var sb strings.Builder
	label := d.Type
	if code := strings.Trim(string(d.Code), `"`); code != "" && code != "null" {
		if label != "" {
			label += "/"
		}
		label += code
	}
	if label != "" {
		sb.WriteString(label + ": ")
	}
	sb.WriteString(capRunes(space.ReplaceAllString(d.Message, " "), messageMax))
	var extra []string
	if len(d.Metadata.Reasons) > 0 {
		extra = append(extra, "reasons: "+strings.Join(d.Metadata.Reasons, ", "))
	}
	if d.Metadata.Provider != "" {
		extra = append(extra, "provider: "+d.Metadata.Provider)
	}
	if len(extra) > 0 {
		sb.WriteString(" (" + strings.Join(extra, "; ") + ")")
	}
	return sb.String(), true
}

// capRunes cuts s to at most n characters (not bytes) and marks the cut.
func capRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}
