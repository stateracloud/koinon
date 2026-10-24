// SPDX-License-Identifier: Apache-2.0

package pricing_test

import (
	"testing"
	"time"

	"github.com/stateracloud/koinon/internal/money"
	"github.com/stateracloud/koinon/pricing"
)

// The CI-run check of the default list: the schema is valid and every entry cites its source.
func TestDefaultListIsValid(t *testing.T) {
	l, rs := pricing.Default()
	if len(l.Entries) < 10 || len(rs) != len(l.Entries) {
		t.Fatalf("expected the default list to hold at least 10 valid entries, got %d (%d parsed)", len(l.Entries), len(rs))
	}
	for _, e := range l.Entries {
		if e.Source.URL == "" || e.Source.Accessed == "" {
			t.Errorf("%s: no source", e.ID)
		}
		if e.Note == "" {
			t.Errorf("%s: no note quoting the page", e.ID)
		}
	}
}

func TestCostBucketsDoNotOverlap(t *testing.T) {
	_, rs := pricing.Default()
	r, ok := pricing.Select(rs, "anthropic", "claude-haiku-5-5", time.Now())
	if !ok {
		t.Fatal("haiku not found")
	}
	// OpenAI convention: 1000 input tokens include 300 cache reads and 100 cache writes.
	u := pricing.Split(1000, 300, 100, 50, 0)
	if u.Input != 600 || u.Context() != 1000 {
		t.Fatalf("split %+v", u)
	}
	_, total, err := r.Price.Cost(u)
	if err != nil {
		t.Fatal(err)
	}
	// 600*0.10 + 300*0.01 + 100*0.125 + 50*0.50 per million = 60+3+12.5+25 = 100.5 micro-USD.
	if want := money.MustParse("0.0001005"); total != want {
		t.Fatalf("total %s want %s", total, want)
	}
	// Over 100K the tier replaces every rate it names.
	_, big, _ := r.Price.Cost(pricing.Usage{Input: 100_001})
	if want := money.MustParse("0.0500005"); big != want {
		t.Fatalf("tier total %s want %s", big, want)
	}
}

func TestMissingRatesFallBack(t *testing.T) {
	p, err := pricing.Spec{Prices: pricing.Wire{Input: "2", Output: "8"}}.Parse()
	if err != nil {
		t.Fatal(err)
	}
	_, total, _ := p.Cost(pricing.Usage{CacheRead: 1_000_000, Reasoning: 1_000_000})
	if total != money.MustParse("10") {
		t.Fatalf("got %s: cache reads bill at the input rate and reasoning at the output rate", total)
	}
}

func TestSelectPrefersExactProviderThenSpecificThenLater(t *testing.T) {
	mk := func(id, prov, glob, from, in string) pricing.Resolved {
		r, err := pricing.Entry{
			ID: id, Provider: prov, Match: pricing.Match{Glob: glob}, ValidFrom: from,
			Spec:      pricing.Spec{Prices: pricing.Wire{Input: in, Output: in}},
			Tokenizer: "bytes", Ratio: "3.0",
		}.Resolve(true)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	rs := []pricing.Resolved{
		mk("any", "*", "*", "2026-01-01", "1"),
		mk("prov", "openrouter", "*", "2026-01-01", "2"),
		mk("exact", "openrouter", "vendor/model", "2026-01-01", "3"),
		mk("exact2", "openrouter", "vendor/model", "2026-06-01", "4"),
	}
	at := func(s string) time.Time { d, _ := time.Parse(time.DateOnly, s); return d }
	for _, c := range []struct{ prov, model, date, want string }{
		{"openrouter", "vendor/model", "2026-03-01", "exact"},
		{"openrouter", "vendor/model", "2026-07-01", "exact2"},
		{"openrouter", "other", "2026-07-01", "prov"},
		{"ollama", "other", "2026-07-01", "any"},
	} {
		got, ok := pricing.Select(rs, c.prov, c.model, at(c.date))
		if !ok || got.Entry.ID != c.want {
			t.Errorf("%v: got %v %v", c, got.Entry.ID, ok)
		}
	}
	if _, ok := pricing.Select(rs, "openrouter", "x", at("2025-01-01")); ok {
		t.Error("an entry before its valid_from matched")
	}
}

func TestRejectsBadEntries(t *testing.T) {
	bad := []pricing.Entry{
		{ID: "a", Provider: "x", Match: pricing.Match{Glob: "a", Regex: "b"}, ValidFrom: "2026-01-01", Spec: pricing.Spec{Prices: pricing.Wire{Input: "1", Output: "1"}}, Tokenizer: "bytes", Ratio: "3"},
		{ID: "a", Provider: "x", Match: pricing.Match{Glob: "a"}, ValidFrom: "2026-01-01", Spec: pricing.Spec{Prices: pricing.Wire{Input: "1.1234567", Output: "1"}}, Tokenizer: "bytes", Ratio: "3"},
		{ID: "a", Provider: "x", Match: pricing.Match{Glob: "a"}, ValidFrom: "2026-01-01", Spec: pricing.Spec{Prices: pricing.Wire{Input: "1"}}, Tokenizer: "bytes", Ratio: "3"},
		{ID: "a", Provider: "x", Match: pricing.Match{Glob: "a"}, ValidFrom: "2026-01-01", Spec: pricing.Spec{Prices: pricing.Wire{Input: "1", Output: "1"}, Tiers: []pricing.WireTier{{ContextOver: 5, Prices: pricing.Wire{Input: "2"}}, {ContextOver: 5, Prices: pricing.Wire{Input: "3"}}}}, Tokenizer: "bytes", Ratio: "3"},
		{ID: "a", Provider: "x", Match: pricing.Match{Regex: "("}, ValidFrom: "2026-01-01", Spec: pricing.Spec{Prices: pricing.Wire{Input: "1", Output: "1"}}, Tokenizer: "bytes", Ratio: "3"},
	}
	for i, e := range bad {
		if _, err := e.Resolve(true); err == nil {
			t.Errorf("entry %d accepted", i)
		}
	}
}

func TestDiff(t *testing.T) {
	l, _ := pricing.Default()
	n := pricing.List{Version: "x", Entries: append([]pricing.Entry(nil), l.Entries[1:]...)}
	n.Entries[0].Prices.Input = "9"
	n.Entries = append(n.Entries, pricing.Entry{ID: "zzz", ValidFrom: "2026-10-24"})
	kinds := map[string]int{}
	for _, c := range pricing.Diff(l, n) {
		kinds[c.Kind]++
	}
	if kinds["added"] != 1 || kinds["removed"] != 1 || kinds["changed"] != 1 {
		t.Fatalf("%v", kinds)
	}
}
