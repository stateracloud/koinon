// SPDX-License-Identifier: Apache-2.0

package adminapi

import (
	"context"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/stateracloud/koinon/internal/outward"
)

// WithOutward sets the service behind the outward switch (ADR 0110 decision 5); koinon-api passes the one
// that knows the restore hold.
func (s *Server) WithOutward(o *outward.Service) *Server {
	s.outward = o
	return s
}

var adminOutwardKinds = map[OutwardTargetKind]outward.Kind{
	OutwardTargetKindMcpServer:        outward.MCPServer,
	OutwardTargetKindModelProvider:    outward.ModelProvider,
	OutwardTargetKindConnection:       outward.CodeHostConnection,
	OutwardTargetKindTraceDestination: outward.TraceDestination,
}

// SetOutwardTargetDisabled implements StrictServerInterface. The token's org owns the target; the platform
// trace destination is not reachable here (it needs a platform admin signed in to the UI API).
func (s *Server) SetOutwardTargetDisabled(ctx context.Context, req SetOutwardTargetDisabledRequestObject) (SetOutwardTargetDisabledResponseObject, error) {
	kind, ok := adminOutwardKinds[req.Kind]
	if !ok {
		st, e := s.problem(&outward.InvalidError{Msg: "unknown outward target kind"})
		return SetOutwardTargetDisableddefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	p, actor := s.actor(ctx)
	note := ""
	if req.Body.Note != nil {
		note = *req.Body.Note
	}
	got, err := s.outward.SetDisabled(ctx, p.Org, actor, kind, pgUUID(uid(req.TargetId)), req.Body.Disabled, note)
	if err != nil {
		st, e := s.problem(err)
		return SetOutwardTargetDisableddefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	out := SetOutwardTargetDisabled200JSONResponse{
		Kind: req.Kind, TargetId: openapi_types.UUID(req.TargetId), Name: got.Name, Disabled: got.Disabled,
	}
	if got.Disabled {
		at := got.At
		out.DisabledAt = &at
		if got.By != "" {
			out.DisabledBy = &got.By
		}
		if got.Note != "" {
			out.Note = &got.Note
		}
	}
	return out, nil
}
