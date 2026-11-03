// SPDX-License-Identifier: Apache-2.0

package adminapi

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/leakguard"
	"github.com/stateracloud/koinon/internal/orgsettings"
)

func (s *Server) leakGuardView(ctx context.Context, org pgtype.UUID) (LeakGuard, error) {
	l, err := s.leakGuard.LeakGuard(ctx, org)
	if err != nil {
		return LeakGuard{}, err
	}
	return LeakGuard{
		MatchThreshold: l.MatchThreshold, DefaultMatchThreshold: orgsettings.DefaultMatchThreshold,
		MinMatchThreshold: leakguard.MinThreshold, MaxMatchThreshold: leakguard.MaxThreshold,
		TrustedHosts: l.TrustedHosts,
	}, nil
}

// GetLeakGuard implements StrictServerInterface.
func (s *Server) GetLeakGuard(ctx context.Context, _ GetLeakGuardRequestObject) (GetLeakGuardResponseObject, error) {
	p, _ := s.actor(ctx)
	v, err := s.leakGuardView(ctx, p.Org)
	if err != nil {
		st, e := s.problem(err)
		return GetLeakGuarddefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	return GetLeakGuard200JSONResponse(v), nil
}

// SetLeakGuard implements StrictServerInterface.
func (s *Server) SetLeakGuard(ctx context.Context, req SetLeakGuardRequestObject) (SetLeakGuardResponseObject, error) {
	p, actor := s.actor(ctx)
	err := s.leakGuard.SetMatchThreshold(ctx, p.Org, actor, req.Body.MatchThreshold)
	if err == nil {
		var v LeakGuard
		if v, err = s.leakGuardView(ctx, p.Org); err == nil {
			return SetLeakGuard200JSONResponse(v), nil
		}
	}
	st, e := s.problem(err)
	return SetLeakGuarddefaultJSONResponse{Body: e, StatusCode: st}, nil
}

// AddFetchTrustedHost implements StrictServerInterface.
func (s *Server) AddFetchTrustedHost(ctx context.Context, req AddFetchTrustedHostRequestObject) (AddFetchTrustedHostResponseObject, error) {
	p, actor := s.actor(ctx)
	_, err := s.leakGuard.AddTrustedHost(ctx, p.Org, actor, req.Body.Host)
	if err == nil {
		var v LeakGuard
		if v, err = s.leakGuardView(ctx, p.Org); err == nil {
			return AddFetchTrustedHost200JSONResponse(v), nil
		}
	}
	st, e := s.problem(err)
	return AddFetchTrustedHostdefaultJSONResponse{Body: e, StatusCode: st}, nil
}

// RemoveFetchTrustedHost implements StrictServerInterface.
func (s *Server) RemoveFetchTrustedHost(ctx context.Context, req RemoveFetchTrustedHostRequestObject) (RemoveFetchTrustedHostResponseObject, error) {
	p, actor := s.actor(ctx)
	err := s.leakGuard.RemoveTrustedHost(ctx, p.Org, actor, req.Host)
	if err == nil {
		var v LeakGuard
		if v, err = s.leakGuardView(ctx, p.Org); err == nil {
			return RemoveFetchTrustedHost200JSONResponse(v), nil
		}
	}
	st, e := s.problem(err)
	return RemoveFetchTrustedHostdefaultJSONResponse{Body: e, StatusCode: st}, nil
}
