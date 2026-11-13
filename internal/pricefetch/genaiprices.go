// SPDX-License-Identifier: Apache-2.0

package pricefetch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/stateracloud/koinon/pricing"
)

// GenaiPrices reads prices/data.json of pydantic/genai-prices at a commit at least 24 hours old
// (ADR 0098 decisions 2.3 to 2.5). The file is a list of providers, each with models whose prices
// are USD per million tokens (input_mtok, output_mtok, cache_read_mtok, cache_write_mtok); a rate is
// a number or an object {"base": n, "tiers": [{"start": tokens, "price": n}]}.
const (
	genaiPricesRepo = "pydantic/genai-prices"
	genaiPricesFile = "prices/data.json"
	// GenaiPricesMinModels is the model-count floor.
	GenaiPricesMinModels = 100
)

// GenaiPrices is the adapter. Build it with NewGenaiPrices.
type GenaiPrices struct{ gh githubFile }

// NewGenaiPrices returns the adapter with the pinned hosts. client may be nil.
func NewGenaiPrices(client *http.Client) *GenaiPrices {
	return &GenaiPrices{gh: newGithubFile(client, SourceGenaiPrices, genaiPricesRepo, genaiPricesFile)}
}

// Name implements Adapter.
func (*GenaiPrices) Name() string { return SourceGenaiPrices }

// MinModels is the model-count floor.
func (*GenaiPrices) MinModels() int { return GenaiPricesMinModels }

// Fetch implements Adapter.
func (g *GenaiPrices) Fetch(ctx context.Context, now time.Time) (Snapshot, error) {
	body, sha, at, rawURL, err := g.gh.fetch(ctx, now)
	if err != nil {
		return Snapshot{}, err
	}
	snap, err := ParseGenaiPrices(body, now)
	if err != nil {
		return Snapshot{}, err
	}
	snap.URL, snap.CommitSHA, snap.CommittedAt = rawURL, sha, at
	return snap, nil
}

// genaiProviders maps genai-prices provider ids to the Koinon provider type: the providers it lists
// first-hand under their own id.
var genaiProviders = map[string]string{"openai": "openai", "anthropic": "anthropic", "deepseek": "deepseek"}

type gpProvider struct {
	ID     string    `json:"id"`
	Models []gpModel `json:"models"`
}

type gpModel struct {
	ID     string          `json:"id"`
	Prices json.RawMessage `json:"prices"`
}

// ParseGenaiPrices decodes and normalizes the file.
func ParseGenaiPrices(body []byte, now time.Time) (Snapshot, error) {
	var providers []gpProvider
	if err := decodeStrict(SourceGenaiPrices, body, &providers); err != nil {
		return Snapshot{}, err
	}
	if providers == nil {
		return Snapshot{}, fmt.Errorf("pricefetch: genai-prices: the body is not a list of providers")
	}
	snap := Snapshot{Source: SourceGenaiPrices, URL: "https://" + githubRawHost + "/" + genaiPricesRepo + "/HEAD/" + genaiPricesFile, Licence: LicenceGenaiPrices, FetchedAt: now.UTC(), SHA256: DigestBody(body)}
	index := map[string]int{}
	for _, p := range providers {
		for _, m := range p.Models {
			snap.Count++
			pt, ok := genaiProviders[p.ID]
			if !ok || !validModelName(m.ID) {
				continue
			}
			addOffer(&snap, index, normalizeGenai(m, pt))
		}
	}
	sortOffers(&snap)
	return snap, nil
}

func normalizeGenai(m gpModel, pt string) Offer {
	o := Offer{Model: m.ID, ProviderType: pt, Ignored: map[string]string{}}
	var prices map[string]json.RawMessage
	switch t := trimJSON(m.Prices); {
	case t == "" || t == "null":
		o.Problems = []string{"the model has no prices"}
		return o
	case t[0] == '[':
		// Prices that depend on a date or a condition: Koinon bills one price at a time (decision 2.7).
		o.Problems = []string{"the prices depend on a date or condition (a list of price sets)"}
		return o
	default:
		if err := json.Unmarshal(m.Prices, &prices); err != nil {
			o.Problems = []string{"prices is not an object"}
			return o
		}
	}
	ts := tierSet{}
	rate := func(key string, set func(w *pricing.Wire, v string)) {
		raw, ok := prices[key]
		if !ok {
			return
		}
		var obj struct {
			Base  json.RawMessage `json:"base"`
			Tiers []struct {
				Start json.RawMessage `json:"start"`
				Price json.RawMessage `json:"price"`
			} `json:"tiers"`
		}
		if t := trimJSON(raw); t != "" && t[0] == '{' {
			if err := json.Unmarshal(raw, &obj); err != nil {
				o.Problems = append(o.Problems, key+": not a tiered price")
				return
			}
			set(&o.Spec.Prices, bucketValue(obj.Base, key+".base", perMillionDirect, &o.Problems))
			for i, tier := range obj.Tiers {
				where := fmt.Sprintf("%s.tiers[%d]", key, i)
				start := maxOutputOf(tier.Start, where+".start", &o.Problems)
				if start < 1 {
					o.Problems = append(o.Problems, where+".start is not a positive token count")
					continue
				}
				set(ts.at(int64(start)), bucketValue(tier.Price, where+".price", perMillionDirect, &o.Problems))
			}
			return
		}
		set(&o.Spec.Prices, bucketValue(raw, key, perMillionDirect, &o.Problems))
	}
	rate("input_mtok", func(w *pricing.Wire, v string) { w.Input = v })
	rate("output_mtok", func(w *pricing.Wire, v string) { w.Output = v })
	rate("cache_read_mtok", func(w *pricing.Wire, v string) { w.CacheRead = v })
	rate("cache_write_mtok", func(w *pricing.Wire, v string) { w.CacheWrite = v })
	// Kept for the provenance only (decision 8): audio and other prices Koinon does not bill.
	keys := make([]string, 0, len(prices))
	for k := range prices {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch k {
		case "input_mtok", "output_mtok", "cache_read_mtok", "cache_write_mtok":
		default:
			o.Ignored[k] = trimJSON(prices[k])
		}
	}
	o.Spec.Tiers = ts.tiers(o.Spec.Prices)
	finishOffer(&o)
	return o
}

func trimJSON(raw json.RawMessage) string {
	return strings.TrimSpace(string(raw))
}
