// SPDX-License-Identifier: Apache-2.0

package ledger

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/budget"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/money"
)

// AgentLimit returns the budget to push to agentgateway for agent at now: the smallest of the
// agent's remaining daily limit, its org's remaining monthly cap, the remaining platform ceiling
// and, while the agent has a run, the run's remaining spend limit (package budget; ADR 0041 decision
// 5, ADR 0081), net of the spend recorded in each window. The run level is why the gateway can stop
// a run between ledger passes: overshoot is bounded by one in-flight response per concurrent call (ADR 0097), as for the others.
//
// agentgateway charges a budget after each response and keeps the running total per key and
// budget name. Pushing the full limit after the gateway lost that total (proxy pod replaced,
// SQLite reset, key rotated: ADR 0049, spike S2) would hand out the whole limit again, so the
// amount is the remainder, never below zero. A remainder must not be charged against a total
// that already holds the same spend, so the budget name carries a generation derived from the
// day and a digest of the recorded spends: every new net amount starts a fresh counter in the
// gateway, which then counts only spend the ledger has not seen yet. The window stays 24 hours so
// that agentgateway can delete the expired counters at startup (a windowless counter would stay
// forever); ADR 0041 accepts the lag after midnight, bounded by one reconcile pass.
//
// Spend the gateway charged but the ledger has not recorded when the amount is computed (the
// OTLP export lag, about a second) is in neither and can be overspent once more; the bound of
// ADR 0041 grows by that lag. The amount is recomputed every pass, and a new UTC day starts a
// new counter, also when the monthly cap binds.
//
// Every key of an org is pushed the same org remaining, so while ingest is stalled N agents can
// spend N times it before the gateway stops them; the ledger cancels the org's runs once it records
// the spend. Old counters are not deleted (agentgateway has no API for it); see ADR 0056.
//
// It returns nil for an agent without an active version and no limit at all.
func AgentLimit(ctx context.Context, q *store.Queries, agent store.Agent, now time.Time) (*gateway.Limit, error) {
	st, ok, err := agentStatus(ctx, q, agent, now)
	if err != nil || !ok {
		return nil, err
	}
	if st, err = withReserves(ctx, q, st); err != nil {
		return nil, err
	}
	net, _, ok := st.Net()
	if !ok {
		return nil, nil
	}
	l := gateway.USDLimit(net, budget.DayWindow, st.Generation(now))
	return &l, nil
}

// withReserves adds to each level what provisional entries in its window hold back (ADR 0056,
// amendment 2026-10-28): the gateway key must not hand out the max_tokens bound of a response whose
// provider figure is pending. Only the pushed limit uses it; budgets, alerts and stops count the
// charges alone. The counter generation then digests the reserves too, so a reserve that is
// released starts a fresh gateway counter.
//
// The run level holds nothing back (ADR 0081, decision 3): a reserve is priced at the request's
// max_tokens and can exceed the whole run limit, which would push a limit of zero for a run the
// ledger has not stopped, and the gateway would refuse the run's wrap-up turn. The agent, org and
// platform levels still hold the reserve, so the key stays bounded by them; the run level's
// overshoot is one in-flight response per concurrent call (ADR 0097), as for every level, and the settlement of the cut response
// checks the run's limits when its cost is known.
func withReserves(ctx context.Context, q *store.Queries, st budget.Status) (budget.Status, error) {
	out := st
	out.Levels = append([]budget.Level(nil), st.Levels...)
	for i, l := range out.Levels {
		since := pgtype.Timestamptz{Time: l.WindowStart, Valid: true}
		var s string
		var err error
		if l.Name == budget.SystemDaily {
			continue // the system key holds nothing back: its provisional entries (awaiting OpenRouter's bill) hold no reserve
		}
		if l.Name == budget.RunSpend {
			continue // the run level holds nothing back: see above
		}
		if l.Name == budget.AgentDaily {
			s, err = q.GetAgentReserveSince(ctx, store.GetAgentReserveSinceParams{OrgID: st.Org, AgentID: st.Agent, Since: since})
		} else {
			s, err = q.GetOrgReserveSince(ctx, store.GetOrgReserveSinceParams{OrgID: st.Org, Since: since})
		}
		if err != nil {
			return st, fmt.Errorf("read reserves: %w", err)
		}
		r, err := money.ParseNumeric(s)
		if err != nil {
			return st, err
		}
		if out.Levels[i].Spent, err = l.Spent.Add(r); err != nil {
			out.Levels[i].Spent = money.MaxLimit // saturate, as sums read from the database do
		}
	}
	return out, nil
}

// SystemLimit returns the budget to push for an org's system key at now: the smaller of its remaining
// daily budget and the org's remaining monthly cap and platform ceiling, net of recorded spend and
// generation-named like AgentLimit's, so the two enforce the same way in both gateway modes.
func SystemLimit(ctx context.Context, q *store.Queries, org pgtype.UUID, now time.Time) (*gateway.Limit, error) {
	st, err := budget.ComputeSystem(ctx, q, org, now)
	if err != nil {
		return nil, fmt.Errorf("read budgets: %w", err)
	}
	if st, err = withReserves(ctx, q, st); err != nil {
		return nil, err
	}
	net, _, ok := st.Net()
	if !ok {
		return nil, nil
	}
	l := gateway.USDLimit(net, budget.DayWindow, st.Generation(now))
	return &l, nil
}

// agentStatus is the agent's limits and spend at now (package budget). ok is false for an agent
// without an active version.
func agentStatus(ctx context.Context, q *store.Queries, agent store.Agent, now time.Time) (budget.Status, bool, error) {
	if !agent.ActiveVersionID.Valid {
		return budget.Status{}, false, nil
	}
	v, err := q.GetAgentVersion(ctx, store.GetAgentVersionParams{OrgID: agent.OrgID, ID: agent.ActiveVersionID})
	if err != nil {
		return budget.Status{}, false, fmt.Errorf("read agent version: %w", err)
	}
	cfg, err := agentconfig.Parse(v.Config)
	if err != nil {
		return budget.Status{}, false, err
	}
	st, err := budget.Compute(ctx, q, agent.OrgID, agent.ID, money.FromCents(cfg.Limits.SpendUSD.Cents()), now)
	if err != nil {
		return budget.Status{}, false, fmt.Errorf("read budgets: %w", err)
	}
	// The run the key is serving adds its own spend limit (ADR 0081).
	run, err := q.GetOwnedRunOfAgent(ctx, store.GetOwnedRunOfAgentParams{OrgID: agent.OrgID, AgentID: agent.ID})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return st, true, nil
	case err != nil:
		return budget.Status{}, false, fmt.Errorf("read the agent's run: %w", err)
	}
	if st, err = st.WithRun(ctx, q, run); err != nil {
		return budget.Status{}, false, fmt.Errorf("read the run's spend: %w", err)
	}
	return st, true, nil
}

// FormatUSD trims a Postgres numeric's trailing zeros, keeping at least two
// fraction digits: "0.0020000000" is "0.002", "3.0000000000" is "3.00".
func FormatUSD(s string) string {
	whole, frac, ok := strings.Cut(s, ".")
	if !ok {
		return whole + ".00"
	}
	frac = strings.TrimRight(frac, "0")
	for len(frac) < 2 {
		frac += "0"
	}
	return whole + "." + frac
}
