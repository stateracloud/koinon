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
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/internal/money"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/providerusage"
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
	// Sources are the providers that can report their own usage of a response later; nil is
	// providerusage.Default. A response of such a provider is recorded provisional and settled by
	// the Settler at the provider's bill; of any other provider the logged usage is final.
	Sources providerusage.Registry
}

// Validate reports a wiring mistake: an Ingester without prices would record every request unpriced.
func (in *Ingester) Validate() error {
	if in.Prices.IsZero() {
		return errors.New("ledger: Prices is required (prices.NewResolver)")
	}
	return nil
}

func (in *Ingester) sources() providerusage.Registry {
	if in.Sources != nil {
		return in.Sources
	}
	return providerusage.Default(nil)
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
	// Rejected entries failed [CheckRecord] and were not recorded.
	Rejected int
}

var errUnattributed = errors.New("unknown org or agent")

// Ingest records each entry in its own transaction: the entry, the run's token
// usage (reported, not enforced) and, when the agent's
// recorded spend reaches its limit, the run's cancel. An entry that fails CheckRecord is counted
// as rejected and skipped. A replayed entry changes
// nothing. It stops at the first database error; entries before it stay
// recorded, and replaying the whole batch is safe.
func (in *Ingester) Ingest(ctx context.Context, entries []Entry) (Result, error) {
	var res Result
	for _, e := range entries {
		// Every driver is held to the same rules, whether or not it checked the record itself.
		if kind, _ := CheckRecord(e); kind != "" {
			res.Rejected++
			in.Metrics.Reject(kind)
			continue
		}
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
				in.Metrics.Ingested.WithLabelValues(principalOf(e)).Inc()
				in.Metrics.IngestLag.Observe(max(in.now().Sub(e.Time), 0).Seconds())
			}
		default:
			res.Duplicates++
		}
	}
	return res, nil
}

func principalOf(e Entry) string {
	if gateway.IsSystemAgent(e.Agent) {
		return PrincipalSystem
	}
	return PrincipalAgent
}

func (in *Ingester) log() *slog.Logger {
	if in.Log != nil {
		return in.Log
	}
	return slog.Default()
}

// Principals of a spend entry (spend_entries.principal), as the fixed label of the ledger's metrics.
const (
	PrincipalAgent  = "agent"
	PrincipalSystem = "system"
)

func (in *Ingester) ingest(ctx context.Context, e Entry) (bool, error) {
	if gateway.IsSystemAgent(e.Agent) {
		return in.ingestSystem(ctx, e)
	}
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
		// The run's row is locked before the entry is written, so concurrent requests of one run are
		// serialised from here to the commit: each counts the entries the others committed (the
		// request limit is exact, ADR 0081), and none holds the key-share lock the entry's foreign
		// key takes on the row while waiting to update it (a deadlock between two such requests).
		if run.ID.Valid {
			if _, err := q.LockRun(ctx, store.LockRunParams{OrgID: org.ID, ID: run.ID}); err != nil {
				return fmt.Errorf("lock the run: %w", err)
			}
		}
		// A streamed Anthropic-format response logs message_start's usage, which looks complete, and
		// agentgateway v1.7.0 has no stream-end signal (ADR 0056, amendment 2026-10-28). Only the
		// attributed run counts: a later run's response is not cut by an earlier run's cancel.
		// Either signal makes it cut: a proxy error logged with the output (an upstream that cut
		// the stream), or the attributed run's cancel (a client hang-up logs no error).
		if e.OutputKnown && e.Error != "" {
			e.Cut = true
		}
		if e.Format == modelformat.Anthropic && e.Stream && e.OutputKnown && run.ID.Valid &&
			run.State == store.RunStateCanceled && run.FinishedAt.Valid && !run.FinishedAt.Time.After(e.Time) {
			e.Cut = true
		}
		pr, err := in.price(ctx, q, org.ID, e)
		if err != nil {
			return err
		}
		// A response whose provider can say what it billed (an OpenRouter generation) is settled
		// later at that bill (a settlement with its response ID; ADR 0056, amendment 2026-11-06),
		// whether it completed or was cut short; a cut Anthropic stream whose provider cannot is held
		// (a settlement without one) until the deadline. Meanwhile a cut response's max_tokens bound
		// is held back from the gateway key. A request the provider refused or failed without usage
		// is final at once (it generated nothing to bill), as is every other entry.
		provisional, settleID := false, pgtype.Text{}
		if pr.model.ProviderID.Valid && billedLater(e, pr) {
			if _, ok := in.sources().For(pr.model.ProviderType, pr.model.ProviderURL, e.ResponseID); ok {
				provisional, settleID = true, pgtype.Text{String: e.ResponseID, Valid: true}
			} else if pr.aborted && e.Cut {
				provisional = true
			}
		}
		if !provisional {
			pr.reserve = 0
		}
		responseID := pgtype.Text{String: e.ResponseID, Valid: e.ResponseID != ""}
		entryID, err := q.InsertSpendEntry(ctx, store.InsertSpendEntryParams{
			OrgID: org.ID, AgentID: agent.ID, Principal: PrincipalAgent, RunID: run.ID, TaskID: run.TaskID,
			RequestID: e.RequestID, KeyID: e.KeyID, Model: e.Model,
			InputTokens: pr.totalIn(), OutputTokens: pr.totalOut(),
			CacheReadTokens: pr.cacheRead, CacheWriteTokens: pr.cacheWrite, CacheWrite1hTokens: pr.cacheWrite1h, ReasoningTokens: pr.reasoning, ReasoningKnown: pr.reasoningKnown,
			CostUsd: pr.cost.Numeric(), GatewayCostUsd: pr.gatewayCost, PriceRef: pr.ref,
			Priced: pr.priced, Estimated: pr.estimated, Estimate: pr.estimate, CostFlagged: pr.flagged,
			RequestBytes: pr.reqBytes, ResponseBytes: pr.respBytes, OccurredAt: at,
			Provisional: provisional, ReserveUsd: pgtype.Text{String: pr.reserve.Numeric(), Valid: pr.reserve > 0}, ResponseID: responseID,
			UpstreamStatus: upstreamStatus(e), ProviderID: pr.model.ProviderID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // already recorded
		} else if err != nil {
			return fmt.Errorf("insert spend entry: %w", err)
		}
		recorded = true
		if provisional {
			deadline := e.Time.Add(SettleWindow)
			next := in.now().Add(settleBackoff(0))
			if !settleID.Valid {
				next = deadline // a hold: nothing to ask, the reserve goes at the deadline
			}
			if err := q.InsertSpendSettlement(ctx, store.InsertSpendSettlementParams{
				OrgID: org.ID, EntryID: entryID, ProviderID: pr.model.ProviderID, ProviderType: pr.model.ProviderType,
				ResponseID:    settleID,
				NextAttemptAt: pgtype.Timestamptz{Time: next, Valid: true},
				Deadline:      pgtype.Timestamptz{Time: deadline, Valid: true},
			}); err != nil {
				return fmt.Errorf("queue settlement: %w", err)
			}
		}
		if run.ID.Valid {
			// Tokens are reported, not enforced (ADR 0081). Estimated tokens count like reported ones.
			if _, err := q.AddRunTokens(ctx, store.AddRunTokensParams{OrgID: org.ID, ID: run.ID, Tokens: pr.totalIn() + pr.totalOut()}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("record run tokens: %w", err)
			}
		}
		// Budgets, alerts and stops count the charge only; the reserve gates the gateway key, so
		// the reconciler is woken to push the key's smaller limit.
		if provisional && pr.reserve > 0 {
			if err := q.NotifyReconciler(ctx, org.ID); err != nil {
				return fmt.Errorf("wake the reconciler: %w", err)
			}
		}
		// The agent's, org's and platform's budgets stop first: the broadest limit reached names the
		// stop. Then the run's own limits (ADR 0081), against the ledger including this entry: its
		// model requests and its spend. CheckRunLimits nests a savepoint.
		return enforce(ctx, tx, in.Notifier, in.now(), enforcement{
			org: org.ID, agent: agent.ID, version: run.AgentVersionID, run: run.ID, at: e.Time, delta: pr.cost,
		})
	})
	return recorded, err
}

// billedLater reports whether a request may carry a provider bill worth settling: anything but a
// request the provider refused or failed without usage, or one charged nothing.
func billedLater(e Entry, pr priced) bool {
	return !pr.waived && !rejectedByUpstream(e) && !failedByUpstream(e)
}

// ingestSystem records a request of an org's system key: the principal is system, there is no
// agent and no run. The spend counts toward the org's monthly cap and the platform ceiling; the
// key's own daily budget bounds it, and the reconciler, woken when a level is crossed, takes the key
// out of the gateway at zero. A spent org-wide limit stops the org's runs, as any entry that reaches
// it does.
func (in *Ingester) ingestSystem(ctx context.Context, e Entry) (bool, error) {
	var recorded bool
	err := pgx.BeginFunc(ctx, in.DB, func(tx pgx.Tx) error {
		q := store.New(tx)
		org, err := q.GetOrgBySlug(ctx, e.Agent.Org)
		if errors.Is(err, pgx.ErrNoRows) {
			return errUnattributed
		} else if err != nil {
			return err
		}
		pr, err := in.price(ctx, q, org.ID, e)
		if err != nil {
			return err
		}
		if _, err := q.InsertSpendEntry(ctx, store.InsertSpendEntryParams{
			OrgID: org.ID, Principal: PrincipalSystem,
			RequestID: e.RequestID, KeyID: e.KeyID, Model: e.Model,
			InputTokens: pr.totalIn(), OutputTokens: pr.totalOut(),
			CacheReadTokens: pr.cacheRead, CacheWriteTokens: pr.cacheWrite, CacheWrite1hTokens: pr.cacheWrite1h, ReasoningTokens: pr.reasoning, ReasoningKnown: pr.reasoningKnown,
			CostUsd: pr.cost.Numeric(), GatewayCostUsd: pr.gatewayCost, PriceRef: pr.ref,
			Priced: pr.priced, Estimated: pr.estimated, Estimate: pr.estimate, CostFlagged: pr.flagged,
			RequestBytes: pr.reqBytes, ResponseBytes: pr.respBytes, OccurredAt: pgtype.Timestamptz{Time: e.Time, Valid: true},
			UpstreamStatus: upstreamStatus(e), ProviderID: pr.model.ProviderID,
		}); errors.Is(err, pgx.ErrNoRows) {
			return nil // already recorded
		} else if err != nil {
			return fmt.Errorf("insert spend entry: %w", err)
		}
		recorded = true
		st, err := budget.ComputeSystem(ctx, q, org.ID, e.Time)
		if err != nil {
			return err
		}
		// People are told about the org's limits, not about the key's own daily budget.
		orgOnly := budget.Status{Org: org.ID}
		for _, l := range st.Levels {
			if l.Name != budget.SystemDaily {
				orgOnly.Levels = append(orgOnly.Levels, l)
			}
		}
		if err := budget.Alert(ctx, q, in.Notifier, orgOnly, pr.cost); err != nil {
			return err
		}
		crossed := false
		for _, r := range st.Current(in.now()) {
			if r.Spent-pr.cost < r.Limit {
				crossed = true
			}
		}
		if crossed {
			if err := q.NotifyReconciler(ctx, org.ID); err != nil {
				return fmt.Errorf("wake the reconciler: %w", err)
			}
		}
		if l, found := orgOnly.Pick(in.now()); found {
			_, err = Stop(ctx, tx, org.ID, pgtype.UUID{}, l)
		}
		return err
	})
	return recorded, err
}

// enforcement is what enforce checks: the agent's, org's and platform's budgets and the run's own
// limits, after a ledger entry of the run was recorded or its cost changed.
type enforcement struct {
	org, agent pgtype.UUID
	// version is the agent version of the run the entry belongs to; unset when the entry has no
	// run (the agent's active version stands in).
	version pgtype.UUID
	// run is the run the entry belongs to; unset for an entry without one.
	run pgtype.UUID
	// at is when the request happened (it picks the windows the entry counts in).
	at time.Time
	// delta is the cost the entry added (a settlement: the change, never below zero).
	delta money.USD
}

// enforce is the one sequence every writer of a ledger entry runs, so they name a stop alike (ADR
// 0081, decision 5): first the agent's daily limit, the org's monthly cap and the platform ceiling
// (budgets), then the run's own limits. The broadest limit reached names the stop: a request that
// reaches both the agent's daily limit and the run's spend limit cancels with the daily limit,
// whether it was ingested or a settlement raised its cost. Ingest and the settler both call it.
func enforce(ctx context.Context, tx pgx.Tx, nf budget.Notifier, now time.Time, x enforcement) error {
	if err := enforceBudgets(ctx, tx, nf, now, x); err != nil {
		return err
	}
	if !x.run.Valid {
		return nil
	}
	// CheckRunLimits nests a savepoint; pgx.ErrNoRows: the run finished already (a budget stop
	// above, or its own end).
	if _, err := scheduler.CheckRunLimits(ctx, tx, x.org, x.run); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("check run limits: %w", err)
	}
	return nil
}

// enforceBudgets checks the agent's daily limit, the org's monthly cap and the platform ceiling with
// the entry just recorded, alerts at 80% and 100%, and cancels at the first limit reached ([Stop]):
// every open run of the agent for its limit, every open run of the org for an org-wide one.
// agentgateway blocks the key's next request anyway; the cancel tells the task which limit, and
// it commits before the reconciler it wakes can take the key out.
func enforceBudgets(ctx context.Context, tx pgx.Tx, nf budget.Notifier, now time.Time, x enforcement) error {
	q := store.New(tx)
	org, agent, delta := x.org, x.agent, x.delta
	var fallback money.USD
	if x.version.Valid {
		version, err := q.GetAgentVersion(ctx, store.GetAgentVersionParams{OrgID: org, ID: x.version})
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
	st, err := budget.Compute(ctx, q, org, agent, fallback, x.at)
	if err != nil {
		return err
	}
	if err := budget.Alert(ctx, q, nf, st, delta); err != nil {
		return err
	}
	// The limit named is budget.Status.Pick's (the broadest reached in its current window: a late
	// entry from last month reaching last month's cap cancels nothing). The reconciler is woken when
	// this entry crossed any current level, so a late entry that crosses the org cap of an agent
	// whose daily limit is long spent still wakes it.
	l, found := st.Pick(now)
	if !found {
		return nil
	}
	crossed := false
	for _, r := range st.Current(now) {
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
	_, err = Stop(ctx, tx, org, agent, l)
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

// upstreamStatus is the status the provider answered when it was an error, else NULL.
func upstreamStatus(e Entry) pgtype.Int2 {
	if e.Upstream && e.Status >= 400 && e.Status <= 599 {
		return pgtype.Int2{Int16: int16(e.Status), Valid: true}
	}
	return pgtype.Int2{}
}
