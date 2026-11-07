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
		if e.MaxOutput == 0 && e.Prices.Input != "0" {
			t.Errorf("%s: a priced entry needs max_output", e.ID)
		}
		if e.Note == "" {
			t.Errorf("%s: no note quoting the page", e.ID)
		}
	}
}

func TestCostBucketsDoNotOverlap(t *testing.T) {
	// An entry of its own, not the default list's: a price update must not change this test.
	r, err := pricing.Entry{
		ID: "test", Provider: "test", Match: pricing.Match{Glob: "test-model"}, ValidFrom: "2000-01-01",
		Spec: pricing.Spec{
			Prices: pricing.Wire{Input: "0.10", Output: "0.50", CacheRead: "0.01", CacheWrite: "0.125"},
			Tiers:  []pricing.WireTier{{ContextOver: 100_000, Prices: pricing.Wire{Input: "0.50", Output: "2.5", CacheRead: "0.05", CacheWrite: "0.625"}}},
		},
		Tokenizer: "bytes", Ratio: "3.0", MaxOutput: 1000,
	}.Resolve(true)
	if err != nil {
		t.Fatal(err)
	}
	// Disjoint buckets: 600 plain input, 300 cache reads, 100 cache writes.
	u := pricing.Usage{Input: 600, CacheRead: 300, CacheWrite: 100, Output: 50}
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

func TestCacheWrite1hHasItsOwnRate(t *testing.T) {
	p, err := pricing.Spec{
		Prices: pricing.Wire{Input: "2", Output: "8", CacheWrite: "2.5", CacheWrite1h: "4"},
		Tiers:  []pricing.WireTier{{ContextOver: 1_000_000, Prices: pricing.Wire{CacheWrite1h: "8"}}},
	}.Parse()
	if err != nil {
		t.Fatal(err)
	}
	// 1M 5-minute writes at 2.5 plus 1M 1-hour writes at 4 (a 1-hour write is not billed at the
	// 5-minute rate, #270).
	b, total, err := p.Cost(pricing.Usage{CacheWrite: 500_000, CacheWrite1h: 500_000})
	if err != nil {
		t.Fatal(err)
	}
	if want := money.MustParse("3.25"); total != want || b.CacheWrite1h != money.MustParse("2") {
		t.Fatalf("total %s (1h %s), want 3.25 (1h 2)", total, b.CacheWrite1h)
	}
	// The 1-hour tokens count toward the context that selects a tier, and a tier may reprice them
	// alone: the 5-minute rate keeps the base rate.
	_, big, _ := p.Cost(pricing.Usage{CacheWrite: 1, CacheWrite1h: 1_000_000})
	if want := money.MustParse("8.0000025"); big != want {
		t.Fatalf("tier total %s want %s", big, want)
	}
	// The gateway catalog carries the rate.
	if g := p.Gateway(); g.CacheWrite1h != "4" || g.Tiers[0].CacheWrite1h != "8" {
		t.Fatalf("gateway price %+v", g)
	}
	// Unset, 1-hour writes cost what 5-minute writes cost (and without that, the input rate).
	q, _ := pricing.Spec{Prices: pricing.Wire{Input: "2", Output: "8", CacheWrite: "2.5"}}.Parse()
	if _, t1, _ := q.Cost(pricing.Usage{CacheWrite1h: 1_000_000}); t1 != money.MustParse("2.5") {
		t.Fatalf("fallback to cache_write: got %s", t1)
	}
	r, _ := pricing.Spec{Prices: pricing.Wire{Input: "2", Output: "8"}}.Parse()
	if _, t2, _ := r.Cost(pricing.Usage{CacheWrite1h: 1_000_000}); t2 != money.MustParse("2") {
		t.Fatalf("fallback to input: got %s", t2)
	}
	if _, err := (pricing.Wire{CacheWrite1h: "x"}).Parse(); err == nil {
		t.Fatal("a bad cache_write_1h parsed")
	}
}

func TestSelectPrefersExactProviderThenSpecificThenLater(t *testing.T) {
	mk := func(id, prov, glob, from, in string) pricing.Resolved {
		r, err := pricing.Entry{
			ID: id, Provider: prov, Match: pricing.Match{Glob: glob}, ValidFrom: from,
			Spec:      pricing.Spec{Prices: pricing.Wire{Input: in, Output: in}},
			Tokenizer: "bytes", Ratio: "3.0", MaxOutput: 1000,
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

func TestPricedEntryNeedsMaxOutput(t *testing.T) {
	e := pricing.Entry{
		ID: "a", Provider: "x", Match: pricing.Match{Glob: "a"}, ValidFrom: "2026-01-01",
		Spec: pricing.Spec{Prices: pricing.Wire{Input: "1", Output: "1"}}, Tokenizer: "bytes", Ratio: "3",
	}
	if _, err := e.Resolve(true); err == nil {
		t.Fatal("a priced entry without max_output was accepted")
	}
	e.MaxOutput = 4096
	if _, err := e.Resolve(true); err != nil {
		t.Fatal(err)
	}
	e.MaxOutput = 0
	e.Prices = pricing.Wire{Input: "0", Output: "0"}
	if _, err := e.Resolve(true); err != nil {
		t.Fatal("a free entry needs no max_output:", err)
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

func TestLaterExactGlobBeatsMigratedCaseSensitiveRegex(t *testing.T) {
	mk := func(id string, m pricing.Match, from string) pricing.Resolved {
		r, err := pricing.Entry{
			ID: id, Provider: "openrouter", Match: m, ValidFrom: from,
			Spec:      pricing.Spec{Prices: pricing.Wire{Input: "1", Output: "1"}},
			Tokenizer: "bytes", Ratio: "3.0", MaxOutput: 1000,
		}.Resolve(true)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	migrated := mk("migrated", pricing.Match{Regex: `(?-i:deepseek/flash)`}, "2026-01-01")
	mine := mk("mine", pricing.Match{Glob: "deepseek/flash"}, "2026-06-01")
	if migrated.Specificity != mine.Specificity {
		t.Errorf("specificity: regex %d, glob %d; inline flags are not literal characters", migrated.Specificity, mine.Specificity)
	}
	got, ok := pricing.Select([]pricing.Resolved{migrated, mine}, "openrouter", "deepseek/flash", time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))
	if !ok || got.Entry.ID != "mine" {
		t.Errorf("got %v %v, want the later exact glob", got.Entry.ID, ok)
	}
}

func TestSources(t *testing.T) {
	l, rs, err := pricing.ParseList([]byte(`version: "2000-01-01.1"
entries:
  - id: a
    provider: x
    match: {glob: a}
    valid_from: 2000-01-01
    prices: {input: "1", output: "2"}
    tokenizer: bytes
    ratio: "3.0"
    max_output: 10
    source: {url: "https://example.com", accessed: 2000-01-01}
    note: test
`))
	if err != nil {
		t.Fatal(err)
	}
	if got, gotRs := pricing.Static(l, rs).Current(); got.Version != "2000-01-01.1" || len(gotRs) != 1 {
		t.Fatalf("static source: %s, %d entries", got.Version, len(gotRs))
	}
	def, _ := pricing.Default()
	if got, _ := pricing.Embedded().Current(); got.Version != def.Version {
		t.Fatalf("embedded source answers %s, the default list is %s", got.Version, def.Version)
	}
}
