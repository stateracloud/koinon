// SPDX-License-Identifier: Apache-2.0

package installbinding

import (
	"context"
	"fmt"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/stateracloud/koinon/internal/audit"
)

// Removal is one thing the declarative importer would remove: a model provider, an MCP server or a
// connection that a Secret declared and whose Secret is gone (ADR 0110 decision 1).
type Removal struct {
	Kind   string `json:"kind"`
	Org    string `json:"org"`
	Secret string `json:"declaredBy"`
}

// Removal states on restore_accepted_removals.
const (
	RemovalWaiting  = "waiting"
	RemovalAccepted = "accepted"
)

// Pending says whether a release is pending for ownerID against the row (ADR 0110 decision 1): the
// hold left held_since set, or the state is copy and this install has not run its release step.
// An absent row (the first upgrade of an existing install) is never pending. The settings-record
// clause of the ADR is not read here: held_since covers every hold a held process saw.
func Pending(ownerID string, r Row, found bool) bool {
	if !found || ownerID == "" {
		return false
	}
	if r.HeldSince != nil {
		return true
	}
	return StateOf(ownerID, r, found) == StateCopy && r.ReleasedBy != ownerID
}

// SettleResult says what Settle did.
type SettleResult struct {
	// Released is true when this call ran the release step.
	Released bool
	// RevokedSessions is the number of sessions the release step deleted.
	RevokedSessions int
	// Accepted is the number of waiting removals this call turned to accepted.
	Accepted int
	// Waiting are the removals on the list that stay plan-only.
	Waiting []Removal
	// allowed holds the removals that may be applied now.
	allowed map[Removal]bool
	// listed holds every removal on the list.
	listed map[Removal]bool
}

// Allowed reports whether the importer may apply r: it is not on the restore's list, or it is
// accepted.
func (s SettleResult) Allowed(r Removal) bool {
	if s.listed == nil {
		return true
	}
	return !s.listed[r] || s.allowed[r]
}

// Settle is the importer's gate (ADR 0110 decision 1). plan is what the importer would remove now.
//
// While a release is pending, one audited transaction (a row lock on install_binding, the check
// repeated under it, so one replica wins) revokes every session, replaces the list with plan in state
// accepted when accept is true and waiting otherwise, records restore.sessions_revoked and
// restore.release in the install audit chain, and clears held_since (sets released_at, released_by).
//
// Otherwise, with accept true, the waiting rows turn to accepted (restore.removals_accepted). Rows whose
// item is not in plan any more (the Secret came back, or the removal was done) are deleted. A disabled
// Binding settles nothing: every removal is allowed.
func (b *Binding) Settle(ctx context.Context, plan []Removal, accept bool) (SettleResult, error) {
	var res SettleResult
	if !b.Enabled() {
		return res, nil
	}
	actor := audit.System("koinon-api:restore-release")
	inPlan := map[Removal]bool{}
	for _, r := range plan {
		inPlan[r] = true
	}
	_, err := audit.InPgTx(ctx, b.pool, func(tx pgx.Tx, ab audit.Batch) error {
		row, found, err := readRow(ctx, tx, true)
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		state := StateWaitingOrAccepted(accept)
		if Pending(b.mark, row, found) {
			releaseID := uuid.New()
			ct, err := tx.Exec(ctx, `DELETE FROM sessions`)
			if err != nil {
				return err
			}
			res.Released, res.RevokedSessions = true, int(ct.RowsAffected())
			if _, err := tx.Exec(ctx, `DELETE FROM restore_accepted_removals`); err != nil {
				return err
			}
			for _, r := range sortedRemovals(plan) {
				if _, err := tx.Exec(ctx,
					`INSERT INTO restore_accepted_removals (kind, org_slug, secret_name, release_id, state) VALUES ($1, $2, $3, $4, $5)`,
					r.Kind, r.Org, r.Secret, releaseID, state); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(ctx,
				`UPDATE install_binding SET held_since = NULL, released_at = now(), released_by = $1 WHERE singleton`, b.mark); err != nil {
				return err
			}
			ab.Install(audit.InstallEvent{
				Actor: actor, Action: "restore.sessions_revoked", TargetKind: "install", TargetID: "install_binding",
				After: map[string]any{"sessions": res.RevokedSessions},
			})
			ab.Install(audit.InstallEvent{
				Actor: actor, Action: "restore.release", TargetKind: "install", TargetID: "install_binding",
				Before: map[string]any{"state": string(StateOf(b.mark, row, found)), "heldSince": row.HeldSince},
				After:  map[string]any{"releaseId": releaseID.String(), "removals": sortedRemovals(plan), "removalState": state, "acceptImporterDeletions": accept},
			})
		} else {
			// Forget items whose Secret is back or whose removal is done.
			rows, err := tx.Query(ctx, `SELECT kind, org_slug, secret_name FROM restore_accepted_removals`)
			if err != nil {
				return err
			}
			var gone []Removal
			for rows.Next() {
				var r Removal
				if err := rows.Scan(&r.Kind, &r.Org, &r.Secret); err != nil {
					rows.Close()
					return err
				}
				if !inPlan[r] {
					gone = append(gone, r)
				}
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			for _, r := range gone {
				if _, err := tx.Exec(ctx, `DELETE FROM restore_accepted_removals WHERE kind = $1 AND org_slug = $2 AND secret_name = $3`, r.Kind, r.Org, r.Secret); err != nil {
					return err
				}
			}
			if accept {
				rows, err := tx.Query(ctx, `UPDATE restore_accepted_removals SET state = 'accepted' WHERE state = 'waiting' RETURNING kind, org_slug, secret_name`)
				if err != nil {
					return err
				}
				var acc []Removal
				for rows.Next() {
					var r Removal
					if err := rows.Scan(&r.Kind, &r.Org, &r.Secret); err != nil {
						rows.Close()
						return err
					}
					acc = append(acc, r)
				}
				rows.Close()
				if err := rows.Err(); err != nil {
					return err
				}
				res.Accepted = len(acc)
				if len(acc) > 0 {
					ab.Install(audit.InstallEvent{
						Actor: actor, Action: "restore.removals_accepted", TargetKind: "install", TargetID: "install_binding",
						After: map[string]any{"removals": sortedRemovals(acc)},
					})
				}
			}
		}
		rows, err := tx.Query(ctx, `SELECT kind, org_slug, secret_name, state FROM restore_accepted_removals`)
		if err != nil {
			return err
		}
		defer rows.Close()
		res.listed, res.allowed = map[Removal]bool{}, map[Removal]bool{}
		for rows.Next() {
			var r Removal
			var st string
			if err := rows.Scan(&r.Kind, &r.Org, &r.Secret, &st); err != nil {
				return err
			}
			res.listed[r] = true
			if st == RemovalAccepted {
				res.allowed[r] = true
			} else if inPlan[r] {
				res.Waiting = append(res.Waiting, r)
			}
		}
		return rows.Err()
	})
	if err != nil {
		return SettleResult{}, fmt.Errorf("restore release: %w", err)
	}
	res.Waiting = sortedRemovals(res.Waiting)
	return res, nil
}

// StateWaitingOrAccepted is the state a removal gets at the release.
func StateWaitingOrAccepted(accept bool) string {
	if accept {
		return RemovalAccepted
	}
	return RemovalWaiting
}

func sortedRemovals(in []Removal) []Removal {
	out := append([]Removal(nil), in...)
	sort.Slice(out, func(a, b int) bool {
		x, y := out[a], out[b]
		if x.Kind != y.Kind {
			return x.Kind < y.Kind
		}
		if x.Org != y.Org {
			return x.Org < y.Org
		}
		return x.Secret < y.Secret
	})
	return out
}

// ListRemovals returns the restore's removal list with its states, for restore status.
func ListRemovals(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}) (waiting, accepted []Removal, err error) {
	rows, err := q.Query(ctx, `SELECT kind, org_slug, secret_name, state FROM restore_accepted_removals ORDER BY 1, 2, 3`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r Removal
		var st string
		if err := rows.Scan(&r.Kind, &r.Org, &r.Secret, &st); err != nil {
			return nil, nil, err
		}
		if st == RemovalAccepted {
			accepted = append(accepted, r)
		} else {
			waiting = append(waiting, r)
		}
	}
	return waiting, accepted, rows.Err()
}
