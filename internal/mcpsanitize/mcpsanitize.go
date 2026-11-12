// SPDX-License-Identifier: Apache-2.0

// Package mcpsanitize normalises the free text a third-party MCP server publishes about its tools
// (a tool's description and every string of its input schema) before anything stores it
// or shows it to a model. Those strings reach the model verbatim, so they are untrusted input
// (research TP22, issue #52): a server can hide instructions in them with control characters,
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
//  4. Prose (a tool's description, a description or title in its schema) is cut to a length cap,
//     counted in characters (runes), ending in an ellipsis. Other schema strings (names, enum
//     values, defaults, patterns) get steps 1 to 3 and keep their length.
//
// The caps follow the research: about 1,024 characters for a tool and 256 for a parameter. Descriptions
// that need more are rare, and a longer description costs every run's context.
package mcpsanitize

import (
	"bytes"
	"encoding/json"
	"regexp"
	"slices"
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
	// An opening "<" or "</" followed by a name that imitates a harness or chat-role block. The
	// bracket may be its fullwidth or small form, the space any Unicode space, and a ChatML-style
	// "|" or fullwidth "｜" may follow it (<|im_start|>).
	lookalike = regexp.MustCompile(`(?i)[<＜﹤]([\s\p{Z}]*/?[\s\p{Z}]*[|｜]?[\s\p{Z}]*)(system|system[-_ ]reminder|koinon[-_a-z0-9]*|untrusted[-_]context|tool[-_]result|tool[-_]use|tool[-_]call|function[-_]calls?|invoke|antml:[a-z_]+|assistant|human|user|developer|instructions?|mcp[-_]tools|im_start|im_end|im_sep|reminder|thinking|tool[-_]response|function[-_]results?|parameters?)\b`)
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
		cut := max(limit-1, 0)
		// Cut before a combining mark or the emoji selector, so the last kept character is whole.
		for cut > 0 && cut < len(runes) && (unicode.In(runes[cut], unicode.Mn, unicode.Me) || runes[cut] == '\ufe0f') {
			cut--
		}
		out = strings.TrimRightFunc(string(runes[:cut]), unicode.IsSpace) + ellipsis
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

// Schema sanitises every string of a JSON input schema, member names included, and returns the
// schema re-encoded. The model reads all of it: a server can hide text in an enum value, a default,
// a property name or a keyword nobody defined as well as in a description. Each string gets the
// whole policy; "description" and "title" also get the per-parameter cap, and other strings keep
// their length, since a cut enum value or pattern would change what the tool accepts. A clean
// string is unchanged, so an ordinary enum, default or name still matches what the tool expects.
// Numbers keep their exact text. A schema that is not valid JSON is returned unchanged with an empty
// report: the caller already replaces those.
func Schema(raw json.RawMessage) (json.RawMessage, Report) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return raw, Report{}
	}
	var rep Report
	v = walk(v, schemaMode, &rep)
	if !rep.Cleaned && !rep.Capped {
		return raw, rep
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // the stored schema stays readable: "&lt;system>", not "\u0026lt;system\u003e"
	if err := enc.Encode(v); err != nil {
		return raw, rep
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), rep
}

// capped are the keywords whose string is prose for the model, cut to MaxParamDescription. Under a
// map of names to schemas ("properties" and the like) the same names are parameters, not keywords,
// and a parameter named "description" is a schema like any other.
var capped = map[string]bool{"description": true, "title": true}

// namedSchemas map a name chosen by the server (a parameter, a definition) to a schema.
var namedSchemas = map[string]bool{
	"properties": true, "patternProperties": true, "$defs": true, "definitions": true, "dependentSchemas": true,
}

// instanceKeys hold instance values: data the tool receives or compares against. Their strings are
// cleaned like any other but never cut, and their keys are data, not keywords.
var instanceKeys = map[string]bool{"default": true, "const": true, "enum": true, "examples": true, "example": true}

// mode is how walk reads the keys of an object.
type mode uint8

const (
	schemaMode mode = iota // keys are keywords
	namesMode              // keys are names chosen by the server, each mapped to a schema
	dataMode               // an instance value: keys and strings are data
)

// walk visits a value of the schema. It has no depth limit of its own: encoding/json already
// refuses nesting deeper than it can decode, and a limit here would let a string hide below it.
func walk(v any, m mode, rep *Report) any {
	switch t := v.(type) {
	case string:
		return cleanString(t, rep)
	case map[string]any:
		out := make(map[string]any, len(t))
		for _, k := range cleanKeys(t, rep) {
			e := t[k.orig]
			switch s, isString := e.(string); {
			case m == dataMode:
				out[k.clean] = walk(e, dataMode, rep)
			case isString && (m == namesMode || capped[k.clean]):
				// prose: a description or title, or a string where a parameter's schema belongs
				clean, r := Text(s, MaxParamDescription)
				rep.merge(r)
				out[k.clean] = clean
			case m == schemaMode && instanceKeys[k.clean]:
				out[k.clean] = walk(e, dataMode, rep)
			case m == schemaMode && namedSchemas[k.clean]:
				out[k.clean] = walk(e, namesMode, rep)
			default:
				out[k.clean] = walk(e, schemaMode, rep)
			}
		}
		return out
	case []any:
		if m != dataMode {
			m = schemaMode // an array of schemas (allOf, prefixItems)
		}
		for i, e := range t {
			t[i] = walk(e, m, rep)
		}
	}
	return v
}

// maxStringLen is the cap Text gets for a string that is not cut: none.
const maxStringLen = int(^uint(0) >> 1)

type key struct{ orig, clean string }

// cleanKeys returns the keys of m with their cleaned form, in a fixed order. When two keys clean to
// the same name, the one that was already clean is kept (else the first in byte order), so the
// result does not depend on map order; dropping the other counts as cleaning.
func cleanKeys(m map[string]any, rep *Report) []key {
	keys := make([]key, 0, len(m))
	for k := range m {
		keys = append(keys, key{orig: k, clean: cleanString(k, rep)})
	}
	slices.SortFunc(keys, func(a, b key) int {
		if c := strings.Compare(a.clean, b.clean); c != 0 {
			return c
		}
		// the key that needed no cleaning first, then byte order
		if (a.orig == a.clean) != (b.orig == b.clean) {
			if a.orig == a.clean {
				return -1
			}
			return 1
		}
		return strings.Compare(a.orig, b.orig)
	})
	out := make([]key, 0, len(keys))
	for _, k := range keys {
		if len(out) > 0 && out[len(out)-1].clean == k.clean {
			rep.Cleaned = true
			continue
		}
		out = append(out, k)
	}
	return out
}

// cleanString applies the policy to a string that is not cut. The report's length stays that of
// the longest description: only Cleaned is taken.
func cleanString(s string, rep *Report) string {
	clean, r := Text(s, maxStringLen)
	rep.Cleaned = rep.Cleaned || r.Cleaned
	return clean
}
