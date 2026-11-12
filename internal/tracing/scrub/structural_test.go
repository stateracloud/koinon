// SPDX-License-Identifier: Apache-2.0

package scrub

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// pairTail is a header pair written value first, which only the structural search sees.
const pairTail = ` {"value":"koinon_test_not-a-real-token","name":"Authorization"} end`

// structuralPrefixes are texts that fail to parse at every bracket, in the shapes that cost a
// bracket-by-bracket search the most.
var structuralPrefixes = map[string]string{
	"stray braces":         "{",
	"stray brackets":       "[",
	"open keys":            `{"`,
	"keys without values":  `{"a":`,
	"unterminated arrays":  `[1,`,
	"unterminated members": `{"a":[1,2,"x"`,
	"braces in strings":    `"{"`,
	"spaced braces":        "{ ",
	"long nested arrays":   "[" + strings.Repeat("1,", 500),
}

// TestStructuralSearchHasNoFailureLimit: a pair after any number of brackets that fail to parse is
// still found (the search once stopped after 64 failures).
func TestStructuralSearchHasNoFailureLimit(t *testing.T) {
	s := New(Config{Level: LevelFull})
	for name, unit := range structuralPrefixes {
		t.Run(name, func(t *testing.T) {
			in := "log " + strings.Repeat(unit, 200) + pairTail
			if out, _ := s.Content(in, 0); strings.Contains(out, "not-a-real-token") {
				t.Errorf("Content left the value: %q", out[max(0, len(out)-120):])
			}
			if out := s.Text(in); strings.Contains(out, "not-a-real-token") {
				t.Errorf("Text left the value: %q", out[max(0, len(out)-120):])
			}
		})
	}
}

// TestStructuralSearchIsLinear: the largest text the search takes, made of brackets that fail to
// parse, is searched to its end in time linear in its length. A search that retried each bracket
// from scratch took tens of seconds on these.
func TestStructuralSearchIsLinear(t *testing.T) {
	if testing.Short() {
		t.Skip("large inputs")
	}
	for name, unit := range structuralPrefixes {
		t.Run(name, func(t *testing.T) {
			in := strings.Repeat(unit, (maxStructuralBytes-len(pairTail))/len(unit)) + pairTail
			start := time.Now()
			spans := findForcedLeaves(in, nil)
			took := time.Since(start)
			if len(spans) != 1 || !strings.Contains(in[spans[0].start:spans[0].end], "not-a-real-token") {
				t.Errorf("spans %v", spans)
			}
			// generous for -race on a loaded runner; the quadratic search took 20 s and more
			if took > 15*time.Second {
				t.Errorf("took %v", took)
			}
		})
	}
}

// TestStructuralScanMatchesEncodingJSON checks the scanner against encoding/json on known shapes.
func TestStructuralScanMatchesEncodingJSON(t *testing.T) {
	for _, doc := range []string{
		`{}`, `[]`, `{"a":1}`, `[1,-0.5e+3,true,false,null,"x"]`, `{"a":{"b":[{"c":"é😀"}]}}`,
		`[01]`, `[1.]`, `[-]`, `[1e]`, `{"a" 1}`, `{"a":1,}`, `[1,]`, `{1:2}`, `["\x"]`, "[\"\x01\"]", `["\u12"]`,
		`[tru]`, `{"a":1 "b":2}`, ` [ 1 , 2 ] `, "[\"\xff\"]", `["\ud800"]`,
		strings.Repeat("[", maxJSONDepth+1) + strings.Repeat("]", maxJSONDepth+1),
		strings.Repeat("[", maxJSONDepth) + strings.Repeat("]", maxJSONDepth),
	} {
		checkScanAgainstJSON(t, doc)
	}
}

// FuzzStructuralScan checks the scanner of findForcedLeaves against encoding/json: a container it
// accepts is valid JSON with the same decoded strings, and a valid document is accepted whole.
func FuzzStructuralScan(f *testing.F) {
	f.Add(`{"value":"x","name":"Authorization"}`)
	f.Add(`[1,[2,{"a":"é"}],null]`)
	f.Add(`{"a":[1,2,"x"`)
	f.Add(`[1,[1,[1,`)
	f.Add("[\"\xff\"]")
	f.Fuzz(func(t *testing.T, doc string) {
		checkScanAgainstJSON(t, doc)
		// and the text search never panics on it, wherever the brackets are
		findForcedLeaves("x "+doc+" "+doc, nil)
	})
}

func checkScanAgainstJSON(t *testing.T, doc string) {
	t.Helper()
	trimmed := strings.TrimLeft(doc, " \t\r\n")
	if trimmed == "" || (trimmed[0] != '{' && trimmed[0] != '[') {
		return
	}
	sc := structScan{in: trimmed}
	r := sc.container(0)
	_, fullErr := parseJSON([]byte(trimmed))
	if !r.ok {
		if fullErr == nil {
			t.Fatalf("scan refused a valid document %q", trimmed)
		}
		return
	}
	prefix := trimmed[:r.end]
	want, err := parseJSON([]byte(prefix))
	switch {
	case r.depth > maxJSONDepth:
		if err == nil {
			t.Fatalf("depth %d but parseJSON accepted %q", r.depth, prefix)
		}
		return
	case err != nil:
		t.Fatalf("scan accepted %q, parseJSON: %v", prefix, err)
	}
	if fullErr == nil && r.end != len(strings.TrimRight(trimmed, " \t\r\n")) {
		t.Fatalf("scan stopped at %d of the valid document %q", r.end, trimmed)
	}
	if got, exp := dump(r.n), dump(want); got != exp {
		t.Fatalf("scan %s, parseJSON %s", got, exp)
	}
}

// dump renders a node tree for comparison: kinds, keys, strings and literals.
func dump(n *node) string {
	var b strings.Builder
	var walk func(*node)
	walk = func(n *node) {
		switch n.kind {
		case strNode:
			q, _ := json.Marshal(n.str)
			b.Write(q)
		case arrNode, objNode:
			b.WriteByte("[{"[n.kind-arrNode])
			for i, k := range n.kids {
				if n.kind == objNode {
					q, _ := json.Marshal(n.keys[i])
					b.Write(q)
					b.WriteByte(':')
				}
				walk(k)
				b.WriteByte(',')
			}
			b.WriteByte("]}"[n.kind-arrNode])
		default:
			b.WriteString(n.lit)
		}
	}
	walk(n)
	return b.String()
}

func BenchmarkStructuralSearch(b *testing.B) {
	for name, unit := range structuralPrefixes {
		in := strings.Repeat(unit, (maxStructuralBytes-len(pairTail))/len(unit)) + pairTail
		b.Run(name, func(b *testing.B) {
			b.SetBytes(int64(len(in)))
			for b.Loop() {
				findForcedLeaves(in, nil)
			}
		})
	}
}
