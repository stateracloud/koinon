// SPDX-License-Identifier: Apache-2.0

package pricefetch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/stateracloud/koinon/internal/money"
	"github.com/stateracloud/koinon/pricing"
)

// OpenRouterURL is the one URL the OpenRouter adapter asks: the public model list, unauthenticated
// (https://openrouter.ai/docs/api/api-reference/models/get-models). Each install fetches it for
// itself; Koinon ships none of its data (ADR 0098 decision 7).
const OpenRouterURL = "https://openrouter.ai/api/v1/models"

// openRouterHost is the pinned host.
const openRouterHost = "openrouter.ai"

// OpenRouterMinModels is the model-count floor: the list had 457 models on 2026-11-06, so a body
// with fewer than this is a broken answer, not a catalogue.
const OpenRouterMinModels = 50

// TierBoundaryOffset converts OpenRouter's pricing.overrides[].min_prompt_tokens to Koinon's
// context_over. Koinon bills a tier when the input side is greater than context_over (pricing.Price.Effective).
// OpenRouter documents min_prompt_tokens as the smallest prompt size at which an override applies,
// which reads "at least", but ADR 0098 open question 1 records that no test against a live
// response has confirmed it. With "at least", a prompt of exactly min_prompt_tokens is in the
// tier, so context_over is min_prompt_tokens - 1. If the boundary turns out to be "more than",
// change this one constant to 0 and the test that pins it; nothing else depends on the choice.
const TierBoundaryOffset = -1

// OpenRouter is the adapter for OpenRouter's model list. Build it with NewOpenRouter.
type OpenRouter struct {
	client   *http.Client
	endpoint string
	host     string
	minModel int
}

// NewOpenRouter returns the adapter with the pinned endpoint. client may be nil (a 30 second
// timeout); redirects are never followed whatever client is passed.
func NewOpenRouter(client *http.Client) *OpenRouter {
	return &OpenRouter{client: client, endpoint: OpenRouterURL, host: openRouterHost, minModel: OpenRouterMinModels}
}

// Name implements Adapter.
func (*OpenRouter) Name() string { return SourceOpenRouter }

// MinModels is the model-count floor of this adapter.
func (o *OpenRouter) MinModels() int { return o.minModel }

// checkEndpoint holds a request to HTTPS and the pinned host, with no user info and no other port.
func checkEndpoint(raw, host string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "https" {
		return fmt.Errorf("pricefetch: %s must be fetched over https", raw)
	}
	if !strings.EqualFold(u.Hostname(), host) || u.User != nil {
		return fmt.Errorf("pricefetch: %s is not on the pinned host %s", raw, host)
	}
	return nil
}

// Fetch implements Adapter.
func (o *OpenRouter) Fetch(ctx context.Context, now time.Time) (Snapshot, error) {
	if err := checkEndpoint(o.endpoint, o.host); err != nil {
		return Snapshot{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.endpoint, nil)
	if err != nil {
		return Snapshot{}, err
	}
	// Unauthenticated: no Authorization header, no secret (ADR 0098 decision 2.3).
	req.Header.Set("Accept", "application/json")
	client := http.Client{Timeout: 30 * time.Second}
	if o.client != nil {
		client = *o.client
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return Snapshot{}, fmt.Errorf("pricefetch: openrouter: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return Snapshot{}, fmt.Errorf("pricefetch: openrouter redirected (%d); not followed", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return Snapshot{}, fmt.Errorf("pricefetch: openrouter answered %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBodyBytes+1))
	if err != nil {
		return Snapshot{}, fmt.Errorf("pricefetch: openrouter: read: %w", err)
	}
	if len(body) > MaxBodyBytes {
		return Snapshot{}, errors.New("pricefetch: openrouter: the body is larger than 16 MiB")
	}
	snap, err := ParseOpenRouter(body, now)
	if err != nil {
		return Snapshot{}, err
	}
	snap.URL = o.endpoint
	snap.ETag = resp.Header.Get("ETag")
	return snap, nil
}

// orList is the list response. Unknown fields are ignored on purpose: OpenRouter adds fields to
// models without notice, and only the price fields matter. Everything Koinon reads is typed and
// checked below.
type orList struct {
	Data []orModel `json:"data"`
}

type orModel struct {
	ID          string                     `json:"id"`
	Pricing     map[string]json.RawMessage `json:"pricing"`
	TopProvider *struct {
		MaxCompletionTokens json.RawMessage `json:"max_completion_tokens"`
	} `json:"top_provider"`
}

// ParseOpenRouter decodes and normalizes a /api/v1/models body. The body must be one JSON object
// with a data array; a model with an unusable price becomes an offer with Problems, not a failed
// fetch, so one broken model never hides the rest.
func ParseOpenRouter(body []byte, now time.Time) (Snapshot, error) {
	d := json.NewDecoder(bytes.NewReader(body))
	var l orList
	if err := d.Decode(&l); err != nil {
		return Snapshot{}, fmt.Errorf("pricefetch: openrouter: decode: %w", err)
	}
	if d.More() {
		return Snapshot{}, errors.New("pricefetch: openrouter: trailing data after the list")
	}
	if l.Data == nil {
		return Snapshot{}, errors.New("pricefetch: openrouter: the body has no data array")
	}
	snap := Snapshot{Source: SourceOpenRouter, URL: OpenRouterURL, Licence: LicenceOpenRouter, FetchedAt: now.UTC(), SHA256: DigestBody(body), Count: len(l.Data)}
	seen := map[string]bool{}
	for _, m := range l.Data {
		if !modelNameRE.MatchString(m.ID) {
			continue // a name Koinon cannot use as a model name
		}
		if seen[m.ID] {
			return Snapshot{}, fmt.Errorf("pricefetch: openrouter: model %q listed twice", m.ID)
		}
		seen[m.ID] = true
		snap.Offers = append(snap.Offers, normalizeOpenRouter(m))
	}
	sort.Slice(snap.Offers, func(i, j int) bool { return snap.Offers[i].Model < snap.Offers[j].Model })
	return snap, nil
}

// modelNameRE is the model name rule of the catalog (models.name), without glob characters.
var modelNameRE = regexp.MustCompile(`^[A-Za-z0-9_.:/@-]{1,256}$`)

// decimalRE is a non-negative decimal as OpenRouter writes per-token prices, with an optional exponent.
var decimalRE = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?([eE][-+]?[0-9]{1,3})?$`)

// perMillion converts a per-token decimal string to USD per million tokens, exactly. The value
// must fit Koinon's six fraction digits; rounding would change a price, so it is refused.
func perMillion(s string) (string, error) {
	if !decimalRE.MatchString(s) {
		return "", fmt.Errorf("%q is not a non-negative decimal", s)
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return "", fmt.Errorf("%q is not a number", s)
	}
	// per token * 10^6 = per million; * 10^6 again makes the six fraction digits whole numbers.
	scaled := new(big.Rat).Mul(r, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(12), nil)))
	if !scaled.IsInt() {
		return "", fmt.Errorf("%q per token has more than six fraction digits per million tokens", s)
	}
	n := scaled.Num()
	million := big.NewInt(1_000_000)
	whole, frac := new(big.Int).QuoRem(n, million, new(big.Int))
	out := whole.String()
	if f := strings.TrimRight(fmt.Sprintf("%06d", frac.Int64()), "0"); f != "" {
		out += "." + f
	}
	if _, err := money.ParseRate(out); err != nil {
		return "", err
	}
	return out, nil
}

// rateSet reads the price fields of a pricing object (or of an override) into a pricing.Wire.
// Per-token prices of the buckets Koinon bills: prompt (input), completion (output),
// input_cache_read, input_cache_write (the 5-minute write), internal_reasoning. A reasoning price
// is a rate only when it is positive and differs from the output price (ADR 0098 decision 8): "0"
// means no separate charge, as it does for most models. A negative price (OpenRouter's -1 for a
// router model) or an unreadable one is a problem. Fields not billed by Koinon go to ignored.
func rateSet(raw map[string]json.RawMessage, ignored map[string]string, prefix string) (w pricing.Wire, problems []string) {
	str := func(key string) (string, bool) {
		v, ok := raw[key]
		if !ok || string(v) == "null" {
			return "", false
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			problems = append(problems, fmt.Sprintf("%s%s: not a string", prefix, key))
			return "", false
		}
		return s, true
	}
	conv := func(key string) string {
		s, ok := str(key)
		if !ok {
			return ""
		}
		if strings.HasPrefix(strings.TrimSpace(s), "-") {
			problems = append(problems, fmt.Sprintf("%s%s: negative price %q (a variable price Koinon cannot bill)", prefix, key, s))
			return ""
		}
		v, err := perMillion(s)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s%s: %v", prefix, key, err))
			return ""
		}
		return v
	}
	w.Input, w.Output = conv("prompt"), conv("completion")
	w.CacheRead, w.CacheWrite = conv("input_cache_read"), conv("input_cache_write")
	if r := conv("internal_reasoning"); r != "" && r != "0" && r != w.Output {
		w.Reasoning = r
	}
	// Kept for the provenance only (decision 8): everything that is a price but not one of the five
	// buckets, such as the 1-hour cache write, per-request and per-image prices.
	for k, v := range raw {
		switch k {
		case "prompt", "completion", "input_cache_read", "input_cache_write", "internal_reasoning", "overrides":
		default:
			ignored[prefix+k] = strings.Trim(string(v), `"`)
		}
	}
	return w, problems
}

func normalizeOpenRouter(m orModel) Offer {
	o := Offer{Model: m.ID, ProviderType: "openrouter", Ignored: map[string]string{}}
	defer func() {
		if len(o.Ignored) == 0 {
			o.Ignored = nil
		}
	}()
	if m.Pricing == nil {
		o.Problems = []string{"the model has no pricing object"}
		return o
	}
	w, probs := rateSet(m.Pricing, o.Ignored, "")
	o.Spec.Prices = w
	o.Problems = append(o.Problems, probs...)
	if w.Input == "" || w.Output == "" {
		o.Problems = append(o.Problems, "prompt and completion prices are required")
	}
	if m.TopProvider != nil && len(m.TopProvider.MaxCompletionTokens) > 0 && string(m.TopProvider.MaxCompletionTokens) != "null" {
		var n int64
		if err := json.Unmarshal(m.TopProvider.MaxCompletionTokens, &n); err != nil || n < 0 || n > 10_000_000 {
			o.Problems = append(o.Problems, "top_provider.max_completion_tokens is not a count")
		} else {
			o.MaxOutput = int(n)
		}
	}
	if raw, ok := m.Pricing["overrides"]; ok && string(raw) != "null" {
		var ovs []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &ovs); err != nil {
			o.Problems = append(o.Problems, "pricing.overrides is not a list of objects")
			return o
		}
		for i, ov := range ovs {
			tier, ok, probs := overrideTier(ov, o.Ignored, i)
			o.Problems = append(o.Problems, probs...)
			if ok {
				o.Spec.Tiers = append(o.Spec.Tiers, tier)
			}
		}
		sort.Slice(o.Spec.Tiers, func(i, j int) bool { return o.Spec.Tiers[i].ContextOver < o.Spec.Tiers[j].ContextOver })
		for i := 1; i < len(o.Spec.Tiers); i++ {
			if o.Spec.Tiers[i].ContextOver == o.Spec.Tiers[i-1].ContextOver {
				o.Problems = append(o.Problems, "two overrides start at the same prompt size")
			}
		}
	}
	return o
}

// timeOfDayKeys mark an override as a time-of-day price, which Koinon cannot bill (decision 2.7).
var timeOfDayKeys = []string{"utc_days", "utc_start", "utc_end"}

// overrideTier converts one pricing.overrides entry to a tier. Only a prompt-size override
// becomes a tier; time-of-day, batch, flex and priority overrides, and overrides with no prompt
// size, are kept in the provenance and never imported.
func overrideTier(ov map[string]json.RawMessage, ignored map[string]string, i int) (pricing.WireTier, bool, []string) {
	prefix := fmt.Sprintf("overrides[%d].", i)
	for _, k := range timeOfDayKeys {
		if _, ok := ov[k]; ok {
			ignored[prefix+"skipped"] = "time-of-day override"
			return pricing.WireTier{}, false, nil
		}
	}
	for _, k := range []string{"batch", "flex", "priority", "service_tier"} {
		if _, ok := ov[k]; ok {
			ignored[prefix+"skipped"] = k + " override"
			return pricing.WireTier{}, false, nil
		}
	}
	rawMin, ok := ov["min_prompt_tokens"]
	if !ok {
		ignored[prefix+"skipped"] = "no min_prompt_tokens"
		return pricing.WireTier{}, false, nil
	}
	var minTokens int64
	if err := json.Unmarshal(rawMin, &minTokens); err != nil || minTokens < 1 || minTokens > 1<<40 {
		return pricing.WireTier{}, false, []string{prefix + "min_prompt_tokens is not a positive count"}
	}
	over := minTokens + TierBoundaryOffset
	if over < 1 {
		return pricing.WireTier{}, false, []string{prefix + "min_prompt_tokens leaves no tier boundary"}
	}
	rates := map[string]json.RawMessage{}
	for k, v := range ov {
		if k != "min_prompt_tokens" {
			rates[k] = v
		}
	}
	w, probs := rateSet(rates, ignored, prefix)
	if w == (pricing.Wire{}) && len(probs) == 0 {
		ignored[prefix+"skipped"] = "sets no billed rate"
		return pricing.WireTier{}, false, nil
	}
	return pricing.WireTier{ContextOver: over, Prices: w}, len(probs) == 0, probs
}
