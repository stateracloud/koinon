// SPDX-License-Identifier: Apache-2.0

// Package budget is the org and agent budgets of plan item BG1 (ADR 0056 amendment 2026-10-24).
//
// Three limits can stop an agent:
//
//   - the agent's daily limit (UTC day), set by an org admin; a template's spendUSD is only the
//     default a new agent starts with;
//   - the org's monthly cap (calendar month in UTC), set by an org admin;
//   - the platform ceiling, per org, set by a platform admin and off by default. An org's cap
//     cannot exceed it, and it is enforced the same way as the cap.
//
// The gateway budget of an agent's key is the smallest remaining amount of the three ([Status.Net]).
// The ledger cancels the run at the first limit reached and says which. Spend is charged after a
// response completes, so a limit can be overshot by the responses in flight when it is reached
// (one per running agent) plus the export lag (ADR 0050).
//
// At 80% and 100% of each limit, owners and admins (platform admins for the ceiling) get an
// in-app notification, once per limit and window. [Notifier] is the seam where email attaches later.
package budget

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/money"
)

// Levels, in the order they are checked.
const (
	AgentDaily      = "agent_daily"
	OrgMonthly      = "org_monthly"
	PlatformCeiling = "platform_ceiling"
)

// Thresholds are the fractions of a limit at which people are told.
const (
	WarnAt = 0.8
	StopAt = 1.0
)

// DayWindow is the agent window.
const DayWindow = 24 * time.Hour

// DayStart is the start of the UTC day containing t.
func DayStart(t time.Time) time.Time { return t.UTC().Truncate(DayWindow) }

// MonthStart is the start of the calendar month (UTC) containing t.
func MonthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// Level is one limit and where it stands.
type Level struct {
	Name        string
	Limit       money.USD
	Spent       money.USD
	WindowStart time.Time
}

// Remaining is what is left, never below zero.
func (l Level) Remaining() money.USD { return l.Limit.SubFloor(l.Spent) }

// Reached reports spent >= limit.
func (l Level) Reached() bool { return l.Spent >= l.Limit }

// Fraction is spent/limit.
func (l Level) Fraction() float64 { return money.Ratio(l.Spent, l.Limit) }

// Status is an agent's limits and spend at a moment.
type Status struct {
	Org, Agent pgtype.UUID
	Levels     []Level
}

// Net is the smallest remaining amount over all levels and the level that binds. ok is false
// when no level applies.
func (s Status) Net() (amount money.USD, binding string, ok bool) {
	for _, l := range s.Levels {
		if r := l.Remaining(); !ok || r < amount {
			amount, binding, ok = r, l.Name, true
		}
	}
	return amount, binding, ok
}

// Generation names the gateway counter for the current net amount: the day plus a digest of
// every spend the amount derives from, so each new amount starts a fresh counter (ADR 0050).
func (s Status) Generation(now time.Time) string {
	h := sha256.New()
	for _, l := range s.Levels {
		h.Write(fmt.Appendf(nil, "%s|%d|%d;", l.Name, l.Limit, l.Spent))
	}
	return fmt.Sprintf("%d-%s", DayStart(now).Unix(), hex.EncodeToString(h.Sum(nil))[:16])
}

// Reached lists the levels at or over their limit.
func (s Status) Reached() []Level {
	var out []Level
	for _, l := range s.Levels {
		if l.Reached() {
			out = append(out, l)
		}
	}
	return out
}

// Org describes a limit set on an org.
type Org struct {
	MonthlyCap      *money.USD
	PlatformCeiling *money.USD
}

func parseNumeric(s string) (money.USD, error) { return money.ParseNumeric(s) }

func orgFromRow(r store.GetOrgBudgetRow) (Org, error) {
	var o Org
	if r.MonthlyCap != "" {
		v, err := parseNumeric(r.MonthlyCap)
		if err != nil {
			return o, err
		}
		o.MonthlyCap = &v
	}
	if r.PlatformCeiling != "" {
		v, err := parseNumeric(r.PlatformCeiling)
		if err != nil {
			return o, err
		}
		o.PlatformCeiling = &v
	}
	return o, nil
}

// GetOrg returns the org's cap and ceiling (both nil when none is set).
func GetOrg(ctx context.Context, q *store.Queries, org pgtype.UUID) (Org, error) {
	r, err := q.GetOrgBudget(ctx, org)
	if errors.Is(err, pgx.ErrNoRows) {
		return Org{}, nil
	} else if err != nil {
		return Org{}, err
	}
	return orgFromRow(r)
}

// AgentDailyLimit is the agent's daily limit: its own row, else fallback (the spendUSD of its
// agent version). ok is false when neither exists.
func AgentDailyLimit(ctx context.Context, q *store.Queries, org, agent pgtype.UUID, fallback money.USD) (money.USD, bool, error) {
	r, err := q.GetAgentBudget(ctx, store.GetAgentBudgetParams{OrgID: org, AgentID: agent})
	switch {
	case err == nil:
		v, err := parseNumeric(r.DailyLimit)
		return v, true, err
	case !errors.Is(err, pgx.ErrNoRows):
		return 0, false, err
	}
	return fallback, fallback > 0, nil
}

// Compute reads the agent's limits and the spend in their windows at now. fallback is the
// agent version's template limit (0 for none).
func Compute(ctx context.Context, q *store.Queries, org, agent pgtype.UUID, fallback money.USD, now time.Time) (Status, error) {
	st := Status{Org: org, Agent: agent}
	spent := func(since time.Time, fn func() (string, error)) (money.USD, error) {
		s, err := fn()
		if err != nil {
			return 0, err
		}
		return money.ParseNumeric(s)
	}
	if limit, ok, err := AgentDailyLimit(ctx, q, org, agent, fallback); err != nil {
		return st, err
	} else if ok {
		since := DayStart(now)
		sp, err := spent(since, func() (string, error) {
			return q.GetAgentSpendSince(ctx, store.GetAgentSpendSinceParams{OrgID: org, AgentID: agent, Since: pgtype.Timestamptz{Time: since, Valid: true}})
		})
		if err != nil {
			return st, err
		}
		st.Levels = append(st.Levels, Level{AgentDaily, limit, sp, since})
	}
	o, err := GetOrg(ctx, q, org)
	if err != nil {
		return st, err
	}
	if o.MonthlyCap == nil && o.PlatformCeiling == nil {
		return st, nil
	}
	since := MonthStart(now)
	sp, err := spent(since, func() (string, error) {
		return q.GetOrgSpendSince(ctx, store.GetOrgSpendSinceParams{OrgID: org, Since: pgtype.Timestamptz{Time: since, Valid: true}})
	})
	if err != nil {
		return st, err
	}
	if o.MonthlyCap != nil {
		st.Levels = append(st.Levels, Level{OrgMonthly, *o.MonthlyCap, sp, since})
	}
	if o.PlatformCeiling != nil {
		st.Levels = append(st.Levels, Level{PlatformCeiling, *o.PlatformCeiling, sp, since})
	}
	return st, nil
}

// Notice is an alert about a limit.
type Notice struct {
	Org   pgtype.UUID
	Kind  string // budget.warning or budget.reached
	Level Level
	Agent pgtype.UUID // set for the agent's daily limit
	// Threshold is 80 or 100.
	Threshold int
}

// Notification kinds.
const (
	KindWarning = "budget.warning"
	KindReached = "budget.reached"
)

// Notifier delivers a Notice. The in-app notifier stores it; an email notifier would wrap it.
type Notifier interface {
	Notify(ctx context.Context, q *store.Queries, n Notice) error
}

// InApp stores notifications for owners and admins (and platform admins for the ceiling).
type InApp struct{}

// Notify implements Notifier.
func (InApp) Notify(ctx context.Context, q *store.Queries, n Notice) error {
	var users []pgtype.UUID
	var err error
	if n.Level.Name == PlatformCeiling {
		users, err = q.ListPlatformAdminIDs(ctx)
	} else {
		users, err = q.ListOrgBudgetRecipients(ctx, n.Org)
	}
	if err != nil {
		return err
	}
	scope := n.Level.Name
	if n.Agent.Valid {
		scope += ":" + n.Agent.String()
	}
	key := fmt.Sprintf("%s:%s:%d", scope, n.Level.WindowStart.Format("2006-01-02"), n.Threshold)
	payload, err := json.Marshal(map[string]any{
		"level": n.Level.Name, "threshold": n.Threshold, "limit_usd": n.Level.Limit.String(), "spent_usd": n.Level.Spent.String(),
		"window_start": n.Level.WindowStart.Format(time.RFC3339), "agent_id": n.Agent.String(),
	})
	if err != nil {
		return err
	}
	for _, u := range users {
		if _, err := q.InsertNotification(ctx, store.InsertNotificationParams{OrgID: n.Org, UserID: u, Kind: n.Kind, DedupeKey: key, Payload: payload}); err != nil {
			return err
		}
	}
	return nil
}

// Alert tells people about each limit that spending delta (the cost of the entry just recorded)
// pushed over 80% or 100%. st includes the delta already.
func Alert(ctx context.Context, q *store.Queries, nf Notifier, st Status, delta money.USD) error {
	if nf == nil {
		nf = InApp{}
	}
	for _, l := range st.Levels {
		before := l
		before.Spent = l.Spent.SubFloor(delta)
		for _, t := range []struct {
			kind string
			frac float64
			pct  int
		}{{KindWarning, WarnAt, 80}, {KindReached, StopAt, 100}} {
			if l.Fraction() < t.frac || before.Fraction() >= t.frac {
				continue
			}
			n := Notice{Org: st.Org, Kind: t.kind, Level: l, Threshold: t.pct}
			if l.Name == AgentDaily {
				n.Agent = st.Agent
			}
			if err := nf.Notify(ctx, q, n); err != nil {
				return fmt.Errorf("budget: notify: %w", err)
			}
		}
	}
	return nil
}

// Errors of the service layer. Messages are safe to show.
var (
	ErrForbidden = errors.New("only a platform admin may set the platform ceiling")
	ErrNotFound  = errors.New("not found")
)

// InvalidError is a validation failure.
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string { return e.Msg }

// Service changes budgets and audits every change in the same transaction.
type Service struct{ pool *pgxpool.Pool }

// New returns a Service.
func New(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

func (s *Service) tx(ctx context.Context, fn func(q *store.Queries) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error { return fn(store.New(tx)) })
}

func checkAmount(name string, v money.USD) error {
	if v <= 0 || v > money.MaxLimit {
		return &InvalidError{Msg: fmt.Sprintf("%s must be greater than 0 and at most %s USD", name, money.MaxLimit)}
	}
	return nil
}

func summary(o Org) map[string]any {
	out := map[string]any{}
	if o.MonthlyCap != nil {
		out["monthlyCapUSD"] = o.MonthlyCap.String()
	}
	if o.PlatformCeiling != nil {
		out["platformCeilingUSD"] = o.PlatformCeiling.String()
	}
	return out
}

// OrgView is an org's budget as callers see it.
type OrgView struct {
	MonthlyCap      *money.USD
	PlatformCeiling *money.USD
	// SpentThisMonth is the org's recorded spend in the current calendar month (UTC).
	SpentThisMonth money.USD
	MonthStart     time.Time
}

// GetOrg returns the org's budget and its spend this month.
func (s *Service) GetOrg(ctx context.Context, org pgtype.UUID, now time.Time) (OrgView, error) {
	q := store.New(s.pool)
	o, err := GetOrg(ctx, q, org)
	if err != nil {
		return OrgView{}, err
	}
	since := MonthStart(now)
	sp, err := q.GetOrgSpendSince(ctx, store.GetOrgSpendSinceParams{OrgID: org, Since: pgtype.Timestamptz{Time: since, Valid: true}})
	if err != nil {
		return OrgView{}, err
	}
	spent, err := money.ParseNumeric(sp)
	return OrgView{MonthlyCap: o.MonthlyCap, PlatformCeiling: o.PlatformCeiling, SpentThisMonth: spent, MonthStart: since}, err
}

// SetOrgCap sets the org's monthly cap, or clears it with nil. It cannot exceed the platform ceiling.
func (s *Service) SetOrgCap(ctx context.Context, org pgtype.UUID, actor audit.Actor, capUSD *money.USD) error {
	if capUSD != nil {
		if err := checkAmount("the monthly cap", *capUSD); err != nil {
			return err
		}
	}
	return s.tx(ctx, func(q *store.Queries) error {
		before, err := GetOrg(ctx, q, org)
		if err != nil {
			return err
		}
		if capUSD != nil && before.PlatformCeiling != nil && *capUSD > *before.PlatformCeiling {
			return &InvalidError{Msg: fmt.Sprintf("the monthly cap cannot exceed the platform ceiling of %s USD", before.PlatformCeiling)}
		}
		arg := pgtype.Text{}
		if capUSD != nil {
			arg = pgtype.Text{String: capUSD.Numeric(), Valid: true}
		}
		if _, err := q.SetOrgMonthlyCap(ctx, store.SetOrgMonthlyCapParams{OrgID: org, MonthlyCap: arg, UpdatedBy: actor.String()}); err != nil {
			return err
		}
		after := before
		after.MonthlyCap = capUSD
		return audit.RecordErr(ctx, q, audit.Event{
			Org: org, Actor: actor, Action: "budget.org_cap.set", TargetKind: "org_budget", TargetID: org.String(),
			Before: summary(before), After: summary(after),
		})
	})
}

// SetPlatformCeiling sets the org's platform ceiling, or clears it with nil. Only a platform admin may;
// the org's cap must already fit under it.
func (s *Service) SetPlatformCeiling(ctx context.Context, org pgtype.UUID, actor audit.Actor, user pgtype.UUID, ceiling *money.USD) error {
	if ceiling != nil {
		if err := checkAmount("the platform ceiling", *ceiling); err != nil {
			return err
		}
	}
	return s.tx(ctx, func(q *store.Queries) error {
		ok, err := q.IsPlatformAdmin(ctx, user)
		if err != nil {
			return err
		}
		if !ok {
			return ErrForbidden
		}
		before, err := GetOrg(ctx, q, org)
		if err != nil {
			return err
		}
		if ceiling != nil && before.MonthlyCap != nil && *before.MonthlyCap > *ceiling {
			return &InvalidError{Msg: fmt.Sprintf("the org's monthly cap of %s USD is above this ceiling; lower the cap first", before.MonthlyCap)}
		}
		arg := pgtype.Text{}
		if ceiling != nil {
			arg = pgtype.Text{String: ceiling.Numeric(), Valid: true}
		}
		if _, err := q.SetOrgPlatformCeiling(ctx, store.SetOrgPlatformCeilingParams{OrgID: org, PlatformCeiling: arg, UpdatedBy: actor.String()}); err != nil {
			return err
		}
		after := before
		after.PlatformCeiling = ceiling
		return audit.RecordErr(ctx, q, audit.Event{
			Org: org, Actor: actor, Action: "budget.platform_ceiling.set", TargetKind: "org_budget", TargetID: org.String(),
			Before: summary(before), After: summary(after),
		})
	})
}

// AgentView is an agent's daily limit and today's spend.
type AgentView struct {
	Agent      pgtype.UUID
	DailyLimit money.USD
	SpentToday money.USD
	DayStart   time.Time
}

// SetAgentDaily sets an agent's daily limit.
func (s *Service) SetAgentDaily(ctx context.Context, org, agent pgtype.UUID, actor audit.Actor, limit money.USD) error {
	if err := checkAmount("the daily limit", limit); err != nil {
		return err
	}
	return s.tx(ctx, func(q *store.Queries) error {
		if _, err := q.GetAgent(ctx, store.GetAgentParams{OrgID: org, ID: agent}); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		var before any
		if cur, err := q.GetAgentBudget(ctx, store.GetAgentBudgetParams{OrgID: org, AgentID: agent}); err == nil {
			before = map[string]any{"dailyLimitUSD": cur.DailyLimit}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if _, err := q.SetAgentBudget(ctx, store.SetAgentBudgetParams{OrgID: org, AgentID: agent, DailyLimit: limit.Numeric(), UpdatedBy: actor.String()}); err != nil {
			return err
		}
		return audit.RecordErr(ctx, q, audit.Event{
			Org: org, Actor: actor, Action: "budget.agent_daily.set", TargetKind: "agent", TargetID: agent.String(),
			Before: before, After: map[string]any{"dailyLimitUSD": limit.String()},
		})
	})
}

// GetAgent returns an agent's effective daily limit (its own, else the template's value from
// fallback) and today's spend.
func (s *Service) GetAgent(ctx context.Context, org, agent pgtype.UUID, fallback money.USD, now time.Time) (AgentView, bool, error) {
	q := store.New(s.pool)
	limit, ok, err := AgentDailyLimit(ctx, q, org, agent, fallback)
	if err != nil || !ok {
		return AgentView{}, ok, err
	}
	since := DayStart(now)
	sp, err := q.GetAgentSpendSince(ctx, store.GetAgentSpendSinceParams{OrgID: org, AgentID: agent, Since: pgtype.Timestamptz{Time: since, Valid: true}})
	if err != nil {
		return AgentView{}, false, err
	}
	spent, err := money.ParseNumeric(sp)
	return AgentView{Agent: agent, DailyLimit: limit, SpentToday: spent, DayStart: since}, true, err
}
