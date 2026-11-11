// SPDX-License-Identifier: Apache-2.0

// Package recovery holds the platform admin recovery actions of ADR 0110 decision 6: reset a local
// password and mint an admin API token. They are plain functions over the database. They take the
// actor as a parameter and know nothing about who may ask: the caller (the reconciler's handler of
// the PlatformAdminRecovery request object, a later change of issue #516) authorises the request with
// Kubernetes RBAC and passes the requester's Kubernetes identity as the actor. There is no pod-exec
// command for them: clusters can forbid exec and the images are distroless (issue #512).
// Issue #512 stays open until the request object of #516 part (d) calls these functions end to end.
//
// Both write locally, so the restore hold (ADR 0107) does not refuse them: after a restore the
// operator needs them most. The secrets they make are returned to the caller, never logged and never
// put in an audit event.
package recovery

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/auth"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/tokens"
)

// MaxTokenTTL is the longest a recovery token lives (ADR 0110 decision 6).
const MaxTokenTTL = 24 * time.Hour

var (
	// ErrNoLocalUser means no local login has the username. A reset never creates one.
	ErrNoLocalUser = errors.New("no local user with that username")
	// ErrNotPlatformAdmin means the user is not a platform admin. Recovery is a platform admin job.
	ErrNotPlatformAdmin = errors.New("the user is not a platform admin")
	// ErrNoOrg means the org slug or ID matches no org.
	ErrNoOrg = errors.New("no org with that slug or ID")
)

// ResetResult is what a password reset changed. Password is the one-time password; the caller seals it
// to the requester and drops it.
type ResetResult struct {
	UserID          pgtype.UUID
	Password        string
	RevokedSessions int64
}

// ResetPassword gives the local login username a new random one-time password, makes the user choose
// another at the next login (must_change_password) and ends every session of the user, in one
// transaction with the auth.password.reset audit event, so the change commits with its event or not at
// all. It needs no session and no current password. The password is hashed here and is in neither the
// log nor the event.
func ResetPassword(ctx context.Context, db audit.Beginner, actor audit.Actor, username string, now time.Time) (ResetResult, error) {
	if username == "" {
		return ResetResult{}, errors.New("a username is required")
	}
	password, err := newOneTimePassword()
	if err != nil {
		return ResetResult{}, err
	}
	if err := auth.ValidateNewPassword(password); err != nil {
		return ResetResult{}, err
	}
	hash, err := auth.HashPasswordContext(ctx, password)
	if err != nil {
		return ResetResult{}, fmt.Errorf("hash password: %w", err)
	}
	res := ResetResult{Password: password}
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
		at := pgtype.Timestamptz{Time: now, Valid: true}
		if err := q.SetLocalPassword(ctx, store.SetLocalPasswordParams{
			UserID: cred.UserID, PasswordHash: hash, MustChangePassword: true, PasswordChangedAt: at,
		}); err != nil {
			return err
		}
		n, err := q.RevokeUserSessions(ctx, store.RevokeUserSessionsParams{Now: at, UserID: cred.UserID})
		if err != nil {
			return err
		}
		res.UserID, res.RevokedSessions = cred.UserID, n
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

// newOneTimePassword is 24 random bytes, URL-safe: far above the minimum length, nothing to escape.
func newOneTimePassword() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("random password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// MintTokenParams says whose admin token to make.
type MintTokenParams struct {
	// Username is the local login name of the platform admin the token acts for.
	Username string
	// Org is the slug or ID of the org the token belongs to.
	Org string
	// Name is the token's name.
	Name string
	// TTL is how long the token lives; zero means MaxTokenTTL. Above MaxTokenTTL is refused.
	TTL time.Duration
}

// MintAdminToken makes an owner-role API token for an existing platform admin, through the tokens
// service, so it is hashed at rest, stamped with the install mark (svc.WithBinding) and audited as
// token.create with actor. The token is as valid as its creator: it stops working when the user stops
// being a platform admin. An org owner who is not a platform admin is refused (ErrNotPlatformAdmin):
// recovery is never an org owner's job.
func MintAdminToken(ctx context.Context, svc *tokens.Service, q *store.Queries, actor audit.Actor, p MintTokenParams) (tokens.Created, error) {
	ttl := p.TTL
	switch {
	case ttl == 0:
		ttl = MaxTokenTTL
	case ttl > MaxTokenTTL:
		return tokens.Created{}, fmt.Errorf("a recovery token lives at most %s", MaxTokenTTL)
	}
	if p.Username == "" || p.Org == "" || strings.TrimSpace(p.Name) == "" {
		return tokens.Created{}, errors.New("a username, an org and a name are required")
	}
	cred, err := q.GetLocalCredentialByUsername(ctx, p.Username)
	if errors.Is(err, pgx.ErrNoRows) {
		return tokens.Created{}, ErrNoLocalUser
	}
	if err != nil {
		return tokens.Created{}, err
	}
	if _, err := q.GetActiveUser(ctx, cred.UserID); errors.Is(err, pgx.ErrNoRows) {
		return tokens.Created{}, ErrNoLocalUser
	} else if err != nil {
		return tokens.Created{}, err
	}
	admin, err := q.IsPlatformAdmin(ctx, cred.UserID)
	if err != nil {
		return tokens.Created{}, err
	}
	if !admin {
		return tokens.Created{}, ErrNotPlatformAdmin
	}
	org, err := resolveOrg(ctx, q, p.Org)
	if err != nil {
		return tokens.Created{}, err
	}
	// A platform admin counts as owner of every org.
	return svc.Create(ctx, org, actor, tokens.Creator{User: cred.UserID, Role: store.OrgRoleOwner}, p.Name, store.OrgRoleOwner, ttl)
}

// resolveOrg takes ref as an org ID, else as a slug.
func resolveOrg(ctx context.Context, q *store.Queries, ref string) (pgtype.UUID, error) {
	var id pgtype.UUID
	if err := id.Scan(ref); err == nil && id.Valid {
		return id, nil
	}
	o, err := q.GetOrgBySlug(ctx, ref)
	if errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, ErrNoOrg
	}
	if err != nil {
		return pgtype.UUID{}, err
	}
	return o.ID, nil
}
