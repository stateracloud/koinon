// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"errors"

	connect "connectrpc.com/connect/v2"
	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/auth"
	"github.com/stateracloud/koinon/internal/db/store"
	outwardv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/outward/v1alpha1"
	"github.com/stateracloud/koinon/internal/outward"
	"github.com/stateracloud/koinon/internal/restorehold"
)

var outwardKindFromProto = map[outwardv1alpha1.OutwardTargetKind]outward.Kind{
	outwardv1alpha1.OutwardTargetKind_OUTWARD_TARGET_KIND_MCP_SERVER:                 outward.MCPServer,
	outwardv1alpha1.OutwardTargetKind_OUTWARD_TARGET_KIND_MODEL_PROVIDER:             outward.ModelProvider,
	outwardv1alpha1.OutwardTargetKind_OUTWARD_TARGET_KIND_CONNECTION:                 outward.CodeHostConnection,
	outwardv1alpha1.OutwardTargetKind_OUTWARD_TARGET_KIND_TRACE_DESTINATION:          outward.TraceDestination,
	outwardv1alpha1.OutwardTargetKind_OUTWARD_TARGET_KIND_PLATFORM_TRACE_DESTINATION: outward.PlatformTraceDestination,
}

var outwardKindToProto = func() map[outward.Kind]outwardv1alpha1.OutwardTargetKind {
	m := map[outward.Kind]outwardv1alpha1.OutwardTargetKind{}
	for p, k := range outwardKindFromProto {
		m[k] = p
	}
	return m
}()

// SetOutwardTargetDisabled implements OutwardService. Org admins switch the org's own targets; the
// platform trace destination needs a platform admin (ADR 0110 decision 5).
func (s *Server) SetOutwardTargetDisabled(ctx context.Context, req *outwardv1alpha1.SetOutwardTargetDisabledRequest) (*outwardv1alpha1.SetOutwardTargetDisabledResponse, error) {
	kind, ok := outwardKindFromProto[req.GetKind()]
	if !ok {
		return nil, connect.NewError(connect.CodeInvalidArgument, "kind is required")
	}
	var (
		org   pgtype.UUID
		actor audit.Actor
	)
	if kind.Platform() {
		p, ok := auth.PrincipalFrom(ctx)
		if !ok {
			return nil, connect.NewError(connect.CodeUnauthenticated, "login required")
		}
		if !p.PlatformAdmin {
			return nil, withReason(connect.NewError(connect.CodePermissionDenied, "only a platform admin can do this"), "PLATFORM_ADMIN_REQUIRED")
		}
		actor = audit.User(p.UserID)
	} else {
		c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
		if err != nil {
			return nil, err
		}
		org, actor = c.org, c.auditActor()
	}
	var id pgtype.UUID
	if !kind.Platform() {
		var err error
		if id, err = parseID("target_id", req.GetTargetId()); err != nil {
			return nil, err
		}
	}
	st, err := s.outward.SetDisabled(requestCtx(ctx), org, actor, kind, id, req.GetDisabled(), req.GetNote())
	if err != nil {
		return nil, s.outwardError(err)
	}
	out := &outwardv1alpha1.OutwardTargetState{
		Kind: outwardKindToProto[kind], TargetId: id.String(), Name: st.Name, Disabled: st.Disabled, DisabledBy: st.By, Note: st.Note,
	}
	if kind.Platform() {
		out.TargetId = "platform"
	}
	if st.Disabled {
		out.DisabledAt = tsProto(pgtype.Timestamptz{Time: st.At, Valid: true})
	}
	return &outwardv1alpha1.SetOutwardTargetDisabledResponse{State: out}, nil
}

func (s *Server) outwardError(err error) error {
	var inv *outward.InvalidError
	switch {
	case errors.As(err, &inv):
		return connect.NewError(connect.CodeInvalidArgument, inv.Msg)
	case errors.Is(err, outward.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, "target not found")
	case errors.Is(err, outward.ErrDeclared):
		return connect.NewError(connect.CodeFailedPrecondition, err.Error())
	case errors.Is(err, restorehold.ErrHeld):
		return connect.NewError(connect.CodeFailedPrecondition, err.Error())
	}
	return s.dbError(err, "outward target")
}

// adminDisabledProto is the switch as a Get or List shows it: nil for an enabled target.
func adminDisabledProto(d *outward.Disabled) *outwardv1alpha1.AdminDisabled {
	if d == nil {
		return nil
	}
	return &outwardv1alpha1.AdminDisabled{At: timestamppb.New(d.At), By: d.By, Note: d.Note, Declared: d.Declared}
}
