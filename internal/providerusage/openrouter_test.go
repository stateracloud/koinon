// SPDX-License-Identifier: Apache-2.0

package providerusage_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stateracloud/koinon/internal/money"
	"github.com/stateracloud/koinon/internal/providerusage"
)

// roundTripFunc lets a test stand in for the network.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// fakeOpenRouter serves h over TLS and returns a client that sends every request to it, recording
// the URL each request asked for.
func fakeOpenRouter(t *testing.T, h http.HandlerFunc) (*http.Client, *[]string) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	target, _ := url.Parse(srv.URL)
	seen := &[]string{}
	inner := srv.Client().Transport
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		*seen = append(*seen, r.URL.String())
		r2 := r.Clone(r.Context())
		r2.URL.Scheme, r2.URL.Host = target.Scheme, target.Host
		return inner.RoundTrip(r2)
	})}, seen
}

const orURL = "https://openrouter.ai/api/v1"

func TestOpenRouterReadsTheGenerationsBill(t *testing.T) {
	const key = "koinon_test_not-a-real-token"
	var gotAuth, gotPath, gotID string
	client, seen := fakeOpenRouter(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath, gotID = r.Header.Get("Authorization"), r.URL.Path, r.URL.Query().Get("id")
		switch gotID {
		case "gen-ready-1":
			// Fields as documented; total_cost may come in exponent form.
			_, _ = w.Write([]byte(`{"data":{"id":"gen-ready-1","total_cost":1.234e-4,"usage":1.234e-4,"cancelled":true,
				"tokens_prompt":1200,"tokens_completion":40,"native_tokens_prompt":1250,"native_tokens_completion":42,
				"native_tokens_reasoning":10,"native_tokens_cached":1000}}`))
		case "gen-byok-1":
			// The org's own upstream key: total_cost is OpenRouter's fee, the upstream billed the rest.
			_, _ = w.Write([]byte(`{"data":{"id":"gen-byok-1","total_cost":0.00005,"is_byok":true,"upstream_inference_cost":0.001,
				"native_tokens_prompt":100,"native_tokens_completion":10}}`))
		case "gen-teapot-1":
			w.WriteHeader(http.StatusTeapot)
		case "gen-busy-1":
			w.Header().Set("Retry-After", "42")
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	src, ok := providerusage.Default(client).For("openrouter", orURL, "gen-ready-1")
	if !ok {
		t.Fatal("openrouter has a source")
	}
	p := providerusage.Provider{Type: "openrouter", URL: orURL + "/", APIKey: key}
	u, err := src.Usage(context.Background(), p, "gen-ready-1")
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer "+key || gotPath != "/api/v1/generation" || gotID != "gen-ready-1" {
		t.Fatalf("request: %q %q %q", gotAuth, gotPath, gotID)
	}
	if (*seen)[0] != providerusage.OpenRouterGenerationURL+"?id=gen-ready-1" {
		t.Fatalf("asked %q, not the pinned endpoint", (*seen)[0])
	}
	want := providerusage.Usage{InputTokens: 1250, OutputTokens: 42, CacheReadTokens: 1000, ReasoningTokens: 10, Cost: money.MustParse("0.0001234"), Cancelled: true}
	if u != want {
		t.Fatalf("got %+v, want %+v", u, want)
	}
	if b, err := u.Billed(); err != nil || b != money.MustParse("0.0001234") {
		t.Fatalf("billed without BYOK is total_cost: %s %v", b, err)
	}
	byok, err := src.Usage(context.Background(), p, "gen-byok-1")
	if err != nil {
		t.Fatal(err)
	}
	if !byok.BYOK || byok.Cost != money.MustParse("0.00005") || byok.UpstreamCost != money.MustParse("0.001") {
		t.Fatalf("BYOK generation: %+v", byok)
	}
	if b, err := byok.Billed(); err != nil || b != money.MustParse("0.00105") {
		t.Fatalf("billed with BYOK is the fee plus the upstream's cost: %s %v", b, err)
	}
	if _, err := src.Usage(context.Background(), p, "gen-later-1"); !errors.Is(err, providerusage.ErrPending) {
		t.Fatalf("404 is pending: %v", err)
	}
	var ra *providerusage.RetryAfterError
	if _, err := src.Usage(context.Background(), p, "gen-busy-1"); !errors.As(err, &ra) || ra.After != 42*time.Second {
		t.Fatalf("429 asks to wait its Retry-After: %v", err)
	}
	if _, err := src.Usage(context.Background(), p, "gen-teapot-1"); err == nil || errors.Is(err, providerusage.ErrPending) {
		t.Fatalf("another status is an error: %v", err)
	}
}

func TestRegistryOnlyForProvidersURLsAndIDsItKnows(t *testing.T) {
	r := providerusage.Default(nil)
	for _, c := range []struct {
		typ, url, id string
		ok           bool
	}{
		{"openrouter", orURL, "gen-abc-123", true},
		{"openrouter", "https://OpenRouter.ai:443/api/v1/", "gen-abc-123", true},
		{"openrouter", orURL, "chatcmpl-1", false}, // not a generation ID
		{"openrouter", orURL, "", false},
		{"openrouter", orURL, "gen-../../x", false},
		{"openrouter", "http://openrouter.ai/api/v1", "gen-abc-123", false}, // not https
		{"openrouter", "https://openrouter.ai.evil.example/v1", "gen-abc-123", false},
		{"openrouter", "https://internal.svc:8080/v1", "gen-abc-123", false}, // an "openrouter" provider elsewhere
		{"openrouter", "https://user@openrouter.ai/api/v1", "gen-abc-123", false},
		{"openai", orURL, "gen-abc-123", false}, // no source: the logged usage is final
	} {
		if _, ok := r.For(c.typ, c.url, c.id); ok != c.ok {
			t.Errorf("%s %s %q: %v", c.typ, c.url, c.id, ok)
		}
	}
}

func TestOpenRouterNeverContactsAnotherURLNorFollowsARedirect(t *testing.T) {
	const key = "koinon_test_not-a-real-token"
	client, seen := fakeOpenRouter(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://elsewhere.example/steal", http.StatusFound)
	})
	follows := *client
	follows.CheckRedirect = nil // a caller's client that would follow; the source must not
	src := providerusage.OpenRouter{Client: &follows}

	if _, err := src.Usage(context.Background(), providerusage.Provider{URL: "https://internal.svc/v1", APIKey: key}, "gen-x-1"); err == nil {
		t.Fatal("a provider URL that is not OpenRouter's is refused")
	}
	if len(*seen) != 0 {
		t.Fatalf("contacted %v", *seen)
	}
	if _, err := src.Usage(context.Background(), providerusage.Provider{URL: orURL, APIKey: key}, "gen-x-1"); err == nil || errors.Is(err, providerusage.ErrPending) {
		t.Fatalf("a redirect is an error: %v", err)
	}
	if len(*seen) != 1 {
		t.Fatalf("followed the redirect: %v", *seen)
	}
}
