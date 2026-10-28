// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"sort"
	"time"
)

// costRow is one line of a costs response.
type costRow struct {
	Date    string  `json:"date"`
	Key     string  `json:"key,omitempty" jsonschema:"the repo, workflow or runner, when grouped"`
	Runs    int     `json:"runs"`
	Minutes float64 `json:"minutes"`
	CostUSD float64 `json:"cost_usd"`
}

// costsResponse is the body of GET /v1/costs and the ci_costs tool's result.
type costsResponse struct {
	From     string    `json:"from"`
	To       string    `json:"to"`
	GroupBy  string    `json:"group_by,omitempty"`
	Currency string    `json:"currency"`
	TotalUSD float64   `json:"total_usd"`
	Rows     []costRow `json:"rows"`
}

// costsQuery is the shared parameters of the REST endpoint and the MCP tool.
type costsQuery struct {
	From    string `json:"from" jsonschema:"first day, YYYY-MM-DD, inclusive (UTC)"`
	To      string `json:"to" jsonschema:"last day, YYYY-MM-DD, inclusive (UTC)"`
	GroupBy string `json:"group_by,omitempty" jsonschema:"repo, workflow or runner; omit for daily totals"`
}

// costs answers a query against the data as of now. The error text is safe to show the caller.
func costs(q costsQuery, now time.Time) (costsResponse, error) {
	from, err := parseDate("from", q.From)
	if err != nil {
		return costsResponse{}, err
	}
	to, err := parseDate("to", q.To)
	if err != nil {
		return costsResponse{}, err
	}
	if to.Before(from) {
		return costsResponse{}, fmt.Errorf("to %s is before from %s", q.To, q.From)
	}
	switch q.GroupBy {
	case "", "repo", "workflow", "runner":
	default:
		return costsResponse{}, fmt.Errorf("group_by %q is not repo, workflow or runner", q.GroupBy)
	}
	today := now.UTC().Truncate(24 * time.Hour)
	oldest := today.AddDate(0, 0, -(historyDays - 1))
	if from.Before(oldest) || to.After(today) {
		return costsResponse{}, fmt.Errorf("data exists only from %s to %s", oldest.Format(dateLayout), today.Format(dateLayout))
	}
	yesterday := today.AddDate(0, 0, -1)

	resp := costsResponse{From: q.From, To: q.To, GroupBy: q.GroupBy, Currency: "USD", Rows: []costRow{}}
	for day := from; !day.After(to); day = day.AddDate(0, 0, 1) {
		agg := map[string]*costRow{}
		for _, r := range dayRows(day, yesterday) {
			key := ""
			switch q.GroupBy {
			case "repo":
				key = r.Repo
			case "workflow":
				key = r.Workflow
			case "runner":
				key = r.Runner
			}
			c := agg[key]
			if c == nil {
				c = &costRow{Date: r.Date, Key: key}
				agg[key] = c
			}
			c.Runs += r.Runs
			c.Minutes += r.Minutes
			c.CostUSD += r.CostUSD
		}
		keys := make([]string, 0, len(agg))
		for k := range agg {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			c := *agg[k]
			c.Minutes, c.CostUSD = round2(c.Minutes), round2(c.CostUSD)
			resp.TotalUSD += c.CostUSD
			resp.Rows = append(resp.Rows, c)
		}
	}
	resp.TotalUSD = round2(resp.TotalUSD)
	return resp, nil
}

func round2(f float64) float64 {
	return float64(int64(f*100+0.5)) / 100
}
