// SPDX-License-Identifier: Apache-2.0

package pricefetch

import (
	"errors"
	"fmt"
	"slices"
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
		{"no current price, stable", entry(cur.ID, "1", "2", ""), nil, nil, "fetch", true, []string{SourceOpenRouter}, ClassAuto},
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

// TestDiffReasoningAsBilled: a reasoning rate equal to the output rate and no reasoning rate bill
// the same, so neither side's spelling makes a change (the source drops a reasoning rate equal to
// output, separateReasoning); a reasoning rate that bills differently still does.
func TestDiffReasoningAsBilled(t *testing.T) {
	w := func(out, reasoning string) pricing.Wire {
		return pricing.Wire{Input: "1", Output: out, Reasoning: reasoning}
	}
	spec := func(base pricing.Wire, tiers ...pricing.WireTier) pricing.Spec {
		return pricing.Spec{Prices: base, Tiers: tiers}
	}
	tier := func(out, reasoning string) pricing.WireTier {
		return pricing.WireTier{ContextOver: 1000, Prices: pricing.Wire{Output: out, Reasoning: reasoning}}
	}
	for _, c := range []struct {
		name      string
		cur, prop pricing.Spec
		changes   []string // bucket@contextOver
	}{
		{"current spells reasoning = output, source drops it", spec(w("2", "2")), spec(w("2", "")), nil},
		{"current has none, source spells it out", spec(w("2", "")), spec(w("2", "2.0")), nil},
		{"both unset", spec(w("2", "")), spec(w("2", "")), nil},
		{"a separate reasoning rate appears", spec(w("2", "")), spec(w("2", "3")), []string{"reasoning@0"}},
		{"output moves, reasoning billed at output follows", spec(w("2", "2")), spec(w("2.2", "")), []string{"output@0", "reasoning@0"}},
		{"output moves, a separate reasoning rate stays", spec(w("2", "3")), spec(w("2.2", "3")), []string{"output@0"}},
		{"tier: reasoning = tier output, dropped", spec(w("2", ""), tier("4", "4")), spec(w("2", ""), tier("4", "")), nil},
		{"tier inherits the base's separate rate", spec(w("2", "3"), tier("4", "")), spec(w("2", "3"), tier("4", "")), nil},
		{"tier: base rate = output dropped, the tier bills at its own output", spec(w("2", "2"), tier("4", "")), spec(w("2", ""), tier("4", "")), []string{"reasoning@1000"}},
	} {
		changes, boundaries := Diff(c.cur, c.prop)
		var got []string
		for _, ch := range changes {
			got = append(got, fmt.Sprintf("%s@%d", ch.Bucket, ch.ContextOver))
		}
		if boundaries || !slices.Equal(got, c.changes) {
			t.Errorf("%s: changes %v (boundaries %v), want %v", c.name, got, boundaries, c.changes)
		}
		if Differs(c.cur, c.prop) != (len(c.changes) > 0) {
			t.Errorf("%s: Differs disagrees with Diff", c.name)
		}
	}
	// A current entry that spells reasoning = output is not "no longer priced" by a source that
	// drops it: a small stable change still applies by itself.
	cur := entry("openrouter-fake-model-a", "1", "2", "")
	cur.Prices.Reasoning = "2"
	facts := Facts{
		Origin: "fetch", Source: SourceOpenRouter, Current: &cur, Proposed: entry(cur.ID, "1.1", "2.2", ""),
		Agreeing: []string{SourceOpenRouter}, Stable: true,
	}
	v := Classify(facts, DefaultBounds)
	if v.Class != ClassAuto {
		t.Errorf("reasoning = output dropped: class %s, want auto (rules: %v)", v.Class, v.Failed)
	}
}

func tiered(e pricing.Entry, over int64, w pricing.Wire) pricing.Entry {
	e.Tiers = []pricing.WireTier{{ContextOver: over, Prices: w}}
	return e
}

// TestClassifyNewModel: a model with no Koinon price applies by itself once its values are stable,
// within every bound that needs no baseline (ADR 0098, amendment of 2026-11-07).
func TestClassifyNewModel(t *testing.T) {
	id := EntryID("openrouter", "fake/model-new")
	withReasoning := entry(id, "1", "2", "")
	withReasoning.Prices.Reasoning = "3"
	for _, c := range []struct {
		name     string
		prop     pricing.Entry
		origin   string
		stable   bool
		agreeing []string
		want     Class
	}{
		{"stable", entry(id, "1", "2", "0.1"), "fetch", true, []string{SourceOpenRouter}, ClassAuto},
		{"stable, a 10x price is no change bound", entry(id, "10", "80", "1"), "fetch", true, []string{SourceOpenRouter}, ClassAuto},
		{"stable, with a reasoning rate apart from output", withReasoning, "fetch", true, []string{SourceOpenRouter}, ClassAuto},
		{"stable, with a tier", tiered(entry(id, "1", "2", ""), 200000, pricing.Wire{Input: "2", Output: "4"}), "fetch", true, []string{SourceOpenRouter}, ClassAuto},
		{"one fetch or under 24 hours", entry(id, "1", "2", ""), "fetch", false, []string{SourceOpenRouter}, ClassNeedsReview},
		{"an import", entry(id, "1", "2", ""), "import", true, []string{SourceOpenRouter}, ClassNeedsReview},
		{"not the provider's own source", entry(id, "1", "2", ""), "fetch", true, []string{"models-dev"}, ClassNeedsReview},
		{"a rate above 1000", entry(id, "1", "1001", ""), "fetch", true, []string{SourceOpenRouter}, ClassConfirm},
		{"input per token, output per million", entry(id, "0.000001", "2", ""), "fetch", true, []string{SourceOpenRouter}, ClassConfirm},
		{"cache read per thousand", entry(id, "1", "2", "1000"), "fetch", true, []string{SourceOpenRouter}, ClassConfirm},
		{"a tier rate per token", tiered(entry(id, "1", "2", ""), 200000, pricing.Wire{Input: "0.000002"}), "fetch", true, []string{SourceOpenRouter}, ClassConfirm},
		{"a zero rate", entry(id, "0", "2", ""), "fetch", true, []string{SourceOpenRouter}, ClassNeedsReview},
		{"a free model", entry(id, "0", "0", ""), "fetch", true, []string{SourceOpenRouter}, ClassNeedsReview},
		{"a zero tier rate", tiered(entry(id, "1", "2", ""), 200000, pricing.Wire{Input: "0"}), "fetch", true, []string{SourceOpenRouter}, ClassNeedsReview},
	} {
		c.prop.Match.Glob = "fake/model-new"
		v := Classify(Facts{Origin: c.origin, Source: c.agreeing[0], Proposed: c.prop, Agreeing: c.agreeing, Stable: c.stable}, DefaultBounds)
		if v.Class != c.want {
			t.Errorf("%s: class %s, want %s (rules: %v)", c.name, v.Class, c.want, v.Failed)
		}
		if len(v.Changes) == 0 {
			t.Errorf("%s: every rate of a new model is a change", c.name)
		}
	}
}

// A new model the source gives no output limit for builds an entry, but BuildEntry reports
// ErrNoMaxOutput, which keeps it from auto (internal/prices sets needs_review for it).
func TestNewModelWithoutMaxOutput(t *testing.T) {
	o := Offer{Model: "fake/model-new", ProviderType: "openrouter", Spec: pricing.Spec{Prices: pricing.Wire{Input: "1", Output: "2"}}}
	e, err := BuildEntry(o, nil, SourceOpenRouter, time.Unix(0, 0))
	if !errors.Is(err, ErrNoMaxOutput) || e.ID == "" {
		t.Fatalf("BuildEntry = %+v, %v; want the entry and ErrNoMaxOutput", e, err)
	}
}

// A new model counts towards the daily cap like any other automatic change: with the day's ten
// already applied under the default cap, an eleventh, new or not, waits for a person.
func TestNewModelCountsTowardsCap(t *testing.T) {
	prop := entry(EntryID("openrouter", "fake/model-new"), "1", "2", "")
	if v := Classify(Facts{Origin: "fetch", Source: SourceOpenRouter, Proposed: prop, Agreeing: []string{SourceOpenRouter}, Stable: true}, DefaultBounds); v.Class != ClassAuto {
		t.Fatalf("class %s: %v", v.Class, v.Failed)
	}
	if CapAllows(DefaultMaxAutoPerDay, 1, DefaultBounds.MaxAutoPerDay) {
		t.Error("the 11th automatic change of the day must not apply under the default cap")
	}
	if !CapAllows(DefaultMaxAutoPerDay-1, 1, DefaultBounds.MaxAutoPerDay) {
		t.Error("the 10th automatic change of the day applies")
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
	// A hand-set cache_write_1h survives a proposal: the source does not import it.
	kept := entry("fake-model-a", "1", "2", "")
	kept.Match = pricing.Match{Glob: "fake/model-a"}
	kept.Provider = "openrouter"
	kept.Spec.Prices.CacheWrite1h = "6"
	if e, err = BuildEntry(o, &kept, SourceOpenRouter, now); err != nil || e.Spec.Prices.CacheWrite1h != "6" {
		t.Errorf("cache_write_1h must be kept: %+v, %v", e.Spec.Prices, err)
	}
	// An offer with problems cannot become an entry.
	if _, err := BuildEntry(Offer{Model: "fake/x", ProviderType: "openrouter", Problems: []string{"negative price"}}, nil, SourceOpenRouter, now); err == nil {
		t.Error("an offer with problems must be refused")
	}
}
