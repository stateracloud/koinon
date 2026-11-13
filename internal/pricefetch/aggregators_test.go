// SPDX-License-Identifier: Apache-2.0

package pricefetch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stateracloud/koinon/pricing"
)

var t0 = time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)

func TestPerMillionDirect(t *testing.T) {
	for _, c := range []struct {
		in, want string
		bad      bool
	}{
		{"3", "3", false},
		{"0.25", "0.25", false},
		{"1.5e-1", "0.15", false},
		{"0.000001", "0.000001", false},
		{"0", "0", false},
		{"0.0000001", "", true},
		{"-1", "", true},
		{"x", "", true},
	} {
		got, err := perMillionDirect(c.in)
		if c.bad != (err != nil) || got != c.want {
			t.Errorf("perMillionDirect(%q) = %q, %v; want %q bad=%v", c.in, got, err, c.want, c.bad)
		}
	}
}

// litellmBody has fake model ids only. 0.1 + 0.2 style floats are the point of the decimal rows:
// float64 arithmetic on 1.1e-06 * 1e6 gives 1.0999999999999999.
const litellmBody = `{
 "sample_spec": {"litellm_provider": "one of https://docs.litellm.ai/docs/providers", "mode": "one of chat"},
 "fake-model-a": {"litellm_provider": "openai", "mode": "chat", "input_cost_per_token": 1.1e-06, "output_cost_per_token": 0.0000044,
   "cache_read_input_token_cost": 2.75e-07, "max_output_tokens": 4096,
   "input_cost_per_token_above_200k_tokens": 2.2e-06, "output_cost_per_token_above_200k_tokens": 8.8e-06,
   "cache_creation_input_token_cost_above_1hr": 4e-06},
 "deepseek/fake-model-b": {"litellm_provider": "deepseek", "mode": "chat", "input_cost_per_token": 1e-07, "output_cost_per_token": 2e-07,
   "output_cost_per_reasoning_token": 2e-07},
 "fake-embed": {"litellm_provider": "openai", "mode": "embedding", "input_cost_per_token": 1e-07},
 "azure/fake-model-a": {"litellm_provider": "azure", "mode": "chat", "input_cost_per_token": 9e-06, "output_cost_per_token": 9e-06},
 "fake-model-neg": {"litellm_provider": "anthropic", "mode": "chat", "input_cost_per_token": -1, "output_cost_per_token": 1e-06}
}`

func TestParseLiteLLM(t *testing.T) {
	s, err := ParseLiteLLM([]byte(litellmBody), t0)
	if err != nil {
		t.Fatal(err)
	}
	if s.Source != SourceLiteLLM || s.Count != 5 || s.Licence != LicenceLiteLLM {
		t.Errorf("snapshot %+v", s)
	}
	by := map[string]Offer{}
	for _, o := range s.Offers {
		by[o.ProviderType+"/"+o.Model] = o
	}
	if len(by) != 3 {
		t.Fatalf("offers %v: want the openai, deepseek and anthropic chat models only", by)
	}
	a := by["openai/fake-model-a"]
	if w := a.Spec.Prices; w.Input != "1.1" || w.Output != "4.4" || w.CacheRead != "0.275" {
		t.Errorf("rates %+v: the decimal text must scale exactly", w)
	}
	if len(a.Spec.Tiers) != 1 || a.Spec.Tiers[0].ContextOver != 200000 || a.Spec.Tiers[0].Prices != (pricing.Wire{Input: "2.2", Output: "8.8"}) {
		t.Errorf("tiers %+v", a.Spec.Tiers)
	}
	if a.MaxOutput != 4096 || a.Ignored["cache_creation_input_token_cost_above_1hr"] != "4e-06" || len(a.Problems) != 0 {
		t.Errorf("offer %+v", a)
	}
	if b := by["deepseek/fake-model-b"]; b.Spec.Prices.Reasoning != "" {
		t.Errorf("a reasoning rate equal to output is no separate bucket: %+v", b.Spec)
	}
	if n := by["anthropic/fake-model-neg"]; len(n.Problems) == 0 {
		t.Errorf("a negative price must be a problem: %+v", n)
	}
}

func TestParseLiteLLMStrict(t *testing.T) {
	for name, body := range map[string]string{
		"not an object": `[]`, "trailing": `{} {}`, "row not an object": `{"fake-model-a": 3}`, "truncated": `{"a":`,
	} {
		if _, err := ParseLiteLLM([]byte(body), t0); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

const genaiBody = `[
 {"id": "openai", "name": "Fake", "models": [
   {"id": "fake-model-a", "prices": {"input_mtok": 1.1, "output_mtok": 4.4, "cache_read_mtok": 0.275, "input_audio_mtok": 9}},
   {"id": "fake-model-tiered", "prices": {"input_mtok": {"base": 1, "tiers": [{"start": 200000, "price": 2}]}, "output_mtok": 4}},
   {"id": "fake-model-dated", "prices": [{"constraint": {"start_datetime": "2026-01-01T00:00:00Z"}, "prices": {"input_mtok": 1, "output_mtok": 2}}]}
 ]},
 {"id": "gemini", "models": [{"id": "fake-model-g", "prices": {"input_mtok": 1, "output_mtok": 2}}]}
]`

func TestParseGenaiPrices(t *testing.T) {
	s, err := ParseGenaiPrices([]byte(genaiBody), t0)
	if err != nil {
		t.Fatal(err)
	}
	if s.Source != SourceGenaiPrices || s.Count != 4 || len(s.Offers) != 3 {
		t.Fatalf("snapshot %+v", s)
	}
	by := map[string]Offer{}
	for _, o := range s.Offers {
		by[o.Model] = o
	}
	if a := by["fake-model-a"]; a.Spec.Prices != (pricing.Wire{Input: "1.1", Output: "4.4", CacheRead: "0.275"}) || a.Ignored["input_audio_mtok"] != "9" {
		t.Errorf("fake-model-a %+v", a)
	}
	if b := by["fake-model-tiered"]; b.Spec.Prices.Input != "1" || len(b.Spec.Tiers) != 1 || b.Spec.Tiers[0] != (pricing.WireTier{ContextOver: 200000, Prices: pricing.Wire{Input: "2"}}) {
		t.Errorf("fake-model-tiered %+v", b)
	}
	if c := by["fake-model-dated"]; len(c.Problems) == 0 {
		t.Errorf("a date-dependent price must be a problem: %+v", c)
	}
	if _, err := ParseGenaiPrices([]byte(`{}`), t0); err == nil {
		t.Error("an object is not a list of providers")
	}
}

const modelsDevBody = `{
 "openai": {"models": {
   "fake-model-a": {"id": "fake-model-a", "cost": {"input": 1.1, "output": 4.4, "cache_read": 0.275, "input_audio": 7,
      "context_over_200k": {"input": 2.2, "output": 8.8}}, "limit": {"output": 4096}},
   "fake-free": {"id": "fake-free"}
 }},
 "openrouter": {"models": {"fake/model-a": {"cost": {"input": 1, "output": 2}}}}
}`

func TestParseModelsDev(t *testing.T) {
	s, err := ParseModelsDev([]byte(modelsDevBody), t0)
	if err != nil {
		t.Fatal(err)
	}
	if s.Source != SourceModelsDev || s.Count != 3 || len(s.Offers) != 1 {
		t.Fatalf("snapshot %+v", s)
	}
	o := s.Offers[0]
	if o.Model != "fake-model-a" || o.ProviderType != "openai" || o.Spec.Prices != (pricing.Wire{Input: "1.1", Output: "4.4", CacheRead: "0.275"}) ||
		len(o.Spec.Tiers) != 1 || o.Spec.Tiers[0].ContextOver != 200000 || o.MaxOutput != 4096 || o.Ignored["input_audio"] != "7" {
		t.Errorf("offer %+v", o)
	}
}

// ghServer serves the commits route and the raw file the way GitHub does, for one repository. The
// commit list ignores the until parameter on purpose: the adapter must check the date itself.
type ghServer struct {
	srv     *httptest.Server
	commits atomic.Value // string: the JSON the commits route returns
	file    atomic.Value // string: the body of the file
	rawPath atomic.Value // the last raw path asked
	hits    atomic.Int32
}

func newGHServer(t *testing.T, repo, file string) *ghServer {
	t.Helper()
	g := &ghServer{}
	g.commits.Store("[]")
	g.file.Store("{}")
	g.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.hits.Add(1)
		switch {
		case r.URL.Path == "/repos/"+repo+"/commits":
			if r.URL.Query().Get("path") != file || r.URL.Query().Get("until") == "" {
				http.Error(w, "bad query", http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(g.commits.Load().(string)))
		case strings.HasPrefix(r.URL.Path, "/"+repo+"/"):
			g.rawPath.Store(r.URL.Path)
			_, _ = w.Write([]byte(g.file.Load().(string)))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func commitJSON(sha string, at time.Time) string {
	return fmt.Sprintf(`[{"sha":%q,"commit":{"committer":{"date":%q}}}]`, sha, at.UTC().Format(time.RFC3339))
}

// commitsJSON is a commit list, newest first, as pairs of sha and age before t0.
func commitsJSON(pairs ...any) string {
	var out []string
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, strings.Trim(commitJSON(pairs[i].(string), t0.Add(-pairs[i+1].(time.Duration))), "[]"))
	}
	return "[" + strings.Join(out, ",") + "]"
}

const (
	shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func (g *ghServer) liteLLM(t *testing.T) *LiteLLM {
	t.Helper()
	l := NewLiteLLM(g.srv.Client())
	g.point(t, &l.gh)
	return l
}

func (g *ghServer) point(t *testing.T, f *githubFile) {
	t.Helper()
	host, port := mustHost(t, g.srv.URL), mustPort(t, g.srv.URL)
	f.api, f.raw = g.srv.URL, g.srv.URL
	f.apiPin, f.rawPin = pin{host: host, port: port}, pin{host: host, port: port}
}

func TestLiteLLMPinsTheCommit(t *testing.T) {
	g := newGHServer(t, liteLLMRepo, liteLLMFile)
	g.file.Store(litellmBody)
	l := g.liteLLM(t)

	// The cooldown boundary: 23h59m is too young, 24h is old enough.
	g.commits.Store(commitJSON(shaA, t0.Add(-24*time.Hour+time.Minute)))
	if _, err := l.Fetch(context.Background(), t0); !errors.Is(err, ErrNoOldCommit) {
		t.Fatalf("a commit of 23h59m: err %v, want ErrNoOldCommit", err)
	}
	if g.rawPath.Load() != nil {
		t.Fatal("no file may be read while the newest commit is in its cooldown")
	}
	g.commits.Store(commitJSON(shaA, t0.Add(-24*time.Hour)))
	snap, err := l.Fetch(context.Background(), t0)
	if err != nil {
		t.Fatal(err)
	}
	if snap.CommitSHA != shaA || !snap.CommittedAt.Equal(t0.Add(-24*time.Hour)) || snap.Count != 5 {
		t.Errorf("snapshot %+v", snap)
	}
	if want := "/" + liteLLMRepo + "/" + shaA + "/" + liteLLMFile; g.rawPath.Load() != want || !strings.HasSuffix(snap.URL, want) {
		t.Errorf("the file must be read at the commit: raw path %v, URL %s", g.rawPath.Load(), snap.URL)
	}

	// A commit reverted within a day. The bad commit X (shaB) landed 30h ago and its revert 20h
	// ago, so X is old enough but was replaced within a day: it is never pinned, the commit before
	// it is. The revert itself is still in its cooldown until it is 24h old.
	const shaC = "cccccccccccccccccccccccccccccccccccccccc"
	g.commits.Store(commitsJSON(shaC, 20*time.Hour, shaB, 30*time.Hour, shaA, 100*time.Hour))
	if snap, err = l.Fetch(context.Background(), t0); err != nil || snap.CommitSHA != shaA {
		t.Errorf("a commit reverted within a day must be skipped: %v %+v", err, snap)
	}
	// Once the revert is a day old it is the pin, and the bad commit is still never read.
	g.commits.Store(commitsJSON(shaC, 25*time.Hour, shaB, 35*time.Hour, shaA, 100*time.Hour))
	if snap, err = l.Fetch(context.Background(), t0); err != nil || snap.CommitSHA != shaC {
		t.Errorf("the revert is the pin once it is a day old: %v %+v", err, snap)
	}
	// A 1h-old commit alone is in the cooldown.
	g.commits.Store(commitJSON(shaB, t0.Add(-time.Hour)))
	if _, err := l.Fetch(context.Background(), t0); !errors.Is(err, ErrNoOldCommit) {
		t.Errorf("a commit of 1h: err %v, want ErrNoOldCommit", err)
	}
	// The stability boundary: the newest commit (shaC) is still in its cooldown, so the pin is
	// shaB when its successor followed it by a day or more, and shaA when it followed within 23h59m.
	g.commits.Store(commitsJSON(shaC, 23*time.Hour, shaB, 47*time.Hour, shaA, 100*time.Hour))
	if snap, err = l.Fetch(context.Background(), t0); err != nil || snap.CommitSHA != shaB {
		t.Errorf("successor exactly 24h later: %v %+v", err, snap)
	}
	g.commits.Store(commitsJSON(shaC, 23*time.Hour, shaB, 46*time.Hour+59*time.Minute, shaA, 100*time.Hour))
	if snap, err = l.Fetch(context.Background(), t0); err != nil || snap.CommitSHA != shaA {
		t.Errorf("successor 23h59m later: %v %+v", err, snap)
	}
	// Every old commit was replaced within a day: nothing is pinned.
	g.commits.Store(commitsJSON(shaC, 40*time.Hour, shaB, 50*time.Hour))
	if snap, err = l.Fetch(context.Background(), t0); err != nil || snap.CommitSHA != shaC {
		t.Errorf("the newest commit has no successor: %v %+v", err, snap)
	}
	g.commits.Store(commitsJSON(shaC, 25*time.Hour, shaB, 30*time.Hour, shaA, 40*time.Hour))
	if snap, err = l.Fetch(context.Background(), t0); err != nil || snap.CommitSHA != shaC {
		t.Errorf("newest old commit: %v %+v", err, snap)
	}
	g.commits.Store(commitsJSON(shaC, 23*time.Hour, shaB, 30*time.Hour, shaA, 40*time.Hour))
	if _, err := l.Fetch(context.Background(), t0); !errors.Is(err, ErrNoOldCommit) {
		t.Errorf("every old commit replaced within a day: err %v, want ErrNoOldCommit", err)
	}

	// Things that are not a commit.
	for name, body := range map[string]string{
		"empty list": `[]`, "not a sha": commitJSON("main", t0.Add(-48*time.Hour)), "short sha": commitJSON("abc", t0.Add(-48*time.Hour)),
		"no date": `[{"sha":"` + shaA + `","commit":{"committer":{}}}]`, "not json": `<html>`,
	} {
		g.commits.Store(body)
		if _, err := l.Fetch(context.Background(), t0); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestGithubFetchRules(t *testing.T) {
	var redirect atomic.Bool
	var big atomic.Bool
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/commits"):
			_, _ = w.Write([]byte(commitJSON(shaA, t0.Add(-48*time.Hour))))
		case redirect.Load():
			http.Redirect(w, r, "https://example.invalid/", http.StatusFound)
		case big.Load():
			_, _ = w.Write([]byte(`{"pad":"` + strings.Repeat("x", MaxBodyBytes) + `"}`))
		default:
			_, _ = w.Write([]byte(`{"a":1}`))
		}
	}))
	defer srv.Close()
	g := &ghServer{srv: srv}
	l := NewLiteLLM(srv.Client())
	g.point(t, &l.gh)

	redirect.Store(true)
	if _, err := l.Fetch(context.Background(), t0); err == nil || !strings.Contains(err.Error(), "redirected") {
		t.Errorf("a redirect must be refused, got %v", err)
	}
	redirect.Store(false)
	big.Store(true)
	if _, err := l.Fetch(context.Background(), t0); err == nil || !strings.Contains(err.Error(), "16 MiB") {
		t.Errorf("a body over the cap must be refused, got %v", err)
	}

	// The pinned hosts: production adapters ask only api.github.com and raw.githubusercontent.com,
	// and a test adapter pointed at another host asks nothing.
	if l := NewLiteLLM(nil); l.gh.apiPin.host != "api.github.com" || l.gh.rawPin.host != "raw.githubusercontent.com" || l.gh.api != "https://api.github.com" || l.gh.raw != "https://raw.githubusercontent.com" {
		t.Errorf("the production adapter is pinned: %+v", l.gh)
	}
	bad := NewGenaiPrices(srv.Client())
	g.point(t, &bad.gh)
	bad.gh.api = strings.Replace(srv.URL, "https", "http", 1)
	if _, err := bad.Fetch(context.Background(), t0); err == nil {
		t.Error("plain http must be refused")
	}
	bad.gh.api = srv.URL
	bad.gh.apiPin.host = "api.github.com"
	if _, err := bad.Fetch(context.Background(), t0); err == nil {
		t.Error("another host must be refused")
	}
}

func TestBelowFloor(t *testing.T) {
	s, err := ParseLiteLLM([]byte(litellmBody), t0)
	if err != nil {
		t.Fatal(err)
	}
	var re *RefusedError
	if err := CheckFloor(s, NewLiteLLM(nil).MinModels(), 0); !errors.As(err, &re) {
		t.Errorf("a 5-model body is below the floor, got %v", err)
	}
	if err := CheckFloor(Snapshot{Source: SourceLiteLLM, Count: 400}, LiteLLMMinModels, 1000); !errors.As(err, &re) {
		t.Errorf("fewer than half the last good fetch's models, got %v", err)
	}
	for _, a := range NewAdapters(nil) {
		if a.Name() == "" || minModelsOf(a) < 1 {
			t.Errorf("adapter %T: name %q floor %d", a, a.Name(), minModelsOf(a))
		}
	}
}

func minModelsOf(a Adapter) int {
	if f, ok := a.(interface{ MinModels() int }); ok {
		return f.MinModels()
	}
	return 0
}

func TestModelsDevFetch(t *testing.T) {
	var redirect atomic.Bool
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if redirect.Load() {
			http.Redirect(w, r, "https://example.invalid/", http.StatusFound)
			return
		}
		w.Header().Set("ETag", `"m1"`)
		_, _ = w.Write([]byte(modelsDevBody))
	}))
	defer srv.Close()
	m := NewModelsDev(srv.Client())
	m.endpoint, m.pin = srv.URL, pin{host: mustHost(t, srv.URL), port: mustPort(t, srv.URL)}
	snap, err := m.Fetch(context.Background(), t0)
	if err != nil || snap.ETag != `"m1"` || snap.CommitSHA != "" || len(snap.Offers) != 1 {
		t.Fatalf("%v %+v", err, snap)
	}
	redirect.Store(true)
	if _, err := m.Fetch(context.Background(), t0); err == nil || !strings.Contains(err.Error(), "redirected") {
		t.Errorf("a redirect must be refused, got %v", err)
	}
	if p := NewModelsDev(nil); p.endpoint != ModelsDevURL || p.pin.host != "models.dev" {
		t.Errorf("the production adapter is pinned: %+v", p)
	}
}

// TestSourceTables: every source has provider types and a licence, models.dev can never auto-apply,
// and the aggregators auto-apply only for providers they track first-hand.
func TestSourceTables(t *testing.T) {
	for _, s := range Sources {
		if len(ProviderTypesOf(s)) == 0 || NewAdapter(s, nil) == nil || NewAdapter(s, nil).Name() != s || SourceURL(s) == "" {
			t.Errorf("source %s is incomplete", s)
		}
	}
	for _, pt := range ProviderTypesOf(SourceModelsDev) {
		if Agrees(pt, SourceModelsDev, []string{SourceModelsDev, SourceLiteLLM, SourceGenaiPrices}) {
			t.Errorf("models.dev must never agree (%s)", pt)
		}
	}
	for _, c := range []struct {
		pt, source string
		want       bool
	}{
		{"openrouter", SourceOpenRouter, true},
		{"openrouter", SourceLiteLLM, false},
		{"openrouter", SourceGenaiPrices, false},
		{"anthropic", SourceLiteLLM, true},
		{"openai", SourceGenaiPrices, true},
		{"deepseek", SourceLiteLLM, true},
		{"anthropic", SourceOpenRouter, false},
		{"fake-type", SourceLiteLLM, false},
		{"fake-type", SourceGenaiPrices, false},
	} {
		if got := Agrees(c.pt, c.source, nil); got != c.want {
			t.Errorf("Agrees(%s, %s) = %v, want %v", c.pt, c.source, got, c.want)
		}
	}
	if got := PricedProviderTypes(); strings.Join(got, ",") != "anthropic,deepseek,openai,openrouter" {
		t.Errorf("priced provider types %v", got)
	}
}
