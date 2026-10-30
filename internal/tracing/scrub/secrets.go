// SPDX-License-Identifier: Apache-2.0

package scrub

import (
	"encoding/json"
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
// whitespace are ignored. Besides the value itself it matches the JSON-escaped form (a secret
// with quotes or newlines appears escaped inside a JSON string that is scrubbed as text) and,
// for a multi-line value such as a PEM key, each long enough line.
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
		add(v)
		if e, err := json.Marshal(v); err == nil {
			// Without the surrounding quotes, and without HTML escaping surprises: both forms.
			add(string(e[1 : len(e)-1]))
		}
		if strings.ContainsAny(v, "\r\n") {
			for _, line := range strings.FieldsFunc(v, func(r rune) bool { return r == '\n' || r == '\r' }) {
				add(strings.TrimSpace(line))
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
