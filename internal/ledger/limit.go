// SPDX-License-Identifier: Apache-2.0

package ledger

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/templates"
)

// NetLimit is the budget pushed to agentgateway for an agent whose limit is
// limit and whose recorded spend in the window containing now is spent (a
// decimal string).
//
// agentgateway charges a budget after each response and keeps the running
// total per key and budget name. Pushing the full limit after the gateway
// lost that total (proxy pod replaced, SQLite reset, key rotated: ADR 0049,
// spike S2) would hand out the whole limit again, so the amount is the
// remainder, limit - spent, never below zero. A remainder must not be charged
// against a total that already holds the same spend, so the budget name
// carries a generation derived from the window and the recorded spend: every
// new net amount starts a fresh counter in the gateway, which then counts only
// spend the ledger has not seen yet.
//
// Spend the gateway charged but the ledger has not recorded when the amount is
// computed (the OTLP export lag, about a second) is in neither and can be
// overspent once more; the bound of ADR 0041 grows by that lag.
func NetLimit(limit templates.USD, spent string, now time.Time) (gateway.Limit, error) {
	s, err := parseUSD(spent)
	if err != nil {
		return gateway.Limit{}, err
	}
	remaining := new(big.Rat).Sub(big.NewRat(limit.Cents(), 100), s)
	if remaining.Sign() < 0 {
		remaining.SetInt64(0)
	}
	amount, _ := remaining.Float64()
	// Floor to nano-USD: the generation only needs to change when the spend does.
	nano := new(big.Int).Quo(new(big.Int).Mul(s.Num(), big.NewInt(1_000_000_000)), s.Denom())
	return gateway.Limit{
		Unit:       gateway.USD,
		Amount:     amount,
		Window:     Window,
		Generation: fmt.Sprintf("%d-%s", WindowStart(now).Unix(), nano.String()),
	}, nil
}

// Reached reports whether spent (a decimal string) has reached limit.
func Reached(limit templates.USD, spent string) (bool, error) {
	s, err := parseUSD(spent)
	if err != nil {
		return false, err
	}
	return s.Cmp(big.NewRat(limit.Cents(), 100)) >= 0, nil
}

// AgentLimit returns the budget to push for agent at now: its active
// version's spend limit net of the spend recorded in the current window. It
// returns nil for an agent without an active version or limit.
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
	if cfg.Limits.SpendUSD <= 0 {
		return nil, nil
	}
	spent, err := q.GetAgentSpendSince(ctx, store.GetAgentSpendSinceParams{
		OrgID: agent.OrgID, AgentID: agent.ID, Since: pgtype.Timestamptz{Time: WindowStart(now), Valid: true},
	})
	if err != nil {
		return nil, fmt.Errorf("read agent spend: %w", err)
	}
	l, err := NetLimit(cfg.Limits.SpendUSD, spent, now)
	if err != nil {
		return nil, err
	}
	return &l, nil
}

func parseUSD(s string) (*big.Rat, error) {
	r, ok := new(big.Rat).SetString(s)
	if !ok || !decimalRE.MatchString(s) {
		return nil, fmt.Errorf("ledger: invalid USD amount %q", s)
	}
	return r, nil
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
