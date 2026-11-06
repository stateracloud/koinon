// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"time"

	connect "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/stateracloud/koinon/internal/db/store"
	mcpserverv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/mcpserver/v1alpha1"
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/sinkclass"
)

// The McpServerService is the UI's face of the MCP server service layer (ADR 0057): the same
// validation, URL policy and audit as the admin API. Every call needs the admin role, so members do
// not see servers or tools at all.

var healthToProto = map[string]mcpserverv1alpha1.Health{
	mcpservers.HealthUnknown:   mcpserverv1alpha1.Health_HEALTH_UNKNOWN,
	mcpservers.HealthHealthy:   mcpserverv1alpha1.Health_HEALTH_HEALTHY,
	mcpservers.HealthUnhealthy: mcpserverv1alpha1.Health_HEALTH_UNHEALTHY,
}

var mcpTierToProto = map[string]mcpserverv1alpha1.Tier{
	mcpservers.TierAsk:   mcpserverv1alpha1.Tier_TIER_ASK,
	mcpservers.TierAuto:  mcpserverv1alpha1.Tier_TIER_AUTO,
	mcpservers.TierNever: mcpserverv1alpha1.Tier_TIER_NEVER,
}

var mcpTierFromProto = map[mcpserverv1alpha1.Tier]string{
	mcpserverv1alpha1.Tier_TIER_ASK:   mcpservers.TierAsk,
	mcpserverv1alpha1.Tier_TIER_AUTO:  mcpservers.TierAuto,
	mcpserverv1alpha1.Tier_TIER_NEVER: mcpservers.TierNever,
}

func optTime(t *time.Time) *timestamppb.Timestamp {
	if t == nil {
		return nil
	}
	return timestamppb.New(*t)
}

func (s *Server) mcpServerProto(org, slug string, m mcpservers.Server) *mcpserverv1alpha1.Server {
	_ = org
	return &mcpserverv1alpha1.Server{
		Id: m.ID, Name: m.Name, Url: m.URL, Transport: m.Transport, HasCredential: m.HasCredential,
		CredentialHeader: m.CredentialHeader, CredentialPrefix: m.CredentialPrefix, Source: m.Source,
		ManagedBySecret: m.ManagedBySecret, DisabledReason: m.DisabledReason, Removed: m.Removed,
		CredentialLostAt: optTime(m.CredentialLostAt), Health: healthToProto[m.Health], HealthReason: m.HealthReason,
		HealthCheckedAt: optTime(m.HealthCheckedAt), HealthSince: optTime(m.HealthSince), ToolsSyncedAt: optTime(m.ToolsSyncedAt),
		CreatedAt: timestamppb.New(m.CreatedAt), UpdatedAt: timestamppb.New(m.UpdatedAt),
		GatewayTarget: mcpservers.TargetName(slug, m.Name),
		Kind:          m.Kind, DataClass: m.DataClass, CodeHostConnectionId: m.CodeHostConnectionID,
	}
}

func mcpToolProto(t mcpservers.Tool) *mcpserverv1alpha1.Tool {
	return &mcpserverv1alpha1.Tool{
		ServerId: t.ServerID, Name: t.Name, Description: t.Description, ReadOnlyHint: t.ReadOnlyHint, Tier: mcpTierToProto[t.Tier],
		ReviewReason: t.ReviewReason, Present: t.Present, FirstSeenAt: timestamppb.New(t.FirstSeenAt),
		LastSeenAt: timestamppb.New(t.LastSeenAt), TierSetBy: t.TierSetBy, TierSetAt: optTime(t.TierSetAt),
		RequiresTwoApprovers: t.RequiresTwoApprovers, ContentHash: t.ContentHash, Unavailable: t.Unavailable,
		DescriptionOriginalLength: int64(t.DescriptionOriginalLength), DescriptionCapped: t.DescriptionCapped, DescriptionCleaned: t.DescriptionCleaned,
		Sink: &mcpserverv1alpha1.ToolSink{
			SinkClass: string(t.Sink.Class), Derivation: string(t.Sink.Derivation), AddressArgs: append([]string{}, t.Sink.AddressArgs...),
			ResultUntrustedInput: t.Sink.ResultUntrustedInput, ResultPrivateData: t.Sink.ResultPrivateData,
		},
	}
}

// mcpError maps the service's errors; messages are safe to show.
func (s *Server) mcpError(err error) error { return s.catalogError(err) }

// mcpCaller authorizes an admin call and checks the service is wired.
func (s *Server) mcpCaller(ctx context.Context, orgID string) (caller, error) {
	c, err := authorize(ctx, orgID, store.OrgRoleAdmin)
	if err != nil {
		return caller{}, err
	}
	if s.mcp == nil {
		return caller{}, connect.NewError(connect.CodeUnimplemented, "remote MCP servers are not configured")
	}
	return c, nil
}

// ListServers implements McpServerService. Admins and owners.
func (s *Server) ListServers(ctx context.Context, req *mcpserverv1alpha1.ListServersRequest) (*mcpserverv1alpha1.ListServersResponse, error) {
	c, err := s.mcpCaller(ctx, req.GetOrgId())
	if err != nil {
		return nil, err
	}
	list, err := s.mcp.List(ctx, c.org)
	if err != nil {
		return nil, s.mcpError(err)
	}
	slug, err := s.orgSlug(ctx, c)
	if err != nil {
		return nil, err
	}
	res := &mcpserverv1alpha1.ListServersResponse{}
	for _, m := range list {
		res.Servers = append(res.Servers, s.mcpServerProto(req.GetOrgId(), slug, m))
	}
	return res, nil
}

func (s *Server) orgSlug(ctx context.Context, c caller) (string, error) {
	slug, err := s.q.GetOrgSlug(ctx, c.org)
	if err != nil {
		return "", s.dbError(err, "org")
	}
	return slug, nil
}

// CreateServer implements McpServerService. Admins and owners.
func (s *Server) CreateServer(ctx context.Context, req *mcpserverv1alpha1.CreateServerRequest) (*mcpserverv1alpha1.CreateServerResponse, error) {
	c, err := s.mcpCaller(ctx, req.GetOrgId())
	if err != nil {
		return nil, err
	}
	in := mcpservers.Input{
		Name: req.GetName(), URL: req.GetUrl(), Transport: req.GetTransport(), CredentialHeader: req.GetCredentialHeader(),
		CredentialPrefix: req.GetCredentialPrefix(), CredentialValue: req.GetCredentialValue(),
	}
	if cl := req.GetClass(); cl != nil {
		in.Class = &mcpservers.Declaration{Kind: cl.GetKind(), Legs: cl.GetDataClass(), CodeHostConnection: cl.GetCodeHostConnectionId()}
	}
	m, err := s.mcp.Create(requestCtx(ctx), c.org, c.auditActor(), c.actor(), in)
	if err != nil {
		return nil, s.mcpError(err)
	}
	slug, err := s.orgSlug(ctx, c)
	if err != nil {
		return nil, err
	}
	return &mcpserverv1alpha1.CreateServerResponse{Server: s.mcpServerProto(req.GetOrgId(), slug, m)}, nil
}

// UpdateServer implements McpServerService. Admins and owners.
func (s *Server) UpdateServer(ctx context.Context, req *mcpserverv1alpha1.UpdateServerRequest) (*mcpserverv1alpha1.UpdateServerResponse, error) {
	c, err := s.mcpCaller(ctx, req.GetOrgId())
	if err != nil {
		return nil, err
	}
	patch := mcpservers.Patch{URL: req.Url, Transport: req.Transport, RemoveCredential: req.GetRemoveCredential()}
	if req.CredentialHeader != nil || req.CredentialPrefix != nil || req.CredentialValue != nil {
		patch.Credential = &mcpservers.CredentialPatch{Header: req.GetCredentialHeader(), Prefix: req.GetCredentialPrefix(), Value: req.CredentialValue}
	}
	m, err := s.mcp.Update(requestCtx(ctx), c.org, c.auditActor(), req.GetServerId(), patch)
	if err != nil {
		return nil, s.mcpError(err)
	}
	slug, err := s.orgSlug(ctx, c)
	if err != nil {
		return nil, err
	}
	return &mcpserverv1alpha1.UpdateServerResponse{Server: s.mcpServerProto(req.GetOrgId(), slug, m)}, nil
}

// SetServerClass implements McpServerService. Admins and owners.
func (s *Server) SetServerClass(ctx context.Context, req *mcpserverv1alpha1.SetServerClassRequest) (*mcpserverv1alpha1.SetServerClassResponse, error) {
	c, err := s.mcpCaller(ctx, req.GetOrgId())
	if err != nil {
		return nil, err
	}
	m, err := s.mcp.SetDataClass(requestCtx(ctx), c.org, c.auditActor(), req.GetServerId(),
		mcpservers.Declaration{Kind: req.GetClass().GetKind(), Legs: req.GetClass().GetDataClass(), CodeHostConnection: req.GetClass().GetCodeHostConnectionId()})
	if err != nil {
		return nil, s.mcpError(err)
	}
	slug, err := s.orgSlug(ctx, c)
	if err != nil {
		return nil, err
	}
	return &mcpserverv1alpha1.SetServerClassResponse{Server: s.mcpServerProto(req.GetOrgId(), slug, m)}, nil
}

// DeleteServer implements McpServerService. Admins and owners.
func (s *Server) DeleteServer(ctx context.Context, req *mcpserverv1alpha1.DeleteServerRequest) (*mcpserverv1alpha1.DeleteServerResponse, error) {
	c, err := s.mcpCaller(ctx, req.GetOrgId())
	if err != nil {
		return nil, err
	}
	if err := s.mcp.Delete(requestCtx(ctx), c.org, c.auditActor(), req.GetServerId()); err != nil {
		return nil, s.mcpError(err)
	}
	return &mcpserverv1alpha1.DeleteServerResponse{}, nil
}

// ListTools implements McpServerService. Admins and owners.
func (s *Server) ListTools(ctx context.Context, req *mcpserverv1alpha1.ListToolsRequest) (*mcpserverv1alpha1.ListToolsResponse, error) {
	c, err := s.mcpCaller(ctx, req.GetOrgId())
	if err != nil {
		return nil, err
	}
	list, err := s.mcp.Tools(ctx, c.org, req.GetServerId())
	if err != nil {
		return nil, s.mcpError(err)
	}
	res := &mcpserverv1alpha1.ListToolsResponse{}
	for _, t := range list {
		res.Tools = append(res.Tools, mcpToolProto(t))
	}
	return res, nil
}

// SetToolTier implements McpServerService. Admins and owners.
func (s *Server) SetToolTier(ctx context.Context, req *mcpserverv1alpha1.SetToolTierRequest) (*mcpserverv1alpha1.SetToolTierResponse, error) {
	c, err := s.mcpCaller(ctx, req.GetOrgId())
	if err != nil {
		return nil, err
	}
	tier, ok := mcpTierFromProto[req.GetTier()]
	if !ok {
		return nil, connect.NewError(connect.CodeInvalidArgument, "tier must be ask, auto or never")
	}
	t, err := s.mcp.SetTier(requestCtx(ctx), c.org, c.auditActor(), req.GetServerId(), req.GetTool(), tier)
	if err != nil {
		return nil, s.mcpError(err)
	}
	return &mcpserverv1alpha1.SetToolTierResponse{Tool: mcpToolProto(t)}, nil
}

// SetToolSink implements McpServerService. Admins and owners.
func (s *Server) SetToolSink(ctx context.Context, req *mcpserverv1alpha1.SetToolSinkRequest) (*mcpserverv1alpha1.SetToolSinkResponse, error) {
	c, err := s.mcpCaller(ctx, req.GetOrgId())
	if err != nil {
		return nil, err
	}
	sk := req.GetSink()
	if sk == nil {
		sk = &mcpserverv1alpha1.ToolSink{} // an empty derivation, which Normalize refuses
	}
	t, err := s.mcp.SetToolDeclaration(requestCtx(ctx), c.org, c.auditActor(), req.GetServerId(), req.GetTool(), req.GetContentHash(), sinkclass.Declaration{
		Class: sinkclass.Class(sk.GetSinkClass()), Derivation: sinkclass.Derivation(sk.GetDerivation()), AddressArgs: sk.GetAddressArgs(),
		ResultUntrustedInput: sk.ResultUntrustedInput, ResultPrivateData: sk.ResultPrivateData,
	})
	if err != nil {
		return nil, s.mcpError(err)
	}
	return &mcpserverv1alpha1.SetToolSinkResponse{Tool: mcpToolProto(t)}, nil
}

// SetToolTwoApprovers implements McpServerService. Admins and owners.
func (s *Server) SetToolTwoApprovers(ctx context.Context, req *mcpserverv1alpha1.SetToolTwoApproversRequest) (*mcpserverv1alpha1.SetToolTwoApproversResponse, error) {
	c, err := s.mcpCaller(ctx, req.GetOrgId())
	if err != nil {
		return nil, err
	}
	t, err := s.mcp.SetTwoApprovers(requestCtx(ctx), c.org, c.auditActor(), req.GetServerId(), req.GetTool(), req.GetRequired())
	if err != nil {
		return nil, s.mcpError(err)
	}
	return &mcpserverv1alpha1.SetToolTwoApproversResponse{Tool: mcpToolProto(t)}, nil
}

// AcknowledgeToolReview implements McpServerService. Admins and owners.
func (s *Server) AcknowledgeToolReview(ctx context.Context, req *mcpserverv1alpha1.AcknowledgeToolReviewRequest) (*mcpserverv1alpha1.AcknowledgeToolReviewResponse, error) {
	c, err := s.mcpCaller(ctx, req.GetOrgId())
	if err != nil {
		return nil, err
	}
	t, err := s.mcp.AcknowledgeReview(requestCtx(ctx), c.org, c.auditActor(), req.GetServerId(), req.GetTool())
	if err != nil {
		return nil, s.mcpError(err)
	}
	return &mcpserverv1alpha1.AcknowledgeToolReviewResponse{Tool: mcpToolProto(t)}, nil
}

func allowanceProto(list []mcpservers.Allowance) []*mcpserverv1alpha1.Allowance {
	out := make([]*mcpserverv1alpha1.Allowance, 0, len(list))
	for _, a := range list {
		out = append(out, &mcpserverv1alpha1.Allowance{ServerId: a.ServerID, Tools: a.Tools})
	}
	return out
}

// GetAgentServers implements McpServerService. Admins and owners.
func (s *Server) GetAgentServers(ctx context.Context, req *mcpserverv1alpha1.GetAgentServersRequest) (*mcpserverv1alpha1.GetAgentServersResponse, error) {
	c, err := s.mcpCaller(ctx, req.GetOrgId())
	if err != nil {
		return nil, err
	}
	agent, err := parseID("agent_id", req.GetAgentId())
	if err != nil {
		return nil, err
	}
	list, err := s.mcp.AgentServers(ctx, c.org, agent)
	if err != nil {
		return nil, s.mcpError(err)
	}
	return &mcpserverv1alpha1.GetAgentServersResponse{Servers: allowanceProto(list)}, nil
}

// SetAgentServers implements McpServerService. Admins and owners.
func (s *Server) SetAgentServers(ctx context.Context, req *mcpserverv1alpha1.SetAgentServersRequest) (*mcpserverv1alpha1.SetAgentServersResponse, error) {
	c, err := s.mcpCaller(ctx, req.GetOrgId())
	if err != nil {
		return nil, err
	}
	agent, err := parseID("agent_id", req.GetAgentId())
	if err != nil {
		return nil, err
	}
	allow := make([]mcpservers.Allowance, 0, len(req.GetServers()))
	for _, a := range req.GetServers() {
		al := mcpservers.Allowance{ServerID: a.GetServerId()}
		if len(a.GetTools()) > 0 {
			al.Tools = append([]string{}, a.GetTools()...)
		}
		allow = append(allow, al)
	}
	list, err := s.mcp.SetAgentServers(requestCtx(ctx), c.org, c.auditActor(), agent, allow)
	if err != nil {
		return nil, s.mcpError(err)
	}
	return &mcpserverv1alpha1.SetAgentServersResponse{Servers: allowanceProto(list)}, nil
}
