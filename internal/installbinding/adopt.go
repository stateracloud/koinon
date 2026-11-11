// SPDX-License-Identifier: Apache-2.0

package installbinding

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/audit"
)

// ErrNoMark is returned by Adopt on a disabled Binding: without a mark there is nothing to adopt with.
var ErrNoMark = errors.New("installbinding: this process has no install mark")

// AdoptResult says what an adopt-restore changed.
type AdoptResult struct {
	// PreviousState is the state before: copy or unknown (or same, for a repeat).
	PreviousState State `json:"previousState"`
	// PreviousMark is the hash of the mark the row carried, "" for an unbound or absent row.
	PreviousMark string `json:"previousMark,omitempty"`
	// RebindTokens is the number of tokens re-enabled.
	RebindTokens int `json:"rebindTokens"`
	// Conflicts are the isolation conflicts (decision 4) that the adopt clears: the targets of this
	// install that equalled the recorded ones of the previous binding. The guards turn off for them.
	Conflicts []Conflict `json:"conflicts,omitempty"`
}

// Adopt makes this install the owner of the database: the internal action behind the
// "adopt-restore" recovery request (ADR 0110 decisions 3 and 6). In one audited transaction it
//
//   - rewrites install_binding with this install's mark and targets (state same) and sets
//     released_by to this install, so the row never names another install as the releaser;
//   - re-enables every API token that is not revoked or expired and carries another mark
//     (token.rebind, one org event each, with the old and the new mark hash);
//   - records one install event, recovery.adopt-restore, that lists the isolation conflicts it
//     clears (checkpoints, public-url, oidc-redirect, each with the shared value).
//
// It does not touch sessions: a refused session is never re-enabled. The actor must be a platform
// admin's or the reconciler's; Adopt checks nothing about who calls it, the caller does (an org owner
// can never reach it).
func (b *Binding) Adopt(ctx context.Context, actor audit.Actor, own Targets) (AdoptResult, error) {
	if !b.Enabled() {
		return AdoptResult{}, ErrNoMark
	}
	var res AdoptResult
	_, err := audit.InPgTx(ctx, b.pool, func(tx pgx.Tx, ab audit.Batch) error {
		if _, err := tx.Exec(ctx, `INSERT INTO install_binding (singleton, mark) VALUES (true, NULL) ON CONFLICT DO NOTHING`); err != nil {
			return err
		}
		old, found, err := readRow(ctx, tx, true)
		if err != nil {
			return err
		}
		res.PreviousState = StateOf(b.mark, old, found)
		res.PreviousMark = old.Mark
		if res.PreviousState == StateCopy && old.Bound {
			// An unbound row recorded no targets: nothing it could conflict with.
			res.Conflicts = Conflicts(old.Targets, own)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE install_binding SET mark = $1, public_url = NULLIF($2, ''), oidc_redirect_url = NULLIF($3, ''),
			        checkpoint_endpoint = NULLIF($4, ''), checkpoint_bucket = NULLIF($5, ''),
			        released_by = $1
			 WHERE singleton`,
			b.mark, own.PublicURL, own.OIDCRedirectURL, own.CheckpointEndpoint, own.CheckpointBucket); err != nil {
			return err
		}
		rows, err := tx.Query(ctx,
			`UPDATE api_tokens AS t SET install_mark = $1
			 FROM (SELECT org_id, id, install_mark AS old FROM api_tokens
			       WHERE revoked_at IS NULL AND expires_at > now() AND install_mark IS DISTINCT FROM $1
			       FOR UPDATE) AS o
			 WHERE t.org_id = o.org_id AND t.id = o.id
			 RETURNING t.org_id, t.id, t.prefix, o.old`, b.mark)
		if err != nil {
			return err
		}
		type rebound struct {
			org, id pgtype.UUID
			prefix  string
			old     pgtype.Text
		}
		var list []rebound
		for rows.Next() {
			var r rebound
			if err := rows.Scan(&r.org, &r.id, &r.prefix, &r.old); err != nil {
				rows.Close()
				return err
			}
			list = append(list, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		res.RebindTokens = len(list)
		for _, r := range list {
			ab.Org(audit.Event{
				Org: r.org, Actor: actor, Action: "token.rebind", TargetKind: "token", TargetID: r.id.String(),
				Before: map[string]any{"installMark": r.old.String},
				After:  map[string]any{"installMark": b.mark, "tokenPrefix": r.prefix},
			})
		}
		ab.Install(audit.InstallEvent{
			Actor: actor, Action: "recovery.adopt-restore", TargetKind: "install", TargetID: "install_binding",
			Before: map[string]any{"state": string(res.PreviousState), "installMark": res.PreviousMark},
			After:  map[string]any{"state": string(StateSame), "installMark": b.mark, "rebound": res.RebindTokens, "conflicts": res.Conflicts},
		})
		return nil
	})
	if err != nil {
		return AdoptResult{}, fmt.Errorf("adopt-restore: %w", err)
	}
	b.setState(StateSame)
	return res, nil
}

// RebindToken re-enables one token that a restored copy refuses, for the platform admin API
// (ADR 0110 decision 3): it stamps the token with this install's mark. Only a platform admin may
// call it; the service checks that. A revoked or expired token is not re-enabled and the call
// reports found=false.
func (b *Binding) RebindToken(ctx context.Context, actor audit.Actor, org pgtype.UUID, tokenID string) (found bool, err error) {
	if !b.Enabled() {
		return false, ErrNoMark
	}
	var id pgtype.UUID
	if err := id.Scan(tokenID); err != nil || !id.Valid {
		return false, nil
	}
	_, err = audit.InPgTx(ctx, b.pool, func(tx pgx.Tx, ab audit.Batch) error {
		var prefix string
		var old pgtype.Text
		err := tx.QueryRow(ctx,
			`WITH o AS (SELECT install_mark AS old FROM api_tokens WHERE org_id = $2 AND id = $3
			            AND revoked_at IS NULL AND expires_at > now() FOR UPDATE)
			 UPDATE api_tokens AS t SET install_mark = $1 FROM o
			 WHERE t.org_id = $2 AND t.id = $3
			 RETURNING t.prefix, o.old`, b.mark, org, id).Scan(&prefix, &old)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		ab.Org(audit.Event{
			Org: org, Actor: actor, Action: "token.rebind", TargetKind: "token", TargetID: id.String(),
			Before: map[string]any{"installMark": old.String},
			After:  map[string]any{"installMark": b.mark, "tokenPrefix": prefix},
		})
		return nil
	})
	return found, err
}
