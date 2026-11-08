// SPDX-License-Identifier: Apache-2.0

// Package tokens issues and verifies admin API tokens (ADR 0056, plan item PV1).
//
// A token is scoped to one org and carries the role admin or owner, never more than its
// creator's. It expires (default 30 days, at most 90), can be revoked, and records when it
// was last used. Koinon stores only the SHA-256 of the token and a short prefix to find the
// row; the token is shown once, when it is created.
//
// Token format: koinon_<8 characters of a-z0-9>_<43 characters of base64url>. The prefix
// is not secret. The secret part carries 256 bits of entropy, so a fast hash suffices.
package tokens

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/auth"
	"github.com/stateracloud/koinon/internal/db/store"
)

// Lifetimes.
const (
	DefaultTTL = 30 * 24 * time.Hour
	MaxTTL     = 90 * 24 * time.Hour
	MinTTL     = time.Hour
)

// Prefix starts every token, so secret scanners can recognise one.
const Prefix = "koinon_"

// Errors.
var (
	// ErrInvalid means the token is unknown, wrong, expired or revoked. Callers do not say which.
	ErrInvalid  = errors.New("tokens: invalid, expired or revoked token")
	ErrNotFound = errors.New("tokens: not found")
)

// InvalidError is a validation failure with a message safe to show.
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string { return e.Msg }

// Token describes a token; it never holds the secret.
type Token struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	Role       string     `json:"role"`
	CreatedAt  time.Time  `json:"createdAt"`
	ExpiresAt  time.Time  `json:"expiresAt"`
	RevokedAt  *time.Time `json:"revokedAt,omitempty"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
	// CreatedBy is the root user of the token's chain; ParentID names the token that created this
	// one, empty for a token made from a session.
	CreatedBy string `json:"createdBy,omitempty"`
	ParentID  string `json:"parentId,omitempty"`
}

// Created is a new token with its secret, shown once.
type Created struct {
	Token
	Secret string
}

// Principal is an authenticated token.
type Principal struct {
	TokenID pgtype.UUID
	Org     pgtype.UUID
	OrgSlug string
	Role    store.OrgRole
	// CreatedBy is the root user of the token's chain.
	CreatedBy pgtype.UUID
	ExpiresAt time.Time
}

// Service issues and verifies tokens.
type Service struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// New returns a Service. now defaults to time.Now.
func New(pool *pgxpool.Pool, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{pool: pool, now: now}
}

func toToken(t store.ApiToken) Token {
	out := Token{ID: t.ID.String(), Name: t.Name, Prefix: t.Prefix, Role: string(t.Role), CreatedAt: t.CreatedAt.Time, ExpiresAt: t.ExpiresAt.Time}
	if t.CreatedBy.Valid {
		out.CreatedBy = t.CreatedBy.String()
	}
	if t.ParentTokenID.Valid {
		out.ParentID = t.ParentTokenID.String()
	}
	if t.RevokedAt.Valid {
		out.RevokedAt = &t.RevokedAt.Time
	}
	if t.LastUsedAt.Valid {
		out.LastUsedAt = &t.LastUsedAt.Time
	}
	return out
}

const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

func newPrefix() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)] // 256 mod 36 bias is irrelevant for a lookup prefix
	}
	return string(b), nil
}

func hashOf(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// Creator is who makes a token. A user in a session starts a fresh chain; a token continues its
// own: the new token has the same root user, names the creating token as its parent, and cannot
// outlive it or outrank it.
type Creator struct {
	// User is the root user: the signed-in user, or for a token the user its chain began with.
	User pgtype.UUID
	// Role is the creator's role (a token's own role, or the user's role in the org).
	Role store.OrgRole
	// Parent is the creating token, nil for a session.
	Parent *Principal
}

// Create issues a token for org with role, expiring after ttl (zero means DefaultTTL, or the
// parent's remaining life if shorter). The token cannot carry more than the creator's role, and
// a child cannot outlive its parent.
func (s *Service) Create(ctx context.Context, org pgtype.UUID, actor audit.Actor, by Creator, name string, role store.OrgRole, ttl time.Duration) (Created, error) {
	name = strings.TrimSpace(name)
	switch {
	case name == "" || len(name) > 200:
		return Created{}, &InvalidError{"name must be 1 to 200 characters"}
	case role != store.OrgRoleAdmin && role != store.OrgRoleOwner:
		return Created{}, &InvalidError{"role must be admin or owner"}
	case !auth.RoleAtLeast(by.Role, role):
		return Created{}, &InvalidError{"a token cannot carry a higher role than its creator's (" + string(by.Role) + ")"}
	case !by.User.Valid:
		return Created{}, errors.New("tokens: a token needs a root user")
	}
	now := s.now()
	var parentID pgtype.UUID
	if ttl == 0 {
		ttl = DefaultTTL
		if by.Parent != nil {
			ttl = min(ttl, by.Parent.ExpiresAt.Sub(now))
		}
	} else if ttl < MinTTL || ttl > MaxTTL {
		return Created{}, &InvalidError{fmt.Sprintf("expiry must be between %s and %s", MinTTL, MaxTTL)}
	}
	if by.Parent != nil {
		parentID = by.Parent.TokenID
		if left := by.Parent.ExpiresAt.Sub(now); ttl > left {
			return Created{}, &InvalidError{fmt.Sprintf("a token cannot outlive the token that creates it, which expires in %s", left.Round(time.Second))}
		}
	}
	if ttl <= 0 {
		return Created{}, &InvalidError{"the creating token has expired"}
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return Created{}, err
	}
	prefix, err := newPrefix()
	if err != nil {
		return Created{}, err
	}
	raw := Prefix + prefix + "_" + base64.RawURLEncoding.EncodeToString(secret)
	uid, err := uuid.NewV7()
	if err != nil {
		return Created{}, err
	}
	id := pgtype.UUID{Bytes: uid, Valid: true}
	var row store.ApiToken
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		if by.Parent != nil {
			// Share-lock the parent until the child is committed: a concurrent revoke locks the tree
			// for update first, so it waits and then sees (and revokes) the child. A parent revoked
			// in the meantime cannot create one.
			cur, err := q.LockAPITokenShare(ctx, store.LockAPITokenShareParams{OrgID: org, ID: by.Parent.TokenID})
			if err != nil {
				return err
			}
			if cur.RevokedAt.Valid || !now.Before(cur.ExpiresAt.Time) {
				return &InvalidError{"the creating token has been revoked or has expired"}
			}
		}
		var err error
		row, err = q.CreateAPIToken(ctx, store.CreateAPITokenParams{
			OrgID: org, ID: id, Name: name, Prefix: prefix, TokenHash: hashOf(raw), Role: role, CreatedBy: by.User,
			ParentTokenID: parentID,
			ExpiresAt:     pgtype.Timestamptz{Time: now.Add(ttl), Valid: true}, CreatedAt: pgtype.Timestamptz{Time: now, Valid: true},
		})
		if err != nil {
			return err
		}
		after := map[string]any{"name": name, "role": string(role), "tokenPrefix": prefix, "expiresAt": row.ExpiresAt.Time}
		if parentID.Valid {
			after["parentTokenId"] = parentID.String()
		}
		return audit.RecordErr(ctx, q, audit.Event{
			Org: org, Actor: actor, Action: "token.create", TargetKind: "token", TargetID: id.String(), After: after,
		})
	})
	if err != nil {
		return Created{}, err
	}
	return Created{Token: toToken(row), Secret: raw}, nil
}

// List returns the org's tokens, newest first.
func (s *Service) List(ctx context.Context, org pgtype.UUID) ([]Token, error) {
	rows, err := store.New(s.pool).ListAPITokens(ctx, org)
	if err != nil {
		return nil, err
	}
	out := make([]Token, 0, len(rows))
	for _, r := range rows {
		out = append(out, toToken(r))
	}
	return out, nil
}

// Revoke revokes a token and every token descended from it. The caller's role must be at least the
// target's. Revoking twice is fine.
func (s *Service) Revoke(ctx context.Context, org pgtype.UUID, actor audit.Actor, callerRole store.OrgRole, id string) (Token, error) {
	var tid pgtype.UUID
	if err := tid.Scan(id); err != nil || !tid.Valid {
		return Token{}, ErrNotFound
	}
	var target store.ApiToken
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		var err error
		if target, err = q.GetAPIToken(ctx, store.GetAPITokenParams{OrgID: org, ID: tid}); err != nil {
			return err
		}
		if !auth.RoleAtLeast(callerRole, target.Role) {
			return &InvalidError{"a token with a lower role cannot revoke a token with a higher one"}
		}
		// Lock the whole subtree first (waiting for creations under it), so the statement below,
		// which takes a fresh snapshot, sees every child committed by then.
		if _, err := q.LockAPITokenTree(ctx, store.LockAPITokenTreeParams{OrgID: org, ID: tid}); err != nil {
			return err
		}
		revoked, err := q.RevokeAPITokenTree(ctx, store.RevokeAPITokenTreeParams{OrgID: org, ID: tid})
		if err != nil {
			return err
		}
		ids := make([]string, 0, len(revoked))
		for _, r := range revoked {
			ids = append(ids, r.ID.String())
		}
		slices.Sort(ids)
		return audit.RecordErr(ctx, q, audit.Event{
			Org: org, Actor: actor, Action: "token.revoke", TargetKind: "token", TargetID: id,
			After: map[string]any{"name": target.Name, "tokenPrefix": target.Prefix, "revokedTokenIds": ids},
		})
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Token{}, ErrNotFound
	}
	if err != nil {
		return Token{}, err
	}
	row, err := store.New(s.pool).GetAPIToken(ctx, store.GetAPITokenParams{OrgID: org, ID: tid})
	if err != nil {
		return Token{}, err
	}
	return toToken(row), nil
}

// AuthError is a failed authentication. When the token's prefix names a real token, TokenID and
// Org say which, so the failure can be written to that org's audit trail.
type AuthError struct {
	Reason string
	// Prefix is the token's public prefix as presented, if it had one; never the secret part.
	Prefix  string
	TokenID pgtype.UUID
	Org     pgtype.UUID
}

func (e *AuthError) Error() string { return ErrInvalid.Error() }
func (e *AuthError) Unwrap() error { return ErrInvalid }

// Authenticate verifies a bearer token and records its use. A token also needs its root user to
// still hold at least the token's role in the org (a demoted or removed creator voids the chain).
// A failure that names a real token is recorded in that token's org audit trail.
func (s *Service) Authenticate(ctx context.Context, raw string) (Principal, error) {
	rest, ok := strings.CutPrefix(raw, Prefix)
	if !ok || len(rest) < 10 || rest[8] != '_' {
		return Principal{}, &AuthError{Reason: "not a Koinon token"}
	}
	prefix := rest[:8]
	q := store.New(s.pool)
	row, err := q.GetAPITokenByPrefix(ctx, prefix)
	if errors.Is(err, pgx.ErrNoRows) {
		// Hash anyway, so unknown prefixes cost the same.
		subtle.ConstantTimeCompare(hashOf(raw), hashOf(raw))
		return Principal{}, &AuthError{Reason: "unknown token", Prefix: prefix}
	}
	if err != nil {
		return Principal{}, err
	}
	reason := ""
	switch {
	case subtle.ConstantTimeCompare(hashOf(raw), row.TokenHash) != 1:
		reason = "wrong secret"
	case row.RevokedAt.Valid:
		reason = "revoked"
	case !s.now().Before(row.ExpiresAt.Time):
		reason = "expired"
	default:
		if holds, err := s.holdsRole(ctx, q, row.CreatedBy, row.OrgID, row.Role); err != nil {
			return Principal{}, err
		} else if !holds {
			reason = "its creator no longer holds the role"
		}
	}
	if reason != "" {
		s.auditFailure(ctx, q, row, reason)
		return Principal{}, &AuthError{Reason: reason, Prefix: prefix, TokenID: row.ID, Org: row.OrgID}
	}
	// Failing to record use must not fail the request.
	_ = q.TouchAPIToken(ctx, store.TouchAPITokenParams{OrgID: row.OrgID, ID: row.ID})
	return Principal{
		TokenID: row.ID, Org: row.OrgID, OrgSlug: row.OrgSlug, Role: row.Role, CreatedBy: row.CreatedBy, ExpiresAt: row.ExpiresAt.Time,
	}, nil
}

// holdsRole reports whether the user is a platform admin or has at least role in the org.
func (s *Service) holdsRole(ctx context.Context, q *store.Queries, user, org pgtype.UUID, role store.OrgRole) (bool, error) {
	if admin, err := q.IsPlatformAdmin(ctx, user); err != nil || admin {
		return admin, err
	}
	ms, err := q.ListEffectiveMemberships(ctx, user)
	if err != nil {
		return false, err
	}
	for _, m := range ms {
		if m.OrgID == org {
			return auth.RoleAtLeast(m.Role, role), nil
		}
	}
	return false, nil
}

// auditFailure writes "failed use of token <id>" to the token's org. It is best effort.
func (s *Service) auditFailure(ctx context.Context, q *store.Queries, row store.GetAPITokenByPrefixRow, reason string) {
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = audit.RecordAfter(actx, q, audit.Event{
		Org: row.OrgID, Actor: audit.System("koinon-api:token-use"), Action: "token.use_failed", TargetKind: "token", TargetID: row.ID.String(),
		After: map[string]any{"reason": reason, "tokenPrefix": row.Prefix},
	})
}
