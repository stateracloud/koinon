// SPDX-License-Identifier: Apache-2.0

package adminapi

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/stateracloud/koinon/internal/budget"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/money"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/pricing"
)

// Prices, budgets and spend (plan items PV2 and BG1). The service layer validates and audits;
// these handlers only translate. Generated types and the pricing types share their JSON names,
// so most conversions go through JSON.

func via(in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func priceVersion(o prices.Override) (PriceVersion, error) {
	var m map[string]any
	if err := via(o.Entry, &m); err != nil {
		return PriceVersion{}, err
	}
	for _, k := range []string{"id", "validFrom", "source", "note"} {
		delete(m, k)
	}
	var v PriceVersion
	if err := via(m, &v); err != nil {
		return PriceVersion{}, err
	}
	v.Id, v.Name, v.ValidFrom, v.RetiredAt = openapi_types.UUID(pgUUID(o.ID).Bytes), o.Name, o.ValidFrom, o.RetiredAt
	v.CreatedBy, v.CreatedAt = o.CreatedBy, o.CreatedAt
	return v, nil
}

// ListPrices implements StrictServerInterface.
func (s *Server) ListPrices(ctx context.Context, _ ListPricesRequestObject) (ListPricesResponseObject, error) {
	p, _ := s.actor(ctx)
	list, err := s.prices.ListOverrides(ctx, p.Org)
	if err != nil {
		st, e := s.problem(err)
		return ListPricesdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	out := ListPrices200JSONResponse{Prices: []PriceVersion{}}
	for _, o := range list {
		v, err := priceVersion(o)
		if err != nil {
			st, e := s.problem(err)
			return ListPricesdefaultJSONResponse{Body: e, StatusCode: st}, nil
		}
		out.Prices = append(out.Prices, v)
	}
	return out, nil
}

// SetPrice implements StrictServerInterface.
func (s *Server) SetPrice(ctx context.Context, req SetPriceRequestObject) (SetPriceResponseObject, error) {
	p, actor := s.actor(ctx)
	b := req.Body
	in := prices.OverrideInput{}
	if b.ValidFrom != nil {
		in.ValidFrom = *b.ValidFrom
	}
	e := map[string]any{"id": b.Name, "provider": b.Provider, "match": b.Match, "prices": b.Prices, "tokenizer": "bytes", "ratio": "3.0"}
	if b.Tiers != nil {
		e["tiers"] = b.Tiers
	}
	if b.Tokenizer != nil {
		e["tokenizer"] = string(*b.Tokenizer)
	}
	if b.Ratio != nil {
		e["ratio"] = *b.Ratio
	}
	if b.ToolOverheadTokens != nil {
		e["toolOverheadTokens"] = *b.ToolOverheadTokens
	}
	if b.HiddenThinking != nil {
		e["hiddenThinking"] = *b.HiddenThinking
	}
	if b.MaxOutput != nil {
		e["maxOutput"] = *b.MaxOutput
	}
	if err := via(e, &in.Entry); err != nil {
		st, er := s.problem(&prices.InvalidError{Msg: "invalid price: " + err.Error()})
		return SetPricedefaultJSONResponse{Body: er, StatusCode: st}, nil
	}
	got, err := s.prices.SetOverride(ctx, p.Org, actor, in)
	if err != nil {
		st, er := s.problem(err)
		return SetPricedefaultJSONResponse{Body: er, StatusCode: st}, nil
	}
	v, err := priceVersion(got)
	if err != nil {
		st, er := s.problem(err)
		return SetPricedefaultJSONResponse{Body: er, StatusCode: st}, nil
	}
	var saved PriceSaved
	if err := via(v, &saved); err != nil {
		st, er := s.problem(err)
		return SetPricedefaultJSONResponse{Body: er, StatusCode: st}, nil
	}
	if w := prices.EntryWarnings(in.Entry); len(w) > 0 {
		saved.Warnings = &w
	}
	return SetPrice201JSONResponse(saved), nil
}

// RetirePrice implements StrictServerInterface.
func (s *Server) RetirePrice(ctx context.Context, req RetirePriceRequestObject) (RetirePriceResponseObject, error) {
	p, actor := s.actor(ctx)
	if err := s.prices.RetireOverride(ctx, p.Org, actor, req.Name); err != nil {
		st, e := s.problem(err)
		return RetirePricedefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	return RetirePrice204Response{}, nil
}

// GetDefaultPrices implements StrictServerInterface.
func (s *Server) GetDefaultPrices(ctx context.Context, _ GetDefaultPricesRequestObject) (GetDefaultPricesResponseObject, error) {
	st0, err := s.prices.Defaults(ctx)
	if err != nil {
		st, e := s.problem(err)
		return GetDefaultPricesdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	out := GetDefaultPrices200JSONResponse{Version: st0.Current, PreviousVersions: append([]string{}, st0.Previous...)}
	if err := via(st0.Entries, &out.Entries); err != nil {
		st, e := s.problem(err)
		return GetDefaultPricesdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	return out, nil
}

// GetDefaultPriceChanges implements StrictServerInterface.
func (s *Server) GetDefaultPriceChanges(ctx context.Context, req GetDefaultPriceChangesRequestObject) (GetDefaultPriceChangesResponseObject, error) {
	var from, to string
	if req.Params.From != nil {
		from = *req.Params.From
	}
	if req.Params.To != nil {
		to = *req.Params.To
	}
	f, t, changes, err := s.prices.DefaultsDiff(ctx, from, to)
	if err != nil {
		st, e := s.problem(err)
		return GetDefaultPriceChangesdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	out := GetDefaultPriceChanges200JSONResponse{To: t, Changes: []PriceChange{}}
	if f != "" {
		out.From = &f
	}
	if changes == nil {
		changes = []pricing.Change{}
	}
	if err := via(changes, &out.Changes); err != nil {
		st, e := s.problem(err)
		return GetDefaultPriceChangesdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	return out, nil
}

// ListFlaggedPrices implements StrictServerInterface.
func (s *Server) ListFlaggedPrices(ctx context.Context, req ListFlaggedPricesRequestObject) (ListFlaggedPricesResponseObject, error) {
	p, _ := s.actor(ctx)
	since := time.Now().Add(-7 * 24 * time.Hour)
	if req.Params.Since != nil {
		since = *req.Params.Since
	}
	list, err := s.prices.ListFlagged(ctx, p.Org, since)
	if err != nil {
		st, e := s.problem(err)
		return ListFlaggedPricesdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	out := ListFlaggedPrices200JSONResponse{Flagged: []FlaggedPrice{}}
	for _, f := range list {
		out.Flagged = append(out.Flagged, FlaggedPrice{
			PriceRef: f.PriceRef, Model: f.Model, Requests: f.Requests, LastSeen: f.LastSeen,
			CostUsd: ledgerUSD(f.CostUSD), GatewayCostUsd: ledgerUSD(f.GatewayCostUSD),
		})
	}
	return out, nil
}

// ledgerUSD trims a Postgres numeric to the money format.
func ledgerUSD(s string) string {
	if v, err := money.ParseNumeric(s); err == nil {
		return v.String()
	}
	return s
}

// GetSpendReport implements StrictServerInterface.
func (s *Server) GetSpendReport(ctx context.Context, req GetSpendReportRequestObject) (GetSpendReportResponseObject, error) {
	p, _ := s.actor(ctx)
	since := budget.MonthStart(time.Now())
	if req.Params.Since != nil {
		since = *req.Params.Since
	}
	rows, err := store.New(s.pool).SpendReport(ctx, store.SpendReportParams{OrgID: p.Org, Since: pgtype.Timestamptz{Time: since, Valid: true}})
	if err != nil {
		st, e := s.problem(err)
		return GetSpendReportdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	sys, err := store.New(s.pool).SpendSystemSince(ctx, store.SpendSystemSinceParams{OrgID: p.Org, Since: pgtype.Timestamptz{Time: since, Valid: true}})
	if err != nil {
		st, e := s.problem(err)
		return GetSpendReportdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	out := GetSpendReport200JSONResponse{Since: since, Models: []SpendLine{}}
	out.SystemChecks.Requests, out.SystemChecks.CostUsd = sys.Requests, ledgerUSD(sys.CostUsd)
	for _, r := range rows {
		out.Models = append(out.Models, SpendLine{
			Model: r.Model, Requests: r.Requests, InputTokens: r.InputTokens, OutputTokens: r.OutputTokens,
			ReportedUsd: ledgerUSD(r.ReportedUsd), EstimatedUsd: ledgerUSD(r.EstimatedUsd), EstimatedRequests: r.EstimatedRequests,
			EstimatedOutputTokens: r.EstimatedOutputTokens, GatewayRequests: r.GatewayRequests,
			GatewayUsd: ledgerUSD(r.GatewayUsd), GatewayComparedUsd: ledgerUSD(r.GatewayComparedUsd), FlaggedRequests: r.FlaggedRequests,
			ProvisionalRequests: r.ProvisionalRequests, ReservedUsd: ledgerUSD(r.ReservedUsd),
		})
	}
	return out, nil
}

func orgBudget(v budget.OrgView) OrgBudget {
	out := OrgBudget{SpentThisMonthUsd: v.SpentThisMonth.String(), MonthStart: v.MonthStart}
	if v.MonthlyCap != nil {
		c := v.MonthlyCap.String()
		out.MonthlyCapUsd = &c
	}
	if v.PlatformCeiling != nil {
		c := v.PlatformCeiling.String()
		out.PlatformCeilingUsd = &c
	}
	return out
}

// GetBudget implements StrictServerInterface.
func (s *Server) GetBudget(ctx context.Context, _ GetBudgetRequestObject) (GetBudgetResponseObject, error) {
	p, _ := s.actor(ctx)
	v, err := s.budgets.GetOrg(ctx, p.Org, time.Now())
	if err != nil {
		st, e := s.problem(err)
		return GetBudgetdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	return GetBudget200JSONResponse(orgBudget(v)), nil
}

// SetBudget implements StrictServerInterface.
func (s *Server) SetBudget(ctx context.Context, req SetBudgetRequestObject) (SetBudgetResponseObject, error) {
	p, actor := s.actor(ctx)
	var capUSD *money.USD
	if req.Body.MonthlyCapUsd != nil {
		v, err := money.Parse(*req.Body.MonthlyCapUsd)
		if err != nil {
			st, e := s.problem(&budget.InvalidError{Msg: err.Error()})
			return SetBudgetdefaultJSONResponse{Body: e, StatusCode: st}, nil
		}
		capUSD = &v
	}
	if err := s.budgets.SetOrgCap(ctx, p.Org, actor, capUSD); err != nil {
		st, e := s.problem(err)
		return SetBudgetdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	v, err := s.budgets.GetOrg(ctx, p.Org, time.Now())
	if err != nil {
		st, e := s.problem(err)
		return SetBudgetdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	return SetBudget200JSONResponse(orgBudget(v)), nil
}

func (s *Server) agentBudget(ctx context.Context, org, agent pgtype.UUID) (AgentBudget, error) {
	var fallback money.USD
	if a, err := store.New(s.pool).GetAgent(ctx, store.GetAgentParams{OrgID: org, ID: agent}); err != nil {
		return AgentBudget{}, budget.ErrNotFound
	} else if a.ActiveVersionID.Valid {
		fallback = s.templateLimit(ctx, org, a.ActiveVersionID)
	}
	v, ok, err := s.budgets.GetAgent(ctx, org, agent, fallback, time.Now())
	if err != nil {
		return AgentBudget{}, err
	}
	if !ok {
		return AgentBudget{}, budget.ErrNotFound
	}
	return AgentBudget{
		AgentId: openapi_types.UUID(agent.Bytes), DailyLimitUsd: v.DailyLimit.String(), SpentTodayUsd: v.SpentToday.String(), DayStart: v.DayStart,
	}, nil
}

// GetAgentBudget implements StrictServerInterface.
func (s *Server) GetAgentBudget(ctx context.Context, req GetAgentBudgetRequestObject) (GetAgentBudgetResponseObject, error) {
	p, _ := s.actor(ctx)
	got, err := s.agentBudget(ctx, p.Org, pgUUID(req.AgentId.String()))
	if err != nil {
		st, e := s.problem(err)
		return GetAgentBudgetdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	return GetAgentBudget200JSONResponse(got), nil
}

// SetAgentBudget implements StrictServerInterface.
func (s *Server) SetAgentBudget(ctx context.Context, req SetAgentBudgetRequestObject) (SetAgentBudgetResponseObject, error) {
	p, actor := s.actor(ctx)
	limit, err := money.Parse(req.Body.DailyLimitUsd)
	if err != nil {
		st, e := s.problem(&budget.InvalidError{Msg: err.Error()})
		return SetAgentBudgetdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	agent := pgUUID(req.AgentId.String())
	if err := s.budgets.SetAgentDaily(ctx, p.Org, agent, actor, limit); err != nil {
		st, e := s.problem(err)
		return SetAgentBudgetdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	got, err := s.agentBudget(ctx, p.Org, agent)
	if err != nil {
		st, e := s.problem(err)
		return SetAgentBudgetdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	return SetAgentBudget200JSONResponse(got), nil
}
