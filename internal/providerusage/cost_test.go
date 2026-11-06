// SPDX-License-Identifier: Apache-2.0

package providerusage_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stateracloud/koinon/internal/money"
	"github.com/stateracloud/koinon/internal/providerusage"
)

const adminKey = "koinon_test_not-a-real-token"

var costDay = time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)

func TestAnthropicCostSumsTheWorkspacesDayInCents(t *testing.T) {
	var gotKey, gotVersion string
	var queries []string
	client, seen := fakeOpenRouter(t, func(w http.ResponseWriter, r *http.Request) {
		gotKey, gotVersion = r.Header.Get("X-Api-Key"), r.Header.Get("Anthropic-Version")
		queries = append(queries, r.URL.RawQuery)
		if r.URL.Query().Get("page") == "" {
			// amount is in cents; another workspace and the default one are not this key's.
			_, _ = w.Write([]byte(`{"data":[{"starting_at":"2026-11-05T00:00:00Z","ending_at":"2026-11-06T00:00:00Z","results":[
				{"currency":"USD","amount":"123.5","workspace_id":"wrkspc_koinon"},
				{"currency":"USD","amount":"999","workspace_id":"wrkspc_other"},
				{"currency":"USD","amount":"7","workspace_id":null}]}],"has_more":true,"next_page":"page_2"}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"results":[{"currency":"USD","amount":"0.5","workspace_id":"wrkspc_koinon"}]}],"has_more":false,"next_page":null}`))
	})
	src, ok := providerusage.DefaultCosts(client).For("anthropic", "https://api.anthropic.com")
	if !ok {
		t.Fatal("anthropic has a cost source")
	}
	got, err := src.DayCost(context.Background(), providerusage.CostProvider{AdminKey: adminKey, Scope: "wrkspc_koinon"}, costDay.Add(5*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got != money.MustParse("1.24") {
		t.Fatalf("cost %s, want 1.24 (124 cents over two pages)", got)
	}
	if gotKey != adminKey || gotVersion == "" {
		t.Fatalf("headers: key %q version %q", gotKey, gotVersion)
	}
	if len(*seen) != 2 || (*seen)[0][:len(providerusage.AnthropicCostURL)] != providerusage.AnthropicCostURL {
		t.Fatalf("asked %v, not the pinned endpoint twice", *seen)
	}
	if want := "bucket_width=1d&ending_at=2026-11-06T00%3A00%3A00Z&group_by%5B%5D=workspace_id&starting_at=2026-11-05T00%3A00%3A00Z"; queries[0] != want {
		t.Fatalf("query %q, want %q", queries[0], want)
	}
	def, err := src.DayCost(context.Background(), providerusage.CostProvider{AdminKey: adminKey, Scope: "default"}, costDay)
	if err != nil || def != money.MustParse("0.07") {
		t.Fatalf("default workspace: %s %v", def, err)
	}
}

func TestOpenAICostSumsTheProjectsDayInDollars(t *testing.T) {
	var gotAuth string
	client, seen := fakeOpenRouter(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Query().Get("start_time") != "1793836800" || r.URL.Query().Get("end_time") != "1793923200" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"object":"page","data":[{"object":"bucket","results":[
			{"object":"organization.costs.result","amount":{"value":0.25,"currency":"usd"},"line_item":null,"project_id":"proj_koinon"},
			{"object":"organization.costs.result","amount":{"value":1.5e-2,"currency":"usd"},"line_item":null,"project_id":"proj_koinon"},
			{"object":"organization.costs.result","amount":{"value":4,"currency":"usd"},"line_item":null,"project_id":"proj_other"}]}],
			"has_more":false,"next_page":null}`))
	})
	src, ok := providerusage.DefaultCosts(client).For("openai", "https://api.openai.com/v1")
	if !ok {
		t.Fatal("openai has a cost source")
	}
	got, err := src.DayCost(context.Background(), providerusage.CostProvider{AdminKey: adminKey, Scope: "proj_koinon"}, costDay)
	if err != nil {
		t.Fatal(err)
	}
	if got != money.MustParse("0.265") || gotAuth != "Bearer "+adminKey {
		t.Fatalf("cost %s auth %q", got, gotAuth)
	}
	if len(*seen) != 1 || (*seen)[0][:len(providerusage.OpenAICostURL)] != providerusage.OpenAICostURL {
		t.Fatalf("asked %v", *seen)
	}
}

func TestCostSourcesServeOnlyTheProvidersOwnAPI(t *testing.T) {
	r := providerusage.DefaultCosts(nil)
	for _, c := range []struct {
		typ, url string
		ok       bool
	}{
		{"anthropic", "https://api.anthropic.com", true},
		{"anthropic", "https://api.anthropic.com/v1/", true},
		{"anthropic", "https://proxy.example/anthropic", false},
		{"openai", "https://api.openai.com/v1", true},
		{"openai", "http://api.openai.com/v1", false},
		{"openai", "https://my-azure.openai.azure.com/v1", false},
		{"openrouter", "https://openrouter.ai/api/v1", false}, // settled per generation instead
	} {
		if _, ok := r.For(c.typ, c.url); ok != c.ok {
			t.Errorf("%s %s: %v", c.typ, c.url, ok)
		}
	}
	a, o := providerusage.AnthropicCost{}, providerusage.OpenAICost{}
	if !a.ValidScope("default") || !a.ValidScope("wrkspc_01abc") || a.ValidScope("proj_x") || a.ValidScope("") {
		t.Error("anthropic scopes")
	}
	if !o.ValidScope("proj_abc123") || o.ValidScope("default") || o.ValidScope("proj_../x") {
		t.Error("openai scopes")
	}
}

func TestCostSourcesRefuseRedirectsAndReportLimits(t *testing.T) {
	client, seen := fakeOpenRouter(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/organization/costs" {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		http.Redirect(w, r, "https://elsewhere.example/steal", http.StatusFound)
	})
	follows := *client
	follows.CheckRedirect = nil
	p := providerusage.CostProvider{AdminKey: adminKey, Scope: "default"}
	if _, err := (providerusage.AnthropicCost{Client: &follows}).DayCost(context.Background(), p, costDay); err == nil {
		t.Fatal("a redirect is an error")
	}
	if len(*seen) != 1 {
		t.Fatalf("followed the redirect: %v", *seen)
	}
	var ra *providerusage.RetryAfterError
	_, err := (providerusage.OpenAICost{Client: client}).DayCost(context.Background(), providerusage.CostProvider{AdminKey: adminKey, Scope: "proj_a"}, costDay)
	if !errors.As(err, &ra) || ra.After != 30*time.Second {
		t.Fatalf("429: %v", err)
	}
}
