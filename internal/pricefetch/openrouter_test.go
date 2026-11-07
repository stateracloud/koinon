// SPDX-License-Identifier: Apache-2.0

package pricefetch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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
	  {"min_prompt_tokens":200001,"prompt":"0.000002","completion":"0.000003"},
	  {"utc_days":[1],"utc_start":"00:00","utc_end":"06:00","prompt":"0.0000005"},
	  {"batch":true,"prompt":"0.0000005"}]}}`)
	o := offer(t, s, "fake/tiered")
	if len(o.Spec.Tiers) != 1 {
		t.Fatalf("only the prompt-size override is a tier: %+v (problems %v)", o.Spec.Tiers, o.Problems)
	}
	if got, want := o.Spec.Tiers[0].ContextOver, int64(200001+TierBoundaryOffset); got != want {
		t.Errorf("context_over %d, want min_prompt_tokens%+d = %d", got, TierBoundaryOffset, want)
	}
	if o.Spec.Tiers[0].Prices.Input != "2" || o.Spec.Tiers[0].Prices.Output != "3" {
		t.Errorf("tier rates: %+v", o.Spec.Tiers[0].Prices)
	}
	if o.Ignored["overrides[1].skipped"] == "" || o.Ignored["overrides[2].skipped"] == "" {
		t.Errorf("time-of-day and batch overrides are recorded as skipped: %v", o.Ignored)
	}
}

// TestTierBoundaryOffset pins the open question of ADR 0098 (is min_prompt_tokens the first token
// of the tier or the last of the one below?). Changing the constant must be a decision, not a drift.
func TestTierBoundaryOffset(t *testing.T) {
	if TierBoundaryOffset != -1 {
		t.Errorf("TierBoundaryOffset = %d; the tier reading is unverified (issue #265) and changing it changes every tiered price", TierBoundaryOffset)
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
