// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"errors"

	connect "connectrpc.com/connect/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/auth"
	"github.com/stateracloud/koinon/internal/db/store"
	tokenv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/token/v1alpha1"
	"github.com/stateracloud/koinon/internal/tokens"
)

// tokenProto converts a token; names maps user IDs to what the UI shows for them.
func tokenProto(t tokens.Token, names map[string]string) *tokenv1alpha1.Token {
	role, _ := roleOf(t.Role)
	out := &tokenv1alpha1.Token{
		Id: t.ID, Name: t.Name, Prefix: t.Prefix, Role: auth.RoleToProto(role),
		CreatedAt: timestamppb.New(t.CreatedAt), ExpiresAt: timestamppb.New(t.ExpiresAt),
		CreatedBy: t.CreatedBy, CreatedByName: names[t.CreatedBy], ParentTokenId: t.ParentID,
	}
	if t.RevokedAt != nil {
		out.RevokedAt = timestamppb.New(*t.RevokedAt)
	}
	if t.LastUsedAt != nil {
		out.LastUsedAt = timestamppb.New(*t.LastUsedAt)
	}
	return out
}

// userNames maps the IDs of the users who created the tokens to their display name or email, with
// one query. A real database error is returned, not shown as a deleted user.
func (s *Server) userNames(ctx context.Context, list []tokens.Token) (map[string]string, error) {
	seen := map[string]bool{}
	var ids []pgtype.UUID
	for _, t := range list {
		if t.CreatedBy == "" || seen[t.CreatedBy] {
			continue
		}
		seen[t.CreatedBy] = true
		id, err := parseID("user", t.CreatedBy)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	names := map[string]string{}
	if len(ids) == 0 {
		return names, nil
	}
	rows, err := s.q.ListUsersByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, u := range rows {
		switch {
		case u.DeletedAt.Valid:
			names[u.ID.String()] = "a deleted user"
		case u.DisplayName != "":
			names[u.ID.String()] = u.DisplayName
		default:
			names[u.ID.String()] = u.Email.String
		}
	}
	return names, nil
}

func roleOf(s string) (store.OrgRole, bool) {
	switch r := store.OrgRole(s); r {
	case store.OrgRoleViewer, store.OrgRoleMember, store.OrgRoleAdmin, store.OrgRoleOwner:
		return r, true
	}
	return "", false
}

// callerRole is the caller's effective role in the org; platform admins hold owner.
func (c caller) role() store.OrgRole {
	if r, ok := c.p.RoleIn(c.org); ok && !c.p.PlatformAdmin {
		return r
	}
	if c.p.PlatformAdmin {
		return store.OrgRoleOwner
	}
	return store.OrgRoleViewer
}

func (c caller) auditActor() audit.Actor { return audit.User(c.p.UserID) }

func (s *Server) tokenError(err error) error {
	var inv *tokens.InvalidError
	switch {
	case errors.As(err, &inv):
		return connect.NewError(connect.CodeInvalidArgument, inv.Msg)
	case errors.Is(err, tokens.ErrNoBinding):
		return connect.NewError(connect.CodeFailedPrecondition, "this install has no install mark (KOINON_INSTALL_MARK), so there is nothing to rebind a token to")
	case errors.Is(err, tokens.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, "token not found")
	}
	return s.dbError(err, "token")
}

func requestCtx(ctx context.Context) context.Context {
	if audit.RequestID(ctx) != "" {
		return ctx
	}
	return audit.WithRequestID(ctx, uuid.NewString())
}

// CreateToken implements TokenService. Admins and owners.
func (s *Server) CreateToken(ctx context.Context, req *tokenv1alpha1.CreateTokenRequest) (*tokenv1alpha1.CreateTokenResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	role, ok := auth.RoleFromProto(req.GetRole())
	if !ok {
		return nil, connect.NewError(connect.CodeInvalidArgument, "role must be admin or owner")
	}
	created, err := s.tokens.Create(requestCtx(ctx), c.org, c.auditActor(), tokens.Creator{User: c.p.UserID, Role: c.role()}, req.GetName(), role, req.GetExpiresIn().AsDuration())
	if err != nil {
		return nil, s.tokenError(err)
	}
	s.log.Info("admin API token created", "org", c.org.String(), "token", created.ID, "prefix", created.Prefix, "role", created.Role, "by", c.actor().String())
	// The token exists and its secret is shown only here: a failed name lookup must not lose it.
	names, err := s.userNames(ctx, []tokens.Token{created.Token})
	if err != nil {
		s.log.Error("could not look up the token creator's name", "token", created.ID, "err", err)
		names = map[string]string{}
	}
	return &tokenv1alpha1.CreateTokenResponse{Token: tokenProto(created.Token, names), Secret: created.Secret}, nil
}

// ListTokens implements TokenService. Admins and owners.
func (s *Server) ListTokens(ctx context.Context, req *tokenv1alpha1.ListTokensRequest) (*tokenv1alpha1.ListTokensResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	list, err := s.tokens.List(ctx, c.org)
	if err != nil {
		return nil, s.tokenError(err)
	}
	res := &tokenv1alpha1.ListTokensResponse{}
	names, err := s.userNames(ctx, list)
	if err != nil {
		return nil, s.tokenError(err)
	}
	for _, t := range list {
		res.Tokens = append(res.Tokens, tokenProto(t, names))
	}
	return res, nil
}

// RevokeToken implements TokenService. Admins and owners.
func (s *Server) RevokeToken(ctx context.Context, req *tokenv1alpha1.RevokeTokenRequest) (*tokenv1alpha1.RevokeTokenResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	t, err := s.tokens.Revoke(requestCtx(ctx), c.org, c.auditActor(), c.role(), req.GetTokenId())
	if err != nil {
		return nil, s.tokenError(err)
	}
	names, err := s.userNames(ctx, []tokens.Token{t})
	if err != nil {
		return nil, s.tokenError(err)
	}
	return &tokenv1alpha1.RevokeTokenResponse{Token: tokenProto(t, names)}, nil
}

// RebindToken implements TokenService. Platform admins only: a token that a restored copy refuses
// because another install issued it is re-enabled here (ADR 0110 decision 3), and an org owner or
// org admin cannot do it, whatever the token's org.
func (s *Server) RebindToken(ctx context.Context, req *tokenv1alpha1.RebindTokenRequest) (*tokenv1alpha1.RebindTokenResponse, error) {
	p, ok := auth.PrincipalFrom(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, "login required")
	}
	if !p.PlatformAdmin {
		return nil, connect.NewError(connect.CodePermissionDenied, "only a platform admin re-enables a token of a restored copy")
	}
	org, err := parseID("org", req.GetOrgId())
	if err != nil {
		return nil, err
	}
	t, err := s.tokens.Rebind(requestCtx(ctx), org, audit.User(p.UserID), req.GetTokenId())
	if err != nil {
		return nil, s.tokenError(err)
	}
	names, err := s.userNames(ctx, []tokens.Token{t})
	if err != nil {
		return nil, s.tokenError(err)
	}
	s.log.Info("API token rebound to this install", "org", req.GetOrgId(), "token", t.ID, "prefix", t.Prefix, "by", p.UserID)
	return &tokenv1alpha1.RebindTokenResponse{Token: tokenProto(t, names)}, nil
}
