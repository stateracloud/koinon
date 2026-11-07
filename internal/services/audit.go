// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"

	connect "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/auth"
	"github.com/stateracloud/koinon/internal/db/store"
	auditv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/audit/v1alpha1"
)

// auditEventProto converts an event. A summary that is not an object (the table forbids it) is left out.
func auditEventProto(e store.AuditEvent) *auditv1alpha1.AuditEvent {
	out := &auditv1alpha1.AuditEvent{
		Id: e.ID.String(), OrgId: e.OrgID.String(), ActorKind: e.ActorKind, ActorId: e.ActorID,
		Action: e.Action, TargetKind: e.TargetKind, TargetId: e.TargetID, RequestId: e.RequestID,
		CreatedAt: timestamppb.New(e.CreatedAt.Time), ChainSeq: e.ChainSeq,
		PrevHash: hex.EncodeToString(e.PrevHash), Hash: hex.EncodeToString(e.Hash),
	}
	var m map[string]any
	if json.Unmarshal(e.Summary, &m) == nil {
		out.Summary, _ = structpb.NewStruct(m)
	}
	return out
}

// installEventProto converts an install-chain event; it has no org.
func installEventProto(e store.InstallAuditEvent) *auditv1alpha1.AuditEvent {
	out := &auditv1alpha1.AuditEvent{
		Id: e.ID.String(), ActorKind: e.ActorKind, ActorId: e.ActorID,
		Action: e.Action, TargetKind: e.TargetKind, TargetId: e.TargetID, RequestId: e.RequestID,
		CreatedAt: timestamppb.New(e.CreatedAt.Time), ChainSeq: e.ChainSeq,
		PrevHash: hex.EncodeToString(e.PrevHash), Hash: hex.EncodeToString(e.Hash),
	}
	var m map[string]any
	if json.Unmarshal(e.Summary, &m) == nil {
		out.Summary, _ = structpb.NewStruct(m)
	}
	return out
}

// List implements AuditService: an admin or owner reads their org's log, a platform admin any
// org's or every org's, and with scope AUDIT_SCOPE_INSTALL the install chain (platform admins only).
func (s *Server) List(ctx context.Context, req *auditv1alpha1.ListRequest) (*auditv1alpha1.ListResponse, error) {
	f := audit.Filter{
		ActionPrefix: req.GetActionPrefix(), ActorKind: req.GetActorKind(), ActorID: req.GetActorId(),
		PageSize: int(req.GetPageSize()), PageToken: req.GetPageToken(),
	}
	if req.GetSince() != nil {
		f.Since = req.GetSince().AsTime()
	}
	if req.GetUntil() != nil {
		f.Until = req.GetUntil().AsTime()
	}
	if req.GetScope() == auditv1alpha1.AuditScope_AUDIT_SCOPE_INSTALL {
		p, ok := auth.PrincipalFrom(ctx)
		if !ok {
			return nil, connect.NewError(connect.CodeUnauthenticated, "login required")
		}
		if !p.PlatformAdmin {
			return nil, connect.NewError(connect.CodePermissionDenied, "only a platform admin reads the install audit log")
		}
		if req.GetOrgId() != "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, "org_id must be empty for the install scope")
		}
		rows, next, err := audit.ListInstall(ctx, s.q, f)
		if errors.Is(err, audit.ErrBadFilter) {
			return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
		}
		if err != nil {
			return nil, s.dbError(err, "install audit events")
		}
		res := &auditv1alpha1.ListResponse{NextPageToken: next}
		for _, e := range rows {
			res.Events = append(res.Events, installEventProto(e))
		}
		return res, nil
	}
	if req.GetOrgId() == "" {
		p, ok := auth.PrincipalFrom(ctx)
		if !ok {
			return nil, connect.NewError(connect.CodeUnauthenticated, "login required")
		}
		if !p.PlatformAdmin {
			return nil, connect.NewError(connect.CodePermissionDenied, "org_id is required; only a platform admin reads every org")
		}
		f.AllOrgs = true
	} else {
		c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
		if err != nil {
			return nil, err
		}
		f.Org = c.org
	}
	rows, next, err := audit.List(ctx, s.q, f)
	if errors.Is(err, audit.ErrBadFilter) {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	if err != nil {
		return nil, s.dbError(err, "audit events")
	}
	res := &auditv1alpha1.ListResponse{NextPageToken: next}
	for _, e := range rows {
		res.Events = append(res.Events, auditEventProto(e))
	}
	return res, nil
}
