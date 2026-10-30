// SPDX-License-Identifier: Apache-2.0

package scrub

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// MinSecretLen is the shortest secret value that is matched, in bytes. A shorter value ("test",
// "1234") would redact ordinary words and numbers all over a trace and protect nothing; real
// credentials are longer. A value shorter than this is ignored by NewSecrets.
const MinSecretLen = 8

// Secrets is an immutable set of known secret values, matched verbatim with an Aho-Corasick
// automaton: one pass over the text however many secrets there are. The router builds one per org
// from the org's secret store and provider record credentials and swaps it when they change.
//
// A nil *Secrets is an empty set. The set holds the values in memory, so it never prints them.
type Secrets struct {
	nodes []acNode
	count int
	max   int // length of the longest pattern
}

type acEdge struct {
	b    byte
	next int
}

type acNode struct {
	edges []acEdge // sorted by b
	fail  int
	// out is the length of the longest pattern that ends at this node, directly or through
	// the failure chain; 0 when none does.
	out int
}

// NewSecrets builds the set. Values shorter than MinSecretLen after trimming surrounding
// whitespace are ignored. Besides the value itself it matches the forms the value takes when it
// is printed by something else:
//
//   - JSON-escaped, with and without HTML escaping, ASCII-only (\uXXXX for non-ASCII) and
//     escaped twice (JSON inside a JSON string);
//   - URL-encoded (query and path escaping) and base64 of the whole value (standard and URL
//     alphabets, padded and raw);
//   - each line of a multi-line value such as a PEM key, split on real and escaped newlines;
//   - for a value that is itself a JSON document (a service-account key file), each of its
//     string leaves, split into lines the same way.
func NewSecrets(values ...string) *Secrets {
	set := map[string]struct{}{}
	add := func(v string) {
		if len(v) >= MinSecretLen {
			set[v] = struct{}{}
		}
	}
	for _, v := range values {
		v = strings.TrimSpace(v)
		if len(v) < MinSecretLen {
			continue
		}
		bases := map[string]struct{}{v: {}}
		for _, piece := range splitLines(v) {
			bases[piece] = struct{}{}
		}
		if v[0] == '{' || v[0] == '[' {
			var doc any
			if json.Unmarshal([]byte(v), &doc) == nil {
				walkStrings(doc, func(leaf string) {
					leaf = strings.TrimSpace(leaf)
					bases[leaf] = struct{}{}
					for _, piece := range splitLines(leaf) {
						bases[piece] = struct{}{}
					}
				})
			}
		}
		for base := range bases {
			if len(base) < MinSecretLen {
				continue
			}
			for _, form := range secretForms(base) {
				add(form)
			}
		}
	}
	s := &Secrets{nodes: make([]acNode, 1)}
	if len(set) == 0 {
		return s
	}
	patterns := make([]string, 0, len(set))
	for p := range set {
		patterns = append(patterns, p)
	}
	sort.Strings(patterns) // deterministic layout
	for _, p := range patterns {
		cur := 0
		for i := 0; i < len(p); i++ {
			cur = s.child(cur, p[i], true)
		}
		s.nodes[cur].out = len(p)
	}
	s.count = len(patterns)
	for _, p := range patterns {
		s.max = max(s.max, len(p))
	}
	s.link()
	return s
}

// Len reports how many patterns the set matches (a secret and its escaped forms count separately).
func (s *Secrets) Len() int {
	if s == nil {
		return 0
	}
	return s.count
}

// String never prints a value.
func (s *Secrets) String() string { return "scrub.Secrets" }

// GoString never prints a value.
func (s *Secrets) GoString() string { return "scrub.Secrets" }

func (s *Secrets) child(n int, b byte, create bool) int {
	edges := s.nodes[n].edges
	i := sort.Search(len(edges), func(i int) bool { return edges[i].b >= b })
	if i < len(edges) && edges[i].b == b {
		return edges[i].next
	}
	if !create {
		return -1
	}
	s.nodes = append(s.nodes, acNode{})
	id := len(s.nodes) - 1
	e := s.nodes[n].edges
	e = append(e, acEdge{})
	copy(e[i+1:], e[i:])
	e[i] = acEdge{b: b, next: id}
	s.nodes[n].edges = e
	return id
}

// link computes failure links breadth-first and folds each node's failure chain into out.
func (s *Secrets) link() {
	queue := make([]int, 0, len(s.nodes))
	for _, e := range s.nodes[0].edges {
		queue = append(queue, e.next) // fail = 0
	}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		for _, e := range s.nodes[n].edges {
			f := s.nodes[n].fail
			var target int
			for {
				if c := s.child(f, e.b, false); c >= 0 {
					target = c
					break
				}
				if f == 0 {
					target = 0
					break
				}
				f = s.nodes[f].fail
			}
			s.nodes[e.next].fail = target
			if s.nodes[target].out > s.nodes[e.next].out {
				s.nodes[e.next].out = s.nodes[target].out
			}
			queue = append(queue, e.next)
		}
	}
}

// find appends a span for the longest pattern ending at each position of in.
func (s *Secrets) find(in string, spans []span) []span {
	if s == nil || s.count == 0 {
		return spans
	}
	cur := 0
	for i := 0; i < len(in); i++ {
		b := in[i]
		for {
			if c := s.child(cur, b, false); c >= 0 {
				cur = c
				break
			}
			if cur == 0 {
				break
			}
			cur = s.nodes[cur].fail
		}
		if out := s.nodes[cur].out; out > 0 {
			spans = append(spans, span{start: i + 1 - out, end: i + 1, kind: KindSecret})
		}
	}
	return spans
}

// splitLines splits v on real and escaped (backslash-n, backslash-r) newlines and trims the pieces.
func splitLines(v string) []string {
	if !strings.ContainsAny(v, "\r\n\\") {
		return nil
	}
	v = strings.NewReplacer(`\r`, "\n", `\n`, "\n", "\r", "\n").Replace(v)
	var out []string
	for _, l := range strings.Split(v, "\n") {
		if l = strings.TrimSpace(l); len(l) >= MinSecretLen {
			out = append(out, l)
		}
	}
	return out
}

func walkStrings(v any, f func(string)) {
	switch t := v.(type) {
	case string:
		f(t)
	case []any:
		for _, e := range t {
			walkStrings(e, f)
		}
	case map[string]any:
		for _, e := range t {
			walkStrings(e, f)
		}
	}
}

// secretForms returns v and the encoded forms it may be printed in.
func secretForms(v string) []string {
	forms := []string{v}
	if e, err := json.Marshal(v); err == nil { // HTML-escaped: \u003c for <
		forms = append(forms, string(e[1:len(e)-1]))
	}
	plain := string(appendString(nil, v))
	plain = plain[1 : len(plain)-1]
	forms = append(forms, plain)
	ascii := asciiEscape(plain, false)
	forms = append(forms, ascii, asciiEscape(plain, true))
	double := string(appendString(nil, plain))
	forms = append(forms, double[1:len(double)-1])
	q := url.QueryEscape(v)
	forms = append(forms, q, url.PathEscape(v), strings.ToLower(q))
	b := []byte(v)
	forms = append(forms, base64.StdEncoding.EncodeToString(b), base64.URLEncoding.EncodeToString(b),
		base64.RawStdEncoding.EncodeToString(b), base64.RawURLEncoding.EncodeToString(b))
	return forms
}

// asciiEscape writes every non-ASCII rune of s as a \uXXXX escape (a surrogate pair above the
// BMP), the way Python's json.dumps does by default.
func asciiEscape(s string, upper bool) string {
	format := "%04x"
	if upper {
		format = "%04X"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r < 0x80:
			b.WriteRune(r)
		case r < 0x10000:
			fmt.Fprintf(&b, `\u`+format, r)
		default:
			r -= 0x10000
			fmt.Fprintf(&b, `\u`+format+`\u`+format, 0xD800+(r>>10), 0xDC00+(r&0x3ff))
		}
	}
	return b.String()
}

// maxLen is the length of the longest pattern; 0 for a nil or empty set.
func (s *Secrets) maxLen() int {
	if s == nil {
		return 0
	}
	return s.max
}
