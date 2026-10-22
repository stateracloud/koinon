// SPDX-License-Identifier: Apache-2.0

// Package auth authenticates humans (ADR 0004, 0022): the bootstrap admin's
// local password, generic OIDC with PKCE, server-side sessions behind an
// HTTP-only cookie, and a connect-rpc interceptor that puts the caller's
// Principal in the request context. Roles always come from Postgres.
package auth

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/procoduck/koinon/internal/db/store"
)

// Membership is the caller's effective role in one org, computed according to
// the org's identity mode.
type Membership struct {
	OrgID          pgtype.UUID
	OrgSlug        string
	OrgDisplayName string
	Role           store.OrgRole
}

// Principal is the authenticated caller of a request.
type Principal struct {
	UserID        pgtype.UUID
	Email         string
	DisplayName   string
	PersonalOrgID pgtype.UUID

	SessionID  pgtype.UUID
	AuthMethod store.AuthMethod

	// PlatformAdmin is the role above all orgs (ADR 0004).
	PlatformAdmin bool
	// PasswordChangeRequired is set while the bootstrap admin still uses the
	// one-time password; the interceptor then allows only WhoAmI and
	// ChangePassword.
	PasswordChangeRequired bool

	Memberships []Membership
}

// RoleIn returns the caller's effective role in org, if any.
func (p *Principal) RoleIn(org pgtype.UUID) (store.OrgRole, bool) {
	for _, m := range p.Memberships {
		if m.OrgID == org {
			return m.Role, true
		}
	}
	return "", false
}

// HasRole reports whether the caller holds at least role min in org. Platform
// admins hold every role in every org.
func (p *Principal) HasRole(org pgtype.UUID, minRole store.OrgRole) bool {
	if p.PlatformAdmin {
		return true
	}
	role, ok := p.RoleIn(org)
	return ok && RoleAtLeast(role, minRole)
}

// RoleAtLeast reports whether role is minRole or higher.
func RoleAtLeast(role, minRole store.OrgRole) bool {
	return roleRank(role) >= roleRank(minRole) && roleRank(minRole) > 0
}

func roleRank(r store.OrgRole) int {
	switch r {
	case store.OrgRoleViewer:
		return 1
	case store.OrgRoleMember:
		return 2
	case store.OrgRoleAdmin:
		return 3
	case store.OrgRoleOwner:
		return 4
	default:
		return 0
	}
}

type principalKey struct{}

// WithPrincipal returns a context carrying p.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the authenticated caller. Every RPC behind the
// interceptor has one.
func PrincipalFrom(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(*Principal)
	return p, ok && p != nil
}
