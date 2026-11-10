// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
)

// ErrNoLocalUser means no local login has the username. The operator's reset never creates one.
var ErrNoLocalUser = errors.New("no local user with that username")

// ResetResult is what a password reset changed.
type ResetResult struct {
	UserID          pgtype.UUID
	RevokedSessions int64
}

// ResetLocalPassword is the operator's break-glass reset (issue #512): it sets a new password for the
// local login username, makes the user choose another at the next login (must_change_password) and ends
// every session of the user, all in one transaction with the auth.password.reset audit event, so the
// change commits with its event or not at all. It needs no session and no current password: whoever can
// run it already holds the database credentials. The password is hashed here and never logged or
// audited. actor names the command (audit.CLI).
//
// It is a local write, so the restore hold (ADR 0107) does not refuse it: after a restore the
// operator needs it most.
func ResetLocalPassword(ctx context.Context, db audit.Beginner, actor audit.Actor, username, password string, now time.Time) (ResetResult, error) {
	if username == "" {
		return ResetResult{}, errors.New("a username is required")
	}
	if err := ValidateNewPassword(password); err != nil {
		return ResetResult{}, err
	}
	hash, err := HashPasswordContext(ctx, password)
	if err != nil {
		return ResetResult{}, fmt.Errorf("hash password: %w", err)
	}
	var res ResetResult
	_, err = audit.InTx(ctx, db, func(q *store.Queries, b audit.Batch) error {
		cred, err := q.GetLocalCredentialByUsername(ctx, username)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoLocalUser
		}
		if err != nil {
			return err
		}
		user, err := q.GetActiveUser(ctx, cred.UserID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoLocalUser
		}
		if err != nil {
			return err
		}
		at := ts(now)
		if err := q.SetLocalPassword(ctx, store.SetLocalPasswordParams{
			UserID: cred.UserID, PasswordHash: hash, MustChangePassword: true, PasswordChangedAt: at,
		}); err != nil {
			return err
		}
		n, err := q.RevokeUserSessions(ctx, store.RevokeUserSessionsParams{Now: at, UserID: cred.UserID})
		if err != nil {
			return err
		}
		res = ResetResult{UserID: cred.UserID, RevokedSessions: n}
		// The event sits in the user's personal org, where the user's login events are.
		b.Org(audit.Event{
			Org: user.PersonalOrgID, Actor: actor, Action: "auth.password.reset", TargetKind: "user", TargetID: cred.UserID.String(),
			After: map[string]any{"username": username, "changeRequired": true, "revokedSessions": n},
		})
		return nil
	})
	if err != nil {
		return ResetResult{}, err
	}
	return res, nil
}
