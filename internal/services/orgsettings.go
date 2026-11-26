// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
	orgsettingsv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/orgsettings/v1alpha1"
	"github.com/stateracloud/koinon/internal/orgsettings"
)

// The OrgSettingsService is the UI's face of the org's run-content retention (issue #69) and home
// backup retention (#219): the same validation and audit as the admin API. Every call needs the
// admin role.

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

// The home backup retention (#219): how long an old home backup repository is kept after a key
// change.

func (s *Server) homeBackupRetentionProto(ctx context.Context, org pgtype.UUID) (*orgsettingsv1alpha1.HomeBackupRetention, error) {
	days, err := s.leakGuard.HomeBackupRetentionDays(ctx, org)
	if err != nil {
		return nil, s.catalogError(err)
	}
	return &orgsettingsv1alpha1.HomeBackupRetention{
		Days: int32(days), DefaultDays: orgsettings.DefaultHomeBackupRetentionDays, //nolint:gosec // bounded by the table's CHECK
		MinDays: orgsettings.MinHomeBackupRetentionDays, MaxDays: orgsettings.MaxHomeBackupRetentionDays,
	}, nil
}

// GetHomeBackupRetention implements OrgSettingsService. Admins and owners.
func (s *Server) GetHomeBackupRetention(ctx context.Context, req *orgsettingsv1alpha1.GetHomeBackupRetentionRequest) (*orgsettingsv1alpha1.GetHomeBackupRetentionResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	r, err := s.homeBackupRetentionProto(ctx, c.org)
	if err != nil {
		return nil, err
	}
	return &orgsettingsv1alpha1.GetHomeBackupRetentionResponse{Retention: r}, nil
}

// SetHomeBackupRetention implements OrgSettingsService. Admins and owners.
func (s *Server) SetHomeBackupRetention(ctx context.Context, req *orgsettingsv1alpha1.SetHomeBackupRetentionRequest) (*orgsettingsv1alpha1.SetHomeBackupRetentionResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	var n *int
	if v := int(req.GetDays()); v != 0 {
		n = &v
	}
	if err := s.leakGuard.SetHomeBackupRetention(requestCtx(ctx), c.org, c.auditActor(), n); err != nil {
		return nil, s.catalogError(err)
	}
	r, err := s.homeBackupRetentionProto(ctx, c.org)
	if err != nil {
		return nil, err
	}
	return &orgsettingsv1alpha1.SetHomeBackupRetentionResponse{Retention: r}, nil
}

// The rule "the approver must not be the requester" (#1175, ADR 0005 amendment 2026-11-23).

// GetApproverNotRequester implements OrgSettingsService. Admins and owners.
func (s *Server) GetApproverNotRequester(ctx context.Context, req *orgsettingsv1alpha1.GetApproverNotRequesterRequest) (*orgsettingsv1alpha1.GetApproverNotRequesterResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	on, err := s.leakGuard.ApproverNotRequester(ctx, c.org)
	if err != nil {
		return nil, s.catalogError(err)
	}
	return &orgsettingsv1alpha1.GetApproverNotRequesterResponse{On: on}, nil
}

// SetApproverNotRequester implements OrgSettingsService. Admins and owners.
func (s *Server) SetApproverNotRequester(ctx context.Context, req *orgsettingsv1alpha1.SetApproverNotRequesterRequest) (*orgsettingsv1alpha1.SetApproverNotRequesterResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	if err := s.leakGuard.SetApproverNotRequester(requestCtx(ctx), c.org, c.auditActor(), req.GetOn()); err != nil {
		return nil, s.catalogError(err)
	}
	on, err := s.leakGuard.ApproverNotRequester(ctx, c.org)
	if err != nil {
		return nil, s.catalogError(err)
	}
	return &orgsettingsv1alpha1.SetApproverNotRequesterResponse{On: on}, nil
}

// The org's run limits (issue #492, ADR 0066): the most runs at once, the trigger share of the queue
// and the sandbox creation rate.

func runLimitProto(l orgsettings.Limit) *orgsettingsv1alpha1.RunLimit {
	//nolint:gosec // bounded by the table's CHECK and the constants in orgsettings
	return &orgsettingsv1alpha1.RunLimit{Value: int32(l.Value), DefaultValue: int32(l.Default), Min: int32(l.Min), Max: int32(l.Max)}
}

func (s *Server) runLimitsProto(ctx context.Context, org pgtype.UUID) (*orgsettingsv1alpha1.RunLimits, error) {
	l, err := s.leakGuard.RunLimits(ctx, org)
	if err != nil {
		return nil, s.catalogError(err)
	}
	v := l.View()
	return &orgsettingsv1alpha1.RunLimits{
		MaxConcurrentRuns: runLimitProto(v.MaxConcurrentRuns), TriggerConcurrency: runLimitProto(v.TriggerConcurrency),
		TriggerRatePerMinute: runLimitProto(v.TriggerRatePerMinute), SandboxCreationsPerMinute: runLimitProto(v.SandboxCreationsPerMinute),
	}, nil
}

// GetRunLimits implements OrgSettingsService. Admins and owners.
func (s *Server) GetRunLimits(ctx context.Context, req *orgsettingsv1alpha1.GetRunLimitsRequest) (*orgsettingsv1alpha1.GetRunLimitsResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	l, err := s.runLimitsProto(ctx, c.org)
	if err != nil {
		return nil, err
	}
	return &orgsettingsv1alpha1.GetRunLimitsResponse{Limits: l}, nil
}

// optionalInt is the patch field of an optional proto field: nil when it is not set.
func optionalInt(v *int32) *int {
	if v == nil {
		return nil
	}
	n := int(*v)
	return &n
}

// SetRunLimits implements OrgSettingsService. Admins and owners. A limit that is not set stays; 0
// returns it to the default.
func (s *Server) SetRunLimits(ctx context.Context, req *orgsettingsv1alpha1.SetRunLimitsRequest) (*orgsettingsv1alpha1.SetRunLimitsResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	p := orgsettings.RunLimitsPatch{
		MaxConcurrentRuns: optionalInt(req.MaxConcurrentRuns), TriggerConcurrency: optionalInt(req.TriggerConcurrency),
		TriggerRatePerMinute: optionalInt(req.TriggerRatePerMinute), SandboxCreationsPerMinute: optionalInt(req.SandboxCreationsPerMinute),
	}
	if err := s.leakGuard.SetRunLimits(requestCtx(ctx), c.org, c.auditActor(), p); err != nil {
		return nil, s.catalogError(err)
	}
	l, err := s.runLimitsProto(ctx, c.org)
	if err != nil {
		return nil, err
	}
	return &orgsettingsv1alpha1.SetRunLimitsResponse{Limits: l}, nil
}
