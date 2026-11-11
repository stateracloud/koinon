// SPDX-License-Identifier: Apache-2.0

package pricefetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const metaBody = `{"data":[
 {"id":"fake/model-a","name":"Fake: Model A","description":"  A fake model.  ","created":1700000000,
  "context_length":200000,
  "architecture":{"input_modalities":["text","image","text"],"output_modalities":["text"],"tokenizer":"Claude"},
  "supported_parameters":["tools","temperature",""],
  "top_provider":{"context_length":100000,"max_completion_tokens":64000},
  "pricing":{"prompt":"0.000001","completion":"0.000002"}},
 {"id":"fake/model-b","context_length":-5,"created":"soon",
  "top_provider":{"context_length":32000,"max_completion_tokens":null},
  "pricing":{"prompt":"-1","completion":"-1"}},
 {"id":"bad name","context_length":1,"pricing":{}}
]}`

func TestParseOpenRouterMeta(t *testing.T) {
	snap, err := ParseOpenRouter([]byte(metaBody), time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Meta) != 2 {
		t.Fatalf("want 2 models with metadata (the unusable name is skipped), got %+v", snap.Meta)
	}
	a := snap.Meta[0]
	if a.Model != "fake/model-a" || a.Name != "Fake: Model A" || a.Description != "A fake model." || a.Created != 1700000000 {
		t.Errorf("display fields: %+v", a)
	}
	if strings.Join(a.InputModalities, ",") != "text,image" || a.Tokenizer != "Claude" || strings.Join(a.SupportedParameters, ",") != "tools,temperature" {
		t.Errorf("lists are cleaned and de-duplicated: %+v", a)
	}
	if a.ContextLength != 200000 || a.MaxOutput != 64000 {
		t.Errorf("limits: %+v", a)
	}
	// A model whose price is unusable still has metadata; a bad number is left out, the other
	// context length fills in, and nothing fails the fetch.
	b := snap.Meta[1]
	if b.Model != "fake/model-b" || b.ContextLength != 32000 || b.MaxOutput != 0 || b.Created != 0 {
		t.Errorf("model b: %+v", b)
	}
	if len(snap.Offers) != 2 || len(snap.Offers[1].Problems) == 0 {
		t.Errorf("the price parse is the same one: %+v", snap.Offers)
	}
}

func TestSnapshotMetaIsNotInTheFile(t *testing.T) {
	snap, err := ParseOpenRouter([]byte(metaBody), time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	b, err := EncodeFile([]Snapshot{snap})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "Fake: Model A") || strings.Contains(string(b), "tokenizer") {
		t.Errorf("OpenRouter metadata has no offline path (ADR 0070 amendment, item 3): %s", b)
	}
}

const endpointsBody = `{"data":{"id":"fake/model-a","endpoints":[
 {"provider_name":"Fake Up","tag":"fake-up","quantization":"fp8","context_length":200000,"max_completion_tokens":64000,
  "max_prompt_tokens":null,"supports_implicit_caching":true,"supported_parameters":["tools"],
  "pricing":{"prompt":"0.000009","completion":"0.000009"}},
 {"provider_name":"Other","uptime_last_30m":250}
]}}`

func TestParseOpenRouterEndpoints(t *testing.T) {
	e, err := ParseOpenRouterEndpoints("fake/model-a", []byte(endpointsBody), time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(e.Endpoints) != 2 {
		t.Fatalf("%+v", e)
	}
	u := e.Endpoints[0]
	if u.Provider != "Fake Up" || u.Quantization != "fp8" || u.ContextLength != 200000 || !u.SupportsImplicitCaching {
		t.Errorf("endpoint: %+v", u)
	}
	for _, bad := range []string{`{}`, `{"data":null}`, `{"data":{}} x`, `nope`} {
		if _, err := ParseOpenRouterEndpoints("fake/model-a", []byte(bad), time.Now()); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}

func TestOpenRouterFetchEndpoints(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.EscapedPath()+"|"+r.Header.Get("Authorization"))
		mu.Unlock()
		if strings.Contains(r.URL.Path, "gone") {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(endpointsBody))
	}))
	defer srv.Close()
	o := &OpenRouter{client: srv.Client(), endpoint: srv.URL + "/api/v1/models", host: mustHost(t, srv.URL), port: mustPort(t, srv.URL), minModel: 1}

	e, err := o.FetchEndpoints(context.Background(), "fake/model-a:free", time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if e.Model != "fake/model-a:free" || len(e.Endpoints) != 2 {
		t.Errorf("%+v", e)
	}
	if _, err := o.FetchEndpoints(context.Background(), "fake/gone", time.Now()); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("a 404 is an error, got %v", err)
	}
	// A name that is not a model name never becomes a path.
	before := len(paths)
	for _, bad := range []string{"", "../etc/passwd x", "a b", strings.Repeat("a", 300)} {
		if _, err := o.FetchEndpoints(context.Background(), bad, time.Now()); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != before {
		t.Errorf("a refused name made a request: %v", paths[before:])
	}
	if paths[0] != "/api/v1/models/fake/model-a:free/endpoints|" {
		t.Errorf("the request must be an unauthenticated GET of the endpoints route, got %q", paths[0])
	}
}
