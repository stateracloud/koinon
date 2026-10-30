// SPDX-License-Identifier: Apache-2.0

// Package mcpsanitize normalises the free text a third-party MCP server publishes about its tools
// (a tool's description and the description strings of its input schema) before anything stores it
// or shows it to a model. Those strings reach the model verbatim, so they are untrusted input
// (research TP22, issue #267): a server can hide instructions in them with control characters,
// ANSI escapes, Unicode tag characters, bidi overrides, zero-width characters or tags that imitate
// the harness's own blocks.
//
// The policy, applied in this order:
//  1. ANSI escape sequences are removed.
//  2. Control characters (except newline and tab), zero-width and invisible format characters,
//     bidi overrides and isolates, variation selectors beyond the emoji one, and Unicode tag
//     characters (U+E0000 to U+E007F) are removed. CRLF becomes LF.
//  3. Tags that imitate a harness or chat-role block (<system>, <system-reminder>, <koinon-*>,
//     <untrusted_context>, <tool_result> and the like, opening or closing) have their angle
//     bracket escaped, so the text stays readable to an admin but no longer parses as a block.
//  4. The text is cut to a length cap, counted in characters (runes), ending in an ellipsis.
//
// The caps follow the research: about 1,024 characters for a tool and 256 for a parameter. Descriptions
// that need more are rare, and a longer description costs every run's context.
package mcpsanitize

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// MaxToolDescription is the cap on a tool's description, in characters.
	MaxToolDescription = 1024
	// MaxParamDescription is the cap on a description or title inside an input schema.
	MaxParamDescription = 256
	// ellipsis ends a capped text.
	ellipsis = "…"
	// maxSchemaDepth bounds the walk of an input schema.
	maxSchemaDepth = 64
)

// Report says what sanitising did to one text, or to a set of them.
type Report struct {
	// OriginalLen is the length in characters of the text as published (for a schema, the longest
	// description in it).
	OriginalLen int
	// Capped says some text was cut to its cap.
	Capped bool
	// Cleaned says characters were removed or a lookalike tag was escaped.
	Cleaned bool
}

func (r *Report) merge(o Report) {
	r.OriginalLen = max(r.OriginalLen, o.OriginalLen)
	r.Capped = r.Capped || o.Capped
	r.Cleaned = r.Cleaned || o.Cleaned
}

var (
	// ESC [ ... final byte (CSI), ESC ] ... BEL or ESC \ (OSC), and other two-byte escapes.
	ansi = regexp.MustCompile("\x1b(?:\\[[0-?]*[ -/]*[@-~]|\\][^\x07\x1b]*(?:\x07|\x1b\\\\)?|[@-Z\\\\-_])")
	// An opening "<" or "</" followed by a name that imitates a harness or chat-role block.
	lookalike = regexp.MustCompile(`(?i)<(\s*/?\s*)(system|system[-_ ]reminder|koinon[-_a-z0-9]*|untrusted[-_]context|tool[-_]result|tool[-_]use|tool[-_]call|function[-_]calls?|invoke|antml:[a-z_]+|assistant|human|user|developer|instructions?|mcp[-_]tools|im_start|im_end|reminder)\b`)
)

// invisible reports runes that render as nothing or change text direction.
func invisible(r rune) bool {
	switch {
	case r == '\n' || r == '\t':
		return false
	case r == '\u2028' || r == '\u2029':
		return false // handled as newlines by the caller
	case unicode.Is(unicode.Cc, r):
		return true
	case r == '\u00ad', r == '͏', r == '؜', r == '᠎', r == '\ufeff':
		return true
	case r >= '\u200b' && r <= '\u200f': // zero-width space and joiners, LRM, RLM
		return true
	case r >= '\u202a' && r <= '\u202e': // bidi embeddings and overrides
		return true
	case r >= '\u2060' && r <= '\u206f': // word joiner, invisible operators, bidi isolates, deprecated formats
		return true
	case r >= '︀' && r <= '︎': // variation selectors (U+FE0F stays: emoji)
		return true
	case r >= '￹' && r <= '￻': // interlinear annotation
		return true
	case r >= 0xe0000 && r <= 0xe007f: // Unicode tag characters
		return true
	case r >= 0xe0100 && r <= 0xe01ef: // variation selectors supplement
		return true
	}
	return false
}

// Text applies the policy to one text with the given cap in characters.
func Text(s string, limit int) (string, Report) {
	rep := Report{OriginalLen: utf8.RuneCountInString(s)}
	out := ansi.ReplaceAllString(s, "")
	out = strings.ReplaceAll(out, "\r\n", "\n")
	out = strings.Map(func(r rune) rune {
		switch {
		case r == '\r', r == '\u2028', r == '\u2029':
			return '\n'
		case invisible(r):
			return -1
		case r == utf8.RuneError:
			return -1
		}
		return r
	}, out)
	out = lookalike.ReplaceAllString(out, "&lt;$1$2")
	rep.Cleaned = stripped(s, out)
	if utf8.RuneCountInString(out) > limit {
		rep.Capped = true
		runes := []rune(out)
		out = strings.TrimRightFunc(string(runes[:max(limit-1, 0)]), unicode.IsSpace) + ellipsis
	}
	return out, rep
}

// stripped reports whether cleaning changed the text beyond normalising line breaks.
func stripped(orig, cleaned string) bool {
	norm := strings.NewReplacer("\r\n", "\n", "\r", "\n", "\u2028", "\n", "\u2029", "\n").Replace(orig)
	return norm != cleaned
}

// Description sanitises a tool's description.
func Description(s string) (string, Report) { return Text(s, MaxToolDescription) }

// Schema sanitises the "description" and "title" strings anywhere in a JSON input schema, with the
// per-parameter cap, and returns the schema re-encoded. A schema that is not valid JSON is returned
// unchanged with an empty report: the caller already replaces those.
func Schema(raw json.RawMessage) (json.RawMessage, Report) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw, Report{}
	}
	var rep Report
	v = walk(v, 0, &rep)
	if !rep.Cleaned && !rep.Capped {
		return raw, rep
	}
	out, err := json.Marshal(v)
	if err != nil {
		return raw, rep
	}
	return out, rep
}

func walk(v any, depth int, rep *Report) any {
	if depth > maxSchemaDepth {
		return v
	}
	switch t := v.(type) {
	case map[string]any:
		for k, e := range t {
			if s, ok := e.(string); ok && (k == "description" || k == "title") {
				clean, r := Text(s, MaxParamDescription)
				t[k] = clean
				rep.merge(r)
				continue
			}
			t[k] = walk(e, depth+1, rep)
		}
	case []any:
		for i, e := range t {
			t[i] = walk(e, depth+1, rep)
		}
	}
	return v
}
