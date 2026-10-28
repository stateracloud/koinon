// SPDX-License-Identifier: Apache-2.0

package ledger

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/budget"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/money"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/scheduler"
)

// Ingester records entries in spend_entries.
type Ingester struct {
	DB  scheduler.DB
	Log *slog.Logger
	// Notifier delivers budget alerts; nil stores in-app notifications. This is the seam where
	// email attaches later.
	Notifier budget.Notifier
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Metrics counts what Ingest and Handler do; nil counts nothing.
	Metrics *Metrics
	// Prices finds the prices of requests; required (prices.NewResolver).
	Prices prices.Resolver
}

func (in *Ingester) now() time.Time {
	if in.Now != nil {
		return in.Now()
	}
	return time.Now()
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
			if in.Metrics != nil {
				in.Metrics.Rejected.WithLabelValues(rejectUnattributed).Inc()
			}
		case err != nil:
			return res, err
		case recorded:
			res.Recorded++
			if in.Metrics != nil {
				in.Metrics.Ingested.Inc()
				in.Metrics.IngestLag.Observe(max(in.now().Sub(e.Time), 0).Seconds())
			}
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
		pr, err := in.price(ctx, q, org.ID, e)
		if err != nil {
			return err
		}
		if _, err := q.InsertSpendEntry(ctx, store.InsertSpendEntryParams{
			OrgID: org.ID, AgentID: agent.ID, RunID: run.ID, TaskID: run.TaskID,
			RequestID: e.RequestID, KeyID: e.KeyID, Model: e.Model,
			InputTokens: pr.inTokens, OutputTokens: pr.outTokens,
			CacheReadTokens: pr.cacheRead, CacheWriteTokens: pr.cacheWrite, ReasoningTokens: pr.reasoning,
			CostUsd: pr.cost.Numeric(), GatewayCostUsd: pr.gatewayCost, PriceRef: pr.ref,
			Priced: pr.priced, Estimated: pr.estimated, Estimate: pr.estimate, CostFlagged: pr.flagged,
			RequestBytes: pr.reqBytes, ResponseBytes: pr.respBytes, OccurredAt: at,
		}); errors.Is(err, pgx.ErrNoRows) {
			return nil // already recorded
		} else if err != nil {
			return fmt.Errorf("insert spend entry: %w", err)
		}
		recorded = true
		if run.ID.Valid {
			// The token cap (plan item A4). RecordTokenUsage nests a savepoint. Estimated
			// tokens count like reported ones.
			if _, err := scheduler.RecordTokenUsage(ctx, tx, org.ID, run.ID, pr.inTokens+pr.outTokens); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("record token usage: %w", err)
			}
		}
		return in.enforce(ctx, q, org.ID, agent.ID, run, e, pr.cost)
	})
	return recorded, err
}

// enforce checks the agent's daily limit, the org's monthly cap and the platform ceiling with
// the entry just recorded, alerts at 80% and 100%, and cancels at the first limit reached ([Stop]):
// every open run of the agent for its limit, every open run of the org for an org-wide one.
// agentgateway blocks the key's next request anyway; the cancel tells the task which limit, and
// it commits before the reconciler it wakes can take the key out.
func (in *Ingester) enforce(ctx context.Context, q *store.Queries, org, agent pgtype.UUID, run store.FindRunAtRow, e Entry, delta money.USD) error {
	var fallback money.USD
	if run.AgentVersionID.Valid {
		version, err := q.GetAgentVersion(ctx, store.GetAgentVersionParams{OrgID: org, ID: run.AgentVersionID})
		if err != nil {
			return fmt.Errorf("read agent version: %w", err)
		}
		cfg, err := agentconfig.Parse(version.Config)
		if err != nil {
			return err
		}
		fallback = money.FromCents(cfg.Limits.SpendUSD.Cents())
	} else if f, err := templateLimit(ctx, q, org, agent); err != nil {
		return err
	} else {
		fallback = f
	}
	st, err := budget.Compute(ctx, q, org, agent, fallback, e.Time)
	if err != nil {
		return err
	}
	if err := budget.Alert(ctx, q, in.Notifier, st, delta); err != nil {
		return err
	}
	// The limit named is budget.Status.Pick's (the broadest reached in its current window: a late
	// entry from last month reaching last month's cap cancels nothing). The reconciler is woken when
	// this entry crossed any current level, so a late entry that crosses the org cap of an agent
	// whose daily limit is long spent still wakes it.
	l, found := st.Pick(in.now())
	if !found {
		return nil
	}
	crossed := false
	for _, r := range st.Current(in.now()) {
		if r.Spent-delta < r.Limit {
			crossed = true
		}
	}
	// The entry that reaches a limit wakes the reconciler, which recomputes every key's limit net of
	// the spend and pushes it. Standalone agentgateway then blocks the key itself; in Kubernetes mode
	// there is no gateway budget, and the reconciler's Apply is what revokes the key (plan item G2).
	// Later entries of a spent window do not wake it again.
	if crossed {
		if err := q.NotifyReconciler(ctx, org); err != nil {
			return fmt.Errorf("wake the reconciler: %w", err)
		}
	}
	_, err = Stop(ctx, q, org, agent, l)
	return err
}

// templateLimit is the spendUSD of the agent's active version, for an agent without a run.
func templateLimit(ctx context.Context, q *store.Queries, org, agent pgtype.UUID) (money.USD, error) {
	a, err := q.GetAgent(ctx, store.GetAgentParams{OrgID: org, ID: agent})
	if err != nil || !a.ActiveVersionID.Valid {
		return 0, err
	}
	v, err := q.GetAgentVersion(ctx, store.GetAgentVersionParams{OrgID: org, ID: a.ActiveVersionID})
	if err != nil {
		return 0, err
	}
	cfg, err := agentconfig.Parse(v.Config)
	if err != nil {
		return 0, err
	}
	return money.FromCents(cfg.Limits.SpendUSD.Cents()), nil
}
