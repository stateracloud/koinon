// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
	leakguardv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/leakguard/v1alpha1"
	"github.com/stateracloud/koinon/internal/leakguard"
	"github.com/stateracloud/koinon/internal/orgsettings"
)

// The LeakGuardService is the UI's face of the org's leak-guard settings (ADR 0084): the same
// validation and audit as the admin API. Every call needs the admin role.

func (s *Server) leakGuardProto(ctx context.Context, org pgtype.UUID) (*leakguardv1alpha1.LeakGuard, error) {
	l, err := s.leakGuard.LeakGuard(ctx, org)
	if err != nil {
		return nil, s.catalogError(err)
	}
	return &leakguardv1alpha1.LeakGuard{
		MatchThreshold: int32(l.MatchThreshold), DefaultMatchThreshold: orgsettings.DefaultMatchThreshold, //nolint:gosec // bounded by the table's CHECK
		MinMatchThreshold: leakguard.MinThreshold, MaxMatchThreshold: leakguard.MaxThreshold,
		TrustedHosts: l.TrustedHosts,
	}, nil
}

// GetLeakGuard implements LeakGuardService. Admins and owners.
func (s *Server) GetLeakGuard(ctx context.Context, req *leakguardv1alpha1.GetLeakGuardRequest) (*leakguardv1alpha1.GetLeakGuardResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	l, err := s.leakGuardProto(ctx, c.org)
	if err != nil {
		return nil, err
	}
	return &leakguardv1alpha1.GetLeakGuardResponse{LeakGuard: l}, nil
}

// SetMatchThreshold implements LeakGuardService. Admins and owners.
func (s *Server) SetMatchThreshold(ctx context.Context, req *leakguardv1alpha1.SetMatchThresholdRequest) (*leakguardv1alpha1.SetMatchThresholdResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	var n *int
	if v := int(req.GetMatchThreshold()); v != 0 {
		n = &v
	}
	if err := s.leakGuard.SetMatchThreshold(requestCtx(ctx), c.org, c.auditActor(), n); err != nil {
		return nil, s.catalogError(err)
	}
	l, err := s.leakGuardProto(ctx, c.org)
	if err != nil {
		return nil, err
	}
	return &leakguardv1alpha1.SetMatchThresholdResponse{LeakGuard: l}, nil
}

// AddTrustedHost implements LeakGuardService. Admins and owners.
func (s *Server) AddTrustedHost(ctx context.Context, req *leakguardv1alpha1.AddTrustedHostRequest) (*leakguardv1alpha1.AddTrustedHostResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	if _, err := s.leakGuard.AddTrustedHost(requestCtx(ctx), c.org, c.auditActor(), req.GetHost()); err != nil {
		return nil, s.catalogError(err)
	}
	l, err := s.leakGuardProto(ctx, c.org)
	if err != nil {
		return nil, err
	}
	return &leakguardv1alpha1.AddTrustedHostResponse{LeakGuard: l}, nil
}

// RemoveTrustedHost implements LeakGuardService. Admins and owners.
func (s *Server) RemoveTrustedHost(ctx context.Context, req *leakguardv1alpha1.RemoveTrustedHostRequest) (*leakguardv1alpha1.RemoveTrustedHostResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	if err := s.leakGuard.RemoveTrustedHost(requestCtx(ctx), c.org, c.auditActor(), req.GetHost()); err != nil {
		return nil, s.catalogError(err)
	}
	l, err := s.leakGuardProto(ctx, c.org)
	if err != nil {
		return nil, err
	}
	return &leakguardv1alpha1.RemoveTrustedHostResponse{LeakGuard: l}, nil
}
