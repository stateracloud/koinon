// SPDX-License-Identifier: Apache-2.0

package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
)

// WithRestoreStatus sets the report behind GET /restore/status (ADR 0110 decision 7); koinon-api passes
// restorestatus.Build with this install's mark and the importer's plan.
func (s *Server) WithRestoreStatus(f func(ctx context.Context) (any, error)) *Server {
	s.restoreStatus = f
	return s
}

// GetRestoreStatus implements StrictServerInterface. It needs a token whose root user is a platform admin,
// checked on every request, the rule of the install audit chain.
func (s *Server) GetRestoreStatus(ctx context.Context, _ GetRestoreStatusRequestObject) (GetRestoreStatusResponseObject, error) {
	fail := func(err error) (GetRestoreStatusResponseObject, error) {
		st, e := s.problem(err)
		return GetRestoreStatusdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	p, _ := s.actor(ctx)
	denied, err := s.installAuditDenied(ctx, p)
	if err != nil {
		return fail(err)
	}
	if denied != nil {
		denied.Error.Message = "the restore status needs a token whose root user is a platform admin"
		return GetRestoreStatusdefaultJSONResponse{Body: *denied, StatusCode: http.StatusForbidden}, nil
	}
	if s.restoreStatus == nil {
		e := forbidden("")
		e.Error.Code, e.Error.Message = Internal, "restore status is not available on this server"
		return GetRestoreStatusdefaultJSONResponse{Body: *e, StatusCode: http.StatusNotImplemented}, nil
	}
	report, err := s.restoreStatus(ctx)
	if err != nil {
		return fail(err)
	}
	raw, err := json.Marshal(report)
	if err != nil {
		return fail(err)
	}
	var out RestoreStatus
	if err := json.Unmarshal(raw, &out); err != nil {
		return fail(err)
	}
	return GetRestoreStatus200JSONResponse(out), nil
}
