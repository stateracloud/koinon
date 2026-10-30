// SPDX-License-Identifier: Apache-2.0

package scrub

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// maxJSONDepth bounds nesting so a hostile document cannot exhaust the stack.
const maxJSONDepth = 200

// ErrInvalidJSON is returned for input that is not one valid JSON value, or is nested deeper than
// maxJSONDepth.
var ErrInvalidJSON = errors.New("scrub: invalid JSON")

// TooDeepPlaceholder is the JSON value emitted in place of a document nested deeper than the
// parser allows. Such a document is never passed through as text, where an escaped credential
// inside it would not be seen.
const TooDeepPlaceholder = `"[koinon: content too deeply nested]"`

var errTooDeep = fmt.Errorf("%w: nested too deeply", ErrInvalidJSON)

const (
	// maxStructuralBytes and maxStructuralFailures bound the search for JSON inside text.
	maxStructuralBytes    = 1 << 20
	maxStructuralFailures = 64
	// minGuard is the least number of bytes dropped from the end of a cut, scrubbed string.
	minGuard = 512
	// maxEmbeddedDepth is how many times a JSON document inside a JSON string is unpacked.
	maxEmbeddedDepth = 3
	// maxEmbeddedBytes is the largest string that is tried as an embedded JSON document; a longer
	// one is cut and scrubbed as text.
	maxEmbeddedBytes = 64 << 10
)

type nodeKind uint8

const (
	nullNode nodeKind = iota
	boolNode
	numNode
	strNode
	arrNode
	objNode
)

// node is a parsed JSON value. Objects keep their member order.
type node struct {
	kind nodeKind
	lit  string // null, true, false and numbers: the literal text
	str  string // decoded string
	keys []string
	kids []*node // array elements, or object member values
	size int     // encoded size, set by measure
	// dirty is set by measure when this subtree dropped children (extra > 0 somewhere).
	dirty bool
	// extra is how many trailing children were dropped before truncation (see bound).
	extra int
	// s and e are the byte range of a string or number token in the parsed text, set only by
	// parseAt.
	s, e int
}

func parseJSON(raw []byte) (*node, error) {
	n, _, err := parseAt(string(raw), 0, false)
	return n, err
}

// parseAt parses one JSON value from the start of src. With track, string and number nodes
// record their byte ranges in src, offset by base. It returns the number of bytes consumed;
// anything after the value is left alone when track is set and is an error otherwise.
func parseAt(src string, base int, track bool) (*node, int, error) {
	dec := json.NewDecoder(strings.NewReader(src))
	dec.UseNumber()
	p := &parser{dec: dec, src: src, base: base, track: track}
	n, err := p.value(0)
	if errors.Is(err, errTooDeep) {
		return nil, 0, errTooDeep
	}
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %w", ErrInvalidJSON, err)
	}
	used := int(dec.InputOffset())
	if !track {
		if _, err := dec.Token(); err != io.EOF {
			return nil, 0, fmt.Errorf("%w: trailing data", ErrInvalidJSON)
		}
	}
	return n, used, nil
}

type parser struct {
	dec   *json.Decoder
	src   string
	base  int
	track bool
}

// leaf records where the token just read sits: between off0 (end of the previous token, which
// may be followed by blanks, a comma or a colon) and the decoder's current offset.
func (p *parser) leaf(n *node, off0 int64) *node {
	if p.track {
		end := int(p.dec.InputOffset())
		start := int(off0)
		for start < end && strings.IndexByte(" \t\r\n,:", p.src[start]) >= 0 {
			start++
		}
		n.s, n.e = p.base+start, p.base+end
	}
	return n
}

func (p *parser) value(depth int) (*node, error) {
	if depth > maxJSONDepth {
		return nil, errTooDeep
	}
	off0 := p.dec.InputOffset()
	tok, err := p.dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		if t == '[' {
			n := &node{kind: arrNode}
			for p.dec.More() {
				k, err := p.value(depth + 1)
				if err != nil {
					return nil, err
				}
				n.kids = append(n.kids, k)
			}
			if _, err := p.dec.Token(); err != nil {
				return nil, err
			}
			return n, nil
		}
		n := &node{kind: objNode}
		for p.dec.More() {
			kt, err := p.dec.Token()
			if err != nil {
				return nil, err
			}
			key, ok := kt.(string)
			if !ok {
				return nil, errors.New("object key is not a string")
			}
			v, err := p.value(depth + 1)
			if err != nil {
				return nil, err
			}
			n.keys = append(n.keys, key)
			n.kids = append(n.kids, v)
		}
		if _, err := p.dec.Token(); err != nil {
			return nil, err
		}
		return n, nil
	case string:
		return p.leaf(&node{kind: strNode, str: t}, off0), nil
	case json.Number:
		return p.leaf(&node{kind: numNode, lit: t.String()}, off0), nil
	case bool:
		if t {
			return &node{kind: boolNode, lit: "true"}, nil
		}
		return &node{kind: boolNode, lit: "false"}, nil
	case nil:
		return &node{kind: nullNode, lit: "null"}, nil
	}
	return nil, errors.New("unexpected token")
}

// appendString appends s as a JSON string. Unlike encoding/json it does not escape <, > and &, so
// the output stays readable; like it, it escapes U+2028 and U+2029 and replaces invalid UTF-8.
func appendString(b []byte, s string) []byte {
	const hexdigits = "0123456789abcdef"
	b = append(b, '"')
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch {
			case c == '"' || c == '\\':
				b = append(b, '\\', c)
			case c == '\n':
				b = append(b, '\\', 'n')
			case c == '\r':
				b = append(b, '\\', 'r')
			case c == '\t':
				b = append(b, '\\', 't')
			case c < 0x20 || c == 0x7f:
				b = append(b, '\\', 'u', '0', '0', hexdigits[c>>4], hexdigits[c&0xf])
			default:
				b = append(b, c)
			}
			i++
			continue
		}
		r, w := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && w == 1:
			b = append(b, `\ufffd`...)
		case r == 0x2028 || r == 0x2029:
			b = append(b, `\u202`...)
			b = append(b, hexdigits[r&0xf])
		default:
			b = append(b, s[i:i+w]...)
		}
		i += w
	}
	return append(b, '"')
}

// encodedWidth is the number of bytes r adds inside a JSON string.
func encodedWidth(r rune) int {
	switch {
	case r == '"' || r == '\\' || r == '\n' || r == '\r' || r == '\t':
		return 2
	case r < 0x20 || r == 0x7f || r == 0x2028 || r == 0x2029 || r == utf8.RuneError:
		return 6
	}
	return utf8.RuneLen(r)
}

func (n *node) appendTo(b []byte) []byte {
	switch n.kind {
	case nullNode, boolNode, numNode:
		return append(b, n.lit...)
	case strNode:
		return appendString(b, n.str)
	case arrNode:
		b = append(b, '[')
		for i, k := range n.kids {
			if i > 0 {
				b = append(b, ',')
			}
			b = k.appendTo(b)
		}
		return append(b, ']')
	}
	b = append(b, '{')
	for i, k := range n.kids {
		if i > 0 {
			b = append(b, ',')
		}
		b = appendString(b, n.keys[i])
		b = append(b, ':')
		b = k.appendTo(b)
	}
	return append(b, '}')
}

// JSON scrubs every string leaf of a JSON value (and every object key) and keeps the structure
// and the member order. A number that the scrubber would redact as text (a card number written as
// a JSON number) becomes a string. The result is compact JSON.
//
// Inside an object, a string under a key named Authorization, Proxy-Authorization, Cookie,
// Set-Cookie, X-Api-Key or X-Auth-Token is replaced whole, and so is the "value" of an object
// that names such a header in its "name" or "key" ({"name":"Authorization","value":"..."}), the
// way HTTP headers appear as lists.
//
// It returns ErrInvalidJSON for input that is not exactly one JSON value; the caller then decides
// whether to treat the bytes as text.
func (s *Scrubber) JSON(raw []byte) ([]byte, error) {
	n, err := parseJSON(raw)
	if errors.Is(err, errTooDeep) {
		return []byte(TooDeepPlaceholder), nil
	}
	if err != nil {
		return nil, err
	}
	s.scrubNode(n, "", 0)
	return n.appendTo(make([]byte, 0, len(raw))), nil
}

// childForces returns, for each child of n, the kind its scalars are replaced with (""
// for none), given the kind forced on n itself. It is the single place that knows which keys and
// array shapes hold credentials, shared by scrubbing and by the span finder for text.
func childForces(n *node, force Kind) []Kind {
	out := make([]Kind, len(n.kids))
	switch n.kind {
	case arrNode:
		for i := range out {
			out[i] = force
		}
		if force == "" && len(n.kids) == 2 && n.kids[0].kind == strNode && n.kids[1].kind == strNode {
			// a ["Authorization", "Basic abc"] header tuple: only when the value looks like a
			// credential, so ["Authorization", "is a header"] stays readable
			if kind := headerKind(n.kids[0].str); kind != "" && credLooking(n.kids[1].str) &&
				(kind != KindCookie || strings.Contains(n.kids[1].str, "=")) {
				out[1] = kind
			}
		}
	case objNode:
		pair := headerPair(n)
		for i := range out {
			kind := force
			if kind == "" {
				if kind = headerKind(n.keys[i]); kind == "" {
					kind = credentialKind(n.keys[i])
				}
			}
			if kind == "" && pair != "" {
				if lk := strings.ToLower(strings.TrimSpace(n.keys[i])); lk == "value" || lk == "values" {
					kind = pair
				}
			}
			out[i] = kind
		}
	}
	return out
}

// replaces reports whether a scalar under a key of kind force is replaced: header keys replace any
// scalar; a credential-named key replaces strings only (a count named max_token is not a secret).
func replaces(n *node, force Kind) bool {
	if force == "" {
		return false
	}
	if n.kind == strNode {
		return n.str != ""
	}
	return force != KindCredential && (n.kind == numNode || n.kind == boolNode)
}

// scrubNode scrubs n in place and reports whether it changed anything. force is the kind to
// replace the scalars under n with when n sits below a header or credential key; embedded
// counts how many JSON strings deep n is. Strings longer than the scrubber's leaf cap are cut
// before scrubbing (see capLeaf), so the work per string is bounded.
func (s *Scrubber) scrubNode(n *node, force Kind, embedded int) bool {
	switch n.kind {
	case strNode:
		if replaces(n, force) {
			n.str = s.replaceAll(force)
			return true
		}
		if embedded < maxEmbeddedDepth && len(n.str) <= maxEmbeddedBytes {
			if out, ok := s.embeddedJSON(n.str, embedded); ok {
				n.str = out
				return true
			}
		}
		out := s.capLeaf(n.str)
		changed := out != n.str
		n.str = out
		return changed
	case numNode, boolNode:
		switch {
		case replaces(n, force):
			n.kind, n.str = strNode, s.replaceAll(force)
			return true
		case s.secrets.Len() > 0 || s.level == LevelRedacted:
			// a numeric secret (a PIN), or a card number written as a JSON number
			if out := s.Text(n.lit); out != n.lit {
				n.kind, n.str = strNode, out
				return true
			}
		}
		return false
	case arrNode, objNode:
		forces := childForces(n, force)
		changed := false
		for i, k := range n.kids {
			if s.scrubNode(k, forces[i], embedded) {
				changed = true
			}
			if n.kind == objNode {
				if key := s.Text(n.keys[i]); key != n.keys[i] {
					n.keys[i], changed = key, true
				}
			}
		}
		return changed
	}
	return false
}

// embeddedJSON scrubs a string that holds a JSON object or array (tool output often does) as JSON.
// It returns the new string value and true only when scrubbing changed something, so a document
// with nothing to hide keeps its exact bytes (layout, escapes and all). A document nested too
// deeply to parse becomes the fixed placeholder, never raw text.
func (s *Scrubber) embeddedJSON(str string, embedded int) (string, bool) {
	t := strings.TrimSpace(str)
	if t == "" || (t[0] != '{' && t[0] != '[') {
		return "", false
	}
	sub, err := parseJSON([]byte(t))
	if errors.Is(err, errTooDeep) {
		return "[koinon: content too deeply nested]", true
	}
	if err != nil {
		return "", false
	}
	if !s.scrubNode(sub, "", embedded+1) {
		// nothing in the leaves; the raw text may still hold something the leaves split
		if out := s.Text(str); out != str {
			return out, true
		}
		return "", false
	}
	return string(sub.appendTo(nil)), true
}

// capLeaf scrubs a string. One longer than the cap is cut to the cap first, scrubbed, and then
// the last guard bytes of the result are dropped, because a credential cut in half by the cap
// would survive as a fragment at the end. A cut string ends with TruncatedSuffix.
func (s *Scrubber) capLeaf(str string) string {
	if s.leafCap <= 0 || len(str) <= s.leafCap {
		return s.Text(str)
	}
	out := s.Text(cutRunes(str, s.leafCap))
	return cutRunes(out, max(0, len(out)-s.guard())) + TruncatedSuffix
}

// guard is how many trailing bytes of a cut, scrubbed prefix are dropped.
func (s *Scrubber) guard() int {
	return max(minGuard, s.secrets.maxLen()+16)
}

// cutRunes returns the longest prefix of s of at most n bytes that ends on a character boundary.
func cutRunes(s string, n int) string {
	if n >= len(s) {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// headerPair returns the header kind when the object names a header in "name", "key" or "header"
// (any case).
func headerPair(n *node) Kind {
	for i, k := range n.keys {
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "name", "key", "header":
			if n.kids[i].kind == strNode {
				if kind := headerKind(n.kids[i].str); kind != "" {
					return kind
				}
			}
		}
	}
	return ""
}

// findForcedLeaves finds JSON objects and arrays written in text (a log line, NDJSON, a tool
// result) and appends a span over every scalar that sits below a header or credential key, in
// whatever order the members are written. A regular expression cannot see "value" before "name"
// or a "in" member between them; the parsed structure can.
func findForcedLeaves(in string, spans []span) []span {
	if len(in) > maxStructuralBytes {
		return spans
	}
	failures := 0
	for i := 0; i < len(in) && failures < maxStructuralFailures; i++ {
		if in[i] != '{' && in[i] != '[' {
			continue
		}
		n, used, err := parseAt(in[i:], i, true)
		if err != nil {
			failures++
			continue
		}
		collectForced(n, "", func(leaf *node, k Kind) {
			a, b := leaf.s, leaf.e
			if leaf.kind == strNode && b-a >= 2 { // the string's content, inside its quotes
				a, b = a+1, b-1
			}
			if b > a {
				spans = append(spans, span{a, b, k})
			}
		})
		i += used - 1
	}
	return spans
}

func collectForced(n *node, force Kind, emit func(*node, Kind)) {
	switch n.kind {
	case strNode, numNode, boolNode:
		if replaces(n, force) && n.e > n.s {
			emit(n, force)
		}
	case arrNode, objNode:
		forces := childForces(n, force)
		for i, k := range n.kids {
			collectForced(k, forces[i], emit)
		}
	}
}

func (s *Scrubber) replaceAll(k Kind) string {
	if s.observe != nil {
		s.observe(k)
	}
	return Replacement(k)
}
