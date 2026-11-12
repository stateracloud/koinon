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
	// maxStructuralBytes bounds the search for JSON inside text. Within it the search is linear in
	// the text (see structScan), so it has no failure limit: a limit would turn the search off for
	// the rest of the text after that many stray brackets.
	maxStructuralBytes = 1 << 20
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
	// cap, when positive, is the most bytes of this string that could be shown (see assignBudgets).
	cap int
	// dirty is set by measure when this subtree dropped children (extra > 0 somewhere).
	dirty bool
	// extra is how many trailing children were dropped before truncation (see bound).
	extra int
	// s and e are the byte range of a string or number token in the text, set only by the
	// structural scan of text (findForcedLeaves).
	s, e int
}

func parseJSON(raw []byte) (*node, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	p := &parser{dec: dec}
	n, err := p.value(0)
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

type parser struct {
	dec *json.Decoder
}

func (p *parser) value(depth int) (*node, error) {
	if depth > maxJSONDepth {
		return nil, errTooDeep
	}
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
		return n.str != "" && (force != KindCredential || !notAValue(n.str))
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
		str := n.str
		cut := false
		if lim := s.leafLimit(n); lim > 0 && len(str) > lim {
			// longer than it could ever be shown: cut first, so the work is bounded. The cut drops
			// any partial credential at the end rather than leave a fragment of it.
			str, cut = s.trimTail(cutRunes(str, lim)), true
		}
		if !cut && embedded < maxEmbeddedDepth && len(str) <= maxEmbeddedBytes {
			if out, ok := s.embeddedJSON(str, embedded); ok {
				n.str = out
				return true
			}
		}
		out := s.Text(str)
		if cut {
			out += TruncatedSuffix
		}
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

// leafMargin is how much more than its budget a string keeps before scrubbing: scrubbing can
// shorten text, so the cut is made beyond the budget.
const leafMargin = 512

// leafLimit is the most bytes of a string that are scrubbed: its own budget plus the margin, else
// the scrubber's general cap, else unlimited (0).
func (s *Scrubber) leafLimit(n *node) int {
	if n.cap > 0 {
		return n.cap + leafMargin
	}
	return s.leafCap
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
				if kind := nameKind(n.kids[i].str); kind != "" {
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
// or a "in" member between them; the parsed structure can. Every '{' and '[' is tried as the start
// of a document, as far into the text as it goes.
func findForcedLeaves(in string, spans []span) []span {
	if len(in) > maxStructuralBytes {
		return spans
	}
	sc := structScan{in: in}
	for i := 0; i < len(in); i++ {
		if in[i] != '{' && in[i] != '[' {
			continue
		}
		r := sc.container(i)
		if !r.ok || r.depth > maxJSONDepth {
			continue
		}
		collectForced(r.n, "", func(leaf *node, k Kind) {
			a, b := leaf.s, leaf.e
			if leaf.kind == strNode && b-a >= 2 { // the string's content, inside its quotes
				a, b = a+1, b-1
			}
			if b > a {
				spans = append(spans, span{a, b, k})
			}
		})
		i = r.end - 1
	}
	return spans
}

// scanned is the outcome of parsing the container that starts at one offset of the text.
type scanned struct {
	ok bool
	n  *node
	// end is the offset just past the container.
	end int
	// depth is how deep the deepest value inside it sits (0 for an empty container or a scalar),
	// as parseJSON counts it: a container deeper than maxJSONDepth is not a document.
	depth int
}

// structScan parses the JSON containers that start anywhere in a text. The text of a container
// is the same whichever start reaches it, so each container is parsed once: its outcome is kept by
// offset and a later start, or an enclosing container, reuses it. A failure fails every container
// open at that point (each needed the failed one to be valid), so they are all kept as failed.
// That makes the work linear in the text: stray brackets, long unterminated arrays and deep
// nesting each cost their length once, not once per bracket before them.
type structScan struct {
	in string
	// memo holds the outcome by start offset, allocated at the first container.
	memo []*scanned
}

// frame is a container being parsed.
type frame struct {
	start int
	n     *node
	depth int
	// key is the member name read before the colon (objects).
	key string
	// st is what the container expects next.
	st scanState
}

type scanState uint8

const (
	stFirst     scanState = iota // a value or the close (array); a key or the close (object)
	stValue                      // a value (array after a comma; object after the colon)
	stKey                        // a key (object after a comma)
	stColon                      // the colon after a key
	stNextOrEnd                  // a comma or the close
)

var failed = &scanned{}

// container returns the outcome of the container at offset start (in[start] is '{' or '[').
func (sc *structScan) container(start int) *scanned {
	if sc.memo == nil {
		sc.memo = make([]*scanned, len(sc.in))
	}
	if r := sc.memo[start]; r != nil {
		return r
	}
	in := sc.in
	stack := []*frame{{start: start, n: newContainer(in[start])}}
	pos := start + 1
	fail := func() *scanned {
		for _, f := range stack {
			sc.memo[f.start] = failed
		}
		return failed
	}
	for {
		f := stack[len(stack)-1]
		pos = skipSpace(in, pos)
		if pos >= len(in) {
			return fail()
		}
		c := in[pos]
		obj := f.n.kind == objNode
		switch f.st {
		case stNextOrEnd, stFirst:
			if (obj && c == '}') || (!obj && c == ']') {
				pos++
				r := &scanned{ok: true, n: f.n, end: pos, depth: f.depth}
				sc.memo[f.start] = r
				stack = stack[:len(stack)-1]
				if len(stack) == 0 {
					return r
				}
				sc.attach(stack[len(stack)-1], r)
				continue
			}
			if f.st == stNextOrEnd {
				if c != ',' {
					return fail()
				}
				pos++
				if obj {
					f.st = stKey
				} else {
					f.st = stValue
				}
				continue
			}
			if obj {
				f.st = stKey
			} else {
				f.st = stValue
			}
			continue
		case stKey:
			if c != '"' {
				return fail()
			}
			e, ok := stringEnd(in, pos)
			if !ok {
				return fail()
			}
			f.key = decodeString(in[pos:e])
			f.st = stColon
			pos = e
		case stColon:
			if c != ':' {
				return fail()
			}
			pos++
			f.st = stValue
		default: // stValue
			if c == '{' || c == '[' {
				if r := sc.memo[pos]; r != nil {
					if !r.ok {
						return fail()
					}
					sc.attach(f, r)
					pos = r.end
					continue
				}
				stack = append(stack, &frame{start: pos, n: newContainer(c)})
				pos++
				continue
			}
			n, e, ok := scalar(in, pos)
			if !ok {
				return fail()
			}
			sc.attach(f, &scanned{ok: true, n: n})
			pos = e
		}
	}
}

func newContainer(c byte) *node {
	if c == '{' {
		return &node{kind: objNode}
	}
	return &node{kind: arrNode}
}

// attach adds the parsed value r to the container f.
func (sc *structScan) attach(f *frame, r *scanned) {
	if f.n.kind == objNode {
		f.n.keys = append(f.n.keys, f.key)
	}
	f.n.kids = append(f.n.kids, r.n)
	f.depth = max(f.depth, r.depth+1)
	f.st = stNextOrEnd
}

func skipSpace(in string, pos int) int {
	for pos < len(in) && (in[pos] == ' ' || in[pos] == '\t' || in[pos] == '\n' || in[pos] == '\r') {
		pos++
	}
	return pos
}

// stringEnd returns the offset just past the JSON string that starts at in[pos] ('"'), and false
// when it is not a valid string (unterminated, a control character, a bad escape).
func stringEnd(in string, pos int) (int, bool) {
	for i := pos + 1; i < len(in); i++ {
		switch c := in[i]; {
		case c == '"':
			return i + 1, true
		case c < 0x20:
			return 0, false
		case c == '\\':
			i++
			if i >= len(in) {
				return 0, false
			}
			switch in[i] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			case 'u':
				if i+4 >= len(in) || !isHex(in[i+1]) || !isHex(in[i+2]) || !isHex(in[i+3]) || !isHex(in[i+4]) {
					return 0, false
				}
				i += 4
			default:
				return 0, false
			}
		}
	}
	return 0, false
}

// decodeString decodes a valid JSON string token as encoding/json does (invalid UTF-8 and lone
// surrogates become U+FFFD).
func decodeString(tok string) string {
	plain := true
	for i := 1; i < len(tok)-1; i++ {
		if tok[i] == '\\' || tok[i] >= utf8.RuneSelf {
			plain = false
			break
		}
	}
	if plain {
		return tok[1 : len(tok)-1]
	}
	var s string
	if err := json.Unmarshal([]byte(tok), &s); err != nil {
		return tok[1 : len(tok)-1] // not reached: the token was checked
	}
	return s
}

// scalar parses the string, number, true, false or null at in[pos] and returns it with the
// offset just past it. Strings and numbers record their byte range, as findForcedLeaves needs.
func scalar(in string, pos int) (*node, int, bool) {
	switch c := in[pos]; {
	case c == '"':
		e, ok := stringEnd(in, pos)
		if !ok {
			return nil, 0, false
		}
		return &node{kind: strNode, str: decodeString(in[pos:e]), s: pos, e: e}, e, true
	case c == '-' || (c >= '0' && c <= '9'):
		e, ok := numberEnd(in, pos)
		if !ok {
			return nil, 0, false
		}
		return &node{kind: numNode, lit: in[pos:e], s: pos, e: e}, e, true
	}
	for _, lit := range [...]string{"true", "false", "null"} {
		if strings.HasPrefix(in[pos:], lit) {
			if lit == "null" {
				return &node{kind: nullNode, lit: lit}, pos + len(lit), true
			}
			return &node{kind: boolNode, lit: lit}, pos + len(lit), true
		}
	}
	return nil, 0, false
}

// numberEnd returns the offset just past the JSON number at in[pos], and false when the text
// there is not one.
func numberEnd(in string, pos int) (int, bool) {
	i := pos
	digits := func() int {
		s := i
		for i < len(in) && in[i] >= '0' && in[i] <= '9' {
			i++
		}
		return i - s
	}
	if in[i] == '-' {
		i++
	}
	if i >= len(in) {
		return 0, false
	}
	if in[i] == '0' {
		i++
	} else if digits() == 0 {
		return 0, false
	}
	if i < len(in) && in[i] == '.' {
		i++
		if digits() == 0 {
			return 0, false
		}
	}
	if i < len(in) && (in[i] == 'e' || in[i] == 'E') {
		i++
		if i < len(in) && (in[i] == '+' || in[i] == '-') {
			i++
		}
		if digits() == 0 {
			return 0, false
		}
	}
	return i, true
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
