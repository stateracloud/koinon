// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"time"

	connect "connectrpc.com/connect/v2"
	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/stateracloud/koinon/internal/budget"
	"github.com/stateracloud/koinon/internal/db/store"
	usagev1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/usage/v1alpha1"
	"github.com/stateracloud/koinon/internal/usage"
)

// UsageService is the usage and cost read model (issue #1268, ADR 0126 section 4). It reads the
// spend ledger and the runs table. It never reads Prometheus. The Get calls and the exports build
// their numbers from the same rows with the same sum ([usage.Sum]).

const maxUsageModelLen = 256

// maxUsageRows caps one response, so a call over a long period cannot build an unbounded message.
const maxUsageRows = 50000

// orgUsage is what GetOrgUsage and ExportOrgUsage share: the checked query, the rows and the sums.
type orgUsage struct {
	org      pgtype.UUID
	from, to time.Time
	rows     []*usagev1alpha1.OrgUsageRow
	totals   usage.Sum
}

// usageRange reads the period of a request.
func usageRange(r *usagev1alpha1.UsageRange) (time.Time, time.Time, error) {
	var from, to time.Time
	if r.GetFrom() != nil {
		if err := r.GetFrom().CheckValid(); err != nil {
			return from, to, connect.NewError(connect.CodeInvalidArgument, "range.from is not a valid time")
		}
		from = r.GetFrom().AsTime()
	}
	if r.GetTo() != nil {
		if err := r.GetTo().CheckValid(); err != nil {
			return from, to, connect.NewError(connect.CodeInvalidArgument, "range.to is not a valid time")
		}
		to = r.GetTo().AsTime()
	}
	f, t, err := usage.Resolve(from, to, time.Now())
	if err != nil {
		return f, t, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	return f, t, nil
}

func usageTotals(s *usage.Sum) *usagev1alpha1.UsageTotals {
	return &usagev1alpha1.UsageTotals{
		Requests: s.Requests, InputTokens: s.Input, OutputTokens: s.Output,
		CacheReadTokens: s.CacheRead, CacheWriteTokens: s.CacheWrite, ReasoningTokens: s.Reasoning,
		CostUsd: s.Cost(), EstimatedUsd: s.Estimated(), CacheReadShare: s.CacheReadShare(),
	}
}

// usageOf sums one row on its own, so the row carries the same figures as the totals.
func usageOf(r usage.Row) (*usagev1alpha1.UsageTotals, usage.Sum, error) {
	var s usage.Sum
	if err := s.Add(r); err != nil {
		return nil, s, err
	}
	return usageTotals(&s), s, nil
}

func usageDay(d pgtype.Date) string { return d.Time.UTC().Format("2006-01-02") }

func uuidText(id pgtype.UUID) string {
	if !id.Valid {
		return ""
	}
	return id.String()
}

func (s *Server) orgUsage(ctx context.Context, orgID string, rng *usagev1alpha1.UsageRange, agentID, model, providerID string) (*orgUsage, error) {
	c, err := authorize(ctx, orgID, store.OrgRoleMember)
	if err != nil {
		return nil, err
	}
	from, to, err := usageRange(rng)
	if err != nil {
		return nil, err
	}
	arg := store.UsageByDayParams{
		OrgID:  c.org,
		FromAt: pgtype.Timestamptz{Time: from, Valid: true},
		ToAt:   pgtype.Timestamptz{Time: to, Valid: true},
	}
	if agentID != "" {
		if arg.AgentID, err = parseID("agent_id", agentID); err != nil {
			return nil, err
		}
	}
	if providerID != "" {
		if arg.ProviderID, err = parseID("provider_id", providerID); err != nil {
			return nil, err
		}
	}
	if model != "" {
		if len(model) > maxUsageModelLen {
			return nil, connect.NewError(connect.CodeInvalidArgument, "model is too long")
		}
		arg.Model = pgtype.Text{String: model, Valid: true}
	}
	rows, err := s.q.UsageByDay(ctx, arg)
	if err != nil {
		return nil, s.dbError(err, "usage")
	}
	if len(rows) > maxUsageRows {
		return nil, connect.NewError(connect.CodeInvalidArgument, "too many rows: use a shorter period or a filter")
	}
	out := &orgUsage{org: c.org, from: from, to: to}
	for _, r := range rows {
		row := usage.Row{
			Requests: r.Requests, Input: r.InputTokens, Output: r.OutputTokens, CacheRead: r.CacheReadTokens,
			CacheWrite: r.CacheWriteTokens, Reasoning: r.ReasoningTokens, Cost: r.CostUsd, Estimated: r.EstimatedUsd,
		}
		t, _, err := usageOf(row)
		if err != nil {
			return nil, s.dbError(err, "usage")
		}
		if err := out.totals.Add(row); err != nil {
			return nil, s.dbError(err, "usage")
		}
		out.rows = append(out.rows, &usagev1alpha1.OrgUsageRow{
			Day: usageDay(r.Day), Principal: r.Principal, AgentId: uuidText(r.AgentID), AgentName: r.AgentName,
			Model: r.Model, ProviderId: uuidText(r.ProviderID), ProviderName: r.ProviderName, Usage: t,
		})
	}
	return out, nil
}

// GetOrgUsage implements UsageService. Members and above, of the org only.
func (s *Server) GetOrgUsage(ctx context.Context, req *usagev1alpha1.GetOrgUsageRequest) (*usagev1alpha1.GetOrgUsageResponse, error) {
	u, err := s.orgUsage(ctx, req.GetOrgId(), req.GetRange(), req.GetAgentId(), req.GetModel(), req.GetProviderId())
	if err != nil {
		return nil, err
	}
	day := budget.DayStart(time.Now())
	runs, err := s.q.UsageRunsSince(ctx, store.UsageRunsSinceParams{OrgID: u.org, Since: pgtype.Timestamptz{Time: day, Valid: true}})
	if err != nil {
		return nil, s.dbError(err, "runs")
	}
	return &usagev1alpha1.GetOrgUsageResponse{
		OrgId: u.org.String(), From: timestamppb.New(u.from), To: timestamppb.New(u.to),
		Rows: u.rows, Totals: usageTotals(&u.totals),
		Runs: &usagev1alpha1.RunCounts{DayStart: timestamppb.New(day), RunsToday: runs.Runs, FailedRunsToday: runs.FailedRuns},
	}, nil
}

var orgUsageHeader = []string{
	"day", "principal", "agent_id", "agent_name", "model", "provider_id", "provider_name",
	"requests", "input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens", "reasoning_tokens",
	"cost_usd", "estimated_usd", "cache_read_share",
}

func totalsCells(t *usagev1alpha1.UsageTotals) []string {
	return []string{
		usage.Int(t.GetRequests()), usage.Int(t.GetInputTokens()), usage.Int(t.GetOutputTokens()),
		usage.Int(t.GetCacheReadTokens()), usage.Int(t.GetCacheWriteTokens()), usage.Int(t.GetReasoningTokens()),
		t.GetCostUsd(), t.GetEstimatedUsd(), usage.Share(t.GetCacheReadShare()),
	}
}

// ExportOrgUsage implements UsageService. Members and above, of the org only.
func (s *Server) ExportOrgUsage(ctx context.Context, req *usagev1alpha1.ExportOrgUsageRequest) (*usagev1alpha1.ExportOrgUsageResponse, error) {
	u, err := s.orgUsage(ctx, req.GetOrgId(), req.GetRange(), req.GetAgentId(), req.GetModel(), req.GetProviderId())
	if err != nil {
		return nil, err
	}
	rows := make([][]string, 0, len(u.rows))
	for _, r := range u.rows {
		rows = append(rows, append([]string{
			r.GetDay(), r.GetPrincipal(), r.GetAgentId(), usage.Text(r.GetAgentName()), usage.Text(r.GetModel()),
			r.GetProviderId(), usage.Text(r.GetProviderName()),
		}, totalsCells(r.GetUsage())...))
	}
	total := append([]string{"total", "", "", "", "", "", ""}, totalsCells(usageTotals(&u.totals))...)
	text, err := usage.CSV(orgUsageHeader, rows, total)
	if err != nil {
		return nil, s.dbError(err, "usage export")
	}
	return &usagev1alpha1.ExportOrgUsageResponse{Filename: usageFilename("org-"+u.org.String(), u.from, u.to), Csv: text}, nil
}

func usageFilename(scope string, from, to time.Time) string {
	return "koinon-usage-" + scope + "-" + from.Format("20060102") + "-" + to.AddDate(0, 0, -1).Format("20060102") + ".csv"
}

type platformUsage struct {
	from, to time.Time
	rows     []*usagev1alpha1.PlatformUsageRow
	totals   usage.Sum
}

func (s *Server) platformUsage(ctx context.Context, rng *usagev1alpha1.UsageRange) (*platformUsage, error) {
	if _, err := requirePlatformAdmin(ctx); err != nil {
		return nil, err
	}
	from, to, err := usageRange(rng)
	if err != nil {
		return nil, err
	}
	rows, err := s.q.PlatformUsageByDay(ctx, store.PlatformUsageByDayParams{
		FromAt: pgtype.Timestamptz{Time: from, Valid: true}, ToAt: pgtype.Timestamptz{Time: to, Valid: true},
	})
	if err != nil {
		return nil, s.dbError(err, "usage")
	}
	if len(rows) > maxUsageRows {
		return nil, connect.NewError(connect.CodeInvalidArgument, "too many rows: use a shorter period")
	}
	out := &platformUsage{from: from, to: to}
	for _, r := range rows {
		row := usage.Row{
			Requests: r.Requests, Input: r.InputTokens, Output: r.OutputTokens, CacheRead: r.CacheReadTokens,
			CacheWrite: r.CacheWriteTokens, Reasoning: r.ReasoningTokens, Cost: r.CostUsd, Estimated: r.EstimatedUsd,
		}
		t, _, err := usageOf(row)
		if err != nil {
			return nil, s.dbError(err, "usage")
		}
		if err := out.totals.Add(row); err != nil {
			return nil, s.dbError(err, "usage")
		}
		out.rows = append(out.rows, &usagev1alpha1.PlatformUsageRow{
			Day: usageDay(r.Day), OrgId: r.OrgID.String(), OrgSlug: r.Slug, OrgName: r.DisplayName, Model: r.Model, Usage: t,
		})
	}
	return out, nil
}

// GetPlatformUsage implements UsageService. Platform admins only.
func (s *Server) GetPlatformUsage(ctx context.Context, req *usagev1alpha1.GetPlatformUsageRequest) (*usagev1alpha1.GetPlatformUsageResponse, error) {
	u, err := s.platformUsage(ctx, req.GetRange())
	if err != nil {
		return nil, err
	}
	day := budget.DayStart(time.Now())
	runs, err := s.q.PlatformUsageRunsSince(ctx, pgtype.Timestamptz{Time: day, Valid: true})
	if err != nil {
		return nil, s.dbError(err, "runs")
	}
	return &usagev1alpha1.GetPlatformUsageResponse{
		From: timestamppb.New(u.from), To: timestamppb.New(u.to), Rows: u.rows, Totals: usageTotals(&u.totals),
		Runs: &usagev1alpha1.RunCounts{DayStart: timestamppb.New(day), RunsToday: runs.Runs, FailedRunsToday: runs.FailedRuns},
	}, nil
}

var platformUsageHeader = []string{
	"day", "org_id", "org_slug", "org_name", "model",
	"requests", "input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens", "reasoning_tokens",
	"cost_usd", "estimated_usd", "cache_read_share",
}

// ExportPlatformUsage implements UsageService. Platform admins only.
func (s *Server) ExportPlatformUsage(ctx context.Context, req *usagev1alpha1.ExportPlatformUsageRequest) (*usagev1alpha1.ExportPlatformUsageResponse, error) {
	u, err := s.platformUsage(ctx, req.GetRange())
	if err != nil {
		return nil, err
	}
	rows := make([][]string, 0, len(u.rows))
	for _, r := range u.rows {
		rows = append(rows, append([]string{
			r.GetDay(), r.GetOrgId(), usage.Text(r.GetOrgSlug()), usage.Text(r.GetOrgName()), usage.Text(r.GetModel()),
		}, totalsCells(r.GetUsage())...))
	}
	total := append([]string{"total", "", "", "", ""}, totalsCells(usageTotals(&u.totals))...)
	text, err := usage.CSV(platformUsageHeader, rows, total)
	if err != nil {
		return nil, s.dbError(err, "usage export")
	}
	return &usagev1alpha1.ExportPlatformUsageResponse{Filename: usageFilename("platform", u.from, u.to), Csv: text}, nil
}
