// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	connect "connectrpc.com/connect/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/auth"
	"github.com/stateracloud/koinon/internal/budget"
	"github.com/stateracloud/koinon/internal/db/store"
	budgetv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/budget/v1alpha1"
	"github.com/stateracloud/koinon/internal/ledger"
	"github.com/stateracloud/koinon/internal/money"
	"github.com/stateracloud/koinon/internal/orgsettings"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/runlimits"
	"github.com/stateracloud/koinon/pricing"
)

// BudgetService and PriceService (plan items BG1 and PV2).

func (s *Server) budgetError(err error) error {
	var bin *budget.InvalidError
	var pin *prices.InvalidError
	switch {
	case errors.As(err, &bin):
		return connect.NewError(connect.CodeInvalidArgument, bin.Msg)
	case errors.As(err, &pin):
		return connect.NewError(connect.CodeInvalidArgument, pin.Msg)
	case errors.Is(err, budget.ErrForbidden):
		return connect.NewError(connect.CodePermissionDenied, err.Error())
	case errors.Is(err, budget.ErrNotFound), errors.Is(err, prices.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, "not found")
	}
	return s.dbError(err, "budget")
}

func orgBudgetProto(v budget.OrgView) *budgetv1alpha1.OrgBudget {
	out := &budgetv1alpha1.OrgBudget{
		SpentThisMonthUsd: v.SpentThisMonth.String(), EstimatedThisMonthUsd: v.EstimatedThisMonth.String(), SystemChecksThisMonthUsd: v.SystemChecksThisMonth.String(), MonthStart: timestamppb.New(v.MonthStart),
	}
	if v.MonthlyCap != nil {
		out.MonthlyCapUsd = v.MonthlyCap.String()
	}
	if v.PlatformCeiling != nil {
		out.PlatformCeilingUsd = v.PlatformCeiling.String()
	}
	return out
}

func optionalAmount(s string) (*money.USD, error) {
	if s == "" {
		return nil, nil
	}
	v, err := money.Parse(s)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	return &v, nil
}

// GetOrgBudget implements BudgetService. Admins and owners.
func (s *Server) GetOrgBudget(ctx context.Context, req *budgetv1alpha1.GetOrgBudgetRequest) (*budgetv1alpha1.GetOrgBudgetResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	v, err := s.budgets.GetOrg(ctx, c.org, time.Now())
	if err != nil {
		return nil, s.budgetError(err)
	}
	return &budgetv1alpha1.GetOrgBudgetResponse{Budget: orgBudgetProto(v)}, nil
}

// SetMonthlyCap implements BudgetService. Admins and owners.
func (s *Server) SetMonthlyCap(ctx context.Context, req *budgetv1alpha1.SetMonthlyCapRequest) (*budgetv1alpha1.SetMonthlyCapResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	capUSD, err := optionalAmount(req.GetMonthlyCapUsd())
	if err != nil {
		return nil, err
	}
	if err := s.budgets.SetOrgCap(requestCtx(ctx), c.org, c.auditActor(), capUSD); err != nil {
		return nil, s.budgetError(err)
	}
	v, err := s.budgets.GetOrg(ctx, c.org, time.Now())
	if err != nil {
		return nil, s.budgetError(err)
	}
	return &budgetv1alpha1.SetMonthlyCapResponse{Budget: orgBudgetProto(v)}, nil
}

// SetPlatformCeiling implements BudgetService. Platform admins only.
func (s *Server) SetPlatformCeiling(ctx context.Context, req *budgetv1alpha1.SetPlatformCeilingRequest) (*budgetv1alpha1.SetPlatformCeilingResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleViewer)
	if err != nil {
		return nil, err
	}
	if !c.p.PlatformAdmin {
		return nil, connect.NewError(connect.CodePermissionDenied, budget.ErrForbidden.Error())
	}
	ceiling, err := optionalAmount(req.GetPlatformCeilingUsd())
	if err != nil {
		return nil, err
	}
	if err := s.budgets.SetPlatformCeiling(requestCtx(ctx), c.org, c.auditActor(), c.p.UserID, ceiling); err != nil {
		return nil, s.budgetError(err)
	}
	v, err := s.budgets.GetOrg(ctx, c.org, time.Now())
	if err != nil {
		return nil, s.budgetError(err)
	}
	return &budgetv1alpha1.SetPlatformCeilingResponse{Budget: orgBudgetProto(v)}, nil
}

// templateDefault is the spendUSD of the agent's active version: the default its limit started from.
func (s *Server) templateDefault(ctx context.Context, org pgtype.UUID, a store.Agent) money.USD {
	var fallback money.USD
	if a.ActiveVersionID.Valid {
		if v, err := s.q.GetAgentVersion(ctx, store.GetAgentVersionParams{OrgID: org, ID: a.ActiveVersionID}); err == nil {
			if cfg, err := agentconfig.Parse(v.Config); err == nil {
				fallback = money.FromCents(cfg.Limits.SpendUSD.Cents())
			}
		}
	}
	return fallback
}

func (s *Server) agentBudgetOf(ctx context.Context, org pgtype.UUID, a store.Agent) (*budgetv1alpha1.AgentBudget, bool, error) {
	fallback := s.templateDefault(ctx, org, a)
	v, ok, err := s.budgets.GetAgent(ctx, org, a.ID, fallback, time.Now())
	if err != nil || !ok {
		return nil, ok, s.budgetErrorOrNil(err)
	}
	return &budgetv1alpha1.AgentBudget{
		AgentId: a.ID.String(), DailyLimitUsd: v.DailyLimit.String(), SpentTodayUsd: v.SpentToday.String(), DayStart: timestamppb.New(v.DayStart),
		AgentName: a.DisplayName, TemplateDefaultUsd: fallback.String(),
	}, true, nil
}

func (s *Server) budgetErrorOrNil(err error) error {
	if err == nil {
		return nil
	}
	return s.budgetError(err)
}

func (s *Server) agentBudgetView(ctx context.Context, org, agent pgtype.UUID) (*budgetv1alpha1.AgentBudget, error) {
	a, err := s.q.GetAgent(ctx, store.GetAgentParams{OrgID: org, ID: agent})
	if err != nil {
		return nil, s.dbError(err, "agent")
	}
	b, ok, err := s.agentBudgetOf(ctx, org, a)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, "this agent has no daily limit")
	}
	return b, nil
}

// ListAgentBudgets implements BudgetService. Admins and owners. It reads the limits, the template
// defaults and today's spend in four queries, however many agents the org has.
func (s *Server) ListAgentBudgets(ctx context.Context, req *budgetv1alpha1.ListAgentBudgetsRequest) (*budgetv1alpha1.ListAgentBudgetsResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	day := pgtype.Timestamptz{Time: budget.DayStart(now), Valid: true}
	agents, err := s.q.ListAgents(ctx, c.org)
	if err != nil {
		return nil, s.dbError(err, "agents")
	}
	limits, err := s.q.ListAgentBudgets(ctx, c.org)
	if err != nil {
		return nil, s.dbError(err, "budgets")
	}
	spends, err := s.q.ListAgentSpendSince(ctx, store.ListAgentSpendSinceParams{OrgID: c.org, Since: day})
	if err != nil {
		return nil, s.dbError(err, "spend")
	}
	cfgs, err := s.q.ListActiveVersionConfigs(ctx, c.org)
	if err != nil {
		return nil, s.dbError(err, "agent versions")
	}
	own := map[pgtype.UUID]string{}
	for _, l := range limits {
		own[l.AgentID] = l.DailyLimit
	}
	spent := map[pgtype.UUID]string{}
	for _, sp := range spends {
		spent[sp.AgentID] = sp.CostUsd
	}
	template := map[pgtype.UUID]money.USD{}
	for _, cf := range cfgs {
		if cfg, err := agentconfig.Parse(cf.Config); err == nil {
			template[cf.AgentID] = money.FromCents(cfg.Limits.SpendUSD.Cents())
		}
	}
	res := &budgetv1alpha1.ListAgentBudgetsResponse{}
	for _, a := range agents {
		if a.Status == store.AgentStatusArchived {
			continue
		}
		limit, ok := template[a.ID], template[a.ID] > 0
		if v, has := own[a.ID]; has {
			n, err := money.ParseNumeric(v)
			if err != nil {
				return nil, s.budgetError(err)
			}
			limit, ok = n, true
		}
		if !ok {
			continue
		}
		today, err := money.ParseNumeric(spent[a.ID])
		if spent[a.ID] == "" {
			today, err = 0, nil
		}
		if err != nil {
			return nil, s.budgetError(err)
		}
		res.Budgets = append(res.Budgets, &budgetv1alpha1.AgentBudget{
			AgentId: a.ID.String(), AgentName: a.DisplayName, DailyLimitUsd: limit.String(), SpentTodayUsd: today.String(),
			DayStart: timestamppb.New(day.Time), TemplateDefaultUsd: template[a.ID].String(),
		})
	}
	return res, nil
}

// ListOrgBudgets implements BudgetService. Platform admins only.
func (s *Server) ListOrgBudgets(ctx context.Context, _ *budgetv1alpha1.ListOrgBudgetsRequest) (*budgetv1alpha1.ListOrgBudgetsResponse, error) {
	p, ok := auth.PrincipalFrom(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, "login required")
	}
	if !p.PlatformAdmin {
		return nil, connect.NewError(connect.CodePermissionDenied, budget.ErrForbidden.Error())
	}
	since := budget.MonthStart(time.Now())
	rows, err := s.q.ListOrgBudgetSummaries(ctx, pgtype.Timestamptz{Time: since, Valid: true})
	if err != nil {
		return nil, s.dbError(err, "orgs")
	}
	res := &budgetv1alpha1.ListOrgBudgetsResponse{MonthStart: timestamppb.New(since)}
	for _, r := range rows {
		spent, err := money.ParseNumeric(r.Spent)
		if err != nil {
			return nil, s.budgetError(err)
		}
		res.Orgs = append(res.Orgs, &budgetv1alpha1.OrgBudgetSummary{
			OrgId: r.OrgID.String(), OrgSlug: r.Slug, OrgName: r.DisplayName, MonthlyCapUsd: formatAmount(r.MonthlyCap),
			PlatformCeilingUsd: formatAmount(r.PlatformCeiling), SpentThisMonthUsd: spent.String(),
		})
	}
	return res, nil
}

// formatAmount writes a numeric text from the database the way money.USD prints, or "" for none.
func formatAmount(s string) string {
	if s == "" {
		return ""
	}
	v, err := money.ParseNumeric(s)
	if err != nil {
		return ""
	}
	return v.String()
}

// GetAgentBudget implements BudgetService. Admins and owners.
func (s *Server) GetAgentBudget(ctx context.Context, req *budgetv1alpha1.GetAgentBudgetRequest) (*budgetv1alpha1.GetAgentBudgetResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	id, err := parseID("agent_id", req.GetAgentId())
	if err != nil {
		return nil, err
	}
	b, err := s.agentBudgetView(ctx, c.org, id)
	if err != nil {
		return nil, err
	}
	return &budgetv1alpha1.GetAgentBudgetResponse{Budget: b}, nil
}

// SetAgentBudget implements BudgetService. Admins and owners.
func (s *Server) SetAgentBudget(ctx context.Context, req *budgetv1alpha1.SetAgentBudgetRequest) (*budgetv1alpha1.SetAgentBudgetResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	id, err := parseID("agent_id", req.GetAgentId())
	if err != nil {
		return nil, err
	}
	limit, err := money.Parse(req.GetDailyLimitUsd())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	if err := s.budgets.SetAgentDaily(requestCtx(ctx), c.org, id, c.auditActor(), limit); err != nil {
		return nil, s.budgetError(err)
	}
	b, err := s.agentBudgetView(ctx, c.org, id)
	if err != nil {
		return nil, err
	}
	return &budgetv1alpha1.SetAgentBudgetResponse{Budget: b}, nil
}

func runLimitValuesProto(wall time.Duration, requests int64, spend money.USD) *budgetv1alpha1.RunLimitValues {
	out := &budgetv1alpha1.RunLimitValues{MaxModelRequests: requests, MaxDurationSeconds: int64(wall / time.Second)}
	if spend > 0 {
		out.MaxSpendUsd = spend.String()
	}
	return out
}

// agentTemplateConfig is the config of the agent's active version (a draft has none yet, so its
// latest version's).
func (s *Server) agentTemplateConfig(ctx context.Context, a store.Agent) (agentconfig.Config, error) {
	var cfg agentconfig.Config
	versions, err := s.q.ListAgentVersions(ctx, store.ListAgentVersionsParams{OrgID: a.OrgID, AgentID: a.ID})
	if err != nil {
		return cfg, s.dbError(err, "agent versions")
	}
	for _, v := range versions {
		if v.ID == a.ActiveVersionID || (!a.ActiveVersionID.Valid && v.Version == a.LatestVersion) {
			if cfg, err = agentconfig.Parse(v.Config); err != nil {
				return cfg, s.dbError(err, "agent version")
			}
		}
	}
	return cfg, nil
}

// agentRunLimitsView is what a run of the agent gets now: the active version's values (the
// template's defaults), the admin's override and the two merged.
func (s *Server) agentRunLimitsView(ctx context.Context, org, agent pgtype.UUID) (*budgetv1alpha1.AgentRunLimits, error) {
	a, err := s.q.GetAgent(ctx, store.GetAgentParams{OrgID: org, ID: agent})
	if err != nil {
		return nil, s.dbError(err, "agent")
	}
	cfg, err := s.agentTemplateConfig(ctx, a)
	if err != nil {
		return nil, err
	}
	base := cfg.RunLimits()
	o, err := runlimits.GetOverride(ctx, s.q, org, agent)
	if err != nil {
		return nil, s.dbError(err, "run limits")
	}
	eff := o.Apply(base)
	return &budgetv1alpha1.AgentRunLimits{
		AgentId: a.ID.String(), AgentName: a.DisplayName,
		Effective:       runLimitValuesProto(eff.WallClock, eff.MaxModelRequests, eff.MaxSpend),
		TemplateDefault: runLimitValuesProto(base.WallClock, base.MaxModelRequests, base.MaxSpend),
		Override:        runLimitValuesProto(o.WallClock, o.MaxModelRequests, o.MaxSpend),
	}, nil
}

// GetAgentRunLimits implements BudgetService. Admins and owners.
func (s *Server) GetAgentRunLimits(ctx context.Context, req *budgetv1alpha1.GetAgentRunLimitsRequest) (*budgetv1alpha1.GetAgentRunLimitsResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	id, err := parseID("agent_id", req.GetAgentId())
	if err != nil {
		return nil, err
	}
	l, err := s.agentRunLimitsView(ctx, c.org, id)
	if err != nil {
		return nil, err
	}
	return &budgetv1alpha1.GetAgentRunLimitsResponse{Limits: l}, nil
}

// SetAgentRunLimits implements BudgetService. Admins and owners.
func (s *Server) SetAgentRunLimits(ctx context.Context, req *budgetv1alpha1.SetAgentRunLimitsRequest) (*budgetv1alpha1.SetAgentRunLimitsResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	id, err := parseID("agent_id", req.GetAgentId())
	if err != nil {
		return nil, err
	}
	in := req.GetOverride()
	o := runlimits.Override{
		MaxModelRequests: in.GetMaxModelRequests(),
		WallClock:        time.Duration(in.GetMaxDurationSeconds()) * time.Second,
	}
	if in.GetMaxSpendUsd() != "" {
		if o.MaxSpend, err = money.Parse(in.GetMaxSpendUsd()); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
		}
	}
	if err := s.budgets.SetAgentRunLimits(requestCtx(ctx), c.org, id, c.auditActor(), o); err != nil {
		return nil, s.budgetError(err)
	}
	l, err := s.agentRunLimitsView(ctx, c.org, id)
	if err != nil {
		return nil, err
	}
	return &budgetv1alpha1.SetAgentRunLimitsResponse{Limits: l}, nil
}

func notificationProto(n store.Notification) *budgetv1alpha1.Notification {
	out := &budgetv1alpha1.Notification{Id: n.ID.String(), Kind: n.Kind, CreatedAt: timestamppb.New(n.CreatedAt.Time)}
	var p struct {
		Level       string `json:"level"`
		Threshold   int32  `json:"threshold"`
		LimitUSD    string `json:"limit_usd"`
		SpentUSD    string `json:"spent_usd"`
		WindowStart string `json:"window_start"`
		AgentID     string `json:"agent_id"`
		Message     string `json:"message"`
	}
	if json.Unmarshal(n.Payload, &p) == nil {
		out.Message = p.Message
		out.Level, out.Threshold, out.LimitUsd, out.SpentUsd = p.Level, p.Threshold, p.LimitUSD, p.SpentUSD
		if p.AgentID != "" && p.AgentID != "00000000-0000-0000-0000-000000000000" {
			out.AgentId = p.AgentID
		}
		if t, err := time.Parse(time.RFC3339, p.WindowStart); err == nil {
			out.WindowStart = timestamppb.New(t)
		}
	}
	if n.ReadAt.Valid {
		out.ReadAt = timestamppb.New(n.ReadAt.Time)
	}
	return out
}

// ListNotifications implements BudgetService. Any member; each sees their own.
func (s *Server) ListNotifications(ctx context.Context, req *budgetv1alpha1.ListNotificationsRequest) (*budgetv1alpha1.ListNotificationsResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleViewer)
	if err != nil {
		return nil, err
	}
	n := req.GetMaxResults()
	if n <= 0 {
		n = 50
	}
	rows, err := s.q.ListNotifications(ctx, store.ListNotificationsParams{OrgID: c.org, UserID: c.actor(), MaxRows: min(n, 200)})
	if err != nil {
		return nil, s.dbError(err, "notifications")
	}
	res := &budgetv1alpha1.ListNotificationsResponse{}
	for _, r := range rows {
		res.Notifications = append(res.Notifications, notificationProto(r))
	}
	return res, nil
}

// MarkNotificationRead implements BudgetService. Any member, for their own notifications.
func (s *Server) MarkNotificationRead(ctx context.Context, req *budgetv1alpha1.MarkNotificationReadRequest) (*budgetv1alpha1.MarkNotificationReadResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleViewer)
	if err != nil {
		return nil, err
	}
	id, err := parseID("notification_id", req.GetNotificationId())
	if err != nil {
		return nil, err
	}
	n, err := s.q.MarkNotificationRead(ctx, store.MarkNotificationReadParams{OrgID: c.org, UserID: c.actor(), ID: id})
	if err != nil {
		return nil, s.dbError(err, "notification")
	}
	if n == 0 {
		return nil, connect.NewError(connect.CodeNotFound, "notification not found")
	}
	return &budgetv1alpha1.MarkNotificationReadResponse{}, nil
}

// ---- PriceService

func ratesProto(w pricing.Wire) *budgetv1alpha1.Rates {
	return &budgetv1alpha1.Rates{Input: w.Input, Output: w.Output, CacheRead: w.CacheRead, CacheWrite: w.CacheWrite, Reasoning: w.Reasoning}
}

func ratesWire(r *budgetv1alpha1.Rates) pricing.Wire {
	return pricing.Wire{Input: r.GetInput(), Output: r.GetOutput(), CacheRead: r.GetCacheRead(), CacheWrite: r.GetCacheWrite(), Reasoning: r.GetReasoning()}
}

func tiersProto(ts []pricing.WireTier) []*budgetv1alpha1.PriceTier {
	var out []*budgetv1alpha1.PriceTier
	for _, t := range ts {
		out = append(out, &budgetv1alpha1.PriceTier{ContextOver: t.ContextOver, Rates: ratesProto(t.Prices)})
	}
	return out
}

func entryProto(e pricing.Entry) *budgetv1alpha1.PriceEntry {
	return &budgetv1alpha1.PriceEntry{
		Id: e.ID, Provider: e.Provider, Match: &budgetv1alpha1.PriceMatch{Glob: e.Match.Glob, Regex: e.Match.Regex}, ValidFrom: e.ValidFrom,
		Rates: ratesProto(e.Prices), Tiers: tiersProto(e.Tiers), Tokenizer: e.Tokenizer, Ratio: e.Ratio,
		ToolOverheadTokens: int32(e.ToolOverheadTokens), //nolint:gosec // validated to at most 100000
		HiddenThinking:     e.HiddenThinking,
		SourceUrl:          e.Source.URL, SourceAccessed: e.Source.Accessed, Note: e.Note,
	}
}

func versionProto(o prices.Override) *budgetv1alpha1.PriceVersion {
	out := &budgetv1alpha1.PriceVersion{
		Id: o.ID, Name: o.Name, Provider: o.Entry.Provider, Match: &budgetv1alpha1.PriceMatch{Glob: o.Entry.Match.Glob, Regex: o.Entry.Match.Regex},
		Rates: ratesProto(o.Entry.Prices), Tiers: tiersProto(o.Entry.Tiers), Tokenizer: o.Entry.Tokenizer, Ratio: o.Entry.Ratio,
		ValidFrom: timestamppb.New(o.ValidFrom), CreatedBy: o.CreatedBy, CreatedAt: timestamppb.New(o.CreatedAt),
		MaxOutput:          int32(o.Entry.MaxOutput),                                                  //nolint:gosec // validated to at most 10M
		ToolOverheadTokens: int32(o.Entry.ToolOverheadTokens), HiddenThinking: o.Entry.HiddenThinking, //nolint:gosec // validated to at most 100k
		Note: o.Entry.Note, NeedsReview: prices.NeedsReview(o.CreatedBy, o.Entry.Note),
	}
	if o.RetiredAt != nil {
		out.RetiredAt = timestamppb.New(*o.RetiredAt)
	}
	return out
}

// ListPrices implements PriceService. Admins and owners.
func (s *Server) ListPrices(ctx context.Context, req *budgetv1alpha1.ListPricesRequest) (*budgetv1alpha1.ListPricesResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	list, err := s.prices.ListOverrides(ctx, c.org)
	if err != nil {
		return nil, s.budgetError(err)
	}
	res := &budgetv1alpha1.ListPricesResponse{}
	for _, o := range list {
		res.Prices = append(res.Prices, versionProto(o))
	}
	return res, nil
}

// SetPrice implements PriceService. Admins and owners.
func (s *Server) SetPrice(ctx context.Context, req *budgetv1alpha1.SetPriceRequest) (*budgetv1alpha1.SetPriceResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	e := pricing.Entry{
		ID: req.GetName(), Provider: req.GetProvider(), Match: pricing.Match{Glob: req.GetMatch().GetGlob(), Regex: req.GetMatch().GetRegex()},
		Tokenizer: req.GetTokenizer(), Ratio: req.GetRatio(), ToolOverheadTokens: int(req.GetToolOverheadTokens()), HiddenThinking: req.GetHiddenThinking(), MaxOutput: int(req.GetMaxOutput()),
	}
	e.Prices = ratesWire(req.GetRates())
	for _, t := range req.GetTiers() {
		e.Tiers = append(e.Tiers, pricing.WireTier{ContextOver: t.GetContextOver(), Prices: ratesWire(t.GetRates())})
	}
	if e.Tokenizer == "" {
		e.Tokenizer = pricing.TokenizerBytes
	}
	if e.Ratio == "" {
		e.Ratio = "3.0"
	}
	in := prices.OverrideInput{Entry: e}
	if req.GetValidFrom() != nil {
		in.ValidFrom = req.GetValidFrom().AsTime()
	}
	got, err := s.prices.SetOverride(requestCtx(ctx), c.org, c.auditActor(), in)
	if err != nil {
		return nil, s.budgetError(err)
	}
	return &budgetv1alpha1.SetPriceResponse{Price: versionProto(got), Warnings: prices.EntryWarnings(e)}, nil
}

// RetirePrice implements PriceService. Admins and owners.
func (s *Server) RetirePrice(ctx context.Context, req *budgetv1alpha1.RetirePriceRequest) (*budgetv1alpha1.RetirePriceResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	if err := s.prices.RetireOverride(requestCtx(ctx), c.org, c.auditActor(), req.GetName()); err != nil {
		return nil, s.budgetError(err)
	}
	return &budgetv1alpha1.RetirePriceResponse{}, nil
}

// GetDefaultPrices implements PriceService. Admins and owners.
func (s *Server) GetDefaultPrices(ctx context.Context, req *budgetv1alpha1.GetDefaultPricesRequest) (*budgetv1alpha1.GetDefaultPricesResponse, error) {
	if _, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin); err != nil {
		return nil, err
	}
	st, err := s.prices.Defaults(ctx)
	if err != nil {
		return nil, s.budgetError(err)
	}
	res := &budgetv1alpha1.GetDefaultPricesResponse{Version: st.Current, PreviousVersions: st.Previous}
	for _, e := range st.Entries {
		res.Entries = append(res.Entries, entryProto(e))
	}
	return res, nil
}

// GetDefaultPriceChanges implements PriceService. Admins and owners.
func (s *Server) GetDefaultPriceChanges(ctx context.Context, req *budgetv1alpha1.GetDefaultPriceChangesRequest) (*budgetv1alpha1.GetDefaultPriceChangesResponse, error) {
	if _, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin); err != nil {
		return nil, err
	}
	from, to, changes, err := s.prices.DefaultsDiff(ctx, req.GetFromVersion(), req.GetToVersion())
	if err != nil {
		return nil, s.budgetError(err)
	}
	res := &budgetv1alpha1.GetDefaultPriceChangesResponse{FromVersion: from, ToVersion: to}
	for _, c := range changes {
		pc := &budgetv1alpha1.PriceChange{Kind: c.Kind, Id: c.ID, ValidFrom: c.ValidFrom, Fields: c.Fields}
		if c.Before != nil {
			pc.Before = entryProto(*c.Before)
		}
		if c.After != nil {
			pc.After = entryProto(*c.After)
		}
		res.Changes = append(res.Changes, pc)
	}
	return res, nil
}

// ListFlaggedPrices implements PriceService. Admins and owners.
func (s *Server) ListFlaggedPrices(ctx context.Context, req *budgetv1alpha1.ListFlaggedPricesRequest) (*budgetv1alpha1.ListFlaggedPricesResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	since := time.Now().Add(-7 * 24 * time.Hour)
	if req.GetSince() != nil {
		since = req.GetSince().AsTime()
	}
	list, err := s.prices.ListFlagged(ctx, c.org, since)
	if err != nil {
		return nil, s.budgetError(err)
	}
	res := &budgetv1alpha1.ListFlaggedPricesResponse{}
	for _, f := range list {
		res.Flagged = append(res.Flagged, &budgetv1alpha1.FlaggedPrice{
			PriceRef: f.PriceRef, Model: f.Model, Requests: f.Requests, LastSeen: timestamppb.New(f.LastSeen),
			CostUsd: ledger.FormatUSD(f.CostUSD), GatewayCostUsd: ledger.FormatUSD(f.GatewayCostUSD),
			GatewayDiff: f.GatewayDiff, UsageMissing: f.UsageMissing, MaxOutputCapped: f.MaxOutputCapped, PriceError: f.PriceError,
		})
	}
	return res, nil
}

// orgContinuationView is the org's default per-task cap for continuation runs.
func (s *Server) orgContinuationView(ctx context.Context, org pgtype.UUID) (*budgetv1alpha1.OrgContinuationLimits, error) {
	set, err := orgsettings.Get(ctx, s.q, org, orgsettings.Defaults())
	if err != nil {
		return nil, s.dbError(err, "org settings")
	}
	// Overridden reads the stored column, not the value, so an org that set the platform default's
	// amount still reads as having its own value.
	row, err := s.q.GetOrgSettings(ctx, org)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, s.dbError(err, "org settings")
	}
	return &budgetv1alpha1.OrgContinuationLimits{
		TaskSpendUsd:                set.ContinuationTaskSpend.String(),
		PlatformDefaultTaskSpendUsd: orgsettings.DefaultContinuationTaskSpend.String(),
		Overridden:                  err == nil && row.ContinuationTaskSpendUsd.Valid,
	}, nil
}

// GetOrgContinuationLimits implements BudgetService. Admins and owners.
func (s *Server) GetOrgContinuationLimits(ctx context.Context, req *budgetv1alpha1.GetOrgContinuationLimitsRequest) (*budgetv1alpha1.GetOrgContinuationLimitsResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	l, err := s.orgContinuationView(ctx, c.org)
	if err != nil {
		return nil, err
	}
	return &budgetv1alpha1.GetOrgContinuationLimitsResponse{Limits: l}, nil
}

// SetOrgContinuationTaskSpend implements BudgetService. Admins and owners.
func (s *Server) SetOrgContinuationTaskSpend(ctx context.Context, req *budgetv1alpha1.SetOrgContinuationTaskSpendRequest) (*budgetv1alpha1.SetOrgContinuationTaskSpendResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	var v money.USD
	if req.GetTaskSpendUsd() != "" {
		if v, err = money.Parse(req.GetTaskSpendUsd()); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
		}
		if v == 0 {
			return nil, connect.NewError(connect.CodeInvalidArgument, "the task spend cap must be more than 0; leave it empty to follow the default")
		}
	}
	if err := s.budgets.SetOrgContinuationTaskSpend(requestCtx(ctx), c.org, c.auditActor(), v); err != nil {
		return nil, s.budgetError(err)
	}
	l, err := s.orgContinuationView(ctx, c.org)
	if err != nil {
		return nil, err
	}
	return &budgetv1alpha1.SetOrgContinuationTaskSpendResponse{Limits: l}, nil
}

func continuationValuesProto(c runlimits.Continuation) *budgetv1alpha1.ContinuationValues {
	return &budgetv1alpha1.ContinuationValues{MaxContinuations: int32(c.Max), TaskSpendUsd: c.TaskSpend.String()} //nolint:gosec // bounded by MaxContinuationsLimit
}

// agentContinuationView is what a task of the agent gets: the template's count and the org's cap,
// the admin's override and the two merged.
func (s *Server) agentContinuationView(ctx context.Context, org, agent pgtype.UUID) (*budgetv1alpha1.AgentContinuationLimits, error) {
	a, err := s.q.GetAgent(ctx, store.GetAgentParams{OrgID: org, ID: agent})
	if err != nil {
		return nil, s.dbError(err, "agent")
	}
	cfg, err := s.agentTemplateConfig(ctx, a)
	if err != nil {
		return nil, err
	}
	set, err := orgsettings.Get(ctx, s.q, org, orgsettings.Defaults())
	if err != nil {
		return nil, s.dbError(err, "org settings")
	}
	o, err := runlimits.GetContinuationOverride(ctx, s.q, org, agent)
	if err != nil {
		return nil, s.dbError(err, "continuation limits")
	}
	eff, err := runlimits.ContinuationFor(ctx, s.q, org, agent, cfg)
	if err != nil {
		return nil, s.dbError(err, "continuation limits")
	}
	ov := &budgetv1alpha1.ContinuationOverride{}
	if o.MaxContinuations != nil {
		n := int32(*o.MaxContinuations) //nolint:gosec // bounded by MaxContinuationsLimit
		ov.MaxContinuations = &n
	}
	if o.TaskSpend > 0 {
		ov.TaskSpendUsd = o.TaskSpend.String()
	}
	return &budgetv1alpha1.AgentContinuationLimits{
		AgentId: a.ID.String(), AgentName: a.DisplayName,
		Effective:       continuationValuesProto(eff),
		TemplateDefault: continuationValuesProto(runlimits.Continuation{Max: cfg.MaxContinuations(), TaskSpend: set.ContinuationTaskSpend}),
		Override:        ov,
	}, nil
}

// GetAgentContinuationLimits implements BudgetService. Admins and owners.
func (s *Server) GetAgentContinuationLimits(ctx context.Context, req *budgetv1alpha1.GetAgentContinuationLimitsRequest) (*budgetv1alpha1.GetAgentContinuationLimitsResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	id, err := parseID("agent_id", req.GetAgentId())
	if err != nil {
		return nil, err
	}
	l, err := s.agentContinuationView(ctx, c.org, id)
	if err != nil {
		return nil, err
	}
	return &budgetv1alpha1.GetAgentContinuationLimitsResponse{Limits: l}, nil
}

// SetAgentContinuationLimits implements BudgetService. Admins and owners.
func (s *Server) SetAgentContinuationLimits(ctx context.Context, req *budgetv1alpha1.SetAgentContinuationLimitsRequest) (*budgetv1alpha1.SetAgentContinuationLimitsResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	id, err := parseID("agent_id", req.GetAgentId())
	if err != nil {
		return nil, err
	}
	in := req.GetOverride()
	var o runlimits.ContinuationOverride
	if in != nil && in.MaxContinuations != nil {
		n := int(*in.MaxContinuations)
		o.MaxContinuations = &n
	}
	if in.GetTaskSpendUsd() != "" {
		if o.TaskSpend, err = money.Parse(in.GetTaskSpendUsd()); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
		}
		if o.TaskSpend == 0 {
			return nil, connect.NewError(connect.CodeInvalidArgument, "the task spend cap must be more than 0; leave it empty to follow the default")
		}
	}
	if err := s.budgets.SetAgentContinuationLimits(requestCtx(ctx), c.org, id, c.auditActor(), o); err != nil {
		return nil, s.budgetError(err)
	}
	l, err := s.agentContinuationView(ctx, c.org, id)
	if err != nil {
		return nil, err
	}
	return &budgetv1alpha1.SetAgentContinuationLimitsResponse{Limits: l}, nil
}
