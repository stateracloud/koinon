// SPDX-License-Identifier: Apache-2.0

package prices_test

import (
	"testing"

	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/pricing"
)

func TestEntryWarnings(t *testing.T) {
	entry := func(provider, glob, cacheRead string) pricing.Entry {
		return pricing.Entry{
			ID: "p", Provider: provider, Match: pricing.Match{Glob: glob},
			Spec: pricing.Spec{Prices: pricing.Wire{Input: "1", Output: "2", CacheRead: cacheRead}},
		}
	}
	for _, c := range []struct {
		name string
		e    pricing.Entry
		warn bool
	}{
		{"unmapped openrouter vendor", entry("openrouter", "google/*", "0.1"), true},
		{"any provider", entry("*", "x", "0.1"), true},
		{"no cache price", entry("openrouter", "google/*", ""), false},
		{"anthropic via openrouter", entry("openrouter", "anthropic/claude-*", "0.1"), false},
		{"qwen via openrouter", entry("openrouter", "qwen/*", "0.1"), false},
		{"openai", entry("openai", "gpt-*", "0.1"), false},
	} {
		got := prices.EntryWarnings(c.e)
		if (len(got) > 0) != c.warn {
			t.Errorf("%s: warnings = %v, want warning %v", c.name, got, c.warn)
		}
	}
	tiered := entry("openrouter", "google/*", "")
	tiered.Tiers = []pricing.WireTier{{ContextOver: 100, Prices: pricing.Wire{CacheRead: "0.2"}}}
	if len(prices.EntryWarnings(tiered)) == 0 {
		t.Error("a cache_read price in a tier is not warned about")
	}
}
