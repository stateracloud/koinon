// SPDX-License-Identifier: Apache-2.0

package adminapi

import (
	"context"
	"encoding/hex"
	"encoding/json"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
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
	prm := req.Params
	f := audit.Filter{Org: p.Org}
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
