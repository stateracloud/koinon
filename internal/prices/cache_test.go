// SPDX-License-Identifier: Apache-2.0

package prices_test

import (
	"strings"
	"testing"

	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/pricing"
)

func TestEntryWarnings(t *testing.T) {
	regexEntry := func(provider, re string) pricing.Entry {
		return pricing.Entry{
			ID: "p", Provider: provider, Match: pricing.Match{Regex: re},
			Spec: pricing.Spec{Prices: pricing.Wire{Input: "1", Output: "2", CacheRead: "0.1"}},
		}
	}
	if w := prices.EntryWarnings(regexEntry("openrouter", "^anthropic/claude-.*")); len(w) != 0 {
		t.Errorf("an anthropic regex warned: %v", w)
	}
	if w := prices.EntryWarnings(regexEntry("openrouter", "^google/.*")); len(w) != 1 {
		t.Errorf("an unmapped vendor regex did not warn: %v", w)
	}
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
		{"direct anthropic", entry("anthropic", "claude-*", "0.1"), true},
	} {
		got := prices.EntryWarnings(c.e)
		if (len(got) > 0) != c.warn {
			t.Errorf("%s: warnings = %v, want warning %v", c.name, got, c.warn)
		}
	}
	if w := prices.EntryWarnings(entry("anthropic", "claude-*", "0.1")); len(w) != 1 || !strings.Contains(w[0], "OpenAI-compatible endpoint does not cache; use OpenRouter") {
		t.Errorf("direct anthropic warning = %v", w)
	}
	tiered := entry("openrouter", "google/*", "")
	tiered.Tiers = []pricing.WireTier{{ContextOver: 100, Prices: pricing.Wire{CacheRead: "0.2"}}}
	if len(prices.EntryWarnings(tiered)) == 0 {
		t.Error("a cache_read price in a tier is not warned about")
	}
}
