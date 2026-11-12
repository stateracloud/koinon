// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"

	connect "connectrpc.com/connect/v2"
	"github.com/jackc/pgx/v5"

	"github.com/stateracloud/koinon/internal/auth/ratelimit"
	"github.com/stateracloud/koinon/internal/db/store"
	authv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/auth/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/auth/v1alpha1/authv1alpha1connect"
)

// allowedDuringPasswordChange lists the procedures a principal may call
// before replacing the one-time bootstrap password.
var allowedDuringPasswordChange = map[string]bool{
	authv1alpha1connect.AuthServiceWhoAmIProcedure:         true,
	authv1alpha1connect.AuthServiceChangePasswordProcedure: true,
}

// Interceptor authenticates every RPC from the session cookie before any
// payload is read, and puts the Principal in the context. No RPC is public.
func (s *Service) Interceptor() connect.ServerInterceptor {
	return func(next connect.ServerFunc) connect.ServerFunc {
		return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
			var token string
			if info, ok := connect.CallInfoForServerContext(ctx); ok {
				token = sessionTokenFromCookies(info.RequestHeader().Values("Cookie"), s.SessionCookie())
			}
			p, err := s.Authenticate(ctx, token)
			if errors.Is(err, ErrUnauthenticated) {
				return connect.NewError(connect.CodeUnauthenticated, "login required")
			}
			if err != nil {
				s.log.Error("authenticating request failed", "procedure", spec.Procedure, "err", err)
				return connect.NewError(connect.CodeInternal, "internal error")
			}
			if p.PasswordChangeRequired && !allowedDuringPasswordChange[spec.Procedure] {
				return connect.NewError(connect.CodePermissionDenied, "password change required")
			}
			return next(WithPrincipal(ctx, p), spec, stream)
		}
	}
}

func sessionTokenFromCookies(lines []string, name string) string {
	for _, line := range lines {
		cookies, err := http.ParseCookie(line)
		if err != nil {
			continue
		}
		for _, c := range cookies {
			if c.Name == name {
				return c.Value
			}
		}
	}
	return ""
}

// Register registers the AuthService on server.
func (s *Service) Register(server *connect.Server) {
	authv1alpha1connect.RegisterAuthServiceHandler(server, rpcHandler{s})
}

type rpcHandler struct{ s *Service }

func (h rpcHandler) WhoAmI(ctx context.Context, _ *authv1alpha1.WhoAmIRequest) (*authv1alpha1.WhoAmIResponse, error) {
	p, ok := PrincipalFrom(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, "login required")
	}
	res := &authv1alpha1.WhoAmIResponse{
		User: &authv1alpha1.User{
			Id:            p.UserID.String(),
			Email:         p.Email,
			DisplayName:   p.DisplayName,
			PersonalOrgId: p.PersonalOrgID.String(),
		},
		PlatformAdmin:          p.PlatformAdmin,
		AuthMethod:             authMethodToProto(p.AuthMethod),
		PasswordChangeRequired: p.PasswordChangeRequired,
	}
	for _, m := range p.Memberships {
		res.Memberships = append(res.Memberships, &authv1alpha1.Membership{
			OrgId:          m.OrgID.String(),
			OrgSlug:        m.OrgSlug,
			OrgDisplayName: m.OrgDisplayName,
			Role:           roleToProto(m.Role),
		})
	}
	return res, nil
}

func (h rpcHandler) ChangePassword(ctx context.Context, req *authv1alpha1.ChangePasswordRequest) (*authv1alpha1.ChangePasswordResponse, error) {
	s := h.s
	p, ok := PrincipalFrom(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, "login required")
	}
	if p.AuthMethod != store.AuthMethodPassword {
		return nil, connect.NewError(connect.CodeFailedPrecondition, "this account has no local password")
	}
	if err := ValidateNewPassword(req.GetNewPassword()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	if req.GetNewPassword() == req.GetCurrentPassword() {
		return nil, connect.NewError(connect.CodeInvalidArgument, "the new password must differ from the current one")
	}
	cred, err := s.q.GetLocalCredential(ctx, p.UserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, "this account has no local password")
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, "internal error").WithCause(err)
	}
	// Guessing the current password is limited per user, before the hash runs. It writes no login
	// audit event: the caller is signed in.
	limitKey := ratelimit.Key(hex.EncodeToString(p.UserID.Bytes[:]), "change-password")
	if wait := s.limiter.Blocked(limitKey, s.now()); wait > 0 {
		return nil, connect.NewError(connect.CodeResourceExhausted, "too many failed attempts; retry in "+ratelimit.RetryAfter(wait)+" seconds")
	}
	if _, ok, err := s.checkPassword(ctx, cred.Username, req.GetCurrentPassword()); errors.Is(err, errBusy) {
		return nil, connect.NewError(connect.CodeResourceExhausted, "too many password checks; retry shortly")
	} else if err != nil {
		return nil, connect.NewError(connect.CodeInternal, "internal error").WithCause(err)
	} else if !ok {
		s.limiter.Fail(limitKey, s.now())
		return nil, connect.NewError(connect.CodePermissionDenied, "current password is incorrect")
	}
	s.limiter.Succeed(limitKey)
	hash, err := HashPasswordContext(ctx, req.GetNewPassword())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, "internal error").WithCause(err)
	}
	if s.afterPasswordCheck != nil {
		s.afterPasswordCheck()
	}
	now := ts(s.now())
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		// Lock the credential first (the order ResetPassword uses), then refuse when the hash is no
		// longer the one the old password was checked against: an operator reset in between wins.
		cur, err := q.LockLocalCredentialUpdate(ctx, p.UserID)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && cur.PasswordHash != cred.PasswordHash) {
			return errCredentialChanged
		}
		if err != nil {
			return err
		}
		if err := q.SetLocalPassword(ctx, store.SetLocalPasswordParams{
			UserID: p.UserID, PasswordHash: hash, MustChangePassword: false, PasswordChangedAt: now,
		}); err != nil {
			return err
		}
		return q.RevokeOtherUserSessions(ctx, store.RevokeOtherUserSessionsParams{
			Now: now, UserID: p.UserID, KeepSessionID: p.SessionID,
		})
	})
	if errors.Is(err, errCredentialChanged) {
		return nil, connect.NewError(connect.CodeAborted, "the password changed while this request ran; sign in again")
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, "internal error").WithCause(err)
	}
	s.log.Info("password changed", "user", p.UserID.String())
	return &authv1alpha1.ChangePasswordResponse{}, nil
}

func authMethodToProto(m store.AuthMethod) authv1alpha1.AuthMethod {
	switch m {
	case store.AuthMethodPassword:
		return authv1alpha1.AuthMethod_AUTH_METHOD_PASSWORD
	case store.AuthMethodOidc:
		return authv1alpha1.AuthMethod_AUTH_METHOD_OIDC
	default:
		return authv1alpha1.AuthMethod_AUTH_METHOD_UNSPECIFIED
	}
}
