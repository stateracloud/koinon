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
	"time"
)

// ModelsDevURL is the one URL the models.dev adapter asks. models.dev makes proposals only: it
// never counts towards auto-apply or agreement, as a primary source or as one of two (ADR 0098
// decisions 2.4 and 7), so it needs no commit pinning.
const (
	ModelsDevURL  = "https://models.dev/api.json"
	modelsDevHost = "models.dev"
	// ModelsDevMinModels is the model-count floor.
	ModelsDevMinModels = 100
)

// ModelsDev is the adapter. Build it with NewModelsDev.
type ModelsDev struct {
	client   *http.Client
	endpoint string
	pin      pin
}

// NewModelsDev returns the adapter with the pinned endpoint. client may be nil.
func NewModelsDev(client *http.Client) *ModelsDev {
	return &ModelsDev{client: client, endpoint: ModelsDevURL, pin: pin{host: modelsDevHost}}
}

// Name implements Adapter.
func (*ModelsDev) Name() string { return SourceModelsDev }

// MinModels is the model-count floor.
func (*ModelsDev) MinModels() int { return ModelsDevMinModels }

// Fetch implements Adapter.
func (m *ModelsDev) Fetch(ctx context.Context, now time.Time) (Snapshot, error) {
	body, hdr, err := httpGet(ctx, m.client, m.endpoint, m.pin, SourceModelsDev, "application/json")
	if err != nil {
		return Snapshot{}, err
	}
	snap, err := ParseModelsDev(body, now)
	if err != nil {
		return Snapshot{}, err
	}
	snap.URL, snap.ETag = m.endpoint, hdr.Get("ETag")
	return snap, nil
}

// modelsDevProviders maps models.dev provider ids to the Koinon provider type. The `openrouter`
// provider of models.dev is a bot copy of OpenRouter's API and is not read.
var modelsDevProviders = map[string]string{"openai": "openai", "anthropic": "anthropic", "deepseek": "deepseek"}

var modelsDevTierRE = regexp.MustCompile(`^context_over_([0-9]+)k$`)

type mdProvider struct {
	Models map[string]mdModel `json:"models"`
}

type mdModel struct {
	ID    string                     `json:"id"`
	Cost  map[string]json.RawMessage `json:"cost"`
	Limit struct {
		Output json.RawMessage `json:"output"`
	} `json:"limit"`
}

// ParseModelsDev decodes and normalizes api.json: an object keyed by provider id, each with a models
// object whose cost fields are USD per million tokens.
func ParseModelsDev(body []byte, now time.Time) (Snapshot, error) {
	var providers map[string]mdProvider
	if err := decodeStrict(SourceModelsDev, body, &providers); err != nil {
		return Snapshot{}, err
	}
	if providers == nil {
		return Snapshot{}, fmt.Errorf("pricefetch: models-dev: the body is not an object")
	}
	snap := Snapshot{Source: SourceModelsDev, URL: ModelsDevURL, Licence: LicenceModelsDev, FetchedAt: now.UTC(), SHA256: DigestBody(body)}
	ids := make([]string, 0, len(providers))
	for id := range providers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	index := map[string]int{}
	for _, id := range ids {
		names := make([]string, 0, len(providers[id].Models))
		for n := range providers[id].Models {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			snap.Count++
			pt, ok := modelsDevProviders[id]
			m := providers[id].Models[n]
			if !ok || m.Cost == nil || !validModelName(n) {
				continue
			}
			addOffer(&snap, index, normalizeModelsDev(n, pt, m))
		}
	}
	sortOffers(&snap)
	return snap, nil
}

func normalizeModelsDev(name, pt string, m mdModel) Offer {
	o := Offer{Model: name, ProviderType: pt, Ignored: map[string]string{}}
	val := func(c map[string]json.RawMessage, key, where string) string {
		return bucketValue(c[key], where+key, perMillionDirect, &o.Problems)
	}
	w := &o.Spec.Prices
	w.Input, w.Output = val(m.Cost, "input", ""), val(m.Cost, "output", "")
	w.CacheRead, w.CacheWrite = val(m.Cost, "cache_read", ""), val(m.Cost, "cache_write", "")
	w.Reasoning = separateReasoning(val(m.Cost, "reasoning", ""), w.Output)
	o.MaxOutput = maxOutputOf(m.Limit.Output, "limit.output", &o.Problems)
	ts := tierSet{}
	keys := make([]string, 0, len(m.Cost))
	for k := range m.Cost {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch k {
		case "input", "output", "cache_read", "cache_write", "reasoning":
			continue
		}
		sm := modelsDevTierRE.FindStringSubmatch(k)
		if sm == nil {
			o.Ignored[k] = trimJSON(m.Cost[k]) // input_audio, output_audio and others Koinon does not bill
			continue
		}
		n, _ := strconv.ParseInt(sm[1], 10, 64)
		var tc map[string]json.RawMessage
		if n < 1 || n > 1<<30 || json.Unmarshal(m.Cost[k], &tc) != nil {
			o.Problems = append(o.Problems, k+": not a tier")
			continue
		}
		tw := ts.at(n * 1000)
		tw.Input, tw.Output = val(tc, "input", k+"."), val(tc, "output", k+".")
		tw.CacheRead, tw.CacheWrite = val(tc, "cache_read", k+"."), val(tc, "cache_write", k+".")
		tw.Reasoning = val(tc, "reasoning", k+".")
	}
	o.Spec.Tiers = ts.tiers(o.Spec.Prices)
	finishOffer(&o)
	return o
}
