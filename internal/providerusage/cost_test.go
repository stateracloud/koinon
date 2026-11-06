// SPDX-License-Identifier: Apache-2.0

package providerusage_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
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
		_, _ = w.Write([]byte(`{"data":[{"starting_at":"2026-11-05T00:00:00Z","results":[{"currency":"USD","amount":"0.5","workspace_id":"wrkspc_koinon"}]}],"has_more":false,"next_page":null}`))
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
	if want := "bucket_width=1d&ending_at=2026-11-06T00%3A00%3A00Z&group_by%5B%5D=workspace_id&limit=1&starting_at=2026-11-05T00%3A00%3A00Z"; queries[0] != want {
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
		_, _ = w.Write([]byte(`{"object":"page","data":[{"object":"bucket","start_time":1793836800,"end_time":1793923200,"results":[
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
	if want := "bucket_width=1d&end_time=1793923200&group_by=project_id&limit=1&project_ids=proj_koinon&start_time=1793836800"; rawQueries(seen)[0] != want {
		t.Fatalf("query %q, want %q", rawQueries(seen)[0], want)
	}
}

// rawQueries returns the raw queries of the URLs asked.
func rawQueries(seen *[]string) []string {
	out := make([]string, 0, len(*seen))
	for _, u := range *seen {
		_, q, _ := strings.Cut(u, "?")
		out = append(out, q)
	}
	return out
}

func TestOpenAICostReadsOneAPIKeysDay(t *testing.T) {
	client, seen := fakeOpenRouter(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"object":"page","data":[{"object":"bucket","start_time":1793836800,"end_time":1793923200,"results":[
			{"object":"organization.costs.result","amount":{"value":0.5,"currency":"usd"},"api_key_id":"key_koinon"},
			{"object":"organization.costs.result","amount":{"value":3,"currency":"usd"},"api_key_id":"key_other"}]}],
			"has_more":false,"next_page":null}`))
	})
	got, err := (providerusage.OpenAICost{Client: client}).DayCost(context.Background(), providerusage.CostProvider{AdminKey: adminKey, Scope: "key_koinon"}, costDay)
	if err != nil || got != money.MustParse("0.5") {
		t.Fatalf("cost %s %v", got, err)
	}
	if want := "api_key_ids=key_koinon&bucket_width=1d&end_time=1793923200&group_by=api_key_id&limit=1&start_time=1793836800"; rawQueries(seen)[0] != want {
		t.Fatalf("query %q, want %q", rawQueries(seen)[0], want)
	}
}

// A report reads only the bucket of the day asked: another day's bucket counts for nothing, and a
// report without the day's bucket is an error, not $0. An empty bucket of the day is $0.
func TestCostSourcesReadOnlyTheDaysBucket(t *testing.T) {
	anthropic := func(body string) (money.USD, error) {
		client, _ := fakeOpenRouter(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) })
		return (providerusage.AnthropicCost{Client: client}).DayCost(context.Background(), providerusage.CostProvider{AdminKey: adminKey, Scope: "default"}, costDay)
	}
	openai := func(body string) (money.USD, error) {
		client, _ := fakeOpenRouter(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) })
		return (providerusage.OpenAICost{Client: client}).DayCost(context.Background(), providerusage.CostProvider{AdminKey: adminKey, Scope: "proj_a"}, costDay)
	}
	for _, c := range []struct {
		name string
		read func(string) (money.USD, error)
		body string
		want string // "" is an error
	}{
		{"anthropic: the day before only", anthropic, `{"data":[{"starting_at":"2026-11-04T00:00:00Z","results":[{"currency":"USD","amount":"500"}]}],"has_more":false}`, ""},
		{"anthropic: no bucket", anthropic, `{"data":[],"has_more":false}`, ""},
		{"anthropic: the day and the day after", anthropic, `{"data":[{"starting_at":"2026-11-05T00:00:00Z","results":[{"currency":"USD","amount":"100"}]},
			{"starting_at":"2026-11-06T00:00:00Z","results":[{"currency":"USD","amount":"900"}]}],"has_more":false}`, "1"},
		{"anthropic: the day, nothing used", anthropic, `{"data":[{"starting_at":"2026-11-05T00:00:00Z","results":[]}],"has_more":false}`, "0"},
		{"openai: the day before only", openai, `{"data":[{"start_time":1793750400,"results":[{"amount":{"value":5,"currency":"usd"},"project_id":"proj_a"}]}],"has_more":false}`, ""},
		{"openai: no bucket", openai, `{"data":[],"has_more":false}`, ""},
		{"openai: the day, nothing used", openai, `{"data":[{"start_time":1793836800,"results":[]}],"has_more":false}`, "0"},
	} {
		got, err := c.read(c.body)
		switch {
		case c.want == "" && err == nil:
			t.Errorf("%s: %s, want an error", c.name, got)
		case c.want != "" && (err != nil || got != money.MustParse(c.want)):
			t.Errorf("%s: %s %v, want %s", c.name, got, err, c.want)
		}
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
	if !o.ValidScope("proj_abc123") || !o.ValidScope("key_abc123") || o.ValidScope("default") || o.ValidScope("proj_../x") ||
		o.ValidScope("sk-proj-abc") || o.ValidScope("key_") {
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
