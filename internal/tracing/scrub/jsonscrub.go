// SPDX-License-Identifier: Apache-2.0

package scrub

import (
	"bytes"
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
	// maxEmbeddedDepth is how many times a JSON document inside a JSON string is unpacked.
	maxEmbeddedDepth = 3
	// maxEmbeddedBytes is the largest string that is tried as an embedded JSON document.
	maxEmbeddedBytes = 1 << 20
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
}

func parseJSON(raw []byte) (*node, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	n, err := parseValue(dec, 0)
	if errors.Is(err, errTooDeep) {
		return nil, errTooDeep
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidJSON, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("%w: trailing data", ErrInvalidJSON)
	}
	return n, nil
}

func parseValue(dec *json.Decoder, depth int) (*node, error) {
	if depth > maxJSONDepth {
		return nil, errTooDeep
	}
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		if t == '[' {
			n := &node{kind: arrNode}
			for dec.More() {
				k, err := parseValue(dec, depth+1)
				if err != nil {
					return nil, err
				}
				n.kids = append(n.kids, k)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return n, nil
		}
		n := &node{kind: objNode}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			key, ok := kt.(string)
			if !ok {
				return nil, errors.New("object key is not a string")
			}
			v, err := parseValue(dec, depth+1)
			if err != nil {
				return nil, err
			}
			n.keys = append(n.keys, key)
			n.kids = append(n.kids, v)
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		return n, nil
	case string:
		return &node{kind: strNode, str: t}, nil
	case json.Number:
		return &node{kind: numNode, lit: t.String()}, nil
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

// scrubNode scrubs n in place. force is the kind to replace every scalar under n with, when the
// scalar sits below a header key; embedded counts how many JSON strings deep n is.
func (s *Scrubber) scrubNode(n *node, force Kind, embedded int) {
	switch n.kind {
	case strNode:
		if force != "" {
			if n.str != "" {
				n.str = s.replaceAll(force)
			}
			return
		}
		if embedded < maxEmbeddedDepth && len(n.str) <= maxEmbeddedBytes {
			if sub := s.embeddedJSON(n.str, embedded); sub != nil {
				n.str = string(sub)
				return
			}
		}
		n.str = s.Text(n.str)
	case numNode, boolNode:
		switch {
		case force != "":
			n.kind, n.str = strNode, s.replaceAll(force)
		case s.secrets.Len() > 0 || s.level == LevelRedacted:
			// a numeric secret (a PIN), or a card number written as a JSON number
			if out := s.Text(n.lit); out != n.lit {
				n.kind, n.str = strNode, out
			}
		}
	case arrNode:
		kind := force
		if kind == "" && len(n.kids) >= 2 && n.kids[0].kind == strNode {
			// a [name, value] tuple, the way header lists are written
			kind = headerKind(n.kids[0].str)
			if kind != "" {
				s.scrubNode(n.kids[0], "", embedded)
				for _, k := range n.kids[1:] {
					s.scrubNode(k, kind, embedded)
				}
				return
			}
		}
		for _, k := range n.kids {
			s.scrubNode(k, force, embedded)
		}
	case objNode:
		pair := headerPair(n)
		for i, k := range n.kids {
			kind := force
			if kind == "" {
				kind = headerKind(n.keys[i])
			}
			if kind == "" && pair != "" {
				if lk := strings.ToLower(strings.TrimSpace(n.keys[i])); lk == "value" || lk == "values" {
					kind = pair
				}
			}
			s.scrubNode(k, kind, embedded)
			n.keys[i] = s.Text(n.keys[i])
		}
	}
}

// embeddedJSON scrubs a string that holds a JSON object or array (tool output often does) as JSON
// and returns it re-encoded as the string's new value, or nil when str is not such a document.
func (s *Scrubber) embeddedJSON(str string, embedded int) []byte {
	t := strings.TrimSpace(str)
	if t == "" || (t[0] != '{' && t[0] != '[') {
		return nil
	}
	sub, err := parseJSON([]byte(t))
	if err != nil {
		return nil
	}
	s.scrubNode(sub, "", embedded+1)
	return sub.appendTo(nil)
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

func (s *Scrubber) replaceAll(k Kind) string {
	if s.observe != nil {
		s.observe(k)
	}
	return Replacement(k)
}
