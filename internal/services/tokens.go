// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"errors"

	connect "connectrpc.com/connect/v2"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/auth"
	"github.com/stateracloud/koinon/internal/db/store"
	tokenv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/token/v1alpha1"
	"github.com/stateracloud/koinon/internal/tokens"
)

func tokenProto(t tokens.Token) *tokenv1alpha1.Token {
	role, _ := roleOf(t.Role)
	out := &tokenv1alpha1.Token{
		Id: t.ID, Name: t.Name, Prefix: t.Prefix, Role: auth.RoleToProto(role),
		CreatedAt: timestamppb.New(t.CreatedAt), ExpiresAt: timestamppb.New(t.ExpiresAt),
	}
	if t.RevokedAt != nil {
		out.RevokedAt = timestamppb.New(*t.RevokedAt)
	}
	if t.LastUsedAt != nil {
		out.LastUsedAt = timestamppb.New(*t.LastUsedAt)
	}
	return out
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
	created, err := s.tokens.Create(requestCtx(ctx), c.org, c.auditActor(), c.p.UserID, c.role(), req.GetName(), role, req.GetExpiresIn().AsDuration())
	if err != nil {
		return nil, s.tokenError(err)
	}
	s.log.Info("admin API token created", "org", c.org.String(), "token", created.ID, "prefix", created.Prefix, "role", created.Role, "by", c.actor().String())
	return &tokenv1alpha1.CreateTokenResponse{Token: tokenProto(created.Token), Secret: created.Secret}, nil
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
	for _, t := range list {
		res.Tokens = append(res.Tokens, tokenProto(t))
	}
	return res, nil
}

// RevokeToken implements TokenService. Admins and owners.
func (s *Server) RevokeToken(ctx context.Context, req *tokenv1alpha1.RevokeTokenRequest) (*tokenv1alpha1.RevokeTokenResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	t, err := s.tokens.Revoke(requestCtx(ctx), c.org, c.auditActor(), req.GetTokenId())
	if err != nil {
		return nil, s.tokenError(err)
	}
	return &tokenv1alpha1.RevokeTokenResponse{Token: tokenProto(t)}, nil
}
