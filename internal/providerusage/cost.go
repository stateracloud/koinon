// SPDX-License-Identifier: Apache-2.0

package providerusage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/internal/money"
)

// A CostSource reads what a direct provider billed one part of an account for one UTC day, from the
// provider's cost API, with an admin key that is not the provider's inference key. The ledger's
// daily reconciliation compares it with the ledger's sum of the provider's entries for the day (ADR
// 0056, amendment 2026-11-06). Anthropic's analytics cost reads a workspace or one API key; a
// workspace must hold this provider's key alone for the figures to compare. OpenAI's reads a
// project or one API key.
//
// A source asks for exactly one bucket, the requested day, and reads only a bucket that starts at
// that day: a report that cannot be complete for it is an error, never $0, so a missing day reads
// as an error and not as out of tolerance.
//
// A source asks only its own pinned endpoint, follows no redirect, and serves only providers whose
// inference URL is that provider's own API, so the bill it reads is the one the ledger's entries ran
// up.
type CostSource interface {
	// Serves reports whether a provider at providerURL is this source's service.
	Serves(providerURL string) bool
	// ValidScope reports whether scope names a part of the account this source can read.
	ValidScope(scope string) bool
	// DayCost returns the scope's cost for the UTC day that starts at day.
	DayCost(ctx context.Context, p CostProvider, day time.Time) (money.USD, error)
}

// CostProvider is whose cost to read and with which credential.
type CostProvider struct {
	// AdminKey reads the cost API; it never leaves the request to the provider.
	AdminKey string
	// Scope is the workspace or project the provider's key belongs to, or the key's own ID.
	Scope string
}

// CostRegistry maps provider types to their cost sources.
type CostRegistry map[string]CostSource

// DefaultCosts is the cost sources Koinon ships. client nil is NewClient().
func DefaultCosts(client *http.Client) CostRegistry {
	if client == nil {
		client = NewClient()
	}
	return CostRegistry{
		"anthropic": AnthropicCost{Client: client},
		"openai":    OpenAICost{Client: client},
	}
}

// Types lists the provider types with a cost source.
func (r CostRegistry) Types() []string {
	out := make([]string, 0, len(r))
	for t := range r {
		out = append(out, t)
	}
	return out
}

// For returns the cost source of a provider of providerType at providerURL, if one serves it.
func (r CostRegistry) For(providerType, providerURL string) (CostSource, bool) {
	s, ok := r[providerType]
	if !ok || !s.Serves(providerURL) {
		return nil, false
	}
	return s, true
}

// maxCostPages bounds the pages one day's report may take.
const maxCostPages = 20

// servesHost reports whether providerURL is https on host (port 443 or none, no user info).
func servesHost(providerURL, host string) bool {
	u, err := url.Parse(providerURL)
	return err == nil && u.Scheme == "https" && strings.EqualFold(u.Hostname(), host) && u.User == nil &&
		(u.Port() == "" || u.Port() == "443")
}

// getJSON asks u with the headers and decodes a 200 into v. 429 is a RetryAfterError; any other
// status is an error naming only the status (a body may echo the request).
func getJSON(ctx context.Context, c *http.Client, name, u string, header http.Header, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	for k, vs := range header {
		req.Header[k] = vs
	}
	req.Header.Set("Accept", "application/json")
	client := http.Client{Timeout: 15 * time.Second}
	if c != nil {
		client = *c
	}
	client.CheckRedirect = NoRedirects
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("providerusage: %s: %w", name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("providerusage: %s: read: %w", name, err)
	}
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return &RetryAfterError{After: retryAfter(resp.Header.Get("Retry-After"), time.Now())}
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return fmt.Errorf("providerusage: %s redirected (%d); not followed", name, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("providerusage: %s answered %d", name, resp.StatusCode)
	}
	d := json.NewDecoder(strings.NewReader(string(body)))
	d.UseNumber()
	if err := d.Decode(v); err != nil {
		return fmt.Errorf("providerusage: %s: decode: %w", name, err)
	}
	return nil
}

// ratOf reads a decimal (a JSON number or a decimal string).
func ratOf(name, field, s string) (*big.Rat, error) {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, fmt.Errorf("providerusage: %s: %s %q is not a number", name, field, s)
	}
	return r, nil
}

// AnthropicCost reads the Usage and Cost Analytics API: GET
// https://api.anthropic.com/v1/organizations/analytics/cost, filtered to and grouped by the scope
// (a workspace or one API key), one 1-day bucket
// (https://platform.claude.com/docs/en/api/beta/organization/analytics/cost and
// https://platform.claude.com/docs/en/manage-claude/usage-cost-analytics-api, read 2026-11-12).
// It replaces the legacy /v1/organizations/cost_report (ADR 0056, amendment 2026-11-12). The Admin
// key goes in the x-api-key header (Bearer carries OAuth tokens only); the API is beta and needs
// the anthropic-beta header. A row's cost.amount is a decimal string in fractional cents (six
// places); the default workspace has a real workspace ID like any other, so there is no "default"
// scope. A combination with no cost has no row, so a day is complete only when data_refreshed_at
// is at or after its end: a report refreshed earlier is an error, never $0.
type AnthropicCost struct {
	Client *http.Client
}

// AnthropicCostURL is the one URL Anthropic's cost source asks.
const AnthropicCostURL = "https://api.anthropic.com/v1/organizations/analytics/cost"

// Anthropic's API version and the beta header of the analytics API.
const (
	anthropicVersion       = "2026-10-21"
	anthropicAnalyticsBeta = "analytics-usage-cost-2026-08-24"
)

// anthropicAPIKey is Anthropic's API key ID form (the key's ID, never the key).
var anthropicAPIKey = regexp.MustCompile(`^apikey_[A-Za-z0-9]{1,120}$`)

// Serves implements CostSource.
func (AnthropicCost) Serves(providerURL string) bool {
	return servesHost(providerURL, "api.anthropic.com")
}

// ValidScope implements CostSource: a workspace ID or an API key ID.
func (AnthropicCost) ValidScope(scope string) bool {
	return modelformat.ValidWorkspaceID(scope) || anthropicAPIKey.MatchString(scope)
}

type anthropicCostReport struct {
	Data []struct {
		StartingAt string `json:"starting_at"`
		Group      struct {
			WorkspaceID *string `json:"workspace_id"`
			APIKeyID    *string `json:"api_key_id"`
		} `json:"group"`
		Cost struct {
			Currency string `json:"currency"`
			Amount   string `json:"amount"`
		} `json:"cost"`
	} `json:"data"`
	HasMore         bool    `json:"has_more"`
	NextPage        string  `json:"next_page"`
	DataRefreshedAt *string `json:"data_refreshed_at"`
}

// DayCost implements CostSource.
func (a AnthropicCost) DayCost(ctx context.Context, p CostProvider, day time.Time) (money.USD, error) {
	if !a.ValidScope(p.Scope) {
		return 0, fmt.Errorf("providerusage: anthropic: %q is not a workspace or API key ID", p.Scope)
	}
	filter, group := "workspace_ids[]", "workspace_id"
	if anthropicAPIKey.MatchString(p.Scope) {
		filter, group = "api_key_ids[]", "api_key_id"
	}
	day = day.UTC().Truncate(24 * time.Hour)
	end := day.Add(24 * time.Hour)
	total, page := new(big.Rat), ""
	for range maxCostPages {
		q := url.Values{
			"starting_at":       {day.Format(time.RFC3339)},
			"ending_at":         {end.Format(time.RFC3339)},
			"time_bucket_width": {"1d"},
			"group_by[]":        {group},
			filter:              {p.Scope},
			"limit":             {"1000"},
		}
		if page != "" {
			q.Set("page", page)
		}
		var rep anthropicCostReport
		h := http.Header{"X-Api-Key": {p.AdminKey}, "Anthropic-Version": {anthropicVersion}, "Anthropic-Beta": {anthropicAnalyticsBeta}}
		if err := getJSON(ctx, a.Client, "anthropic analytics cost", AnthropicCostURL+"?"+q.Encode(), h, &rep); err != nil {
			return 0, err
		}
		// Every page reads one snapshot; a snapshot older than the day's end misses part of it.
		if rep.DataRefreshedAt == nil {
			return 0, fmt.Errorf("providerusage: anthropic analytics cost: no data for %s yet", day.Format(time.DateOnly))
		}
		if at, err := time.Parse(time.RFC3339, *rep.DataRefreshedAt); err != nil || at.Before(end) {
			return 0, fmt.Errorf("providerusage: anthropic analytics cost: data for %s not complete yet", day.Format(time.DateOnly))
		}
		for _, r := range rep.Data {
			if start, err := time.Parse(time.RFC3339, r.StartingAt); err != nil || !start.Equal(day) {
				continue
			}
			id := r.Group.WorkspaceID
			if group == "api_key_id" {
				id = r.Group.APIKeyID
			}
			if id == nil || *id != p.Scope {
				continue
			}
			if !strings.EqualFold(r.Cost.Currency, "USD") {
				return 0, fmt.Errorf("providerusage: anthropic analytics cost: currency %q is not USD", r.Cost.Currency)
			}
			cents, err := ratOf("anthropic analytics cost", "cost.amount", r.Cost.Amount)
			if err != nil {
				return 0, err
			}
			total.Add(total, cents.Quo(cents, big.NewRat(100, 1)))
		}
		if !rep.HasMore || rep.NextPage == "" {
			return money.FromRat(total)
		}
		page = rep.NextPage
	}
	return 0, fmt.Errorf("providerusage: anthropic analytics cost: more than %d pages", maxCostPages)
}

// OpenAICost reads the organization costs API: GET https://api.openai.com/v1/organization/costs,
// one 1-day bucket, filtered to and grouped by the scope: a project (project_ids, group_by
// project_id) or one API key (api_key_ids, group_by api_key_id)
// (https://platform.openai.com/docs/api-reference/usage/costs; shapes read from OpenAI's OpenAPI
// description, github.com/openai/openai-openapi, 2026-11-06). amount.value is in dollars; a
// bucket's start_time (Unix seconds) is the day it covers.
type OpenAICost struct {
	Client *http.Client
}

// OpenAICostURL is the one URL OpenAI's cost source asks.
const OpenAICostURL = "https://api.openai.com/v1/organization/costs"

// openAIProject and openAIKey are OpenAI's project and API key ID forms (the key's ID, never the key).
var (
	openAIProject = regexp.MustCompile(`^proj_[A-Za-z0-9]{1,123}$`)
	openAIKey     = regexp.MustCompile(`^key_[A-Za-z0-9]{1,124}$`)
)

// Serves implements CostSource.
func (OpenAICost) Serves(providerURL string) bool { return servesHost(providerURL, "api.openai.com") }

// ValidScope implements CostSource: a project ID or an API key ID.
func (OpenAICost) ValidScope(scope string) bool {
	return openAIProject.MatchString(scope) || openAIKey.MatchString(scope)
}

type openAICostPage struct {
	Data []struct {
		StartTime int64 `json:"start_time"`
		Results   []struct {
			Amount struct {
				Value    json.Number `json:"value"`
				Currency string      `json:"currency"`
			} `json:"amount"`
			ProjectID *string `json:"project_id"`
			APIKeyID  *string `json:"api_key_id"`
		} `json:"results"`
	} `json:"data"`
	HasMore  bool   `json:"has_more"`
	NextPage string `json:"next_page"`
}

// DayCost implements CostSource.
func (o OpenAICost) DayCost(ctx context.Context, p CostProvider, day time.Time) (money.USD, error) {
	if !o.ValidScope(p.Scope) {
		return 0, fmt.Errorf("providerusage: openai: %q is not a project or API key ID", p.Scope)
	}
	filter, group := "project_ids", "project_id"
	if openAIKey.MatchString(p.Scope) {
		filter, group = "api_key_ids", "api_key_id"
	}
	day = day.UTC().Truncate(24 * time.Hour)
	total, page, found := new(big.Rat), "", false
	for range maxCostPages {
		q := url.Values{
			"start_time":   {strconv.FormatInt(day.Unix(), 10)},
			"end_time":     {strconv.FormatInt(day.Add(24*time.Hour).Unix(), 10)},
			"bucket_width": {"1d"},
			"limit":        {"1"},
			filter:         {p.Scope},
			"group_by":     {group},
		}
		if page != "" {
			q.Set("page", page)
		}
		var rep openAICostPage
		h := http.Header{"Authorization": {"Bearer " + p.AdminKey}}
		if err := getJSON(ctx, o.Client, "openai costs", OpenAICostURL+"?"+q.Encode(), h, &rep); err != nil {
			return 0, err
		}
		for _, b := range rep.Data {
			if b.StartTime != day.Unix() {
				continue
			}
			found = true
			for _, r := range b.Results {
				id := r.ProjectID
				if group == "api_key_id" {
					id = r.APIKeyID
				}
				if id == nil || *id != p.Scope {
					continue
				}
				if !strings.EqualFold(r.Amount.Currency, "usd") {
					return 0, fmt.Errorf("providerusage: openai costs: currency %q is not USD", r.Amount.Currency)
				}
				v, err := ratOf("openai costs", "amount.value", r.Amount.Value.String())
				if err != nil {
					return 0, err
				}
				total.Add(total, v)
			}
		}
		if !rep.HasMore || rep.NextPage == "" {
			if !found {
				return 0, fmt.Errorf("providerusage: openai costs: no bucket for %s", day.Format(time.DateOnly))
			}
			return money.FromRat(total)
		}
		page = rep.NextPage
	}
	return 0, fmt.Errorf("providerusage: openai costs: more than %d pages", maxCostPages)
}
