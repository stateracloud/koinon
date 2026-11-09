// SPDX-License-Identifier: Apache-2.0

package pricing

import (
	"reflect"
	"slices"
	"strings"
)

// Change is one difference between two default lists.
type Change struct {
	// Kind is "added", "removed" or "changed".
	Kind      string `json:"kind"`
	ID        string `json:"id"`
	ValidFrom string `json:"validFrom"`
	Before    *Entry `json:"before,omitempty"`
	After     *Entry `json:"after,omitempty"`
	// Fields names what changed, for "changed".
	Fields []string `json:"fields,omitempty"`
}

// Diff lists what changed from old to new, sorted by entry.
func Diff(oldL, newL List) []Change {
	key := func(e Entry) string { return e.ID + "@" + e.ValidFrom }
	olds := map[string]Entry{}
	for _, e := range oldL.Entries {
		olds[key(e)] = e
	}
	var out []Change
	seen := map[string]bool{}
	for _, n := range newL.Entries {
		k := key(n)
		seen[k] = true
		o, ok := olds[k]
		n := n
		if !ok {
			out = append(out, Change{Kind: "added", ID: n.ID, ValidFrom: n.ValidFrom, After: &n})
			continue
		}
		if fields := changedFields(o, n); len(fields) > 0 {
			o := o
			out = append(out, Change{Kind: "changed", ID: n.ID, ValidFrom: n.ValidFrom, Before: &o, After: &n, Fields: fields})
		}
	}
	for k, o := range olds {
		if !seen[k] {
			o := o
			out = append(out, Change{Kind: "removed", ID: o.ID, ValidFrom: o.ValidFrom, Before: &o})
		}
	}
	slices.SortFunc(out, func(a, b Change) int {
		return strings.Compare(a.ID+a.ValidFrom+a.Kind, b.ID+b.ValidFrom+b.Kind)
	})
	return out
}

func changedFields(a, b Entry) []string {
	var f []string
	add := func(name string, same bool) {
		if !same {
			f = append(f, name)
		}
	}
	add("provider", a.Provider == b.Provider)
	add("not_hosts", slices.Equal(a.NotHosts, b.NotHosts))
	add("match", a.Match == b.Match)
	add("prices", a.Prices == b.Prices)
	add("tiers", reflect.DeepEqual(a.Tiers, b.Tiers) || (len(a.Tiers) == 0 && len(b.Tiers) == 0))
	add("tokenizer", a.Tokenizer == b.Tokenizer)
	add("ratio", a.Ratio == b.Ratio)
	add("tool_overhead_tokens", a.ToolOverheadTokens == b.ToolOverheadTokens)
	add("hidden_thinking", a.HiddenThinking == b.HiddenThinking)
	add("source", a.Source == b.Source)
	return f
}
