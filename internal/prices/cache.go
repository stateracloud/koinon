// SPDX-License-Identifier: Apache-2.0

package prices

import (
	"strings"

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
