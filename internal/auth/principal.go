// SPDX-License-Identifier: Apache-2.0

// Package auth authenticates humans (ADR 0004, 0022): the bootstrap admin's
// local password, generic OIDC with PKCE, server-side sessions behind an
// HTTP-only cookie, and a connect-rpc interceptor that puts the caller's
// Principal in the request context. Roles always come from Postgres.
package auth

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
	authv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/auth/v1alpha1"
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

// orgRoles lists the org roles from lowest to highest with their wire form.
// It is the one ordering of roles: roleRank and roleToProto both read it.
var orgRoles = []struct {
	role  store.OrgRole
	proto authv1alpha1.OrgRole
}{
	{store.OrgRoleViewer, authv1alpha1.OrgRole_ORG_ROLE_VIEWER},
	{store.OrgRoleMember, authv1alpha1.OrgRole_ORG_ROLE_MEMBER},
	{store.OrgRoleAdmin, authv1alpha1.OrgRole_ORG_ROLE_ADMIN},
	{store.OrgRoleOwner, authv1alpha1.OrgRole_ORG_ROLE_OWNER},
}

// roleRank returns 1 for the lowest role and higher numbers for higher roles,
// 0 for anything that is not a role.
func roleRank(r store.OrgRole) int {
	for i, o := range orgRoles {
		if o.role == r {
			return i + 1
		}
	}
	return 0
}

func roleToProto(r store.OrgRole) authv1alpha1.OrgRole {
	if i := roleRank(r); i > 0 {
		return orgRoles[i-1].proto
	}
	return authv1alpha1.OrgRole_ORG_ROLE_UNSPECIFIED
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

// RoleToProto is the wire form of an org role.
func RoleToProto(r store.OrgRole) authv1alpha1.OrgRole { return roleToProto(r) }

// RoleFromProto is the org role of a wire value; ok is false for unspecified or unknown values.
func RoleFromProto(r authv1alpha1.OrgRole) (store.OrgRole, bool) {
	for _, o := range orgRoles {
		if o.proto == r {
			return o.role, true
		}
	}
	return "", false
}
