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
// and no run fails because of it. Reserve before the backend call and Release the reservation if the call
// fails, so an agent whose creation keeps failing cannot starve the org's healthy agents.
func ReserveSandboxCreation(ctx context.Context, pool *pgxpool.Pool, org, agent pgtype.UUID, kind string, limit int) (res Reservation, ok bool, retryAfter time.Duration, err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return res, false, 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	if err := q.LockOrgSandboxCreations(ctx, org); err != nil {
		return res, false, 0, err
	}
	n, err := q.CountOrgSandboxCreations(ctx, org)
	if err != nil {
		return res, false, 0, err
	}
	if int(n) >= limit {
		secs, err := q.SandboxCreationSlotFreesIn(ctx, org)
		if err != nil {
			return res, false, 0, err
		}
		return res, false, time.Duration(secs * float64(time.Second)), nil
	}
	if err := q.PruneOrgSandboxCreations(ctx, org); err != nil {
		return res, false, 0, err
	}
	id, err := q.RecordSandboxCreation(ctx, store.RecordSandboxCreationParams{OrgID: org, AgentID: agent, Kind: kind})
	if err != nil {
		return res, false, 0, err
	}
	return Reservation{org: org, id: id}, true, 0, tx.Commit(ctx)
}

// Reservation is one taken sandbox creation slot.
type Reservation struct{ org, id pgtype.UUID }

// Release gives the slot back, for a backend call that failed.
func (r Reservation) Release(ctx context.Context, pool *pgxpool.Pool) error {
	return store.New(pool).ReleaseSandboxCreation(ctx, store.ReleaseSandboxCreationParams{OrgID: r.org, ID: r.id})
}
