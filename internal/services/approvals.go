// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	connect "connectrpc.com/connect/v2"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/approvals"
	"github.com/stateracloud/koinon/internal/codehost"
	"github.com/stateracloud/koinon/internal/db/store"
	approvalv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/approval/v1alpha1"
	"github.com/stateracloud/koinon/internal/githubtools"
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/savedwork"
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
		Id: idString(a.ID), OrgId: idString(a.OrgID), TicketId: idString(a.TaskID), RunId: idString(a.RunID),
		AgentId: idString(a.AgentID), Connector: a.Connector, Tool: a.Tool, Target: a.Target,
		ArgsJson: a.ArgsJson, ArgsSha256: a.ArgsSha256,
		Decision: approvalDecisionToProto[a.Decision], Status: approvalStatusToProto[a.Status],
		DecidedBy: idString(a.DecidedBy), DecidedAt: tsProto(a.DecidedAt), DecisionNote: a.DecisionNote.String,
		Error: a.Error.String, Executions: int64(a.Executions),
		ExpiresAt: tsProto(a.ExpiresAt), CreatedAt: tsProto(a.CreatedAt), ExecutedAt: tsProto(a.ExecutedAt),
		CanDecide:         canDecide && a.Status == store.ApprovalStatusPending,
		ApprovalsRequired: int32(a.ApprovalsRequired), FirstApprover: idString(a.FirstApprover),
		System: a.System, ScopeId: idString(a.ScopeID),
	}
	if len(a.Result) > 0 {
		out.Result = payloadProto(a.Result)
	}
	var risk githubtools.Risk
	if json.Unmarshal(a.Risk, &risk) == nil {
		out.Risk = &approvalv1alpha1.Risk{
			Files: int64(risk.Files), Bytes: int64(risk.Bytes), Deletions: risk.Deletions, Ci: risk.CI,
			Scripts: risk.Scripts, Dependencies: risk.Dependencies, Binary: risk.Binary, Executable: risk.Executable,
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
		if _, err := s.q.GetTask(ctx, store.GetTaskParams{OrgID: c.org, ID: ticket}); err != nil {
			return nil, s.dbError(err, "ticket")
		}
		rows, err = s.q.ListApprovalRequestsForTask(ctx, store.ListApprovalRequestsForTaskParams{OrgID: c.org, TaskID: ticket})
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
	out := approvalProto(row, c.p.HasRole(c.org, store.OrgRoleAdmin), true)
	s.enrichMCP(ctx, out, row)
	s.enrichSavedWork(ctx, out, row)
	s.enrichCI(ctx, out, row)
	if row.ScopeID.Valid {
		if sc, err := s.q.GetApprovalScope(ctx, store.GetApprovalScopeParams{OrgID: c.org, ID: row.ScopeID}); err == nil {
			out.ScopeKind = scopeKindToProto[sc.Kind]
		}
	}
	return &approvalv1alpha1.GetApprovalResponse{Approval: out}, nil
}

// enrichCI fills the paths whose changes run CI on the request's code host: the connector's CI
// paths and the directories configured on the connection the run's agent version uses, so the
// card warns about the same paths the workflow guard refuses. Without a connection it falls back
// to the connector's own paths.
func (s *Server) enrichCI(ctx context.Context, out *approvalv1alpha1.Approval, row store.ApprovalRequest) {
	p, ok := s.hosts.Get(row.Connector)
	if !ok {
		return
	}
	var config []byte
	if run, err := s.q.GetRun(ctx, store.GetRunParams{OrgID: row.OrgID, ID: row.RunID}); err == nil {
		if v, err := s.q.GetAgentVersion(ctx, store.GetAgentVersionParams{OrgID: row.OrgID, ID: run.AgentVersionID}); err == nil {
			if cfg, err := agentconfig.Parse(v.Config); err == nil {
				if b, ok := cfg.Binding(row.Connector); ok {
					var id pgtype.UUID
					if id.Scan(b.ConnectionID) == nil && id.Valid {
						if c, err := s.q.GetConnection(ctx, store.GetConnectionParams{OrgID: row.OrgID, ID: id}); err == nil {
							config = c.Config
						}
					}
				}
			}
		}
	}
	g, err := codehost.GuardOf(p.Capabilities(), config)
	if err != nil {
		g, _ = codehost.NewGuard(p.Capabilities(), nil)
	}
	out.CiPaths, out.CiFoldCase = g.Paths(), g.FoldCase()
}

// enrichMCP fills the MCP call of an approval: what the request was bound to and what the tool is
// now, so the card can show a changed or flagged tool.
func (s *Server) enrichMCP(ctx context.Context, out *approvalv1alpha1.Approval, row store.ApprovalRequest) {
	if row.Connector != "mcp" {
		return
	}
	var b mcpservers.CallBinding
	if json.Unmarshal(row.CallBinding, &b) != nil {
		return
	}
	call := &approvalv1alpha1.McpCall{ServerId: b.ServerID, ContentHash: b.ContentHash, ServerName: strings.TrimPrefix(row.Target, "mcp:")}
	var sid pgtype.UUID
	if sid.Scan(b.ServerID) == nil {
		if t, err := s.q.GetMCPTool(ctx, store.GetMCPToolParams{OrgID: row.OrgID, ServerID: sid, Name: row.Tool}); err == nil {
			call.CurrentContentHash, call.ReviewReason, call.ToolDescription = t.ContentHash, t.ReviewReason.String, t.Description
		}
	}
	call.LeakGuard = leakGuardProto(row.Risk)
	out.Call = &approvalv1alpha1.Approval_Mcp{Mcp: call}
}

// leakGuardProto reads the leak guard's part of a card's risk (mcpguard's assessment.risk, under
// "leak"); nil when the card is not the guard's.
func leakGuardProto(risk []byte) *approvalv1alpha1.LeakGuard {
	var r struct {
		Leak *struct {
			Reasons     []string          `json:"reasons"`
			SinkClass   string            `json:"sink_class"`
			Destination map[string]string `json:"destination"`
			Legs        struct {
				UntrustedInput bool `json:"untrusted_input"`
				PrivateData    bool `json:"private_data"`
			} `json:"legs"`
		} `json:"leak"`
	}
	if json.Unmarshal(risk, &r) != nil || r.Leak == nil {
		return nil
	}
	return &approvalv1alpha1.LeakGuard{
		Reasons: r.Leak.Reasons, SinkClass: r.Leak.SinkClass, Destination: r.Leak.Destination,
		UntrustedInput: r.Leak.Legs.UntrustedInput, PrivateData: r.Leak.Legs.PrivateData,
	}
}

// enrichSavedWork fills the diffstat of a "push saved work" request from the saved work's manifest.
// The sealed files are not opened here.
func (s *Server) enrichSavedWork(ctx context.Context, out *approvalv1alpha1.Approval, row store.ApprovalRequest) {
	if !row.System || row.Tool != savedwork.ToolPush {
		return
	}
	args, err := savedwork.DecodePushArgs(row.ArgsJson)
	if err != nil {
		return
	}
	var id pgtype.UUID
	if id.Scan(args.SavedWorkID) != nil {
		return
	}
	sw, err := s.q.GetSavedWorkManifest(ctx, store.GetSavedWorkManifestParams{OrgID: row.OrgID, ID: id})
	if err != nil {
		return
	}
	var m savedwork.Manifest
	if json.Unmarshal(sw.Manifest, &m) != nil {
		return
	}
	call := &approvalv1alpha1.SavedWorkCall{
		Repository: sw.Repo, Base: sw.BaseSha, Branch: sw.Branch, SizeBytes: sw.SizeBytes, Skipped: m.Skipped,
		Status: sw.Status, ExpiresAt: tsProto(sw.ExpiresAt),
	}
	for _, f := range m.Files {
		call.Files = append(call.Files, &approvalv1alpha1.SavedFile{
			Path: f.Path, Status: f.Status, Added: int32(f.Added), Deleted: int32(f.Deleted), Size: f.Size, Binary: f.Binary, //nolint:gosec // line counts
		})
	}
	out.Call = &approvalv1alpha1.Approval_SavedWork{SavedWork: call}
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
	if err := s.refuseWhileHeld(); err != nil {
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
			OldMode: f.OldMode, NewMode: f.NewMode,
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
	case errors.Is(err, approvals.ErrSameApprover), errors.Is(err, approvals.ErrRequesterApproves):
		return connect.NewError(connect.CodeFailedPrecondition, err.Error())
	case errors.Is(err, approvals.ErrScopeInvalid):
		return connect.NewError(connect.CodeInvalidArgument, err.Error())
	case errors.Is(err, approvals.ErrScopeNotAllowed):
		return connect.NewError(connect.CodeFailedPrecondition, err.Error())
	case errors.Is(err, approvals.ErrScopeNotFound):
		return connect.NewError(connect.CodeNotFound, err.Error())
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
	var scope approvals.Scope
	switch req.GetScope() {
	case approvalv1alpha1.ScopeKind_SCOPE_KIND_UNSPECIFIED, approvalv1alpha1.ScopeKind_SCOPE_KIND_ONCE:
	case approvalv1alpha1.ScopeKind_SCOPE_KIND_TASK:
		scope.Kind = approvals.ScopeTask
	case approvalv1alpha1.ScopeKind_SCOPE_KIND_AGENT:
		scope.Kind = approvals.ScopeAgent
	default:
		return nil, connect.NewError(connect.CodeInvalidArgument, "unknown approval scope")
	}
	if req.GetScopeUntil() != nil {
		scope.Until = req.GetScopeUntil().AsTime()
	}
	// A scope wider than once needs an org admin; deciding at all already does (decision above).
	row, err := s.approvals.ApproveScoped(ctx, c.org, id, c.actor(), req.GetArgsSha256(), note, scope)
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

var scopeKindToProto = map[store.ApprovalScopeKind]approvalv1alpha1.ScopeKind{
	store.ApprovalScopeKindTask:  approvalv1alpha1.ScopeKind_SCOPE_KIND_TASK,
	store.ApprovalScopeKindAgent: approvalv1alpha1.ScopeKind_SCOPE_KIND_AGENT,
}

func scopeProto(sc store.ApprovalScope) *approvalv1alpha1.ApprovalScope {
	return &approvalv1alpha1.ApprovalScope{
		Id: idString(sc.ID), OrgId: idString(sc.OrgID), AgentId: idString(sc.AgentID), Connector: sc.Connector,
		Tool: sc.Tool, Target: sc.Target, Kind: scopeKindToProto[sc.Kind], TaskId: idString(sc.TaskID),
		ExpiresAt: tsProto(sc.ExpiresAt), CreatedBy: idString(sc.CreatedBy), CreatedAt: tsProto(sc.CreatedAt),
		SourceApprovalId: idString(sc.SourceApprovalID), RevokedAt: tsProto(sc.RevokedAt), RevokedBy: idString(sc.RevokedBy),
	}
}

// ListApprovalScopes implements ApprovalService.
func (s *Server) ListApprovalScopes(ctx context.Context, req *approvalv1alpha1.ListApprovalScopesRequest) (*approvalv1alpha1.ListApprovalScopesResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleViewer)
	if err != nil {
		return nil, err
	}
	agent, err := parseID("agent_id", req.GetAgentId())
	if err != nil {
		return nil, err
	}
	if _, err := s.q.GetAgent(ctx, store.GetAgentParams{OrgID: c.org, ID: agent}); err != nil {
		return nil, s.dbError(err, "agent")
	}
	if s.approvals == nil {
		return nil, connect.NewError(connect.CodeUnavailable, "approvals are not configured on this server")
	}
	rows, err := s.approvals.ListScopes(ctx, c.org, agent)
	if err != nil {
		return nil, s.dbError(err, "approval scopes")
	}
	res := &approvalv1alpha1.ListApprovalScopesResponse{}
	for _, r := range rows {
		res.Scopes = append(res.Scopes, scopeProto(r))
	}
	return res, nil
}

// RevokeApprovalScope implements ApprovalService.
func (s *Server) RevokeApprovalScope(ctx context.Context, req *approvalv1alpha1.RevokeApprovalScopeRequest) (*approvalv1alpha1.RevokeApprovalScopeResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	if s.approvals == nil {
		return nil, connect.NewError(connect.CodeUnavailable, "approvals are not configured on this server")
	}
	id, err := parseID("scope_id", req.GetScopeId())
	if err != nil {
		return nil, err
	}
	if !c.actor().Valid {
		return nil, connect.NewError(connect.CodePermissionDenied, "revoking a scope needs a signed-in user")
	}
	row, err := s.approvals.RevokeScope(ctx, c.org, id, c.actor())
	if err != nil {
		return nil, s.approvalError(err)
	}
	return &approvalv1alpha1.RevokeApprovalScopeResponse{Scope: scopeProto(row)}, nil
}
