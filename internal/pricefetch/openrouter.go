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
	"slices"
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
// context_over. Koinon bills a tier when the input side is greater than context_over
// (pricing.Price.Effective). OpenRouter's own SDK, npm package @openrouter/sdk 1.5.64, file
// esm/models/pricingoverride.d.ts (generated from OpenRouter's OpenAPI schema), documents
// PricingOverride.min_prompt_tokens as "Condition: the entry applies when the total prompt tokens
// of a request are strictly greater than this threshold". Both read "greater than", so
// context_over is min_prompt_tokens itself (ADR 0098 open question 1, resolved 2026-11-07). The
// same type documents how entries combine, which mergeTiers follows: "An entry applies only when
// all of its condition fields (e.g. min_prompt_tokens, or the utc_start/utc_end time window) match
// the request; among applicable entries, later entries win per price key; price keys absent from
// an entry inherit the base price."
const TierBoundaryOffset = 0

// OpenRouter is the adapter for OpenRouter's model list (and, for the per-install catalog, its
// per-model endpoints route). Build it with NewOpenRouter.
type OpenRouter struct {
	client   *http.Client
	endpoint string
	host     string
	// port is the pinned port; empty is 443, the only one in production (tests pin their server's).
	port     string
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

// checkEndpoint holds a request to HTTPS and the pinned host and port (empty: 443), with no user
// info.
func checkEndpoint(raw, host, port string) error {
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
	if port == "" {
		port = "443"
	}
	got := u.Port()
	if got == "" {
		got = "443" // the https default
	}
	if got != port {
		return fmt.Errorf("pricefetch: %s is not on the pinned port %s", raw, port)
	}
	return nil
}

// get does one unauthenticated GET of rawURL (held to https and the pinned host and port) and returns
// the body, capped at MaxBodyBytes, and the headers. Redirects are never followed.
func (o *OpenRouter) get(ctx context.Context, rawURL string) ([]byte, http.Header, error) {
	if err := checkEndpoint(rawURL, o.host, o.port); err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, nil, err
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
		return nil, nil, fmt.Errorf("pricefetch: openrouter: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return nil, nil, fmt.Errorf("pricefetch: openrouter redirected (%d); not followed", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return nil, nil, fmt.Errorf("pricefetch: openrouter answered %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBodyBytes+1))
	if err != nil {
		return nil, nil, fmt.Errorf("pricefetch: openrouter: read: %w", err)
	}
	if len(body) > MaxBodyBytes {
		return nil, nil, errors.New("pricefetch: openrouter: the body is larger than 16 MiB")
	}
	return body, resp.Header, nil
}

// Fetch implements Adapter.
func (o *OpenRouter) Fetch(ctx context.Context, now time.Time) (Snapshot, error) {
	body, hdr, err := o.get(ctx, o.endpoint)
	if err != nil {
		return Snapshot{}, err
	}
	snap, err := ParseOpenRouter(body, now)
	if err != nil {
		return Snapshot{}, err
	}
	snap.URL = o.endpoint
	snap.ETag = hdr.Get("ETag")
	return snap, nil
}

// FetchEndpoints implements EndpointsFetcher: GET /api/v1/models/{author}/{slug}/endpoints for one
// model of the list. The model id comes from the list this adapter fetched; each path segment is
// escaped, and the request is held to the same pinned host as the list.
func (o *OpenRouter) FetchEndpoints(ctx context.Context, model string, now time.Time) (ModelEndpoints, error) {
	if !modelNameRE.MatchString(model) {
		return ModelEndpoints{}, fmt.Errorf("pricefetch: openrouter: %q is not a model name", model)
	}
	segs := strings.Split(model, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	body, _, err := o.get(ctx, o.endpoint+"/"+strings.Join(segs, "/")+"/endpoints")
	if err != nil {
		return ModelEndpoints{}, err
	}
	return ParseOpenRouterEndpoints(model, body, now)
}

// orList is the list response. Unknown fields are ignored on purpose: OpenRouter adds fields to
// models without notice, and only the price fields matter. Everything Koinon reads is typed and
// checked below.
type orList struct {
	Data []orModel `json:"data"`
}

type orModel struct {
	ID            string                     `json:"id"`
	Name          string                     `json:"name"`
	Description   string                     `json:"description"`
	Created       json.RawMessage            `json:"created"`
	ContextLength json.RawMessage            `json:"context_length"`
	Pricing       map[string]json.RawMessage `json:"pricing"`
	Architecture  *struct {
		InputModalities  []string `json:"input_modalities"`
		OutputModalities []string `json:"output_modalities"`
		Tokenizer        string   `json:"tokenizer"`
	} `json:"architecture"`
	SupportedParameters []string `json:"supported_parameters"`
	TopProvider         *struct {
		ContextLength       json.RawMessage `json:"context_length"`
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
		snap.Meta = append(snap.Meta, metaOf(m))
	}
	sort.Slice(snap.Meta, func(i, j int) bool { return snap.Meta[i].Model < snap.Meta[j].Model })
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
// means no separate charge, as it does for most models; rateSet returns it raw and
// separateReasoning applies that rule. A negative price (OpenRouter's -1 for a
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
	// The raw reasoning price; the caller applies separateReasoning once the output rate it is
	// compared with is known (a tier's may be inherited from the base price).
	w.Reasoning = conv("internal_reasoning")
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
	w.Reasoning = separateReasoning(w.Reasoning, w.Output)
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
		var applicable []sizeOverride
		for i, ov := range ovs {
			so, ok, probs := overrideTier(ov, o.Ignored, i)
			o.Problems = append(o.Problems, probs...)
			if ok {
				applicable = append(applicable, so)
			}
		}
		o.Spec.Tiers = mergeTiers(o.Spec.Prices, applicable)
	}
	return o
}

// timeOfDayKeys mark an override as a time-of-day price, which Koinon cannot bill (decision 2.7).
var timeOfDayKeys = []string{"utc_days", "utc_start", "utc_end"}

// sizeOverride is one pricing.overrides entry whose only condition is the prompt size, in list
// order: its threshold and the raw rates it sets (rateSet, reasoning not yet resolved).
type sizeOverride struct {
	minPromptTokens int64
	rates           pricing.Wire
}

// overrideTier reads one pricing.overrides entry. Only a prompt-size override can become a tier;
// an entry with any time condition, a batch, flex or priority entry, and one with no prompt size
// are kept in the provenance and never imported (decision 2.7): OpenRouter applies an entry only
// when all of its conditions match, so a tier built from a time-windowed entry would bill that
// price at every hour.
func overrideTier(ov map[string]json.RawMessage, ignored map[string]string, i int) (sizeOverride, bool, []string) {
	prefix := fmt.Sprintf("overrides[%d].", i)
	for _, k := range timeOfDayKeys {
		if _, ok := ov[k]; ok {
			ignored[prefix+"skipped"] = "time-of-day override"
			return sizeOverride{}, false, nil
		}
	}
	for _, k := range []string{"batch", "flex", "priority", "service_tier"} {
		if _, ok := ov[k]; ok {
			ignored[prefix+"skipped"] = k + " override"
			return sizeOverride{}, false, nil
		}
	}
	rawMin, ok := ov["min_prompt_tokens"]
	if !ok {
		ignored[prefix+"skipped"] = "no min_prompt_tokens"
		return sizeOverride{}, false, nil
	}
	var minTokens int64
	if err := json.Unmarshal(rawMin, &minTokens); err != nil || minTokens < 1 || minTokens > 1<<40 {
		return sizeOverride{}, false, []string{prefix + "min_prompt_tokens is not a positive count"}
	}
	if minTokens+TierBoundaryOffset < 1 {
		return sizeOverride{}, false, []string{prefix + "min_prompt_tokens leaves no tier boundary"}
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
		return sizeOverride{}, false, nil
	}
	return sizeOverride{minPromptTokens: minTokens, rates: w}, len(probs) == 0, probs
}

// mergeTiers turns the prompt-size overrides into Koinon tiers with OpenRouter's rules: for a
// request, every entry whose threshold the prompt exceeds applies, later entries in the list win
// per price key, and a key no applicable entry sets keeps the base price. Koinon overlays its tiers
// in ascending order (pricing.Price.Effective), so each tier carries the merged value of every key
// any applicable entry sets: the result is the same whatever order the list has, and two entries
// with one threshold merge instead of conflicting. A tier that prices exactly like the tier below
// it is dropped.
func mergeTiers(base pricing.Wire, ovs []sizeOverride) []pricing.WireTier {
	thresholds := make([]int64, 0, len(ovs))
	for _, ov := range ovs {
		thresholds = append(thresholds, ov.minPromptTokens)
	}
	slices.Sort(thresholds)
	thresholds = slices.Compact(thresholds)
	var tiers []pricing.WireTier
	prev := base
	for _, t := range thresholds {
		var w pricing.Wire
		for _, ov := range ovs { // list order: a later entry overwrites an earlier one per key
			if ov.minPromptTokens > t {
				continue
			}
			for _, f := range []struct {
				dst *string
				src string
			}{
				{&w.Input, ov.rates.Input},
				{&w.Output, ov.rates.Output},
				{&w.CacheRead, ov.rates.CacheRead},
				{&w.CacheWrite, ov.rates.CacheWrite},
				{&w.Reasoning, ov.rates.Reasoning},
			} {
				if f.src != "" {
					*f.dst = f.src
				}
			}
		}
		if w.Reasoning != "" {
			out := w.Output
			if out == "" {
				out = base.Output
			}
			w.Reasoning = separateReasoning(w.Reasoning, out)
			if w.Reasoning == "" && prev.Reasoning != "" {
				// No separate charge in this tier, but the price below it has one: say so, or the
				// tier would inherit that reasoning rate.
				w.Reasoning = out
			}
		}
		if w == (pricing.Wire{}) || effectiveWire(prev, w) == prev {
			continue
		}
		tiers = append(tiers, pricing.WireTier{ContextOver: t + TierBoundaryOffset, Prices: w})
		prev = effectiveWire(prev, w)
	}
	return tiers
}

// effectiveWire overlays the rates t sets on b, as pricing.Price.Effective does.
func effectiveWire(b, t pricing.Wire) pricing.Wire {
	pick := func(a, o string) string {
		if o != "" {
			return o
		}
		return a
	}
	return pricing.Wire{
		Input: pick(b.Input, t.Input), Output: pick(b.Output, t.Output), CacheRead: pick(b.CacheRead, t.CacheRead),
		CacheWrite: pick(b.CacheWrite, t.CacheWrite), Reasoning: pick(b.Reasoning, t.Reasoning),
	}
}

// separateReasoning applies decision 8 to a raw reasoning price: it is a rate only when it is
// positive and differs from the output rate it would otherwise bill at.
func separateReasoning(raw, output string) string {
	if raw == "" || raw == "0" || raw == output {
		return ""
	}
	return raw
}
