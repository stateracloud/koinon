// SPDX-License-Identifier: Apache-2.0

// Package providerusage asks a model provider for its own record of what it billed. The spend
// ledger uses it to settle each response at the billed cost (ADR 0056, amendments 2026-10-28 and
// 2026-11-06): the entry is charged Koinon's price first and replaced by the provider's figure once
// the provider has it.
//
// A provider type has a [Source] when it publishes such a record per response. OpenRouter does
// (GET /api/v1/generation?id=); a provider without one keeps the logged entry as final.
//
// A direct provider type has a [CostSource] when its cost API reports a day's cost for part of an
// account (Anthropic's cost report per workspace, OpenAI's costs per project); the daily
// reconciliation compares that with the ledger and changes no entry.
package providerusage

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/stateracloud/koinon/internal/money"
	"github.com/stateracloud/koinon/pricing"
)

// Usage is a provider's record of one response, in agentgateway's consistent semantics: input
// includes cache reads and writes, output includes reasoning.
type Usage struct {
	InputTokens, OutputTokens         int64
	CacheReadTokens, CacheWriteTokens int64
	ReasoningTokens                   int64
	// CacheWrite1hTokens is the part of CacheWriteTokens written with a 1-hour TTL. No provider
	// record carries it today; settling fills it from the gateway's log (ledger.Settler).
	CacheWrite1hTokens int64
	// ReasoningKnown says the record reports the reasoning split (OpenRouter's
	// native_tokens_reasoning above 0, ADR 0069 amendment of 2026-11-06); without it
	// ReasoningTokens is 0 for want of a figure and settling keeps the gateway's split.
	ReasoningKnown bool
	// Cost is what the provider billed (OpenRouter's total_cost).
	Cost money.USD
	// UpstreamCost is what the upstream provider billed the org's own key for the response, when
	// the aggregator ran it with that key (OpenRouter BYOK: is_byok, upstream_inference_cost). Cost
	// is then only the aggregator's fee. Zero otherwise.
	UpstreamCost money.USD
	// BYOK is set when the response ran on the org's own upstream key at the aggregator.
	BYOK bool
	// Cancelled is the provider's own mark that the response was canceled before it ended
	// (OpenRouter's cancelled). It is recorded with the settlement; the figures stand either way.
	Cancelled bool
}

// Buckets is the usage in the disjoint buckets the price applies to (input without the cache
// parts, output without reasoning). A provider record reports totals, so the subtraction is this
// source's mapping, as the gateway driver's is for the usage record. Counts that do not add up
// clamp at zero.
func (u Usage) Buckets() pricing.Usage {
	cacheRead, cacheWrite, reasoning := max(u.CacheReadTokens, 0), max(u.CacheWriteTokens, 0), max(u.ReasoningTokens, 0)
	write1h := min(max(u.CacheWrite1hTokens, 0), cacheWrite)
	return pricing.Usage{
		Input:     max(u.InputTokens-cacheRead-cacheWrite, 0),
		CacheRead: cacheRead, CacheWrite: cacheWrite - write1h, CacheWrite1h: write1h,
		Output:    max(u.OutputTokens-reasoning, 0),
		Reasoning: reasoning,
	}
}

// Billed is what the response cost the org: the provider's cost plus, with the org's own upstream
// key (BYOK), what the upstream billed that key. The ledger charges it (ADR 0056, amendment
// 2026-11-06).
func (u Usage) Billed() (money.USD, error) {
	if u.BYOK {
		return u.Cost.Add(u.UpstreamCost)
	}
	return u.Cost, nil
}

// RetryAfterError is a provider refusing for now (HTTP 429): ask again no sooner than After.
type RetryAfterError struct {
	After time.Duration
}

func (e *RetryAfterError) Error() string {
	return fmt.Sprintf("providerusage: rate limited; retry after %s", e.After)
}

// Provider is where to ask and with which credential.
type Provider struct {
	// Type is the provider type (catalog), for example "openrouter".
	Type string
	// URL is the provider's OpenAI-compatible base URL, as stored on the provider.
	URL string
	// APIKey is the provider's key; it never leaves the request to the provider.
	APIKey string
}

// ErrPending means the provider has no record of the response yet; ask again later.
var ErrPending = errors.New("providerusage: the provider has no record of the response yet")

// Source looks up responses of one provider type.
type Source interface {
	// Serves reports whether a provider at providerURL (as stored on the provider) is this
	// source's service. A source asks only its own fixed endpoint, never the provider's URL, so a
	// provider record pointing elsewhere is not settled at all rather than sending its key there.
	Serves(providerURL string) bool
	// Accepts reports whether id is a response ID this source can look up.
	Accepts(id string) bool
	// Usage returns the provider's record of the response, ErrPending when it has none yet, or
	// another error (also worth retrying until the caller's deadline).
	Usage(ctx context.Context, p Provider, id string) (Usage, error)
}

// Registry maps provider types to their sources.
type Registry map[string]Source

// Default is the sources Koinon ships. client nil is NewClient().
func Default(client *http.Client) Registry {
	if client == nil {
		client = NewClient()
	}
	return Registry{"openrouter": OpenRouter{Client: client}}
}

// NewClient is the HTTP client sources use: a 15-second timeout and no redirects followed (a
// redirect would carry the provider key's request to another host).
func NewClient() *http.Client {
	return &http.Client{Timeout: 15 * time.Second, CheckRedirect: NoRedirects}
}

// NoRedirects is an http.Client CheckRedirect that never follows a redirect: the redirect response
// itself is returned, which a source treats as an error.
func NoRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// For returns the source for a response of providerType served at providerURL, if there is one
// that serves that URL and accepts id.
func (r Registry) For(providerType, providerURL, id string) (Source, bool) {
	s, ok := r[providerType]
	if !ok || id == "" || !s.Serves(providerURL) || !s.Accepts(id) {
		return nil, false
	}
	return s, true
}
