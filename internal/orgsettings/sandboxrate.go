// SPDX-License-Identifier: Apache-2.0

package orgsettings

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/db/store"
)

// Kinds of sandbox creation the ledger counts: a new sandbox, and the start of a stopped one. Both
// are the cold starts a burst of runs causes.
const (
	CreationCreate = "create"
	CreationStart  = "start"
)

// ReserveSandboxCreation is the per-org sandbox creation rate limit (issue #27, ADR 0066 amendment
// 2026-11-12). It lives in Postgres with the other run limits so it is audited and tuned in one
// place. Under a per-org advisory lock it counts the creations of the last minute: below limit it
// records one for agent and returns ok; at the limit it returns not ok and how long until a slot
// frees. The caller leaves the run queued or starting and retries on its next pass, so a burst waits
// and no run fails because of it. Reserve before the backend call: a creation that then fails still
// counts, which keeps a failing backend from being hammered.
func ReserveSandboxCreation(ctx context.Context, pool *pgxpool.Pool, org, agent pgtype.UUID, kind string, limit int) (ok bool, retryAfter time.Duration, err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	if err := q.LockOrgSandboxCreations(ctx, org); err != nil {
		return false, 0, err
	}
	n, err := q.CountOrgSandboxCreations(ctx, org)
	if err != nil {
		return false, 0, err
	}
	if int(n) >= limit {
		secs, err := q.SandboxCreationSlotFreesIn(ctx, org)
		if err != nil {
			return false, 0, err
		}
		return false, time.Duration(secs * float64(time.Second)), nil
	}
	if err := q.PruneOrgSandboxCreations(ctx, org); err != nil {
		return false, 0, err
	}
	if err := q.RecordSandboxCreation(ctx, store.RecordSandboxCreationParams{OrgID: org, AgentID: agent, Kind: kind}); err != nil {
		return false, 0, err
	}
	return true, 0, tx.Commit(ctx)
}
