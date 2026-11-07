// SPDX-License-Identifier: Apache-2.0

package pricefetch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stateracloud/koinon/pricing"
)

func TestPerMillion(t *testing.T) {
	for _, c := range []struct {
		in, want string
		bad      bool
	}{
		{"0.000001", "1", false},
		{"0.0000002", "0.2", false},
		{"0.00000125", "1.25", false},
		{"1e-6", "1", false},
		{"0", "0", false},
		{"0.0000000000001", "", true}, // 0.0000001 per million: more than six fraction digits
		{"-1", "", true},
		{"abc", "", true},
		{"", "", true},
	} {
		got, err := perMillion(c.in)
		if c.bad != (err != nil) || got != c.want {
			t.Errorf("perMillion(%q) = %q, %v; want %q, bad=%v", c.in, got, err, c.want, c.bad)
		}
	}
}

func parse(t *testing.T, models string) Snapshot {
	t.Helper()
	s, err := ParseOpenRouter([]byte(`{"data":[`+models+`]}`), time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func offer(t *testing.T, s Snapshot, model string) Offer {
	t.Helper()
	for _, o := range s.Offers {
		if o.Model == model {
			return o
		}
	}
	t.Fatalf("no offer for %s", model)
	return Offer{}
}

func TestParseOpenRouter(t *testing.T) {
	s := parse(t, `
	 {"id":"fake/model-a","pricing":{"prompt":"0.000001","completion":"0.000002","input_cache_read":"0.0000001","input_cache_write_1h":"0.000004","request":"0"},"top_provider":{"max_completion_tokens":8192}},
	 {"id":"fake/model-b","pricing":{"prompt":"0.000001","completion":"0.000002","internal_reasoning":"0"}},
	 {"id":"fake/model-c","pricing":{"prompt":"0.000001","completion":"0.000002","internal_reasoning":"0.000002"}},
	 {"id":"fake/model-d","pricing":{"prompt":"0.000001","completion":"0.000002","internal_reasoning":"0.000003"}},
	 {"id":"fake/router","pricing":{"prompt":"-1","completion":"-1"}},
	 {"id":"bad name with spaces","pricing":{"prompt":"0.000001","completion":"0.000002"}}`)
	if s.Count != 6 || len(s.Offers) != 5 {
		t.Errorf("count %d, offers %d; the unusable name is skipped but counted", s.Count, len(s.Offers))
	}
	a := offer(t, s, "fake/model-a")
	if a.Spec.Prices.Input != "1" || a.Spec.Prices.Output != "2" || a.Spec.Prices.CacheRead != "0.1" || a.MaxOutput != 8192 || len(a.Problems) != 0 {
		t.Errorf("model-a: %+v", a)
	}
	if a.Spec.Prices.CacheWrite != "" {
		t.Errorf("the 1-hour cache write must not become the cache write rate: %q", a.Spec.Prices.CacheWrite)
	}
	if a.Ignored["input_cache_write_1h"] == "" {
		t.Errorf("the 1-hour cache write is kept in the provenance: %v", a.Ignored)
	}
	if b := offer(t, s, "fake/model-b"); b.Spec.Prices.CacheRead != "" || b.Spec.Prices.Reasoning != "" {
		t.Errorf("a missing cache price or a zero reasoning price is never written: %+v", b.Spec.Prices)
	}
	if c := offer(t, s, "fake/model-c"); c.Spec.Prices.Reasoning != "" {
		t.Errorf("reasoning equal to output is no separate rate: %q", c.Spec.Prices.Reasoning)
	}
	if d := offer(t, s, "fake/model-d"); d.Spec.Prices.Reasoning != "3" {
		t.Errorf("reasoning above output is a rate: %q", d.Spec.Prices.Reasoning)
	}
	if r := offer(t, s, "fake/router"); len(r.Problems) == 0 {
		t.Error("a negative (variable) price is a problem")
	}
}

// A null max_completion_tokens is no output limit, not a problem: the offer prices the model and
// BuildEntry later asks a person for the limit (ErrNoMaxOutput) instead of refusing the model.
func TestParseOpenRouterNullMaxCompletionTokens(t *testing.T) {
	s := parse(t, `{"id":"fake/model-a","pricing":{"prompt":"0.000001","completion":"0.000002"},"top_provider":{"max_completion_tokens":null}},
	 {"id":"fake/model-b","pricing":{"prompt":"0.000001","completion":"0.000002"},"top_provider":{}}`)
	for _, m := range []string{"fake/model-a", "fake/model-b"} {
		o := offer(t, s, m)
		if o.MaxOutput != 0 || len(o.Problems) != 0 || o.Spec.Prices.Input != "1" {
			t.Errorf("%s: want a priced offer with no output limit and no problems, got %+v", m, o)
		}
		e, err := BuildEntry(o, nil, SourceOpenRouter, time.Unix(0, 0))
		if !errors.Is(err, ErrNoMaxOutput) {
			t.Errorf("%s: BuildEntry error %v, want ErrNoMaxOutput", m, err)
		}
		if e.ID == "" || e.MaxOutput != 0 || e.Prices.Output != "2" {
			t.Errorf("%s: the entry is still returned for review: %+v", m, e)
		}
	}
}

func TestParseOpenRouterTiers(t *testing.T) {
	s := parse(t, `{"id":"fake/tiered","pricing":{"prompt":"0.000001","completion":"0.000002","overrides":[
	  {"min_prompt_tokens":200000,"prompt":"0.000002","completion":"0.000003"},
	  {"utc_days":[1],"utc_start":"00:00","utc_end":"06:00","prompt":"0.0000005"},
	  {"batch":true,"prompt":"0.0000005"}]}}`)
	o := offer(t, s, "fake/tiered")
	if len(o.Spec.Tiers) != 1 {
		t.Fatalf("only the prompt-size override is a tier: %+v (problems %v)", o.Spec.Tiers, o.Problems)
	}
	if got, want := o.Spec.Tiers[0].ContextOver, int64(200000); got != want {
		t.Errorf("context_over %d, want min_prompt_tokens = %d", got, want)
	}
	if o.Spec.Tiers[0].Prices.Input != "2" || o.Spec.Tiers[0].Prices.Output != "3" {
		t.Errorf("tier rates: %+v", o.Spec.Tiers[0].Prices)
	}
	if o.Ignored["overrides[1].skipped"] == "" || o.Ignored["overrides[2].skipped"] == "" {
		t.Errorf("time-of-day and batch overrides are recorded as skipped: %v", o.Ignored)
	}
}

// TestTierBoundaryOffset pins ADR 0098 open question 1, resolved from OpenRouter's SDK
// (@openrouter/sdk 1.5.64, esm/models/pricingoverride.d.ts): min_prompt_tokens applies when the
// prompt is "strictly greater than this threshold", which is what context_over means. Changing the
// constant changes every tiered price: it must be a decision, not a drift.
func TestTierBoundaryOffset(t *testing.T) {
	if TierBoundaryOffset != 0 {
		t.Errorf("TierBoundaryOffset = %d; OpenRouter documents min_prompt_tokens as strictly greater than, so context_over = min_prompt_tokens", TierBoundaryOffset)
	}
	s := parse(t, `{"id":"fake/tiered","pricing":{"prompt":"0.000001","completion":"0.000002","overrides":[{"min_prompt_tokens":200000,"prompt":"0.000002"}]}}`)
	o := offer(t, s, "fake/tiered")
	e, err := BuildEntry(Offer{Model: o.Model, ProviderType: o.ProviderType, Spec: o.Spec, MaxOutput: 1000}, nil, SourceOpenRouter, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.Parse()
	if err != nil {
		t.Fatal(err)
	}
	if in := *p.Effective(200000).Input; in.PerMillion() != "1" {
		t.Errorf("a prompt of exactly min_prompt_tokens is not in the tier: input %s", in.PerMillion())
	}
	if in := *p.Effective(200001).Input; in.PerMillion() != "2" {
		t.Errorf("a prompt one token above min_prompt_tokens is in the tier: input %s", in.PerMillion())
	}
}

// TestParseOpenRouterOverrideRules covers the rules OpenRouter's PricingOverride documents: an entry
// applies only when all of its conditions match (so one with a time condition is never a tier),
// later applicable entries win per price key, and keys absent from an entry keep the base price.
func TestParseOpenRouterOverrideRules(t *testing.T) {
	base := `"prompt":"0.000001","completion":"0.000002"`
	for _, c := range []struct {
		name      string
		overrides string
		want      []pricing.WireTier
	}{
		{
			"a time window with a prompt size is not a tier",
			`{"min_prompt_tokens":1000,"utc_start":100,"utc_end":400,"prompt":"0.0000005"}`, nil,
		},
		{
			"weekdays alone are a time condition",
			`{"min_prompt_tokens":1000,"utc_days":["monday"],"prompt":"0.0000005"}`, nil,
		},
		{
			"absent keys inherit the base price",
			`{"min_prompt_tokens":1000,"prompt":"0.000003"}`,
			[]pricing.WireTier{{ContextOver: 1000, Prices: pricing.Wire{Input: "3"}}},
		},
		{
			"two entries at one threshold merge, the later wins per key",
			`{"min_prompt_tokens":1000,"prompt":"0.000003","completion":"0.000004"},{"min_prompt_tokens":1000,"prompt":"0.000005"}`,
			[]pricing.WireTier{{ContextOver: 1000, Prices: pricing.Wire{Input: "5", Output: "4"}}},
		},
		{
			"a higher tier keeps a lower entry's key it does not set",
			`{"min_prompt_tokens":1000,"prompt":"0.000003","completion":"0.000004"},{"min_prompt_tokens":2000,"prompt":"0.000005"}`,
			[]pricing.WireTier{{ContextOver: 1000, Prices: pricing.Wire{Input: "3", Output: "4"}}, {ContextOver: 2000, Prices: pricing.Wire{Input: "5", Output: "4"}}},
		},
		{
			"a later lower-threshold entry wins above a higher one too",
			`{"min_prompt_tokens":2000,"prompt":"0.000005","completion":"0.000006"},{"min_prompt_tokens":1000,"prompt":"0.000003"}`,
			[]pricing.WireTier{{ContextOver: 1000, Prices: pricing.Wire{Input: "3"}}, {ContextOver: 2000, Prices: pricing.Wire{Input: "3", Output: "6"}}},
		},
		{
			"an entry that changes nothing above the tier below is no tier",
			`{"min_prompt_tokens":2000,"prompt":"0.000005"},{"min_prompt_tokens":1000,"prompt":"0.000003"}`,
			[]pricing.WireTier{{ContextOver: 1000, Prices: pricing.Wire{Input: "3"}}},
		},
	} {
		s := parse(t, `{"id":"fake/model-a","pricing":{`+base+`,"overrides":[`+c.overrides+`]}}`)
		o := offer(t, s, "fake/model-a")
		if len(o.Problems) != 0 {
			t.Errorf("%s: problems %v", c.name, o.Problems)
		}
		if !reflect.DeepEqual(o.Spec.Tiers, c.want) {
			t.Errorf("%s: tiers %+v, want %+v", c.name, o.Spec.Tiers, c.want)
		}
		if o.Spec.Prices.Input != "1" || o.Spec.Prices.Output != "2" {
			t.Errorf("%s: the base price is unchanged: %+v", c.name, o.Spec.Prices)
		}
	}
}

// A tier's reasoning price follows decision 8 against the tier's own output rate, inherited from
// the base where the entry sets none, and a tier with no separate charge does not inherit one.
func TestParseOpenRouterTierReasoning(t *testing.T) {
	s := parse(t, `{"id":"fake/model-a","pricing":{"prompt":"0.000001","completion":"0.000002","internal_reasoning":"0.000003","overrides":[
	  {"min_prompt_tokens":1000,"completion":"0.000004"},
	  {"min_prompt_tokens":2000,"internal_reasoning":"0"}]}},
	 {"id":"fake/model-b","pricing":{"prompt":"0.000001","completion":"0.000002","overrides":[
	  {"min_prompt_tokens":1000,"internal_reasoning":"0.000002","prompt":"0.000003"}]}}`)
	want := []pricing.WireTier{
		{ContextOver: 1000, Prices: pricing.Wire{Output: "4"}},                 // reasoning 3 inherited from the base
		{ContextOver: 2000, Prices: pricing.Wire{Output: "4", Reasoning: "4"}}, // "0": bills at output, not the base's 3
	}
	if got := offer(t, s, "fake/model-a").Spec.Tiers; !reflect.DeepEqual(got, want) {
		t.Errorf("model-a tiers %+v, want %+v", got, want)
	}
	// Reasoning equal to the inherited base output is no separate rate.
	if got := offer(t, s, "fake/model-b").Spec.Tiers; !reflect.DeepEqual(got, []pricing.WireTier{{ContextOver: 1000, Prices: pricing.Wire{Input: "3"}}}) {
		t.Errorf("model-b tiers %+v", got)
	}
}

func TestParseOpenRouterRejects(t *testing.T) {
	for name, body := range map[string]string{
		"no data":   `{}`,
		"not json":  `<html>`,
		"trailing":  `{"data":[]} {"data":[]}`,
		"duplicate": `{"data":[{"id":"a/b","pricing":{}},{"id":"a/b","pricing":{}}]}`,
	} {
		if _, err := ParseOpenRouter([]byte(body), time.Now()); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestOpenRouterFetch(t *testing.T) {
	const body = `{"data":[{"id":"fake/model-a","pricing":{"prompt":"0.000001","completion":"0.000002"}}]}`
	var auth, method atomic.Value
	var hits atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		auth.Store(r.Header.Get("Authorization") + r.Header.Get("Cookie"))
		method.Store(r.Method)
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "https://example.invalid/", http.StatusFound)
		case "/big":
			_, _ = w.Write([]byte(`{"data":[],"pad":"` + strings.Repeat("x", MaxBodyBytes) + `"}`))
		case "/down":
			http.Error(w, "no", http.StatusServiceUnavailable)
		default:
			w.Header().Set("ETag", `"v1"`)
			_, _ = w.Write([]byte(body))
		}
	}))
	defer srv.Close()
	host := mustHost(t, srv.URL)
	adapter := func(path string) *OpenRouter {
		return &OpenRouter{client: srv.Client(), endpoint: srv.URL + path, host: host, minModel: 1}
	}

	snap, err := adapter("/models").Fetch(context.Background(), time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if snap.Count != 1 || snap.ETag != `"v1"` || snap.SHA256 != DigestBody([]byte(body)) || snap.URL != srv.URL+"/models" {
		t.Errorf("snapshot: %+v", snap)
	}
	if a := auth.Load(); a != "" || method.Load() != http.MethodGet {
		t.Errorf("the request must be an unauthenticated GET, got auth/cookie %q method %v", a, method.Load())
	}
	for path, want := range map[string]string{"/redirect": "redirect", "/big": "16 MiB", "/down": "503"} {
		if _, err := adapter(path).Fetch(context.Background(), time.Now()); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err %v, want it to mention %q", path, err, want)
		}
	}

	// The endpoint is held to https and the pinned host before any request.
	before := hits.Load()
	for name, o := range map[string]*OpenRouter{
		"plain http":  {client: srv.Client(), endpoint: strings.Replace(srv.URL, "https", "http", 1), host: host},
		"other host":  {client: srv.Client(), endpoint: srv.URL, host: "openrouter.ai"},
		"user info":   {client: srv.Client(), endpoint: strings.Replace(srv.URL, "https://", "https://u:p@", 1), host: host},
		"not a URL":   {client: srv.Client(), endpoint: "::", host: host},
		"empty":       {client: srv.Client(), endpoint: "", host: host},
		"file scheme": {client: srv.Client(), endpoint: "file:///etc/passwd", host: host},
	} {
		if _, err := o.Fetch(context.Background(), time.Now()); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if hits.Load() != before {
		t.Error("a refused endpoint must not reach the server")
	}
	if got := NewOpenRouter(nil); got.endpoint != OpenRouterURL || got.host != "openrouter.ai" || got.MinModels() != OpenRouterMinModels {
		t.Errorf("the production adapter is pinned: %+v", got)
	}
}

func mustHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Hostname()
}

func TestCheckFloor(t *testing.T) {
	for _, c := range []struct {
		count, min, last int
		bad              bool
	}{
		{500, 50, 0, false}, {10, 50, 0, true}, {200, 50, 457, true}, {230, 50, 457, false}, {50, 50, 0, false},
	} {
		err := CheckFloor(Snapshot{Source: "x", Count: c.count}, c.min, c.last)
		if (err != nil) != c.bad {
			t.Errorf("CheckFloor(%d, %d, %d) = %v", c.count, c.min, c.last, err)
		}
	}
}

func TestFileRoundTrip(t *testing.T) {
	snap := Snapshot{Source: SourceOpenRouter, URL: OpenRouterURL, FetchedAt: time.Unix(5, 0).UTC(), SHA256: strings.Repeat("a", 64), Count: 1, Offers: []Offer{{Model: "fake/model-a", ProviderType: "openrouter"}}}
	b, err := EncodeFile([]Snapshot{snap})
	if err != nil {
		t.Fatal(err)
	}
	f, err := DecodeFile(strings.NewReader(string(b)))
	if err != nil || len(f.Snapshots) != 1 || f.Snapshots[0].Offers[0].Model != "fake/model-a" {
		t.Fatalf("round trip: %+v, %v", f, err)
	}
	for name, bad := range map[string]string{
		"unknown field":  strings.Replace(string(b), `"snapshots"`, `"extra":1,"snapshots"`, 1),
		"unknown source": strings.Replace(string(b), SourceOpenRouter, "evil", 1),
		"not json":       "nope",
	} {
		if _, err := DecodeFile(strings.NewReader(bad)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
