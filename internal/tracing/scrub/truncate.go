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

// planContainer decides how a container's budget is shared: how many leading children are kept
// (m, 0 when not even one fits) and how many bytes each kept child may use, by water-filling:
// ascending by size, each takes its size or an equal share of what is left.
func planContainer(n *node, budget int) (m int, give []int) {
	isObj := n.kind == objNode
	k := len(n.kids)
	fixed := make([]int, k) // per-child bytes besides the value: the key and colon
	if isObj {
		for i := range n.kids {
			fixed[i] = len(appendString(nil, n.keys[i])) + 1
		}
	}
	avail := budget - 2
	need := 0
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
		return 0, nil
	}
	remaining := avail - commas(m)
	if k-m+n.extra > 0 {
		remaining -= markerReserve
	}
	for i := 0; i < m; i++ {
		remaining -= fixed[i]
	}
	order := make([]int, m)
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return n.kids[order[a]].size < n.kids[order[b]].size })
	give = make([]int, m)
	for idx, i := range order {
		share := remaining / (m - idx)
		g := minInt(n.kids[i].size, share)
		give[i] = g
		remaining -= g
	}
	return m, give
}

func (t *truncator) fitContainer(n *node, budget int) *node {
	isObj := n.kind == objNode
	m, give := planContainer(n, budget)
	if m == 0 {
		return strLeaf("…")
	}
	dropped := len(n.kids) - m + n.extra
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

// assignBudgets walks the tree the way truncation will, on the raw sizes, and gives every string
// the bytes it could ever show (cap), so scrubbing can cut long strings first and its work is
// bounded by the limit, not by the input. Children that would be dropped are blanked: if scrubbing
// shrinks the others, the cut may show a few more, and they must not appear unscrubbed.
func assignBudgets(n *node, budget int) {
	switch n.kind {
	case strNode:
		if n.size > budget {
			n.cap = max(budget, minShare)
		}
	case arrNode, objNode:
		if n.size <= budget && !n.dirty {
			return
		}
		m, give := planContainer(n, budget)
		for i, k := range n.kids {
			if i >= m {
				blank(k)
				continue
			}
			assignBudgets(k, maxInt(give[i], minShare))
		}
	}
}

// blank empties a subtree that will not be shown.
func blank(n *node) {
	switch n.kind {
	case strNode:
		n.str = ""
	case numNode, boolNode:
		n.kind, n.lit = nullNode, "null"
	case arrNode, objNode:
		for _, k := range n.kids {
			blank(k)
		}
	}
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
	// maxLimit is the largest limit Content honours.
	maxLimit = 1 << 20
)

// Content scrubs and truncates one content attribute value. When s is a JSON object, array or
// string it is scrubbed as JSON (structure kept) and truncated with TruncateJSON; otherwise it is
// scrubbed as text and cut with TruncateText. limit 0 means DefaultMaxBytes; one above 1 MiB is
// lowered to it. truncated reports a cut.
//
// Work is bounded by the limit, whatever the size of the input:
//
//   - Text is cut to the limit plus a margin before it is scrubbed. In JSON, containers keep only
//     the children the limit could ever show, and each string is cut to the share of the limit it
//     could ever show, plus a margin.
//   - Every such cut first drops the partial credential at its end (trimTail), so the cut cannot
//     leave a fragment of a secret, in the text or in any escaped reading of it.
//   - A JSON document too large to parse (over 4 MiB) is scrubbed as text from its start and
//     returned as one JSON string, so the output is still valid JSON.
//
// Scrubbing still comes before the final cut.
func (s *Scrubber) Content(in string, limit int) (out string, truncated bool) {
	if limit <= 0 {
		limit = DefaultMaxBytes
	}
	limit = min(max(limit, MinMaxBytes), maxLimit)
	sc := *s
	sc.leafCap = limit + contentMargin
	t := strings.TrimSpace(in)
	jsonShaped := t != "" && (t[0] == '{' || t[0] == '[' || t[0] == '"')
	cut := false
	if len(in) > maxContentBytes {
		in, cut = sc.trimTail(cutRunes(in, maxContentBytes)), true
	} else if jsonShaped {
		n, err := parseJSON([]byte(t))
		if errors.Is(err, errTooDeep) {
			return TooDeepPlaceholder, true
		}
		if err == nil {
			bound(n, limit/8+8)
			measure(n)
			assignBudgets(n, limit+contentMargin)
			sc.scrubNode(n, "", 0)
			b, c := truncateNode(n, limit)
			return string(b), c
		}
	}
	prefix := false
	if len(in) > limit+contentMargin {
		in, prefix = sc.trimTail(cutRunes(in, limit+contentMargin)), true
	}
	res := sc.Text(in)
	if cut || prefix {
		res += TruncatedSuffix
		truncated = true
	}
	if jsonShaped && cut {
		// too large to parse: keep the output JSON by returning the scrubbed start as one string
		enc := appendString(nil, res)
		if len(enc) > limit {
			enc = appendString(nil, cutString(res, limit))
		}
		return string(enc), true
	}
	if truncated {
		if len(res) <= limit {
			return res, true
		}
		res, _ = TruncateText(strings.TrimSuffix(res, TruncatedSuffix), limit)
		return res, true
	}
	return TruncateText(res, limit)
}

// bound drops the children of a container beyond the first maxKids, recording how many in
// extra, so that truncation can still say "N more". The fair-share cut in fitContainer keeps a
// prefix of at most limit/16 children, so the dropped ones could never have been shown. Object
// members named name, key or header are kept wherever they are: they say what a "value" member is.
func bound(n *node, maxKids int) {
	if len(n.kids) > maxKids {
		keepIdx := make([]int, 0, maxKids+4)
		for i := range n.kids {
			if i < maxKids || (n.kind == objNode && isPairName(n.keys[i])) {
				keepIdx = append(keepIdx, i)
			}
		}
		n.extra += len(n.kids) - len(keepIdx)
		kids := make([]*node, len(keepIdx))
		var keys []string
		if n.keys != nil {
			keys = make([]string, len(keepIdx))
		}
		for j, i := range keepIdx {
			kids[j] = n.kids[i]
			if keys != nil {
				keys[j] = n.keys[i]
			}
		}
		n.kids, n.keys = kids, keys
	}
	for _, k := range n.kids {
		bound(k, maxKids)
	}
}

func isPairName(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "name", "key", "header":
		return true
	}
	return false
}
