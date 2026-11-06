// SPDX-License-Identifier: Apache-2.0

package providerusage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/stateracloud/koinon/internal/money"
)

// OpenRouter reads a generation's stats: GET https://openrouter.ai/api/v1/generation?id=<generation
// ID>, with the provider key as a bearer token (https://openrouter.ai/docs/api/api-reference/generations/get-generation,
// read 2026-10-28). The generation ID is the id of every chunk of a chat completion stream.
// total_cost is what OpenRouter billed; the native_* counts are the upstream's own tokens. With the
// org's own upstream key (is_byok), total_cost is OpenRouter's fee and upstream_inference_cost what
// the upstream billed that key.
// It serves only providers whose URL is https://openrouter.ai, and asks only its pinned endpoint.
type OpenRouter struct {
	// Client is the HTTP client (nil: NewClient's settings); redirects are never followed.
	Client *http.Client
}

// OpenRouterGenerationURL is the one URL OpenRouter's source asks.
const OpenRouterGenerationURL = "https://openrouter.ai/api/v1/generation"

// Serves implements Source: https and the host openrouter.ai, nothing else.
func (OpenRouter) Serves(providerURL string) bool {
	u, err := url.Parse(providerURL)
	return err == nil && u.Scheme == "https" && strings.EqualFold(u.Hostname(), "openrouter.ai") && u.User == nil &&
		(u.Port() == "" || u.Port() == "443")
}

// generationID is OpenRouter's documented pattern (at most 128 characters).
var generationID = regexp.MustCompile(`^gen-[0-9A-Za-z-]{1,124}$`)

// Accepts implements Source.
func (OpenRouter) Accepts(id string) bool { return generationID.MatchString(id) }

type generation struct {
	Data struct {
		TotalCost              json.Number `json:"total_cost"`
		TokensPrompt           *int64      `json:"tokens_prompt"`
		TokensCompletion       *int64      `json:"tokens_completion"`
		NativeTokensPrompt     *int64      `json:"native_tokens_prompt"`
		NativeTokensCompletion *int64      `json:"native_tokens_completion"`
		NativeTokensReasoning  *int64      `json:"native_tokens_reasoning"`
		NativeTokensCached     *int64      `json:"native_tokens_cached"`
		Cancelled              *bool       `json:"cancelled"`
		IsBYOK                 *bool       `json:"is_byok"`
		UpstreamInferenceCost  json.Number `json:"upstream_inference_cost"`
	} `json:"data"`
}

// Usage implements Source. 404 is ErrPending: the stats of a generation appear shortly after it
// ends, and the caller retries until its deadline.
func (o OpenRouter) Usage(ctx context.Context, p Provider, id string) (Usage, error) {
	if !o.Accepts(id) {
		return Usage{}, fmt.Errorf("providerusage: %q is not an OpenRouter generation ID", id)
	}
	if !o.Serves(p.URL) {
		return Usage{}, fmt.Errorf("providerusage: provider URL is not OpenRouter's")
	}
	u, err := url.Parse(OpenRouterGenerationURL)
	if err != nil {
		return Usage{}, err
	}
	u.RawQuery = url.Values{"id": {id}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Usage{}, err
	}
	if p.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	req.Header.Set("Accept", "application/json")
	// Never follow a redirect, whatever client was passed: it would take the key elsewhere.
	client := http.Client{Timeout: 15 * time.Second}
	if o.Client != nil {
		client = *o.Client
	}
	client.CheckRedirect = NoRedirects
	resp, err := client.Do(req)
	if err != nil {
		return Usage{}, fmt.Errorf("providerusage: openrouter: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Usage{}, fmt.Errorf("providerusage: openrouter: read: %w", err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return Usage{}, ErrPending
	case resp.StatusCode == http.StatusTooManyRequests:
		return Usage{}, &RetryAfterError{After: retryAfter(resp.Header.Get("Retry-After"), time.Now())}
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return Usage{}, fmt.Errorf("providerusage: openrouter redirected (%d); not followed", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		// The body may echo the request; only the status is reported.
		return Usage{}, fmt.Errorf("providerusage: openrouter answered %d", resp.StatusCode)
	}
	var g generation
	d := json.NewDecoder(strings.NewReader(string(body)))
	d.UseNumber()
	if err := d.Decode(&g); err != nil {
		return Usage{}, fmt.Errorf("providerusage: openrouter: decode: %w", err)
	}
	if g.Data.TotalCost == "" {
		return Usage{}, ErrPending
	}
	cost, err := usd("total_cost", g.Data.TotalCost)
	if err != nil {
		return Usage{}, err
	}
	byok := g.Data.IsBYOK != nil && *g.Data.IsBYOK
	var upstream money.USD
	if byok && g.Data.UpstreamInferenceCost != "" {
		if upstream, err = usd("upstream_inference_cost", g.Data.UpstreamInferenceCost); err != nil {
			return Usage{}, err
		}
	}
	first := func(vs ...*int64) int64 {
		for _, v := range vs {
			if v != nil && *v >= 0 {
				return *v
			}
		}
		return 0
	}
	out := Usage{
		// OpenRouter's prompt count includes cached tokens and its completion count includes
		// reasoning, as agentgateway's consistent semantics do.
		InputTokens:     first(g.Data.NativeTokensPrompt, g.Data.TokensPrompt),
		OutputTokens:    first(g.Data.NativeTokensCompletion, g.Data.TokensCompletion),
		CacheReadTokens: first(g.Data.NativeTokensCached),
		ReasoningTokens: first(g.Data.NativeTokensReasoning),
		ReasoningKnown:  g.Data.NativeTokensReasoning != nil && *g.Data.NativeTokensReasoning >= 0,
		Cost:            cost,
		UpstreamCost:    upstream,
		BYOK:            byok,
		Cancelled:       g.Data.Cancelled != nil && *g.Data.Cancelled,
	}
	out.CacheReadTokens = min(out.CacheReadTokens, out.InputTokens)
	out.ReasoningTokens = min(out.ReasoningTokens, out.OutputTokens)
	return out, nil
}

// usd reads a JSON number of dollars; a negative one is refused (money.FromRat).
func usd(field string, n json.Number) (money.USD, error) {
	r, ok := new(big.Rat).SetString(n.String())
	if !ok {
		return 0, fmt.Errorf("providerusage: openrouter: %s %q is not a number", field, n)
	}
	c, err := money.FromRat(r)
	if err != nil {
		return 0, fmt.Errorf("providerusage: openrouter: %s: %w", field, err)
	}
	return c, nil
}

// retryAfter reads a Retry-After header (seconds or an HTTP date), at least a second and at most
// an hour; a missing or unreadable one is a minute.
func retryAfter(v string, now time.Time) time.Duration {
	d := time.Minute
	if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
		d = time.Duration(secs) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = t.Sub(now)
	}
	return min(max(d, time.Second), time.Hour)
}
