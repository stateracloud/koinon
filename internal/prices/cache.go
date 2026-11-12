// SPDX-License-Identifier: Apache-2.0

package prices

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/pricing"
)

// CacheMode says what Koinon must do so that a model's provider caches prompts (ADR 0068, 0069).
// It is the one place that maps a model's wire format and provider to that.
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
	// CacheNative: the model speaks a format with its own cache markers (Anthropic Messages) and the
	// harness places them itself, so nothing is configured and Marker is "" (ADR 0069 decision 6).
	CacheNative CacheMode = "native"
	// CacheUnknown: the model has a cache_read price but its format and provider are not mapped
	// here. Add its mode to cacheModeOf before listing the model with cache prices.
	CacheUnknown CacheMode = "unknown"
)

// Marker is the cache-marker format a harness must be told to send, or "" when none.
func (m CacheMode) Marker() string {
	if m == CacheAnthropic || m == CacheAlibaba {
		return string(m)
	}
	return ""
}

// CacheModeFor is the cache mode of a resolved model: CacheNone without a cache_read price,
// otherwise the mode of its format and provider.
func CacheModeFor(providerType, model string, format modelformat.Format, p pricing.Price) CacheMode {
	if p.Base.CacheRead == nil {
		return CacheNone
	}
	return cacheModeOf(providerType, model, format)
}

// cacheModeOf maps a model to its cache mode. The wire format decides first: a native format
// carries its own cache markers whoever serves it. Chat completions then depend on the provider
// (ADR 0068); OpenRouter routes by the vendor prefix of the model name, so an OpenRouter Claude
// model that is still on chat completions (ADR 0069 decision 4) keeps Anthropic markers.
func cacheModeOf(providerType, model string, format modelformat.Format) CacheMode {
	if format == modelformat.Anthropic {
		return CacheNative
	}
	switch providerType {
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
// prompt caching because Koinon has no cache mode for it.
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
	// A glob such as "google/*" names a vendor; probe with its literal prefix and the format such a
	// model gets by default.
	prefix := literalPrefix(name)
	if !isUncached(cacheModeOf(e.Provider, prefix, modelformat.Default(e.Provider, prefix))) {
		return nil
	}
	return []string{"Price " + e.ID + ": " + UncachedReason(e.Provider, name)}
}

// ModelWarnings are the save-time warnings for a model: its resolved price has a cache_read
// price but its provider format is not mapped. A model without a price has none.
func (s *Service) ModelWarnings(ctx context.Context, org pgtype.UUID, providerID pgtype.UUID, name string, format modelformat.Format) []string {
	prov, err := store.New(s.pool).GetProvider(ctx, store.GetProviderParams{OrgID: org, ID: providerID})
	if err != nil {
		return nil
	}
	res, err := s.res.Resolve(ctx, store.New(s.pool), org, Model{Name: name, ProviderType: prov.Type, ProviderURL: prov.Url, Format: format}, time.Now())
	if err != nil {
		return nil
	}
	if !isUncached(CacheModeFor(prov.Type, name, format, res.Price)) {
		return nil
	}
	return []string{"Model " + name + ": " + UncachedReason(prov.Type, name)}
}

// isUncached is true for the modes in which a model with a cache price still runs uncached.
func isUncached(m CacheMode) bool { return m == CacheUnknown }

// Uncached reports whether a model with a cache price runs without caching (see Marker).
func (m CacheMode) Uncached() bool { return isUncached(m) }

// literalPrefix is the part of a glob or regex pattern before its first wildcard or regex
// metacharacter, without a leading "^": "google/*" gives "google/", "^anthropic/claude-.*" gives
// "anthropic/claude-".
func literalPrefix(pattern string) string {
	pattern = strings.TrimPrefix(pattern, "^")
	if i := strings.IndexAny(pattern, `*?.+()[]{}|\\$^`); i >= 0 {
		return pattern[:i]
	}
	return pattern
}
