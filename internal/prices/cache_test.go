// SPDX-License-Identifier: Apache-2.0

package prices_test

import (
	"testing"

	"github.com/stateracloud/koinon/internal/modelformat"
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
		{"direct anthropic: Messages, native markers", entry("anthropic", "claude-*", "0.1"), false},
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

func TestCacheModeFor(t *testing.T) {
	withCache := func() pricing.Price {
		_, entries := pricing.Default()
		for _, r := range entries {
			if r.Price.Base.CacheRead != nil {
				return r.Price
			}
		}
		t.Fatal("no default entry has a cache_read price")
		return pricing.Price{}
	}
	for _, c := range []struct {
		name, provider, model string
		format                modelformat.Format
		want                  prices.CacheMode
	}{
		{"anthropic format is native", "anthropic", "claude-haiku-5-5", modelformat.Anthropic, prices.CacheNative},
		{"the format decides over the provider", "openrouter", "anthropic/claude-haiku-5.5", modelformat.Anthropic, prices.CacheNative},
		{"openrouter claude on chat completions keeps markers", "openrouter", "anthropic/claude-haiku-5.5", modelformat.OpenAI, prices.CacheAnthropic},
		{"openrouter qwen", "openrouter", "qwen/qwen4", modelformat.OpenAI, prices.CacheAlibaba},
		{"openai", "openai", "gpt-6-luna", modelformat.OpenAI, prices.CacheAuto},
		{"an unmapped provider", "newprovider", "m", modelformat.OpenAI, prices.CacheUnknown},
		{"chat completions on the anthropic provider is unmapped", "anthropic", "claude-haiku-5-5", modelformat.OpenAI, prices.CacheUnknown},
	} {
		if got := prices.CacheModeFor(c.provider, c.model, c.format, withCache()); got != c.want {
			t.Errorf("%s: mode = %q, want %q", c.name, got, c.want)
		}
		if got := prices.CacheModeFor(c.provider, c.model, c.format, pricing.Price{}); got != prices.CacheNone {
			t.Errorf("%s: without a cache_read price the mode = %q, want none", c.name, got)
		}
	}
	if m := prices.CacheNative; m.Marker() != "" || m.Uncached() {
		t.Errorf("native: marker %q, uncached %v", m.Marker(), m.Uncached())
	}
}
