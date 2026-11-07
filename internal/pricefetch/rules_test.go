// SPDX-License-Identifier: Apache-2.0

package pricefetch

import (
	"testing"
	"time"

	"github.com/stateracloud/koinon/pricing"
)

func entry(id, in, out, cacheRead string) pricing.Entry {
	return pricing.Entry{
		ID: id, Provider: "openrouter", Match: pricing.Match{Glob: "fake/model-a"}, ValidFrom: "2000-01-01",
		Spec:      pricing.Spec{Prices: pricing.Wire{Input: in, Output: out, CacheRead: cacheRead}},
		Tokenizer: "bytes", Ratio: "3.0", MaxOutput: 1000, Source: pricing.Source{URL: "https://example.com", Accessed: "2000-01-01"},
	}
}

func TestClassify(t *testing.T) {
	cur := entry("openrouter-fake-model-a", "1", "2", "")
	for _, c := range []struct {
		name     string
		prop     pricing.Entry
		current  *pricing.Entry
		baseline *pricing.Spec
		origin   string
		stable   bool
		agreeing []string
		want     Class
	}{
		{"small stable change", entry(cur.ID, "1.1", "2.2", ""), &cur, nil, "fetch", true, []string{SourceOpenRouter}, ClassAuto},
		{"exactly 25 percent", entry(cur.ID, "1.25", "2.5", ""), &cur, nil, "fetch", true, []string{SourceOpenRouter}, ClassAuto},
		{"just over 25 percent", entry(cur.ID, "1.26", "2", ""), &cur, nil, "fetch", true, []string{SourceOpenRouter}, ClassNeedsReview},
		{"first sighting", entry(cur.ID, "1.1", "2.2", ""), &cur, nil, "fetch", false, []string{SourceOpenRouter}, ClassNeedsReview},
		{"import is never auto", entry(cur.ID, "1.1", "2.2", ""), &cur, nil, "import", true, []string{SourceOpenRouter}, ClassNeedsReview},
		{"a new priced bucket", entry(cur.ID, "1", "2", "0.1"), &cur, nil, "fetch", true, []string{SourceOpenRouter}, ClassNeedsReview},
		{"no current price", entry(cur.ID, "1", "2", ""), nil, nil, "fetch", true, []string{SourceOpenRouter}, ClassNeedsReview},
		{"another entry id", entry("other", "1.1", "2.2", ""), &cur, nil, "fetch", true, []string{SourceOpenRouter}, ClassNeedsReview},
		{"over 3x", entry(cur.ID, "4", "8", ""), &cur, nil, "fetch", true, []string{SourceOpenRouter}, ClassConfirm},
		{"under a third", entry(cur.ID, "0.3", "0.6", ""), &cur, nil, "fetch", true, []string{SourceOpenRouter}, ClassConfirm},
		{"unit mix-up per token", entry(cur.ID, "0.000001", "0.000002", ""), &cur, nil, "fetch", true, []string{SourceOpenRouter}, ClassConfirm},
		{"unit mix-up per thousand", entry(cur.ID, "1000", "2000", ""), &cur, nil, "fetch", true, []string{SourceOpenRouter}, ClassConfirm},
		{"above the sanity limit", entry(cur.ID, "1", "1001", ""), &cur, nil, "fetch", true, []string{SourceOpenRouter}, ClassConfirm},
		{"zeroing", entry(cur.ID, "0", "2", ""), &cur, nil, "fetch", true, []string{SourceOpenRouter}, ClassConfirm},
		{"not the provider's own source", entry(cur.ID, "1.1", "2.2", ""), &cur, nil, "fetch", true, []string{"models-dev"}, ClassNeedsReview},
	} {
		src := SourceOpenRouter
		if len(c.agreeing) == 1 {
			src = c.agreeing[0]
		}
		v := Classify(Facts{Origin: c.origin, Source: src, Current: c.current, Baseline: c.baseline, Proposed: c.prop, Agreeing: c.agreeing, Stable: c.stable}, DefaultBounds)
		if v.Class != c.want {
			t.Errorf("%s: class %s, want %s (rules: %v)", c.name, v.Class, c.want, v.Failed)
		}
	}
}

// TestClassifyBaseline: small steps cannot add up past the bound, because every step is measured
// against the last person-reviewed price.
func TestClassifyBaseline(t *testing.T) {
	reviewed := pricing.Spec{Prices: pricing.Wire{Input: "1", Output: "2"}}
	cur := entry("openrouter-fake-model-a", "1.2", "2.4", "") // an earlier automatic +20%
	next := entry(cur.ID, "1.3", "2.6", "")                   // +8% of cur, but +30% of the reviewed price
	v := Classify(Facts{Origin: "fetch", Source: SourceOpenRouter, Current: &cur, Baseline: &reviewed, Proposed: next, Agreeing: []string{SourceOpenRouter}, Stable: true}, DefaultBounds)
	if v.Class != ClassNeedsReview {
		t.Errorf("class %s, want needs_review: %v", v.Class, v.Failed)
	}
	if v := Classify(Facts{Origin: "fetch", Source: SourceOpenRouter, Current: &cur, Proposed: next, Agreeing: []string{SourceOpenRouter}, Stable: true}, DefaultBounds); v.Class != ClassAuto {
		t.Errorf("without a baseline the move is measured against the current price: %s %v", v.Class, v.Failed)
	}
}

func TestBoundsValidate(t *testing.T) {
	if err := DefaultBounds.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, b := range []Bounds{{MaxChangePercent: 26, MaxAutoPerDay: 10}, {MaxChangePercent: 25, MaxAutoPerDay: 11}, {MaxChangePercent: 0, MaxAutoPerDay: 10}, {MaxChangePercent: 25, MaxAutoPerDay: 0}} {
		if err := b.Validate(); err == nil {
			t.Errorf("%+v must be refused: a bound can only be tightened", b)
		}
	}
	if err := (Bounds{MaxChangePercent: 10, MaxAutoPerDay: 3}).Validate(); err != nil {
		t.Errorf("tightening is allowed: %v", err)
	}
}

func TestCapAllows(t *testing.T) {
	if !CapAllows(0, 10, 10) || CapAllows(0, 11, 10) || CapAllows(5, 6, 10) || !CapAllows(5, 5, 10) {
		t.Error("the cap is on applied today plus this run's candidates")
	}
}

func TestEntryIDAndBuildEntry(t *testing.T) {
	if got := EntryID("openrouter", "DeepSeek/V4.1-Flash"); got != "openrouter-deepseek-v4-1-flash" {
		t.Errorf("EntryID = %q", got)
	}
	o := Offer{Model: "fake/model-a", ProviderType: "openrouter", Spec: pricing.Spec{Prices: pricing.Wire{Input: "1.1", Output: "2.2"}}, MaxOutput: 5000}
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	e, err := BuildEntry(o, nil, SourceOpenRouter, now)
	if err != nil {
		t.Fatal(err)
	}
	if e.ID != EntryID("openrouter", "fake/model-a") || e.Match.Glob != "fake/model-a" || e.Source.URL == "" {
		t.Errorf("new entry: %+v", e)
	}
	// An entry that covers other models too (a glob) is not reused: the proposal is its own entry.
	wide := entry("fake-wildcard", "9", "9", "")
	wide.Match = pricing.Match{Glob: "fake/*"}
	e, err = BuildEntry(o, &wide, SourceOpenRouter, now)
	if err != nil || e.ID == "fake-wildcard" {
		t.Errorf("a glob entry must not be overwritten: %+v, %v", e, err)
	}
	// An offer with problems cannot become an entry.
	if _, err := BuildEntry(Offer{Model: "fake/x", ProviderType: "openrouter", Problems: []string{"negative price"}}, nil, SourceOpenRouter, now); err == nil {
		t.Error("an offer with problems must be refused")
	}
}
