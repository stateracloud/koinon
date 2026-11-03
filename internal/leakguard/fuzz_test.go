// SPDX-License-Identifier: Apache-2.0

package leakguard_test

import (
	"context"
	"testing"
	"unicode"

	"github.com/stateracloud/koinon/internal/leakguard"
)

// FuzzNormalise checks the matching form: letters and digits only, idempotent, and every rune
// mapped to a source offset in order.
func FuzzNormalise(f *testing.F) {
	for _, s := range []string{"", "Payroll EXPORT", "ＰＡＹ𝐩ﬁ", "раyrоll", "ΡΑΥ ναρ", "Straße ıİK", "a\u0301\u200b\xff", "日本 Ελλάδα ①"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		c := leakguard.Normalise(s)
		if len(c.Runes) != len(c.Off) {
			t.Fatalf("runes %d, offsets %d", len(c.Runes), len(c.Off))
		}
		prev := 0
		for i, r := range c.Runes {
			if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
				t.Fatalf("rune %q at %d is not a letter or digit", r, i)
			}
			if c.Off[i] < prev || c.Off[i] >= len(s) {
				t.Fatalf("offset %d at %d out of order or range", c.Off[i], i)
			}
			prev = c.Off[i]
		}
		if again := leakguard.Normalise(c.String()).String(); again != c.String() {
			t.Fatalf("not idempotent: %q -> %q -> %q", s, c.String(), again)
		}
	})
}

// FuzzDecode checks the decoders stay within their budgets and always return the input first.
func FuzzDecode(f *testing.F) {
	for _, s := range []string{"", "d=%41%42", "cGF5cm9sbCBleHBvcnQgam9i", "7061796f6c6c206578706f7274", "%2541%2542", "a+b c/d-e_f"} {
		f.Add(s)
	}
	lim := leakguard.Limits{MaxWorkBytes: 1 << 20}
	f.Fuzz(func(t *testing.T, s string) {
		vs, err := leakguard.Decode(context.Background(), s, lim)
		if err != nil {
			return // a budget: the caller fails closed
		}
		if len(vs) == 0 || vs[0].Text != s || vs[0].Chain != "" {
			t.Fatalf("the first variant is not the input")
		}
		if len(vs) > leakguard.DefaultLimits().MaxVariants {
			t.Fatalf("%d variants", len(vs))
		}
		seen := map[string]bool{}
		for _, v := range vs {
			if seen[v.Text] {
				t.Fatalf("duplicate variant %q", v.Text)
			}
			seen[v.Text] = true
		}
	})
}

// FuzzTaint plants private text in fuzzer-shaped arguments: the call is never allowed.
func FuzzTaint(f *testing.F) {
	f.Add("prefix ", " suffix", uint8(0))
	f.Add("%", "%%", uint8(1))
	f.Add("aGVs", "==", uint8(2))
	fp := newFingerprinter(testKey)
	checker := leakguard.TaintChecker{Store: taintedStore(fp, privateLine), Limits: leakguard.Limits{MaxWorkBytes: 4 << 20}}
	const planted = "payroll export job runs nightly on the finance"
	f.Fuzz(func(t *testing.T, before, after string, enc uint8) {
		var p string
		switch enc % 4 {
		case 0:
			p = planted
		case 1:
			p = percentAll(planted)
		case 2:
			p = b64(planted)
		default:
			p = hexOf(planted)
		}
		// separators around the planted text keep a fuzzed neighbour from joining a base64 or hex run
		d, err := checker.Check(context.Background(), testScope, fp, []string{before + " " + p + " " + after})
		if err != nil {
			t.Fatalf("check: %v", err)
		}
		if d.Allow {
			t.Fatalf("planted text allowed: %q %q %d", before, after, enc%4)
		}
	})
}

// FuzzCheckURL checks the URL rule never panics and a block always says why.
func FuzzCheckURL(f *testing.F) {
	for _, s := range []string{"https://github.com/x", "http://[::1]:80/", "https://a@b.example/%41", "https://x.example/?q=" + b64(privateLine)} {
		f.Add(s)
	}
	p := leakguard.DefaultURLPolicy()
	f.Fuzz(func(t *testing.T, raw string) {
		for _, d := range []leakguard.Decision{p.CheckURL(leakguard.NewRunState(), raw), p.CheckSearch(leakguard.NewRunState(), raw)} {
			if !d.Allow && (d.Reason == "" || d.Detail == "") {
				t.Fatalf("block without a reason: %+v", d)
			}
		}
	})
}
