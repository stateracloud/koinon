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

// Create issues a token for org with role, expiring after ttl (zero means DefaultTTL). creatorRole
// is the creator's role in org (a token creator's own role, or a user's); the token cannot
// carry more. creator is the user who creates it, or invalid when a token does.
func (s *Service) Create(ctx context.Context, org pgtype.UUID, actor audit.Actor, creator pgtype.UUID, creatorRole store.OrgRole, name string, role store.OrgRole, ttl time.Duration) (Created, error) {
	name = strings.TrimSpace(name)
	switch {
	case name == "" || len(name) > 200:
		return Created{}, &InvalidError{"name must be 1 to 200 characters"}
	case role != store.OrgRoleAdmin && role != store.OrgRoleOwner:
		return Created{}, &InvalidError{"role must be admin or owner"}
	case !auth.RoleAtLeast(creatorRole, role):
		return Created{}, &InvalidError{"a token cannot carry a higher role than its creator's (" + string(creatorRole) + ")"}
	}
	if ttl == 0 {
		ttl = DefaultTTL
	}
	if ttl < MinTTL || ttl > MaxTTL {
		return Created{}, &InvalidError{fmt.Sprintf("expiry must be between %s and %s", MinTTL, MaxTTL)}
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
	now := s.now()
	var row store.ApiToken
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		var err error
		row, err = q.CreateAPIToken(ctx, store.CreateAPITokenParams{
			OrgID: org, ID: id, Name: name, Prefix: prefix, TokenHash: hashOf(raw), Role: role, CreatedBy: creator,
			ExpiresAt: pgtype.Timestamptz{Time: now.Add(ttl), Valid: true}, CreatedAt: pgtype.Timestamptz{Time: now, Valid: true},
		})
		if err != nil {
			return err
		}
		return audit.RecordErr(ctx, q, audit.Event{
			Org: org, Actor: actor, Action: "token.create", TargetKind: "token", TargetID: id.String(),
			After: map[string]any{"name": name, "role": string(role), "tokenPrefix": prefix, "expiresAt": row.ExpiresAt.Time},
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

// Revoke revokes a token. Revoking twice is fine.
func (s *Service) Revoke(ctx context.Context, org pgtype.UUID, actor audit.Actor, id string) (Token, error) {
	var tid pgtype.UUID
	if err := tid.Scan(id); err != nil || !tid.Valid {
		return Token{}, ErrNotFound
	}
	var row store.ApiToken
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		var err error
		if row, err = q.RevokeAPIToken(ctx, store.RevokeAPITokenParams{OrgID: org, ID: tid}); err != nil {
			return err
		}
		return audit.RecordErr(ctx, q, audit.Event{
			Org: org, Actor: actor, Action: "token.revoke", TargetKind: "token", TargetID: id,
			After: map[string]any{"name": row.Name, "tokenPrefix": row.Prefix},
		})
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Token{}, ErrNotFound
	}
	if err != nil {
		return Token{}, err
	}
	return toToken(row), nil
}

// Authenticate verifies a bearer token and records its use.
func (s *Service) Authenticate(ctx context.Context, raw string) (Principal, error) {
	rest, ok := strings.CutPrefix(raw, Prefix)
	if !ok || len(rest) < 10 || rest[8] != '_' {
		return Principal{}, ErrInvalid
	}
	q := store.New(s.pool)
	row, err := q.GetAPITokenByPrefix(ctx, rest[:8])
	if errors.Is(err, pgx.ErrNoRows) {
		// Hash anyway, so unknown prefixes cost the same.
		subtle.ConstantTimeCompare(hashOf(raw), hashOf(raw))
		return Principal{}, ErrInvalid
	}
	if err != nil {
		return Principal{}, err
	}
	if subtle.ConstantTimeCompare(hashOf(raw), row.TokenHash) != 1 ||
		row.RevokedAt.Valid || !s.now().Before(row.ExpiresAt.Time) {
		return Principal{}, ErrInvalid
	}
	// Failing to record use must not fail the request.
	_ = q.TouchAPIToken(ctx, store.TouchAPITokenParams{OrgID: row.OrgID, ID: row.ID})
	return Principal{TokenID: row.ID, Org: row.OrgID, OrgSlug: row.OrgSlug, Role: row.Role}, nil
}
