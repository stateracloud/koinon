// SPDX-License-Identifier: Apache-2.0

package installbinding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"

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

// ErrReleasePending is returned for a sign-in while [Binding.SessionsHeld] is true.
var ErrReleasePending = errors.New("sign-in is paused until the restore release step has run")

// ReleasePending reads the row and says whether a release is pending for this process. A disabled
// Binding is never pending.
func (b *Binding) ReleasePending(ctx context.Context) (bool, error) {
	if !b.Enabled() {
		return false, nil
	}
	r, found, err := readRow(ctx, b.pool, false)
	if err != nil {
		return false, fmt.Errorf("read install_binding: %w", err)
	}
	return Pending(b.mark, r, found), nil
}

// HoldSessions sets the session gate of an unheld start (ADR 0110 decision 1: an unheld koinon-api
// never serves the sessions of the dump). While it is set, every session is refused and no sign-in
// makes a new one, so the gate fails closed when the release step cannot run yet (a Kubernetes List
// or a database error). The release step clears it when it succeeds, or when the row shows that no
// release is pending (another replica ran it).
func (b *Binding) HoldSessions(on bool) {
	if b != nil {
		b.sessionsHeld.Store(on)
	}
}

// SessionsHeld says whether the session gate is set. A nil Binding is never held.
func (b *Binding) SessionsHeld() bool {
	return b != nil && b.sessionsHeld.Load()
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
// item is not in plan any more (the Secret came back, or the removal was done) are deleted.
// This is correct only while plan is complete: every caller derives it from the importer's own absent-item
// walk (declarative PlanRemovals), so a partial plan would silently drop the gate for the missing rows. A disabled
// Binding settles nothing: every removal is allowed.
func (b *Binding) Settle(ctx context.Context, plan []Removal, accept bool) (SettleResult, error) {
	return b.SettleWith(ctx, func(context.Context) ([]Removal, error) { return plan, nil }, accept)
}

// SettleWith is Settle with the plan computed inside the transaction, after the row lock on
// install_binding is taken. A plan computed before the lock could be stale when it is applied: another
// replica may have released, or an item may have appeared, between the plan and the settle, and a held
// copy would then act outward on an item the list does not cover. Under the lock no release can run
// between the plan and the settle. plan is called once, with the outer context; a nil planFn means no plan is
// available: a pending release is recorded with an empty list and a later pass changes nothing.
func (b *Binding) SettleWith(ctx context.Context, planFn func(context.Context) ([]Removal, error), accept bool) (SettleResult, error) {
	var res SettleResult
	if !b.Enabled() {
		return res, nil
	}
	actor := audit.System("koinon-api:restore-release")
	_, err := audit.InPgTx(ctx, b.pool, func(tx pgx.Tx, ab audit.Batch) error {
		res = SettleResult{}
		row, found, err := readRow(ctx, tx, true)
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		// A nil planFn means no plan is available (no Kubernetes access): the list is then neither
		// pruned nor accepted, or every row would look gone and the next importer would remove it.
		noPlan := planFn == nil
		var plan []Removal
		if !noPlan {
			if plan, err = planFn(ctx); err != nil {
				return err
			}
		}
		inPlan := map[Removal]bool{}
		for _, r := range plan {
			inPlan[r] = true
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
				ins, err := tx.Exec(ctx,
					`INSERT INTO restore_accepted_removals (org_id, kind, secret_name, release_id, state)
					 SELECT id, $1, $3, $4, $5 FROM orgs WHERE slug = $2`,
					r.Kind, r.Org, r.Secret, releaseID, state)
				if err != nil {
					return err
				}
				if ins.RowsAffected() != 1 {
					// An org that is gone would leave the item off the list, and the importer would then remove it.
					return fmt.Errorf("record the removal of %s %s: org %q not found", r.Kind, r.Secret, r.Org)
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
				After:  map[string]any{"releaseId": releaseID.String(), "planHash": PlanHash(plan), "removals": sortedRemovals(plan), "removalState": state, "acceptImporterDeletions": accept},
			})
		} else if !noPlan {
			// Forget items whose Secret is back or whose removal is done.
			rows, err := tx.Query(ctx, `SELECT r.kind, o.slug, r.secret_name FROM restore_accepted_removals r JOIN orgs o ON o.id = r.org_id`)
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
				if _, err := tx.Exec(ctx, `DELETE FROM restore_accepted_removals r USING orgs o WHERE o.id = r.org_id AND r.kind = $1 AND o.slug = $2 AND r.secret_name = $3`, r.Kind, r.Org, r.Secret); err != nil {
					return err
				}
			}
			if accept {
				rows, err := tx.Query(ctx, `UPDATE restore_accepted_removals r SET state = 'accepted' FROM orgs o WHERE o.id = r.org_id AND r.state = 'waiting' RETURNING r.kind, o.slug, r.secret_name`)
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
		rows, err := tx.Query(ctx, `SELECT r.kind, o.slug, r.secret_name, r.state FROM restore_accepted_removals r JOIN orgs o ON o.id = r.org_id`)
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

// PlanHash is the hex SHA-256 of the sorted plan as JSON, recorded in restore.release so a later
// reader can tell which plan the release settled. An empty plan hashes the empty list.
func PlanHash(plan []Removal) string {
	b, err := json.Marshal(append([]Removal{}, sortedRemovals(plan)...)) //nolint:gosec // Secret is the name of the declaring Kubernetes Secret, not a secret value
	if err != nil {
		// A struct of three strings always marshals.
		panic(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
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
},
) (waiting, accepted []Removal, err error) {
	rows, err := q.Query(ctx, `SELECT r.kind, o.slug, r.secret_name, r.state FROM restore_accepted_removals r JOIN orgs o ON o.id = r.org_id ORDER BY 1, 2, 3`)
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

// ReleasePendingMetric registers koinon_restore_release_pending on reg (ADR 0110 decision 1): 1 while
// this process is unheld and a release is pending or removals of a restore wait for
// restore.acceptImporterDeletions, else 0. Only koinon-api registers it; a held process reports 0 (the
// hold has its own gauge). The value is read from the database at scrape time; a failed read keeps
// the last value.
func (b *Binding) ReleasePendingMetric(reg prometheus.Registerer) {
	var mu sync.Mutex
	last := 0.0
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "koinon_restore_release_pending",
		Help: "1 while koinon-api is unheld and a restore release is pending or its removals wait for restore.acceptImporterDeletions, else 0.",
	}, func() float64 {
		mu.Lock()
		defer mu.Unlock()
		if !b.Enabled() || b.held {
			return 0
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		v, err := b.releasePendingValue(ctx)
		if err != nil {
			b.log.Debug("koinon_restore_release_pending: read failed, keeping the last value", "err", err)
			return last
		}
		last = v
		return v
	}))
}

func (b *Binding) releasePendingValue(ctx context.Context) (float64, error) {
	pending, err := b.ReleasePending(ctx)
	if err != nil {
		return 0, err
	}
	if pending {
		return 1, nil
	}
	var waiting bool
	if err := b.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM restore_accepted_removals WHERE state = 'waiting')`).Scan(&waiting); err != nil {
		return 0, err
	}
	if waiting {
		return 1, nil
	}
	return 0, nil
}
