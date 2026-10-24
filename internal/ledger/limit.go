// SPDX-License-Identifier: Apache-2.0

package ledger

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/budget"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/money"
)

// AgentLimit returns the budget to push to agentgateway for agent at now: the smallest of the
// agent's remaining daily limit, its org's remaining monthly cap and the remaining platform
// ceiling (package budget), net of the spend recorded in each window.
//
// agentgateway charges a budget after each response and keeps the running total per key and
// budget name. Pushing the full limit after the gateway lost that total (proxy pod replaced,
// SQLite reset, key rotated: ADR 0049, spike S2) would hand out the whole limit again, so the
// amount is the remainder, never below zero. A remainder must not be charged against a total
// that already holds the same spend, so the budget name carries a generation derived from the
// day and a digest of the recorded spends: every new net amount starts a fresh counter in the
// gateway, which then counts only spend the ledger has not seen yet.
//
// Spend the gateway charged but the ledger has not recorded when the amount is computed (the
// OTLP export lag, about a second) is in neither and can be overspent once more; the bound of
// ADR 0041 grows by that lag. The window is 24 hours even when the monthly cap binds: the
// amount is recomputed every pass, and a new UTC day starts a new counter.
//
// It returns nil for an agent without an active version and no limit at all.
func AgentLimit(ctx context.Context, q *store.Queries, agent store.Agent, now time.Time) (*gateway.Limit, error) {
	if !agent.ActiveVersionID.Valid {
		return nil, nil
	}
	v, err := q.GetAgentVersion(ctx, store.GetAgentVersionParams{OrgID: agent.OrgID, ID: agent.ActiveVersionID})
	if err != nil {
		return nil, fmt.Errorf("read agent version: %w", err)
	}
	cfg, err := agentconfig.Parse(v.Config)
	if err != nil {
		return nil, err
	}
	st, err := budget.Compute(ctx, q, agent.OrgID, agent.ID, money.FromCents(cfg.Limits.SpendUSD.Cents()), now)
	if err != nil {
		return nil, fmt.Errorf("read budgets: %w", err)
	}
	net, _, ok := st.Net()
	if !ok {
		return nil, nil
	}
	l := gateway.USDLimit(net, budget.DayWindow, st.Generation(now))
	return &l, nil
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
