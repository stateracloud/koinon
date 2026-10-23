// SPDX-License-Identifier: Apache-2.0

// Package scheduler queues and cancels runs (plan item A4, ADRs 0030, 0049).
//
//   - One active run per agent; further runs queue in FIFO order. Queueing
//     only inserts a queued run: agentd claims runs with store.ClaimNextRun,
//     which enforces the order and the one-active-run rule.
//   - A run gets the caps of the agent version it executes (from the
//     template): a wall-clock cap enforced by Sweeper and a token cap enforced
//     by RecordTokenUsage, which whoever records usage calls.
//   - Every cancel writes a run event and a ticket event saying why.
//
// Functions taking a *store.Queries run inside the caller's transaction.
package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/db/store"
)

// Event kinds written by the scheduler, on both the run and the ticket.
const (
	KindRunQueued   = "run.queued"
	KindRunCanceled = "run.canceled"
)

// CancelReason is the machine-readable cause of a cancel, in the event
// payload's "reason".
type CancelReason string

// Cancel reasons.
const (
	ReasonUser          CancelReason = "user"
	ReasonTicketDone    CancelReason = "ticket_done"
	ReasonAgentArchived CancelReason = "agent_archived"
	ReasonWallClockCap  CancelReason = "wall_clock_cap"
	ReasonTokenCap      CancelReason = "token_cap"
	// ReasonSpendLimit: the agent reached its spend limit (plan item L1).
	ReasonSpendLimit CancelReason = "spend_limit"
	// ReasonApprovalExpired: the run was parked on an approval nobody decided in time
	// (ADR 0049: 7 days).
	ReasonApprovalExpired CancelReason = "approval_expired"
)

// ErrAgentNotActive means the ticket's agent cannot take runs.
var ErrAgentNotActive = errors.New("agent is not active")

// Actor is who caused a scheduler action, for the ticket event.
type Actor struct {
	// UserID is set for a user; otherwise the actor is the system.
	UserID pgtype.UUID
}

// System is the actor for automatic actions (caps, sweeps).
var System = Actor{}

// Enqueue queues a run of the ticket's agent for the ticket, with the caps of
// the agent's active version. It queues nothing, and returns false, when the
// ticket has no agent or already has a queued run (that run will see every
// message appended before it starts). The caller must hold the ticket's row
// lock (for example by having appended a ticket event in the same
// transaction), so concurrent calls for one ticket queue at most one run.
func Enqueue(ctx context.Context, q *store.Queries, ticket store.Ticket) (store.Run, bool, error) {
	if !ticket.AgentID.Valid {
		return store.Run{}, false, nil
	}
	org := ticket.OrgID
	queued, err := q.HasQueuedRunForTicket(ctx, store.HasQueuedRunForTicketParams{OrgID: org, TicketID: ticket.ID})
	if err != nil {
		return store.Run{}, false, err
	}
	if queued {
		return store.Run{}, false, nil
	}
	agent, err := q.GetAgent(ctx, store.GetAgentParams{OrgID: org, ID: ticket.AgentID})
	if err != nil {
		return store.Run{}, false, fmt.Errorf("load agent: %w", err)
	}
	if agent.Status != store.AgentStatusActive || !agent.ActiveVersionID.Valid {
		return store.Run{}, false, ErrAgentNotActive
	}
	version, err := q.GetAgentVersion(ctx, store.GetAgentVersionParams{OrgID: org, ID: agent.ActiveVersionID})
	if err != nil {
		return store.Run{}, false, fmt.Errorf("load agent version: %w", err)
	}
	cfg, err := agentconfig.Parse(version.Config)
	if err != nil {
		return store.Run{}, false, err
	}
	wallClock, maxTokens := cfg.RunCaps()
	run, err := q.CreateRun(ctx, store.CreateRunParams{
		OrgID: org, AgentID: agent.ID, TicketID: ticket.ID,
		MaxWallClockSeconds: int32(wallClock / time.Second), //nolint:gosec // bounded by templates.MaxRunDuration
		MaxTokens:           maxTokens,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Run{}, false, ErrAgentNotActive
	}
	if err != nil {
		return store.Run{}, false, fmt.Errorf("create run: %w", err)
	}
	payload := mustJSON(map[string]any{
		"max_wall_clock_seconds": run.MaxWallClockSeconds,
		"max_tokens":             run.MaxTokens,
	})
	ev, err := q.AppendRunControlEvent(ctx, store.AppendRunControlEventParams{
		OrgID: org, RunID: run.ID, Kind: KindRunQueued, Payload: payload,
	})
	if err != nil {
		return store.Run{}, false, fmt.Errorf("append run event: %w", err)
	}
	run.LastEventSeq = ev.Seq
	if _, err := q.AppendTicketEvent(ctx, store.AppendTicketEventParams{
		OrgID: org, TicketID: ticket.ID, Kind: KindRunQueued, ActorKind: store.TicketActorKindSystem,
		RunID: run.ID, Payload: payload,
	}); err != nil {
		return store.Run{}, false, fmt.Errorf("append ticket event: %w", err)
	}
	return run, true, nil
}

// Cancel cancels a run that has not finished. pgx.ErrNoRows: it has finished
// or does not exist in the org.
func Cancel(ctx context.Context, q *store.Queries, org, runID pgtype.UUID, reason CancelReason, message string, actor Actor) (store.Run, error) {
	run, err := q.CancelRun(ctx, store.CancelRunParams{OrgID: org, ID: runID, OutcomeReason: message})
	if err != nil {
		return store.Run{}, err
	}
	return RecordCancel(ctx, q, run, reason, message, actor, nil)
}

// CancelQueuedForTicket cancels the ticket's queued runs, when it moves to
// Done.
func CancelQueuedForTicket(ctx context.Context, q *store.Queries, ticket store.Ticket, actor Actor) ([]store.Run, error) {
	const msg = "ticket moved to Done"
	runs, err := q.CancelQueuedRunsForTicket(ctx, store.CancelQueuedRunsForTicketParams{
		OrgID: ticket.OrgID, TicketID: ticket.ID, OutcomeReason: msg,
	})
	if err != nil {
		return nil, err
	}
	return recordAll(ctx, q, runs, ReasonTicketDone, msg, actor)
}

// CancelForAgent cancels every unfinished run of an agent, when it is
// archived.
func CancelForAgent(ctx context.Context, q *store.Queries, org, agentID pgtype.UUID, actor Actor) ([]store.Run, error) {
	const msg = "agent archived"
	runs, err := q.CancelOpenRunsForAgent(ctx, store.CancelOpenRunsForAgentParams{
		OrgID: org, AgentID: agentID, OutcomeReason: msg,
	})
	if err != nil {
		return nil, err
	}
	return recordAll(ctx, q, runs, ReasonAgentArchived, msg, actor)
}

func recordAll(ctx context.Context, q *store.Queries, runs []store.Run, reason CancelReason, msg string, actor Actor) ([]store.Run, error) {
	out := make([]store.Run, 0, len(runs))
	for _, r := range runs {
		r, err := RecordCancel(ctx, q, r, reason, msg, actor, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// RecordCancel writes the run and ticket events for a run that was just
// canceled. extra is merged into the payload.
func RecordCancel(ctx context.Context, q *store.Queries, run store.Run, reason CancelReason, message string, actor Actor, extra map[string]any) (store.Run, error) {
	p := map[string]any{"reason": string(reason), "message": message}
	for k, v := range extra {
		p[k] = v
	}
	payload := mustJSON(p)
	ev, err := q.AppendRunControlEvent(ctx, store.AppendRunControlEventParams{
		OrgID: run.OrgID, RunID: run.ID, Kind: KindRunCanceled, Payload: payload,
	})
	if err != nil {
		return store.Run{}, fmt.Errorf("append run event: %w", err)
	}
	run.LastEventSeq = ev.Seq
	te := store.AppendTicketEventParams{
		OrgID: run.OrgID, TicketID: run.TicketID, Kind: KindRunCanceled,
		ActorKind: store.TicketActorKindSystem, RunID: run.ID, Payload: payload,
	}
	if actor.UserID.Valid {
		te.ActorKind, te.ActorUserID = store.TicketActorKindUser, actor.UserID
	}
	if _, err := q.AppendTicketEvent(ctx, te); err != nil {
		return store.Run{}, fmt.Errorf("append ticket event: %w", err)
	}
	return run, nil
}

// DB is what the scheduler needs from a pool to open transactions.
type DB interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	store.DBTX
}

// RecordTokenUsage is the token-cap hook: whoever learns a run's token usage
// (agentd from ACP usage updates, or the spend ledger from agentgateway's
// access log) calls it with the tokens used since the last report. When the
// run's total reaches its cap, the run is canceled with a ticket event. It
// returns the updated run; pgx.ErrNoRows means the run has finished or does
// not exist in the org, and the usage is dropped.
func RecordTokenUsage(ctx context.Context, db DB, org, runID pgtype.UUID, tokens int64) (store.Run, error) {
	if tokens < 0 {
		return store.Run{}, fmt.Errorf("negative token usage %d", tokens)
	}
	var out store.Run
	err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		q := store.New(tx)
		run, err := q.AddRunTokens(ctx, store.AddRunTokensParams{OrgID: org, ID: runID, Tokens: tokens})
		if err != nil {
			return err
		}
		out = run
		if run.TokensUsed < run.MaxTokens {
			return nil
		}
		msg := fmt.Sprintf("token cap exceeded (%d of %d tokens)", run.TokensUsed, run.MaxTokens)
		canceled, err := q.CancelRun(ctx, store.CancelRunParams{OrgID: org, ID: runID, OutcomeReason: msg})
		if err != nil {
			return err
		}
		out, err = RecordCancel(ctx, q, canceled, ReasonTokenCap, msg, System, map[string]any{
			"max_tokens": run.MaxTokens, "tokens_used": run.TokensUsed,
		})
		return err
	})
	return out, err
}

// SweepWallClock cancels every owned run past its wall-clock cap, org by org,
// and returns how many it canceled. Several API replicas may sweep at once: a
// run canceled by one no longer matches the others.
func SweepWallClock(ctx context.Context, db DB) (int, error) {
	orgs, err := store.New(db).ListOrgIDs(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, org := range orgs {
		err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
			q := store.New(tx)
			runs, err := q.CancelRunsOverWallClock(ctx, store.CancelRunsOverWallClockParams{
				OrgID: org, OutcomeReason: "wall-clock cap exceeded",
			})
			if err != nil {
				return err
			}
			for _, r := range runs {
				capd := time.Duration(r.MaxWallClockSeconds) * time.Second
				msg := fmt.Sprintf("wall-clock cap exceeded (%s)", capd)
				if _, err := RecordCancel(ctx, q, r, ReasonWallClockCap, msg, System, map[string]any{
					"max_wall_clock_seconds": r.MaxWallClockSeconds,
				}); err != nil {
					return err
				}
			}
			n += len(runs)
			return nil
		})
		if err != nil {
			return n, fmt.Errorf("sweep org %s: %w", org.String(), err)
		}
	}
	return n, nil
}

// Sweeper runs SweepWallClock every interval until ctx ends.
func Sweeper(ctx context.Context, db DB, interval time.Duration, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		n, err := SweepWallClock(ctx, db)
		if err != nil && ctx.Err() == nil {
			log.Error("wall-clock sweep failed", "err", err)
		} else if n > 0 {
			log.Info("canceled runs over their wall-clock cap", "runs", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // only maps of plain values are passed
	}
	return b
}
