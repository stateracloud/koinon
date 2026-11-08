// SPDX-License-Identifier: Apache-2.0

package prices_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stateracloud/koinon/internal/money"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/pricing"
)

func quoteFor(t *testing.T, s pricing.Spec) prices.Quote {
	t.Helper()
	p, err := s.Parse()
	if err != nil {
		t.Fatal(err)
	}
	return prices.Quote{Model: prices.Model{Name: "fake/m"}, Resolution: prices.Resolution{Ref: "default@t#p", Price: p}}
}

func usd(t *testing.T, s string) money.USD {
	t.Helper()
	v, err := money.ParseNumeric(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// Rates are USD per million tokens, so a million tokens of a bucket cost its rate.
func TestQuotePriceBuckets(t *testing.T) {
	qt := quoteFor(t, pricing.Spec{Prices: pricing.Wire{Input: "1", Output: "4", CacheRead: "0.1", CacheWrite: "1.25", Reasoning: "8"}})
	got, err := qt.Price(pricing.Usage{Input: 1_000_000, CacheRead: 1_000_000, CacheWrite: 1_000_000, Output: 1_000_000, Reasoning: 1_000_000})
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct{ got, want money.USD }{
		"input": {got.Breakdown.Input, usd(t, "1")}, "cache read": {got.Breakdown.CacheRead, usd(t, "0.1")},
		"cache write": {got.Breakdown.CacheWrite, usd(t, "1.25")}, "output": {got.Breakdown.Output, usd(t, "4")},
		"reasoning": {got.Breakdown.Reasoning, usd(t, "8")}, "total": {got.Cost, usd(t, "14.35")},
	} {
		if c.got != c.want {
			t.Errorf("%s: got %s, want %s", name, c.got, c.want)
		}
	}
}

// A cache bucket without its own rate is billed at the input rate; reasoning at the output rate.
func TestQuotePriceFallbackRates(t *testing.T) {
	qt := quoteFor(t, pricing.Spec{Prices: pricing.Wire{Input: "2", Output: "6"}})
	got, err := qt.Price(pricing.Usage{CacheRead: 1_000_000, CacheWrite: 1_000_000, Reasoning: 1_000_000})
	if err != nil {
		t.Fatal(err)
	}
	if got.Breakdown.CacheRead != usd(t, "2") || got.Breakdown.CacheWrite != usd(t, "2") || got.Breakdown.Reasoning != usd(t, "6") {
		t.Errorf("fallback rates: %+v", got.Breakdown)
	}
}

// The tier is chosen by the input side: input plus cache reads and writes.
func TestQuotePriceTier(t *testing.T) {
	qt := quoteFor(t, pricing.Spec{
		Prices: pricing.Wire{Input: "1", Output: "2"},
		Tiers:  []pricing.WireTier{{ContextOver: 200_000, Prices: pricing.Wire{Input: "3", Output: "6"}}},
	})
	base, err := qt.Price(pricing.Usage{Input: 100_000, Output: 1_000_000})
	if err != nil {
		t.Fatal(err)
	}
	if base.Breakdown.Output != usd(t, "2") {
		t.Errorf("below the tier: output %s", base.Breakdown.Output)
	}
	// 100k input + 150k cache reads exceeds the tier's 200k although neither bucket does alone.
	tiered, err := qt.Price(pricing.Usage{Input: 100_000, CacheRead: 150_000, Output: 1_000_000})
	if err != nil {
		t.Fatal(err)
	}
	if tiered.Breakdown.Output != usd(t, "6") {
		t.Errorf("above the tier: output %s", tiered.Breakdown.Output)
	}
}

func TestQuotePriceOverflowIsUnusable(t *testing.T) {
	qt := quoteFor(t, pricing.Spec{Prices: pricing.Wire{Input: "100000", Output: "1"}})
	_, err := qt.Price(pricing.Usage{Input: 1 << 60})
	reason, ok := prices.Unpriceable(err)
	if !ok || reason != prices.ReasonUnusablePrice {
		t.Fatalf("got %v (%q, %v), want an unusable price", err, reason, ok)
	}
}

func TestUnpriceable(t *testing.T) {
	for name, c := range map[string]struct {
		err    error
		reason prices.Reason
		ok     bool
	}{
		"no price":         {prices.ErrUnpriced, prices.ReasonNoPrice, true},
		"wrapped no price": {fmt.Errorf("x: %w", prices.ErrUnpriced), prices.ReasonNoPrice, true},
		"unusable":         {&prices.BadPriceError{Err: errors.New("rate")}, prices.ReasonUnusablePrice, true},
		"wrapped unusable": {fmt.Errorf("x: %w", &prices.BadPriceError{Err: errors.New("rate")}), prices.ReasonUnusablePrice, true},
		"a database error": {errors.New("connection refused"), "", false},
		"nil":              {nil, "", false},
	} {
		reason, ok := prices.Unpriceable(c.err)
		if reason != c.reason || ok != c.ok {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", name, reason, ok, c.reason, c.ok)
		}
	}
}
