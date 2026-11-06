// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
	orgsettingsv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/orgsettings/v1alpha1"
	"github.com/stateracloud/koinon/internal/orgsettings"
)

// The OrgSettingsService is the UI's face of the org's run-content retention (issue #69): the same
// validation and audit as the admin API. Every call needs the admin role.

func (s *Server) runContentRetentionProto(ctx context.Context, org pgtype.UUID) (*orgsettingsv1alpha1.RunContentRetention, error) {
	days, err := s.leakGuard.RunContentRetentionDays(ctx, org)
	if err != nil {
		return nil, s.catalogError(err)
	}
	return &orgsettingsv1alpha1.RunContentRetention{
		Days: int32(days), DefaultDays: orgsettings.DefaultRunContentRetentionDays, //nolint:gosec // bounded by the table's CHECK
		MinDays: orgsettings.MinRunContentRetentionDays, MaxDays: orgsettings.MaxRunContentRetentionDays,
	}, nil
}

// GetRunContentRetention implements OrgSettingsService. Admins and owners.
func (s *Server) GetRunContentRetention(ctx context.Context, req *orgsettingsv1alpha1.GetRunContentRetentionRequest) (*orgsettingsv1alpha1.GetRunContentRetentionResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	r, err := s.runContentRetentionProto(ctx, c.org)
	if err != nil {
		return nil, err
	}
	return &orgsettingsv1alpha1.GetRunContentRetentionResponse{Retention: r}, nil
}

// SetRunContentRetention implements OrgSettingsService. Admins and owners.
func (s *Server) SetRunContentRetention(ctx context.Context, req *orgsettingsv1alpha1.SetRunContentRetentionRequest) (*orgsettingsv1alpha1.SetRunContentRetentionResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	var n *int
	if v := int(req.GetDays()); v != 0 {
		n = &v
	}
	if err := s.leakGuard.SetRunContentRetention(requestCtx(ctx), c.org, c.auditActor(), n); err != nil {
		return nil, s.catalogError(err)
	}
	r, err := s.runContentRetentionProto(ctx, c.org)
	if err != nil {
		return nil, err
	}
	return &orgsettingsv1alpha1.SetRunContentRetentionResponse{Retention: r}, nil
}
