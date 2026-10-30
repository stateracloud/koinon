// SPDX-License-Identifier: Apache-2.0

package scrub

import (
	"errors"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	// DefaultMaxBytes is the default per-attribute limit for content (ADR 0074, decision 6).
	DefaultMaxBytes = 16 << 10
	// MinMaxBytes is the smallest limit honoured; a smaller one is raised to it. Below this there is
	// no room to say what was cut.
	MinMaxBytes = 64
	// TruncatedKey is the member added to an object that lost members; its value is how many.
	TruncatedKey = "koinon.truncated"
	// TruncatedSuffix ends a string that was cut.
	TruncatedSuffix = "…[truncated]"

	minShare      = 16 // bytes a kept child is always given
	markerReserve = 48 // bytes kept free for the "N more" marker of a container
)

// TruncateJSON cuts a JSON value to at most limit bytes of compact JSON and keeps it valid JSON.
// A limit of 0 or less means DefaultMaxBytes; one below MinMaxBytes is raised to it.
//
// A value that fits is returned compacted, unchanged otherwise. A value that does not fit is cut
// to a fair share: the space is divided among the children of each array or object, children
// that are small keep all of theirs and the leftover goes to the big ones, so one huge system
// prompt cannot push out the user's message. Then:
//
//   - a cut string ends with TruncatedSuffix;
//   - an array that lost elements gets a last element, the string "…[N more items]";
//   - an object that lost members gets a last member TruncatedKey whose value is the count lost.
//
// truncated reports whether anything was cut, so the caller can set an attribute for it.
func TruncateJSON(raw []byte, limit int) (out []byte, truncated bool, err error) {
	n, err := parseJSON(raw)
	if errors.Is(err, errTooDeep) {
		return []byte(TooDeepPlaceholder), true, nil
	}
	if err != nil {
		return nil, false, err
	}
	out, truncated = truncateNode(n, limit)
	return out, truncated, nil
}

func truncateNode(n *node, limit int) ([]byte, bool) {
	if limit <= 0 {
		limit = DefaultMaxBytes
	}
	if limit < MinMaxBytes {
		limit = MinMaxBytes
	}
	measure(n)
	if n.size <= limit && !n.dirty {
		return n.appendTo(make([]byte, 0, n.size)), false
	}
	t := &truncator{}
	out := t.fit(n, limit).appendTo(make([]byte, 0, limit))
	if len(out) > limit { // unreachable by construction; never emit an oversize attribute
		out = appendString(nil, TruncatedSuffix)
	}
	return out, true
}

// measure fills node.size with the encoded size of every node.
func measure(n *node) int {
	n.dirty = n.extra > 0
	defer func() {
		if n.extra > 0 {
			n.size += markerReserve
		}
	}()
	switch n.kind {
	case nullNode, boolNode, numNode:
		n.size = len(n.lit)
	case strNode:
		n.size = len(appendString(nil, n.str))
	case arrNode:
		n.size = 2
		for i, k := range n.kids {
			n.size += measure(k)
			n.dirty = n.dirty || k.dirty
			if i > 0 {
				n.size++
			}
		}
	case objNode:
		n.size = 2
		for i, k := range n.kids {
			n.size += len(appendString(nil, n.keys[i])) + 1 + measure(k)
			n.dirty = n.dirty || k.dirty
			if i > 0 {
				n.size++
			}
		}
	}
	return n.size
}

type truncator struct{}

// fit returns a node whose encoded size is at most budget (budget >= minShare).
func (t *truncator) fit(n *node, budget int) *node {
	if n.size <= budget && !n.dirty {
		return n
	}
	switch n.kind {
	case strNode:
		return strLeaf(cutString(n.str, budget))
	case arrNode, objNode:
		if budget < 2+markerReserve {
			return strLeaf("…")
		}
		return t.fitContainer(n, budget)
	}
	return strLeaf("…") // a literal longer than its share
}

func strLeaf(s string) *node {
	n := &node{kind: strNode, str: s}
	n.size = len(appendString(nil, s))
	return n
}

// cutString shortens s so that its encoded form, with TruncatedSuffix, fits in budget bytes.
func cutString(s string, budget int) string {
	room := budget - 2 - len(TruncatedSuffix)
	if room < 0 {
		return "…"
	}
	used, end := 0, 0
	for end < len(s) {
		r, w := utf8.DecodeRuneInString(s[end:])
		cw := encodedWidth(r)
		if used+cw > room {
			break
		}
		used += cw
		end += w
	}
	return s[:end] + TruncatedSuffix
}

func (t *truncator) fitContainer(n *node, budget int) *node {
	isObj := n.kind == objNode
	k := len(n.kids)
	fixed := make([]int, k) // per-child bytes besides the value: the key and colon
	if isObj {
		for i := range n.kids {
			fixed[i] = len(appendString(nil, n.keys[i])) + 1
		}
	}
	avail := budget - 2
	// keep as many leading children as can each have their minimum.
	m, need := 0, 0
	for i := 0; i < k; i++ {
		c := fixed[i] + minInt(n.kids[i].size, minShare)
		if i > 0 {
			c++
		}
		if need+c > avail-markerReserve {
			break
		}
		need += c
		m++
	}
	if m == 0 {
		return strLeaf("…")
	}
	dropped := k - m + n.extra
	remaining := avail - commas(m)
	if dropped > 0 {
		remaining -= markerReserve
	}
	for i := 0; i < m; i++ {
		remaining -= fixed[i]
	}
	// water-fill: ascending by size, each takes its size or an equal share of what is left.
	order := make([]int, m)
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return n.kids[order[a]].size < n.kids[order[b]].size })
	give := make([]int, m)
	for idx, i := range order {
		share := remaining / (m - idx)
		g := minInt(n.kids[i].size, share)
		give[i] = g
		remaining -= g
	}
	out := &node{kind: n.kind}
	for i := 0; i < m; i++ {
		var child *node
		if n.kids[i].size <= give[i] && !n.kids[i].dirty {
			child = n.kids[i]
		} else {
			child = t.fit(n.kids[i], maxInt(give[i], minShare))
		}
		out.kids = append(out.kids, child)
		if isObj {
			out.keys = append(out.keys, n.keys[i])
		}
	}
	if dropped > 0 {
		if isObj {
			out.keys = append(out.keys, TruncatedKey)
			out.kids = append(out.kids, &node{kind: numNode, lit: strconv.Itoa(dropped)})
		} else {
			out.kids = append(out.kids, &node{kind: strNode, str: "…[" + strconv.Itoa(dropped) + " more items]"})
		}
	}
	return out
}

func commas(n int) int {
	if n <= 1 {
		return 0
	}
	return n // between the kept children, and one before a marker; a slight over-reserve
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TruncateText cuts plain text to at most limit bytes on a character boundary and ends it with
// TruncatedSuffix. A limit of 0 or less means DefaultMaxBytes.
func TruncateText(s string, limit int) (string, bool) {
	if limit <= 0 {
		limit = DefaultMaxBytes
	}
	if limit < MinMaxBytes {
		limit = MinMaxBytes
	}
	if len(s) <= limit {
		return s, false
	}
	cut := limit - len(TruncatedSuffix)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + TruncatedSuffix, true
}

const (
	// contentMargin is how much more than the limit Content scrubs before it cuts; scrubbing
	// can shrink text (a key block becomes a short marker), so the cut has to be made beyond the limit.
	contentMargin = 4 << 10
	// maxContentBytes is the most of an input Content looks at.
	maxContentBytes = 4 << 20
)

// Content scrubs and truncates one content attribute value. When s is a JSON object, array or
// string it is scrubbed as JSON (structure kept) and truncated with TruncateJSON; otherwise it is
// scrubbed as text and cut with TruncateText. limit 0 means DefaultMaxBytes. truncated reports a cut.
//
// Work is bounded whatever the size of the input. Text is cut to the limit plus a margin before it
// is scrubbed, and the last few bytes of the scrubbed prefix are dropped, because a credential cut
// in half at the margin would otherwise survive as a fragment. In JSON, containers keep only
// the children the limit could ever show and each string is cut the same way. Scrubbing still comes
// before the final cut, so no half secret survives it either.
func (s *Scrubber) Content(in string, limit int) (out string, truncated bool) {
	if limit <= 0 {
		limit = DefaultMaxBytes
	}
	limit = max(limit, MinMaxBytes)
	sc := *s
	sc.leafCap = limit + contentMargin
	cut := false
	if len(in) > maxContentBytes {
		in, cut = cutRunes(in, maxContentBytes), true
	}
	if t := strings.TrimSpace(in); t != "" && (t[0] == '{' || t[0] == '[' || t[0] == '"') {
		n, err := parseJSON([]byte(t))
		if errors.Is(err, errTooDeep) {
			return TooDeepPlaceholder, true
		}
		if err == nil {
			bound(n, limit/8+8)
			sc.scrubNode(n, "", 0)
			b, c := truncateNode(n, limit)
			return string(b), c || cut
		}
	}
	prefix := false
	if len(in) > limit+contentMargin {
		in, prefix = cutRunes(in, limit+contentMargin), true
	}
	res := sc.Text(in)
	if prefix {
		res = cutRunes(res, max(0, len(res)-sc.guard()))
	}
	if cut || prefix {
		if len(res)+len(TruncatedSuffix) <= limit {
			return res + TruncatedSuffix, true
		}
		res, _ = TruncateText(res, limit)
		return res, true
	}
	return TruncateText(res, limit)
}

// bound drops the children of a container beyond the first maxKids, recording how many in
// extra, so that truncation can still say "N more". The fair-share cut in fitContainer keeps a
// prefix of at most limit/16 children, so the dropped ones could never have been shown.
func bound(n *node, maxKids int) {
	if len(n.kids) > maxKids {
		n.extra += len(n.kids) - maxKids
		n.kids = n.kids[:maxKids]
		if n.keys != nil {
			n.keys = n.keys[:maxKids]
		}
	}
	for _, k := range n.kids {
		bound(k, maxKids)
	}
}
