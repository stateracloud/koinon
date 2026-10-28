// SPDX-License-Identifier: Apache-2.0

package prices

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/pricing"
)

// CacheMode says what Koinon must do so that a model's provider caches prompts (ADR 0068). It is
// the one place that maps a provider's format to that.
type CacheMode string

const (
	// CacheNone: the price entry has no cache_read price, so the model is not known to cache.
	CacheNone CacheMode = ""
	// CacheAuto: the provider caches by itself (OpenAI, DeepSeek, Grok); nothing to send.
	CacheAuto CacheMode = "auto"
	// CacheAnthropic: the request needs Anthropic cache_control breakpoints.
	CacheAnthropic CacheMode = "anthropic"
	// CacheAlibaba: the request needs Alibaba (Qwen) cache_control breakpoints.
	CacheAlibaba CacheMode = "alibaba"
	// CacheUnknown: the model has a cache_read price but its provider format is not mapped here.
	// Add its mode to cacheModeOf before listing the model with cache prices.
	CacheUnknown CacheMode = "unknown"
)

// Marker is the cache-marker format a flavor harness must be told to send, or "" when none.
func (m CacheMode) Marker() string {
	if m == CacheAnthropic || m == CacheAlibaba {
		return string(m)
	}
	return ""
}

// CacheModeFor is the cache mode of a resolved model: CacheNone without a cache_read price,
// otherwise the mode of its provider format.
func CacheModeFor(providerType, model string, p pricing.Price) CacheMode {
	if p.Base.CacheRead == nil {
		return CacheNone
	}
	return cacheModeOf(providerType, model)
}

// cacheModeOf maps a provider format to its cache mode. OpenRouter routes by the vendor prefix
// of the model name.
func cacheModeOf(providerType, model string) CacheMode {
	switch providerType {
	case "anthropic":
		return CacheAnthropic
	case "openai", "deepseek":
		return CacheAuto
	case "openrouter":
		vendor, _, _ := strings.Cut(model, "/")
		switch vendor {
		case "anthropic":
			return CacheAnthropic
		case "qwen":
			return CacheAlibaba
		case "openai", "deepseek", "x-ai":
			return CacheAuto
		}
	}
	return CacheUnknown
}

// UncachedReason is the text recorded and shown when a model with a cache price runs without
// prompt caching because its provider format has no cache mode.
func UncachedReason(providerType, model string) string {
	return "the model has a cache_read price but Koinon has no way to turn on prompt caching for " +
		"provider format \"" + providerType + "\" and model \"" + model + "\"; it runs without caching (ADR 0068)"
}

// EntryWarnings are the save-time warnings for a price entry: it has a cache_read price (in its
// base rates or a tier) but the provider format it names is not mapped to a cache mode.
func EntryWarnings(e pricing.Entry) []string {
	if e.Prices.CacheRead == "" && !slices.ContainsFunc(e.Tiers, func(t pricing.WireTier) bool { return t.Prices.CacheRead != "" }) {
		return nil
	}
	name := e.Match.Glob
	if name == "" {
		name = e.Match.Regex
	}
	// A glob such as "google/*" names a vendor; probe with its literal prefix.
	if cacheModeOf(e.Provider, strings.TrimRight(name, "*?")) != CacheUnknown {
		return nil
	}
	return []string{"Price " + e.ID + ": " + UncachedReason(e.Provider, name)}
}

// ModelWarnings are the save-time warnings for a model: its resolved price has a cache_read
// price but its provider format is not mapped. A model without a price has none.
func (s *Service) ModelWarnings(ctx context.Context, org pgtype.UUID, providerID pgtype.UUID, name string) []string {
	prov, err := store.New(s.pool).GetProvider(ctx, store.GetProviderParams{OrgID: org, ID: providerID})
	if err != nil {
		return nil
	}
	res, err := Resolve(ctx, store.New(s.pool), org, Model{Name: name, ProviderType: prov.Type}, time.Now())
	if err != nil {
		return nil
	}
	if CacheModeFor(prov.Type, name, res.Price) != CacheUnknown {
		return nil
	}
	return []string{"Model " + name + ": " + UncachedReason(prov.Type, name)}
}
