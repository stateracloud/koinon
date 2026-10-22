// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	connect "connectrpc.com/connect/v2"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/procoduck/koinon/internal/approvals"
	"github.com/procoduck/koinon/internal/db/store"
	approvalv1alpha1 "github.com/procoduck/koinon/internal/gen/koinon/approval/v1alpha1"
	"github.com/procoduck/koinon/internal/githubtools"
)

// maxNoteBytes bounds an approver's note.
const maxNoteBytes = 2000

var approvalStatusToProto = map[store.ApprovalStatus]approvalv1alpha1.Status{
	store.ApprovalStatusPending:  approvalv1alpha1.Status_STATUS_PENDING,
	store.ApprovalStatusApproved: approvalv1alpha1.Status_STATUS_APPROVED,
	store.ApprovalStatusDenied:   approvalv1alpha1.Status_STATUS_DENIED,
	store.ApprovalStatusExecuted: approvalv1alpha1.Status_STATUS_EXECUTED,
	store.ApprovalStatusFailed:   approvalv1alpha1.Status_STATUS_FAILED,
	store.ApprovalStatusExpired:  approvalv1alpha1.Status_STATUS_EXPIRED,
	store.ApprovalStatusCanceled: approvalv1alpha1.Status_STATUS_CANCELED,
}

var approvalDecisionToProto = map[store.ApprovalDecision]approvalv1alpha1.Decision{
	store.ApprovalDecisionAsk:   approvalv1alpha1.Decision_DECISION_ASK,
	store.ApprovalDecisionAllow: approvalv1alpha1.Decision_DECISION_ALLOW,
}

// approvalProto converts a request. canDecide is whether the caller is an approver; detail
// adds the decoded call.
func approvalProto(a store.ApprovalRequest, canDecide, detail bool) *approvalv1alpha1.Approval {
	out := &approvalv1alpha1.Approval{
		Id: idString(a.ID), OrgId: idString(a.OrgID), TicketId: idString(a.TicketID), RunId: idString(a.RunID),
		AgentId: idString(a.AgentID), Connector: a.Connector, Tool: a.Tool, Target: a.Target,
		ArgsJson: a.ArgsJson, ArgsSha256: a.ArgsSha256,
		Decision: approvalDecisionToProto[a.Decision], Status: approvalStatusToProto[a.Status],
		DecidedBy: idString(a.DecidedBy), DecidedAt: tsProto(a.DecidedAt), DecisionNote: a.DecisionNote.String,
		Error: a.Error.String, Executions: int64(a.Executions),
		ExpiresAt: tsProto(a.ExpiresAt), CreatedAt: tsProto(a.CreatedAt), ExecutedAt: tsProto(a.ExecutedAt),
		CanDecide: canDecide && a.Status == store.ApprovalStatusPending,
	}
	if len(a.Result) > 0 {
		out.Result = payloadProto(a.Result)
	}
	var risk githubtools.Risk
	if json.Unmarshal(a.Risk, &risk) == nil {
		out.Risk = &approvalv1alpha1.Risk{
			Files: int64(risk.Files), Bytes: int64(risk.Bytes), Deletions: risk.Deletions, Ci: risk.CI,
			Scripts: risk.Scripts, Dependencies: risk.Dependencies, Binary: risk.Binary,
		}
	}
	if !detail {
		return out
	}
	if call, err := githubtools.DecodeCall(a.Tool, []byte(a.ArgsJson)); err == nil {
		call.SetApprovalCall(out)
	}
	return out
}

// ListApprovals implements ApprovalService.
func (s *Server) ListApprovals(ctx context.Context, req *approvalv1alpha1.ListApprovalsRequest) (*approvalv1alpha1.ListApprovalsResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleViewer)
	if err != nil {
		return nil, err
	}
	var rows []store.ApprovalRequest
	if req.GetTicketId() != "" {
		ticket, err := parseID("ticket_id", req.GetTicketId())
		if err != nil {
			return nil, err
		}
		if _, err := s.q.GetTicket(ctx, store.GetTicketParams{OrgID: c.org, ID: ticket}); err != nil {
			return nil, s.dbError(err, "ticket")
		}
		rows, err = s.q.ListApprovalRequestsForTicket(ctx, store.ListApprovalRequestsForTicketParams{OrgID: c.org, TicketID: ticket})
		if err != nil {
			return nil, s.dbError(err, "approvals")
		}
	} else {
		rows, err = s.q.ListPendingApprovalRequests(ctx, c.org)
		if err != nil {
			return nil, s.dbError(err, "approvals")
		}
	}
	approver := c.p.HasRole(c.org, store.OrgRoleAdmin)
	res := &approvalv1alpha1.ListApprovalsResponse{}
	for _, r := range rows {
		res.Approvals = append(res.Approvals, approvalProto(r, approver, false))
	}
	return res, nil
}

// GetApproval implements ApprovalService.
func (s *Server) GetApproval(ctx context.Context, req *approvalv1alpha1.GetApprovalRequest) (*approvalv1alpha1.GetApprovalResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleViewer)
	if err != nil {
		return nil, err
	}
	id, err := parseID("approval_id", req.GetApprovalId())
	if err != nil {
		return nil, err
	}
	row, err := s.q.GetApprovalRequest(ctx, store.GetApprovalRequestParams{OrgID: c.org, ID: id})
	if err != nil {
		return nil, s.dbError(err, "approval")
	}
	return &approvalv1alpha1.GetApprovalResponse{Approval: approvalProto(row, c.p.HasRole(c.org, store.OrgRoleAdmin), true)}, nil
}

var fileStatusToProto = map[approvals.FileStatus]approvalv1alpha1.FileStatus{
	approvals.StatusUnknown:  approvalv1alpha1.FileStatus_FILE_STATUS_UNSPECIFIED,
	approvals.StatusAdded:    approvalv1alpha1.FileStatus_FILE_STATUS_ADDED,
	approvals.StatusModified: approvalv1alpha1.FileStatus_FILE_STATUS_MODIFIED,
	approvals.StatusDeleted:  approvalv1alpha1.FileStatus_FILE_STATUS_DELETED,
}

// GetApprovalDiff implements ApprovalService. Whoever may see the request may see its diff.
func (s *Server) GetApprovalDiff(ctx context.Context, req *approvalv1alpha1.GetApprovalDiffRequest) (*approvalv1alpha1.GetApprovalDiffResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleViewer)
	if err != nil {
		return nil, err
	}
	id, err := parseID("approval_id", req.GetApprovalId())
	if err != nil {
		return nil, err
	}
	row, err := s.q.GetApprovalRequest(ctx, store.GetApprovalRequestParams{OrgID: c.org, ID: id})
	if err != nil {
		return nil, s.dbError(err, "approval")
	}
	if s.approvals == nil {
		return nil, connect.NewError(connect.CodeUnavailable, "approvals are not configured on this server")
	}
	d, err := s.approvals.Diff(ctx, row)
	if errors.Is(err, approvals.ErrNoDiff) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err.Error())
	}
	if err != nil {
		return nil, s.dbError(err, "approval")
	}
	res := &approvalv1alpha1.GetApprovalDiffResponse{Base: d.Base}
	for _, f := range d.Files {
		res.Files = append(res.Files, &approvalv1alpha1.FileDiff{
			Path: f.Path, Status: fileStatusToProto[f.Status], UnifiedDiff: f.Diff, Binary: f.Binary, FallbackReason: f.Fallback,
		})
	}
	return res, nil
}

// decision checks an Approve or Deny request: admins and owners only.
func (s *Server) decision(ctx context.Context, orgID, approvalID, note string) (caller, pgtype.UUID, string, error) {
	c, err := authorize(ctx, orgID, store.OrgRoleAdmin)
	if err != nil {
		return caller{}, pgtype.UUID{}, "", err
	}
	if s.approvals == nil {
		return caller{}, pgtype.UUID{}, "", connect.NewError(connect.CodeUnavailable, "approvals are not configured on this server")
	}
	id, err := parseID("approval_id", approvalID)
	if err != nil {
		return caller{}, pgtype.UUID{}, "", err
	}
	note = strings.TrimSpace(note)
	if len(note) > maxNoteBytes {
		return caller{}, pgtype.UUID{}, "", connect.NewError(connect.CodeInvalidArgument, "note is longer than 2000 bytes")
	}
	if !c.actor().Valid {
		return caller{}, pgtype.UUID{}, "", connect.NewError(connect.CodePermissionDenied, "approvals need a signed-in user")
	}
	return c, id, note, nil
}

// approvalError maps the approvals package's errors. A hash mismatch is Aborted (HTTP 409,
// plan item P1): the request changed since the approver loaded it, and reloading shows the
// new version. A request that is no longer pending stays FailedPrecondition.
func (s *Server) approvalError(err error) error {
	switch {
	case errors.Is(err, approvals.ErrHashMismatch):
		return connect.NewError(connect.CodeAborted, err.Error())
	case errors.Is(err, approvals.ErrNotPending):
		return connect.NewError(connect.CodeFailedPrecondition, err.Error())
	}
	return s.dbError(err, "approval")
}

// Approve implements ApprovalService.
func (s *Server) Approve(ctx context.Context, req *approvalv1alpha1.ApproveRequest) (*approvalv1alpha1.ApproveResponse, error) {
	c, id, note, err := s.decision(ctx, req.GetOrgId(), req.GetApprovalId(), req.GetNote())
	if err != nil {
		return nil, err
	}
	if req.GetArgsSha256() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, "args_sha256 is required: approve the hash you were shown")
	}
	row, err := s.approvals.Approve(ctx, c.org, id, c.actor(), req.GetArgsSha256(), note)
	if err != nil {
		return nil, s.approvalError(err)
	}
	return &approvalv1alpha1.ApproveResponse{Approval: approvalProto(row, true, true)}, nil
}

// Deny implements ApprovalService.
func (s *Server) Deny(ctx context.Context, req *approvalv1alpha1.DenyRequest) (*approvalv1alpha1.DenyResponse, error) {
	c, id, note, err := s.decision(ctx, req.GetOrgId(), req.GetApprovalId(), req.GetNote())
	if err != nil {
		return nil, err
	}
	row, err := s.approvals.Deny(ctx, c.org, id, c.actor(), note)
	if err != nil {
		return nil, s.approvalError(err)
	}
	return &approvalv1alpha1.DenyResponse{Approval: approvalProto(row, true, true)}, nil
}
