// SPDX-License-Identifier: Apache-2.0

// Package providerusage asks a model provider for its own record of one response: the usage and
// cost it billed. The spend ledger uses it to settle a response that was cut short and so carried
// no usage (ADR 0056, amendment 2026-10-28): the entry is charged the streamed output first and
// replaced by the provider's figure once the provider has it.
//
// A provider type has a [Source] when it publishes such a record per response. OpenRouter does
// (GET /api/v1/generation?id=); a provider without one keeps the streamed-usage entry as final.
package providerusage

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/stateracloud/koinon/internal/money"
)

// Usage is a provider's record of one response, in agentgateway's consistent semantics: input
// includes cache reads and writes, output includes reasoning.
type Usage struct {
	InputTokens, OutputTokens         int64
	CacheReadTokens, CacheWriteTokens int64
	ReasoningTokens                   int64
	// Cost is what the provider billed.
	Cost money.USD
	// Cancelled is the provider's own mark that the response was canceled before it ended
	// (OpenRouter's cancelled). It is recorded with the settlement; the figures stand either way.
	Cancelled bool
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
