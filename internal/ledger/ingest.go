// SPDX-License-Identifier: Apache-2.0

package ledger

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/scheduler"
)

// Ingester records entries in spend_entries.
type Ingester struct {
	DB  scheduler.DB
	Log *slog.Logger
}

// Result counts what Ingest did with its entries.
type Result struct {
	// Recorded entries are new.
	Recorded int
	// Duplicates were recorded before (a replayed export).
	Duplicates int
	// Unattributed entries name an org or agent that does not exist.
	Unattributed int
}

var errUnattributed = errors.New("unknown org or agent")

// Ingest records each entry in its own transaction: the entry, the run's token
// usage (which may cancel the run at its token cap) and, when the agent's
// recorded spend reaches its limit, the run's cancel. A replayed entry changes
// nothing. It stops at the first database error; entries before it stay
// recorded, and replaying the whole batch is safe.
func (in *Ingester) Ingest(ctx context.Context, entries []Entry) (Result, error) {
	var res Result
	for _, e := range entries {
		recorded, err := in.ingest(ctx, e)
		switch {
		case errors.Is(err, errUnattributed):
			res.Unattributed++
		case err != nil:
			return res, err
		case recorded:
			res.Recorded++
		default:
			res.Duplicates++
		}
	}
	return res, nil
}

func (in *Ingester) log() *slog.Logger {
	if in.Log != nil {
		return in.Log
	}
	return slog.Default()
}

func (in *Ingester) ingest(ctx context.Context, e Entry) (bool, error) {
	var agentID pgtype.UUID
	if err := agentID.Scan(e.Agent.Name); err != nil {
		return false, errUnattributed
	}
	var recorded bool
	err := pgx.BeginFunc(ctx, in.DB, func(tx pgx.Tx) error {
		q := store.New(tx)
		org, err := q.GetOrgBySlug(ctx, e.Agent.Org)
		if errors.Is(err, pgx.ErrNoRows) {
			return errUnattributed
		} else if err != nil {
			return err
		}
		agent, err := q.GetAgent(ctx, store.GetAgentParams{OrgID: org.ID, ID: agentID})
		if errors.Is(err, pgx.ErrNoRows) {
			return errUnattributed
		} else if err != nil {
			return err
		}
		at := pgtype.Timestamptz{Time: e.Time, Valid: true}
		run, err := q.FindRunAt(ctx, store.FindRunAtParams{
			OrgID: org.ID, AgentID: agent.ID, At: at, GraceSeconds: int32(RunGrace.Seconds()),
		})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		cost := e.CostUSD
		if cost == "" {
			cost = "0"
			in.log().Warn("spend ledger: request without a cost", "agent", e.Agent.String(), "model", e.Model, "request", e.RequestID)
		}
		if _, err := q.InsertSpendEntry(ctx, store.InsertSpendEntryParams{
			OrgID: org.ID, AgentID: agent.ID, RunID: run.ID, TicketID: run.TicketID,
			RequestID: e.RequestID, KeyID: e.KeyID, Model: e.Model,
			InputTokens: e.InputTokens, OutputTokens: e.OutputTokens,
			CostUsd: cost, Priced: e.CostUSD != "", OccurredAt: at,
		}); errors.Is(err, pgx.ErrNoRows) {
			return nil // already recorded
		} else if err != nil {
			return fmt.Errorf("insert spend entry: %w", err)
		}
		recorded = true
		if !run.ID.Valid {
			return nil
		}
		// The token cap (plan item A4). RecordTokenUsage nests a savepoint.
		if _, err := scheduler.RecordTokenUsage(ctx, tx, org.ID, run.ID, e.InputTokens+e.OutputTokens); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("record token usage: %w", err)
		}
		return enforceLimit(ctx, q, org.ID, agent.ID, run, e)
	})
	return recorded, err
}

// enforceLimit cancels the run when the agent's spend in the current window
// has reached the limit of the run's agent version. agentgateway blocks the
// key's next request anyway; the cancel tells the ticket why.
func enforceLimit(ctx context.Context, q *store.Queries, org, agent pgtype.UUID, run store.FindRunAtRow, e Entry) error {
	version, err := q.GetAgentVersion(ctx, store.GetAgentVersionParams{OrgID: org, ID: run.AgentVersionID})
	if err != nil {
		return fmt.Errorf("read agent version: %w", err)
	}
	cfg, err := agentconfig.Parse(version.Config)
	if err != nil {
		return err
	}
	limit := cfg.Limits.SpendUSD
	if limit <= 0 {
		return nil
	}
	since := WindowStart(e.Time)
	spent, err := q.GetAgentSpendSince(ctx, store.GetAgentSpendSinceParams{
		OrgID: org, AgentID: agent, Since: pgtype.Timestamptz{Time: since, Valid: true},
	})
	if err != nil {
		return err
	}
	reached, err := Reached(limit, spent)
	if err != nil || !reached {
		return err
	}
	msg := fmt.Sprintf("agent spend limit reached ($%s of $%s since %s)",
		FormatUSD(spent), limit.String(), since.Format("2006-01-02 15:04 MST"))
	r, err := q.CancelRun(ctx, store.CancelRunParams{OrgID: org, ID: run.ID, OutcomeReason: msg})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // already finished
	} else if err != nil {
		return err
	}
	_, err = scheduler.RecordCancel(ctx, q, r, scheduler.ReasonSpendLimit, msg, scheduler.System, map[string]any{
		"spend_limit_usd": limit.String(), "spent_usd": FormatUSD(spent), "window_start": since.Format("2006-01-02T15:04:05Z"),
	})
	return err
}
