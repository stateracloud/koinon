// SPDX-License-Identifier: Apache-2.0

package pricefetch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// LiteLLM reads model_prices_and_context_window.json of BerriAI/litellm at a commit at least 24
// hours old (ADR 0098 decisions 2.3 to 2.5). Only the repository file is read, never the PyPI
// package.
const (
	liteLLMRepo = "BerriAI/litellm"
	liteLLMFile = "model_prices_and_context_window.json"
	// LiteLLMMinModels is the model-count floor: the file lists well over a thousand models.
	LiteLLMMinModels = 200
)

// LiteLLM is the adapter. Build it with NewLiteLLM.
type LiteLLM struct{ gh githubFile }

// NewLiteLLM returns the adapter with the pinned hosts. client may be nil.
func NewLiteLLM(client *http.Client) *LiteLLM {
	return &LiteLLM{gh: newGithubFile(client, SourceLiteLLM, liteLLMRepo, liteLLMFile)}
}

// Name implements Adapter.
func (*LiteLLM) Name() string { return SourceLiteLLM }

// MinModels is the model-count floor.
func (*LiteLLM) MinModels() int { return LiteLLMMinModels }

// Fetch implements Adapter.
func (l *LiteLLM) Fetch(ctx context.Context, now time.Time) (Snapshot, error) {
	body, sha, at, rawURL, err := l.gh.fetch(ctx, now)
	if err != nil {
		return Snapshot{}, err
	}
	snap, err := ParseLiteLLM(body, now)
	if err != nil {
		return Snapshot{}, err
	}
	snap.URL, snap.CommitSHA, snap.CommittedAt = rawURL, sha, at
	return snap, nil
}

// liteLLMProviders maps LiteLLM's litellm_provider to the Koinon provider type it prices: the
// providers LiteLLM lists first-hand (their own model ids, not another gateway's copy). A row of any
// other provider (azure, bedrock, vertex_ai, openrouter, ...) is counted and never offered.
var liteLLMProviders = map[string]string{"openai": "openai", "anthropic": "anthropic", "deepseek": "deepseek"}

// liteLLMModes are the modes whose token prices Koinon bills.
var liteLLMModes = map[string]bool{"chat": true, "completion": true, "responses": true}

var liteLLMTierRE = regexp.MustCompile(`^(input_cost_per_token|output_cost_per_token|cache_read_input_token_cost|cache_creation_input_token_cost|output_cost_per_reasoning_token)_above_([0-9]+)k_tokens$`)

// ParseLiteLLM decodes and normalizes the file: one JSON object keyed by model id, prices as JSON
// numbers in USD per token, read from their decimal text. "sample_spec" is the file's own
// documentation row and is not a model.
func ParseLiteLLM(body []byte, now time.Time) (Snapshot, error) {
	var rows map[string]json.RawMessage
	if err := decodeStrict(SourceLiteLLM, body, &rows); err != nil {
		return Snapshot{}, err
	}
	if rows == nil {
		return Snapshot{}, fmt.Errorf("pricefetch: litellm: the body is not an object")
	}
	snap := Snapshot{Source: SourceLiteLLM, URL: "https://" + githubRawHost + "/" + liteLLMRepo + "/HEAD/" + liteLLMFile, Licence: LicenceLiteLLM, FetchedAt: now.UTC(), SHA256: DigestBody(body)}
	keys := make([]string, 0, len(rows))
	for k := range rows {
		if k != "sample_spec" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	snap.Count = len(keys)
	index := map[string]int{}
	for _, k := range keys {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(rows[k], &m); err != nil || m == nil {
			return Snapshot{}, fmt.Errorf("pricefetch: litellm: model %q: %w", k, errNotObject)
		}
		var provider, mode string
		_ = json.Unmarshal(m["litellm_provider"], &provider)
		_ = json.Unmarshal(m["mode"], &mode)
		pt, ok := liteLLMProviders[provider]
		if !ok || !liteLLMModes[mode] {
			continue
		}
		name := strings.TrimPrefix(k, provider+"/")
		if !validModelName(name) {
			continue
		}
		addOffer(&snap, index, normalizeLiteLLM(name, pt, m))
	}
	sortOffers(&snap)
	return snap, nil
}

func normalizeLiteLLM(name, pt string, m map[string]json.RawMessage) Offer {
	o := Offer{Model: name, ProviderType: pt, Ignored: map[string]string{}}
	conv := func(s string) (string, error) { return perMillion(s) }
	val := func(key string) string { return bucketValue(m[key], key, conv, &o.Problems) }
	w := &o.Spec.Prices
	w.Input, w.Output = val("input_cost_per_token"), val("output_cost_per_token")
	w.CacheRead, w.CacheWrite = val("cache_read_input_token_cost"), val("cache_creation_input_token_cost")
	w.Reasoning = separateReasoning(val("output_cost_per_reasoning_token"), w.Output)
	o.MaxOutput = maxOutputOf(m["max_output_tokens"], "max_output_tokens", &o.Problems)
	ts := tierSet{}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if sm := liteLLMTierRE.FindStringSubmatch(k); sm != nil {
			k1, _ := strconv.ParseInt(sm[2], 10, 64)
			if k1 < 1 || k1 > 1<<30 {
				o.Problems = append(o.Problems, k+": not a tier threshold")
				continue
			}
			tw := ts.at(k1 * 1000)
			v := val(k)
			switch sm[1] {
			case "input_cost_per_token":
				tw.Input = v
			case "output_cost_per_token":
				tw.Output = v
			case "cache_read_input_token_cost":
				tw.CacheRead = v
			case "cache_creation_input_token_cost":
				tw.CacheWrite = v
			default:
				tw.Reasoning = v
			}
			continue
		}
		// Kept for the provenance only (decision 8): every other price, such as the 1-hour cache
		// write, the batch, flex and priority variants, and per-image or per-second prices.
		if strings.Contains(k, "cost") || strings.Contains(k, "price") {
			switch k {
			case "input_cost_per_token", "output_cost_per_token", "cache_read_input_token_cost", "cache_creation_input_token_cost", "output_cost_per_reasoning_token":
			default:
				o.Ignored[k] = strings.Trim(string(m[k]), `"`)
			}
		}
	}
	o.Spec.Tiers = ts.tiers(o.Spec.Prices)
	finishOffer(&o)
	return o
}
