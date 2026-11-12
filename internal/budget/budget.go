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
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/money"
	"github.com/stateracloud/koinon/internal/orgsettings"
	"github.com/stateracloud/koinon/internal/runlimits"
)

// Levels, in the order they are checked.
const (
	AgentDaily      = "agent_daily"
	OrgMonthly      = "org_monthly"
	PlatformCeiling = "platform_ceiling"
	// SystemDaily is the daily budget of an org's system key (the provider echo check), a level
	// of ComputeSystem only.
	SystemDaily = "system_daily"
	// RunSpend is the spend limit of the run an agent's key is serving (ADR 0081). It is a level of
	// the key's net limit while the run is open, narrower than the agent's daily limit, and covers
	// only that run. A task scope (old issue 183) is another level of the same kind.
	RunSpend = "run_spend"
)

// SystemDailyLimit is the daily budget of an org's system key: a check costs a fraction of a
// cent, and this bounds a runaway caller. Its spend counts toward the org's monthly cap.
var SystemDailyLimit = money.FromCents(10)

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
	// Run is the run a RunSpend level covers; unset for the others.
	Run pgtype.UUID
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

// Current lists the levels at or over their limit in the window that is current at now. A
// limit cancels runs only in its current window: a late entry from last month reaching last
// month's cap must not stop this month's runs.
func (s Status) Current(now time.Time) []Level {
	var out []Level
	for _, l := range s.Reached() {
		if l.Name == RunSpend {
			out = append(out, l) // the run is open, so its level is current
			continue
		}
		current := MonthStart(now)
		if l.Name == AgentDaily || l.Name == SystemDaily {
			current = DayStart(now)
		}
		if l.WindowStart.Equal(current) {
			out = append(out, l)
		}
	}
	return out
}

// Pick is the one limit a stop names when several are reached: of the levels reached in their
// current window at now ([Status.Current]), the broadest scope first, platform ceiling, then org
// monthly cap, then agent daily limit, then the run's own limit, because the broadest one decides which runs stop (an
// org-wide limit stops every run of the org) and raising only a narrower limit would not let the
// agent run again. ok is false when none is reached. Every stop path reports the limit Pick
// returns: the ledger and the reconciler here; agentd adopts it (rc.6, PR 2).
func (s Status) Pick(now time.Time) (Level, bool) {
	rank := map[string]int{PlatformCeiling: 0, OrgMonthly: 1, AgentDaily: 2, RunSpend: 3, SystemDaily: 4}
	var pick Level
	found := false
	for _, l := range s.Current(now) {
		if !found || rank[l.Name] < rank[pick.Name] {
			pick, found = l, true
		}
	}
	return pick, found
}

// Title names the level for a person.
func (l Level) Title() string {
	switch l.Name {
	case OrgMonthly:
		return "org monthly cap"
	case PlatformCeiling:
		return "platform ceiling"
	case SystemDaily:
		return "system checks daily limit"
	case RunSpend:
		return "run spend limit"
	}
	return "agent daily limit"
}

// StopMessage is why a run is stopped by the level, as the run's outcome reason.
func (l Level) StopMessage() string {
	if l.Name == RunSpend {
		return fmt.Sprintf("%s reached ($%s of $%s)", l.Title(), l.Spent, l.Limit)
	}
	return fmt.Sprintf("%s reached ($%s of $%s since %s)", l.Title(), l.Spent, l.Limit, l.WindowStart.Format("2006-01-02 15:04 MST"))
}

// StopDetail is the level's part of the payload of a run canceled by a limit.
func (l Level) StopDetail() map[string]any {
	d := map[string]any{"limit": l.Name, "spend_limit_usd": l.Limit.String(), "spent_usd": l.Spent.String()}
	if l.Name != RunSpend {
		d["window_start"] = l.WindowStart.Format("2006-01-02T15:04:05Z")
	}
	return d
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

// WithRun adds the run level to st: the spend limit of run and what the run has spent, from its
// ledger entries. A run without a spend limit (queued before ADR 0081) adds nothing. The run's
// window starts when it was claimed.
func (s Status) WithRun(ctx context.Context, q *store.Queries, run store.Run) (Status, error) {
	l, ok, err := RunLevel(ctx, q, run)
	if err != nil || !ok {
		return s, err
	}
	out := s
	out.Levels = append(append([]Level(nil), s.Levels...), l)
	return out, nil
}

// RunLevel is run's spend limit and spend as a level. ok is false when the run has no limit.
func RunLevel(ctx context.Context, q *store.Queries, run store.Run) (Level, bool, error) {
	limit, ok, err := RunSpendLimit(run)
	if err != nil || !ok {
		return Level{}, false, err
	}
	u, err := q.GetRunUsage(ctx, store.GetRunUsageParams{OrgID: run.OrgID, RunID: run.ID})
	if err != nil {
		return Level{}, false, err
	}
	spent, err := money.ParseNumeric(u.CostUsd)
	if err != nil {
		return Level{}, false, err
	}
	since := run.CreatedAt.Time
	if run.StartedAt.Valid {
		since = run.StartedAt.Time
	}
	return Level{Name: RunSpend, Limit: limit, Spent: spent, WindowStart: since, Run: run.ID}, true, nil
}

// RunSpendLimit is the run's spend limit; ok is false when it has none.
func RunSpendLimit(run store.Run) (money.USD, bool, error) {
	if !run.MaxSpendUsd.Valid {
		return 0, false, nil
	}
	v, err := run.MaxSpendUsd.Value()
	if err != nil {
		return 0, false, fmt.Errorf("budget: run spend limit: %w", err)
	}
	str, _ := v.(string)
	limit, err := money.ParseNumeric(str)
	return limit, err == nil && limit > 0, err
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
		st.Levels = append(st.Levels, Level{Name: AgentDaily, Limit: limit, Spent: sp, WindowStart: since})
	}
	levels, err := orgLevels(ctx, q, org, now)
	st.Levels = append(st.Levels, levels...)
	return st, err
}

// orgLevels are the org's monthly cap and the platform ceiling with the org's spend this month.
// Every principal's spend counts, system checks included.
func orgLevels(ctx context.Context, q *store.Queries, org pgtype.UUID, now time.Time) ([]Level, error) {
	o, err := GetOrg(ctx, q, org)
	if err != nil {
		return nil, err
	}
	if o.MonthlyCap == nil && o.PlatformCeiling == nil {
		return nil, nil
	}
	since := MonthStart(now)
	s, err := q.GetOrgSpendSince(ctx, store.GetOrgSpendSinceParams{OrgID: org, Since: pgtype.Timestamptz{Time: since, Valid: true}})
	if err != nil {
		return nil, err
	}
	sp, err := money.ParseNumeric(s)
	if err != nil {
		return nil, err
	}
	var out []Level
	if o.MonthlyCap != nil {
		out = append(out, Level{Name: OrgMonthly, Limit: *o.MonthlyCap, Spent: sp, WindowStart: since})
	}
	if o.PlatformCeiling != nil {
		out = append(out, Level{Name: PlatformCeiling, Limit: *o.PlatformCeiling, Spent: sp, WindowStart: since})
	}
	return out, nil
}

// ComputeSystem reads the limits of an org's system key and the spend in their windows at now:
// its own daily budget (SystemDailyLimit) and the org-wide levels, which its spend counts toward.
func ComputeSystem(ctx context.Context, q *store.Queries, org pgtype.UUID, now time.Time) (Status, error) {
	st := Status{Org: org}
	since := DayStart(now)
	s, err := q.GetSystemSpendSince(ctx, store.GetSystemSpendSinceParams{OrgID: org, Since: pgtype.Timestamptz{Time: since, Valid: true}})
	if err != nil {
		return st, err
	}
	sp, err := money.ParseNumeric(s)
	if err != nil {
		return st, err
	}
	st.Levels = append(st.Levels, Level{Name: SystemDaily, Limit: SystemDailyLimit, Spent: sp, WindowStart: since})
	levels, err := orgLevels(ctx, q, org, now)
	st.Levels = append(st.Levels, levels...)
	return st, err
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
		if l.Name == RunSpend {
			continue // the agent is told at 80% (the wrap-up notice); a person is not notified per run
		}
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

// tx runs fn in a transaction. A concurrent change of the cap and the ceiling that would leave the
// cap above the ceiling is caught by the table's CHECK (SQLSTATE 23514) and answered as a
// validation error, so two admins cannot both succeed.
func (s *Service) tx(ctx context.Context, fn func(q *store.Queries, b audit.Batch) error) error {
	_, err := audit.InTx(ctx, s.pool, fn)
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23514" {
		return &InvalidError{Msg: "the monthly cap cannot exceed the platform ceiling (changed at the same time); check the current values and try again"}
	}
	return err
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
	// EstimatedThisMonth is the part of it that came from estimated usage.
	EstimatedThisMonth money.USD
	// SystemChecksThisMonth is the part of it that came from the org's system key.
	SystemChecksThisMonth money.USD
	MonthStart            time.Time
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
	if err != nil {
		return OrgView{}, err
	}
	es, err := q.GetOrgEstimatedSpendSince(ctx, store.GetOrgEstimatedSpendSinceParams{OrgID: org, Since: pgtype.Timestamptz{Time: since, Valid: true}})
	if err != nil {
		return OrgView{}, err
	}
	estimated, err := money.ParseNumeric(es)
	if err != nil {
		return OrgView{}, err
	}
	sys, err := q.SpendSystemSince(ctx, store.SpendSystemSinceParams{OrgID: org, Since: pgtype.Timestamptz{Time: since, Valid: true}})
	if err != nil {
		return OrgView{}, err
	}
	systemChecks, err := money.ParseNumeric(sys.CostUsd)
	return OrgView{MonthlyCap: o.MonthlyCap, PlatformCeiling: o.PlatformCeiling, SpentThisMonth: spent, EstimatedThisMonth: estimated, SystemChecksThisMonth: systemChecks, MonthStart: since}, err
}

// SetOrgCap sets the org's monthly cap, or clears it with nil. It cannot exceed the platform ceiling.
func (s *Service) SetOrgCap(ctx context.Context, org pgtype.UUID, actor audit.Actor, capUSD *money.USD) error {
	if capUSD != nil {
		if err := checkAmount("the monthly cap", *capUSD); err != nil {
			return err
		}
	}
	return s.tx(ctx, func(q *store.Queries, b audit.Batch) error {
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
		b.Org(audit.Event{
			Org: org, Actor: actor, Action: "budget.org_cap.set", TargetKind: "org_budget", TargetID: org.String(),
			Before: summary(before), After: summary(after),
		})
		return nil
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
	return s.tx(ctx, func(q *store.Queries, b audit.Batch) error {
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
		b.Org(audit.Event{
			Org: org, Actor: actor, Action: "budget.platform_ceiling.set", TargetKind: "org_budget", TargetID: org.String(),
			Before: summary(before), After: summary(after),
		})
		return nil
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
	return s.tx(ctx, func(q *store.Queries, b audit.Batch) error {
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
		b.Org(audit.Event{
			Org: org, Actor: actor, Action: "budget.agent_daily.set", TargetKind: "agent", TargetID: agent.String(),
			Before: before, After: map[string]any{"dailyLimitUSD": limit.String()},
		})
		return nil
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

// Largest values an admin may set for a run's limits (the template schema's bounds, ADR 0081).
const (
	MaxRunModelRequests = 100_000
	MaxRunDuration      = 24 * time.Hour
)

// SetAgentRunLimits replaces the agent's overrides of its run limits (ADR 0081): a zero field
// clears that override and the agent follows its version's value again; all zero removes the row.
// It applies to runs queued afterwards; a queued or running run keeps the limits it was queued with.
func (s *Service) SetAgentRunLimits(ctx context.Context, org, agent pgtype.UUID, actor audit.Actor, o runlimits.Override) error {
	switch {
	case o.MaxModelRequests < 0 || o.MaxModelRequests > MaxRunModelRequests:
		return &InvalidError{Msg: fmt.Sprintf("the model request limit must be between 1 and %d", MaxRunModelRequests)}
	case o.MaxSpend < 0 || o.MaxSpend > money.MaxLimit:
		return &InvalidError{Msg: fmt.Sprintf("the run spend limit must be greater than 0 and at most %s USD", money.MaxLimit)}
	case o.WallClock < 0 || o.WallClock > MaxRunDuration || o.WallClock%time.Second != 0:
		return &InvalidError{Msg: fmt.Sprintf("the run duration must be a whole number of seconds, at most %s", MaxRunDuration)}
	}
	return s.tx(ctx, func(q *store.Queries, b audit.Batch) error {
		if _, err := q.GetAgent(ctx, store.GetAgentParams{OrgID: org, ID: agent}); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		before, err := runlimits.GetOverride(ctx, q, org, agent)
		if err != nil {
			return err
		}
		if o.IsZero() {
			if err := q.ClearAgentRunLimits(ctx, store.ClearAgentRunLimitsParams{OrgID: org, AgentID: agent}); err != nil {
				return err
			}
			if _, err := q.DeleteAgentRunLimits(ctx, store.DeleteAgentRunLimitsParams{OrgID: org, AgentID: agent}); err != nil {
				return err
			}
		} else if _, err := q.SetAgentRunLimits(ctx, store.SetAgentRunLimitsParams{
			OrgID: org, AgentID: agent, UpdatedBy: actor.String(),
			MaxModelRequests: pgtype.Int8{Int64: o.MaxModelRequests, Valid: o.MaxModelRequests > 0},
			MaxSpendUsd:      pgtype.Text{String: o.MaxSpend.Numeric(), Valid: o.MaxSpend > 0},
			MaxDurationSecs:  pgtype.Int4{Int32: int32(o.WallClock / time.Second), Valid: o.WallClock > 0}, //nolint:gosec // bounded by MaxRunDuration
		}); err != nil {
			return err
		}
		b.Org(audit.Event{
			Org: org, Actor: actor, Action: "budget.agent_run_limits.set", TargetKind: "agent", TargetID: agent.String(),
			Before: overrideSummary(before), After: overrideSummary(o),
		})
		return nil
	})
}

// MaxContinuationsLimit is the most continuation runs a task can be given.
const MaxContinuationsLimit = 10

// SetAgentContinuationLimits replaces the agent's continuation overrides (issue #99): a nil
// MaxContinuations follows the template (0 switches continuation off) and a zero TaskSpend follows
// the org's default. The run-limit overrides stay. It applies from the next time a run hits its time
// limit.
func (s *Service) SetAgentContinuationLimits(ctx context.Context, org, agent pgtype.UUID, actor audit.Actor, o runlimits.ContinuationOverride) error {
	switch {
	case o.MaxContinuations != nil && (*o.MaxContinuations < 0 || *o.MaxContinuations > MaxContinuationsLimit):
		return &InvalidError{Msg: fmt.Sprintf("the number of continuation runs must be between 0 and %d", MaxContinuationsLimit)}
	case o.TaskSpend < 0 || o.TaskSpend > money.MaxLimit:
		return &InvalidError{Msg: fmt.Sprintf("the task spend cap must be greater than 0 and at most %s USD", money.MaxLimit)}
	}
	return s.tx(ctx, func(q *store.Queries, b audit.Batch) error {
		if _, err := q.GetAgent(ctx, store.GetAgentParams{OrgID: org, ID: agent}); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		before, err := runlimits.GetContinuationOverride(ctx, q, org, agent)
		if err != nil {
			return err
		}
		arg := store.SetAgentContinuationLimitsParams{
			OrgID: org, AgentID: agent, UpdatedBy: actor.String(),
			TaskSpendUsd: pgtype.Text{String: o.TaskSpend.Numeric(), Valid: o.TaskSpend > 0},
		}
		if o.MaxContinuations != nil {
			arg.MaxContinuations = pgtype.Int4{Int32: int32(*o.MaxContinuations), Valid: true} //nolint:gosec // bounded above
		}
		if err := q.SetAgentContinuationLimits(ctx, arg); err != nil {
			return err
		}
		b.Org(audit.Event{
			Org: org, Actor: actor, Action: "budget.agent_continuation_limits.set", TargetKind: "agent", TargetID: agent.String(),
			Before: continuationSummary(before), After: continuationSummary(o),
		})
		return nil
	})
}

// SetOrgContinuationTaskSpend sets the org's default per-task USD cap for continuation runs; zero
// is the platform default.
func (s *Service) SetOrgContinuationTaskSpend(ctx context.Context, org pgtype.UUID, actor audit.Actor, v money.USD) error {
	if v < 0 || v > money.MaxLimit {
		return &InvalidError{Msg: fmt.Sprintf("the task spend cap must be greater than 0 and at most %s USD", money.MaxLimit)}
	}
	return s.tx(ctx, func(q *store.Queries, b audit.Batch) error {
		before, err := orgsettings.Get(ctx, q, org, orgsettings.Defaults())
		if err != nil {
			return err
		}
		if err := orgsettings.SetContinuationTaskSpend(ctx, q, org, v); err != nil {
			return err
		}
		b.Org(audit.Event{
			Org: org, Actor: actor, Action: "budget.org_continuation_task_spend.set", TargetKind: "org", TargetID: org.String(),
			Before: map[string]any{"taskSpendUSD": before.ContinuationTaskSpend.String()}, After: map[string]any{"taskSpendUSD": v.String()},
		})
		return nil
	})
}

func continuationSummary(o runlimits.ContinuationOverride) map[string]any {
	m := map[string]any{"taskSpendUSD": o.TaskSpend.String()}
	if o.MaxContinuations != nil {
		m["maxContinuations"] = *o.MaxContinuations
	}
	return m
}

func overrideSummary(o runlimits.Override) map[string]any {
	return map[string]any{
		"maxModelRequests": o.MaxModelRequests, "maxSpendUSD": o.MaxSpend.String(), "maxDurationSeconds": int64(o.WallClock / time.Second),
	}
}
