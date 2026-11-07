// SPDX-License-Identifier: Apache-2.0

package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/budget"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/runoutcome"
	"github.com/stateracloud/koinon/internal/scheduler"
)

// Stop cancels, with the budget reason, every open run of the scope that the reached limit l
// covers: the run alone for its own spend limit, the agent's runs for its daily limit, every run of the org for the monthly cap or the
// platform ceiling. Queued and parked runs are canceled too: they would start, or resume, on a key
// that has nothing left. It returns how many runs it canceled. db is a transaction or a pool.
//
// Cancel, not park, is the transition (ADR 0050): it is the control plane's
// ([runoutcome.CancelUnowned]), needs no epoch, and an owner's later report (a model request refused with 401 or 429 once the key is out of budget)
// then matches no row, so the run keeps the budget reason.
//
// The run's own limit goes through [scheduler.CheckRunLimits], the cancel of every other writer of
// the run's entries: it carries what the agent last said and the run's usage (ADR 0081, decision
// 7), whichever path reaches the limit first.
func Stop(ctx context.Context, db scheduler.DB, org, agent pgtype.UUID, l budget.Level) (int, error) {
	q := store.New(db)
	var runs []store.Run
	var err error
	switch l.Name {
	case budget.RunSpend:
		r, err := scheduler.CheckRunLimits(ctx, db, org, l.Run)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return 0, nil // finished meanwhile, or gone
		case err != nil:
			return 0, fmt.Errorf("check run limits: %w", err)
		case r.State == store.RunStateCanceled:
			return 1, nil
		}
		return 0, nil
	case budget.AgentDaily:
		runs, err = q.ListOpenRunsOfAgent(ctx, store.ListOpenRunsOfAgentParams{OrgID: org, AgentID: agent})
	default:
		runs, err = q.ListOpenRunsOfOrg(ctx, org)
	}
	if err != nil {
		return 0, fmt.Errorf("list open runs: %w", err)
	}
	c := runoutcome.Cancellation{
		Reason: runoutcome.ReasonSpendLimit, Message: l.StopMessage(), Class: store.RunFailureClassBudget,
		Actor: runoutcome.System, Extra: l.StopDetail(),
	}
	n := 0
	for _, r := range runs {
		_, err := runoutcome.CancelUnowned(ctx, q, org, r.ID, c)
		if errors.Is(err, pgx.ErrNoRows) {
			continue // finished meanwhile
		} else if err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// StopSpent stops the runs of an agent whose budget has nothing left at now, before the caller
// takes its key out of the gateway (old issue 188): the reconciler calls it for every key
// it is about to push a limit of zero for, so a run in flight records the budget stop and not the
// gateway's 401 or 429. The ledger cancels at the entry that reaches a limit already; this covers
// every other way a key runs out: an admin lowering a limit below the spend, an entry recorded
// without a run, a run queued or parked while the agent's budget was spent.
//
// The budget is the charges only: a key whose pushed limit is zero only because of reserves for
// pending provider figures (ADR 0056, amendment 2026-10-28) stops nothing. The limit stopped at
// and named is [budget.Status.Pick]'s. It returns the runs it canceled; an agent with budget left, or no limit, stops nothing.
func StopSpent(ctx context.Context, db scheduler.DB, agent store.Agent, now time.Time) (int, error) {
	n := 0
	err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		q := store.New(tx)
		st, ok, err := agentStatus(ctx, q, agent, now)
		if err != nil || !ok {
			return err
		}
		if net, _, ok := st.Net(); !ok || net > 0 {
			return nil
		}
		pick, found := st.Pick(now)
		if !found {
			return nil
		}
		n, err = Stop(ctx, tx, agent.OrgID, agent.ID, pick)
		return err
	})
	return n, err
}
