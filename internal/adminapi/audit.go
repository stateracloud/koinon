// SPDX-License-Identifier: Apache-2.0

package adminapi

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/tokens"
)

// Audit log (ADR 0023). A token reads its own org's events and nothing else: the org is the
// token's, there is no parameter to name another.

func auditEvent(e store.AuditEvent) AuditEvent {
	out := AuditEvent{
		Id: openapi_types.UUID(e.ID.Bytes), ActorKind: AuditEventActorKind(e.ActorKind), ActorId: e.ActorID,
		Action: e.Action, TargetKind: e.TargetKind, TargetId: e.TargetID, RequestId: e.RequestID,
		CreatedAt: e.CreatedAt.Time, ChainSeq: e.ChainSeq,
		PrevHash: hex.EncodeToString(e.PrevHash), Hash: hex.EncodeToString(e.Hash),
		Summary: map[string]any{},
	}
	_ = json.Unmarshal(e.Summary, &out.Summary)
	return out
}

// ListAuditEvents implements StrictServerInterface.
func (s *Server) ListAuditEvents(ctx context.Context, req ListAuditEventsRequestObject) (ListAuditEventsResponseObject, error) {
	p, _ := s.actor(ctx)
	if sc := req.Params.Scope; sc != nil && !sc.Valid() {
		st, e := s.problem(fmt.Errorf("%w: scope must be org or install", audit.ErrBadFilter))
		return ListAuditEventsdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	if req.Params.Scope != nil && *req.Params.Scope == ListAuditEventsParamsScopeInstall {
		return s.listInstallAuditEvents(ctx, p, req.Params)
	}
	if !p.Org.Valid {
		return ListAuditEventsdefaultJSONResponse{Body: Error{Error: struct {
			Code    ErrorCode `json:"code"`
			Message string    `json:"message"`
		}{PermissionDenied, "this token has no org"}}, StatusCode: http.StatusForbidden}, nil
	}
	f := auditFilter(req.Params)
	f.Org = p.Org
	rows, next, err := audit.List(ctx, store.New(s.pool), f)
	if err != nil {
		st, e := s.problem(err)
		return ListAuditEventsdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	out := ListAuditEvents200JSONResponse{Events: []AuditEvent{}}
	for _, r := range rows {
		out.Events = append(out.Events, auditEvent(r))
	}
	if next != "" {
		out.NextPageToken = &next
	}
	return out, nil
}

// auditFilter maps the query parameters to a filter.
func auditFilter(prm ListAuditEventsParams) audit.Filter {
	var f audit.Filter
	if prm.Since != nil {
		f.Since = *prm.Since
	}
	if prm.Until != nil {
		f.Until = *prm.Until
	}
	if prm.ActionPrefix != nil {
		f.ActionPrefix = *prm.ActionPrefix
	}
	if prm.ActorKind != nil {
		f.ActorKind = string(*prm.ActorKind)
	}
	if prm.ActorId != nil {
		f.ActorID = *prm.ActorId
	}
	if prm.PageSize != nil {
		f.PageSize = *prm.PageSize
	}
	if prm.PageToken != nil {
		f.PageToken = *prm.PageToken
	}
	return f
}

// installAuditDenied is the install chain's rule for reading and exporting: the token's root user must
// be a platform admin, checked on every request, so demoting the admin takes the access away at once. It
// returns the 403 body when the token may not, and an error when the check itself failed.
func (s *Server) installAuditDenied(ctx context.Context, p tokens.Principal) (*Error, error) {
	admin, err := store.New(s.pool).IsPlatformAdmin(ctx, p.CreatedBy)
	if err != nil {
		return nil, err
	}
	if admin {
		return nil, nil
	}
	return forbidden("the install audit log needs a token whose root user is a platform admin"), nil
}

// forbidden is the 403 body.
func forbidden(msg string) *Error {
	return &Error{Error: struct {
		Code    ErrorCode `json:"code"`
		Message string    `json:"message"`
	}{PermissionDenied, msg}}
}

// listInstallAuditEvents reads the install chain (ADR 0100). The check is on the token's root
// user at every request, not cached: demoting the admin takes the access away at once.
func (s *Server) listInstallAuditEvents(ctx context.Context, p tokens.Principal, prm ListAuditEventsParams) (ListAuditEventsResponseObject, error) {
	if denied, err := s.installAuditDenied(ctx, p); denied != nil || err != nil {
		if err != nil {
			st, e := s.problem(err)
			return ListAuditEventsdefaultJSONResponse{Body: e, StatusCode: st}, nil
		}
		return ListAuditEventsdefaultJSONResponse{Body: *denied, StatusCode: http.StatusForbidden}, nil
	}
	rows, next, err := audit.ListInstall(ctx, store.New(s.pool), auditFilter(prm))
	if err != nil {
		st, e := s.problem(err)
		return ListAuditEventsdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	out := ListAuditEvents200JSONResponse{Events: []AuditEvent{}}
	for _, r := range rows {
		out.Events = append(out.Events, auditEvent(store.AuditEvent{
			ID: r.ID, ActorKind: r.ActorKind, ActorID: r.ActorID, Action: r.Action, TargetKind: r.TargetKind,
			TargetID: r.TargetID, Summary: r.Summary, RequestID: r.RequestID, CreatedAt: r.CreatedAt,
			ChainSeq: r.ChainSeq, PrevHash: r.PrevHash, Hash: r.Hash,
		}))
	}
	if next != "" {
		out.NextPageToken = &next
	}
	return out, nil
}
