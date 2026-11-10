// SPDX-License-Identifier: Apache-2.0

package adminapi

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/sinkclass"
)

// WithMCP sets the service behind the remote MCP server endpoints (plan item MC1). Without it they
// answer 503.
func (s *Server) WithMCP(m *mcpservers.Service) *Server {
	s.mcp = m
	return s
}

var errNoMCP = &mcpservers.InvalidError{Msg: "remote MCP servers are not configured"}

func (s *Server) noMCP() (int, Error) {
	_, e := s.problem(errNoMCP)
	return http.StatusServiceUnavailable, e
}

func toMcpServer(m mcpservers.Server) McpServer {
	out := McpServer{
		CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt, Id: openapi_types.UUID(pgUUID(m.ID).Bytes), Name: m.Name, Url: m.URL,
		Transport: McpServerTransport(m.Transport), HasCredential: m.HasCredential, Source: Source(m.Source),
		Health: McpServerHealth(m.Health), HealthCheckedAt: m.HealthCheckedAt, HealthSince: m.HealthSince, ToolsSyncedAt: m.ToolsSyncedAt,
		Kind: McpServerKind(m.Kind), DataClass: append([]string{}, m.DataClass...),
	}
	if m.CodeHostConnectionID != "" {
		id := openapi_types.UUID(pgUUID(m.CodeHostConnectionID).Bytes)
		out.CodeHostConnectionId = &id
	}
	str := func(s string) *string {
		if s == "" {
			return nil
		}
		return &s
	}
	out.CredentialHeader, out.CredentialPrefix = str(m.CredentialHeader), str(m.CredentialPrefix)
	out.DisabledReason, out.ManagedBySecret, out.HealthReason = str(m.DisabledReason), str(m.ManagedBySecret), str(m.HealthReason)
	if m.Removed {
		t := true
		out.Removed = &t
	}
	out.CredentialLostAt = m.CredentialLostAt
	out.AdminDisabled = adminDisabledJSON(m.AdminDisabled)
	return out
}

func toMcpTool(t mcpservers.Tool) McpTool {
	out := McpTool{
		ServerId: openapi_types.UUID(pgUUID(t.ServerID).Bytes), Name: t.Name, Description: t.Description, ReadOnlyHint: t.ReadOnlyHint,
		Tier: McpToolTier(t.Tier), Present: t.Present, ContentHash: t.ContentHash, DescriptionOriginalLength: t.DescriptionOriginalLength, DescriptionCapped: t.DescriptionCapped, DescriptionCleaned: t.DescriptionCleaned, RequiresTwoApprovers: &t.RequiresTwoApprovers, FirstSeenAt: t.FirstSeenAt, LastSeenAt: t.LastSeenAt, TierSetAt: t.TierSetAt,
		Sink: McpToolSink{
			SinkClass: string(t.Sink.Class), Derivation: string(t.Sink.Derivation), AddressArgs: append([]string{}, t.Sink.AddressArgs...),
			ResultUntrustedInput: t.Sink.ResultUntrustedInput, ResultPrivateData: t.Sink.ResultPrivateData,
		},
	}
	if t.ReviewReason != "" {
		out.ReviewReason = &t.ReviewReason
	}
	if t.Unavailable != "" {
		out.Unavailable = &t.Unavailable
	}
	if t.TierSetBy != "" {
		out.TierSetBy = &t.TierSetBy
	}
	return out
}

// ListMcpServers implements StrictServerInterface.
func (s *Server) ListMcpServers(ctx context.Context, _ ListMcpServersRequestObject) (ListMcpServersResponseObject, error) {
	if s.mcp == nil {
		st, e := s.noMCP()
		return ListMcpServersdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	p, _ := s.actor(ctx)
	list, err := s.mcp.List(ctx, p.Org)
	if err != nil {
		st, e := s.problem(err)
		return ListMcpServersdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	out := ListMcpServers200JSONResponse{Servers: []McpServer{}}
	for _, x := range list {
		out.Servers = append(out.Servers, toMcpServer(x))
	}
	return out, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// CreateMcpServer implements StrictServerInterface.
func (s *Server) CreateMcpServer(ctx context.Context, req CreateMcpServerRequestObject) (CreateMcpServerResponseObject, error) {
	if s.mcp == nil {
		st, e := s.noMCP()
		return CreateMcpServerdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	p, actor := s.actor(ctx)
	in := mcpservers.Input{
		Name: req.Body.Name, URL: req.Body.Url, CredentialHeader: deref(req.Body.CredentialHeader),
		CredentialPrefix: deref(req.Body.CredentialPrefix), CredentialValue: deref(req.Body.CredentialValue),
	}
	if req.Body.Transport != nil {
		in.Transport = string(*req.Body.Transport)
	}
	if req.Body.Kind != nil || req.Body.DataClass != nil {
		d := mcpservers.DefaultDeclaration()
		if req.Body.Kind != nil {
			d.Kind = string(*req.Body.Kind)
		}
		if req.Body.DataClass != nil {
			d.Legs = *req.Body.DataClass
		}
		in.Class = &d
	}
	got, err := s.mcp.Create(ctx, p.Org, actor, pgtype.UUID{}, in)
	if err != nil {
		st, e := s.problem(err)
		return CreateMcpServerdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	return CreateMcpServer201JSONResponse(toMcpServer(got)), nil
}

// GetMcpServer implements StrictServerInterface.
func (s *Server) GetMcpServer(ctx context.Context, req GetMcpServerRequestObject) (GetMcpServerResponseObject, error) {
	if s.mcp == nil {
		st, e := s.noMCP()
		return GetMcpServerdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	p, _ := s.actor(ctx)
	got, err := s.mcp.Get(ctx, p.Org, uid(req.ServerId))
	if err != nil {
		st, e := s.problem(err)
		return GetMcpServerdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	return GetMcpServer200JSONResponse(toMcpServer(got)), nil
}

// UpdateMcpServer implements StrictServerInterface.
func (s *Server) UpdateMcpServer(ctx context.Context, req UpdateMcpServerRequestObject) (UpdateMcpServerResponseObject, error) {
	if s.mcp == nil {
		st, e := s.noMCP()
		return UpdateMcpServerdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	p, actor := s.actor(ctx)
	b := req.Body
	patch := mcpservers.Patch{URL: b.Url, RemoveCredential: b.RemoveCredential != nil && *b.RemoveCredential}
	if b.Transport != nil {
		t := string(*b.Transport)
		patch.Transport = &t
	}
	if b.CredentialHeader != nil || b.CredentialPrefix != nil || b.CredentialValue != nil {
		patch.Credential = &mcpservers.CredentialPatch{Header: deref(b.CredentialHeader), Prefix: deref(b.CredentialPrefix), Value: b.CredentialValue}
	}
	got, err := s.mcp.Update(ctx, p.Org, actor, uid(req.ServerId), patch)
	if err != nil {
		st, e := s.problem(err)
		return UpdateMcpServerdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	return UpdateMcpServer200JSONResponse(toMcpServer(got)), nil
}

// DeleteMcpServer implements StrictServerInterface.
func (s *Server) DeleteMcpServer(ctx context.Context, req DeleteMcpServerRequestObject) (DeleteMcpServerResponseObject, error) {
	if s.mcp == nil {
		st, e := s.noMCP()
		return DeleteMcpServerdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	p, actor := s.actor(ctx)
	if err := s.mcp.Delete(ctx, p.Org, actor, uid(req.ServerId)); err != nil {
		st, e := s.problem(err)
		return DeleteMcpServerdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	return DeleteMcpServer204Response{}, nil
}

// ListMcpTools implements StrictServerInterface.
func (s *Server) ListMcpTools(ctx context.Context, req ListMcpToolsRequestObject) (ListMcpToolsResponseObject, error) {
	if s.mcp == nil {
		st, e := s.noMCP()
		return ListMcpToolsdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	p, _ := s.actor(ctx)
	list, err := s.mcp.Tools(ctx, p.Org, uid(req.ServerId))
	if err != nil {
		st, e := s.problem(err)
		return ListMcpToolsdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	out := ListMcpTools200JSONResponse{Tools: []McpTool{}}
	for _, t := range list {
		out.Tools = append(out.Tools, toMcpTool(t))
	}
	return out, nil
}

// SetMcpToolTier implements StrictServerInterface.
func (s *Server) SetMcpToolTier(ctx context.Context, req SetMcpToolTierRequestObject) (SetMcpToolTierResponseObject, error) {
	if s.mcp == nil {
		st, e := s.noMCP()
		return SetMcpToolTierdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	p, actor := s.actor(ctx)
	got, err := s.mcp.SetTier(ctx, p.Org, actor, uid(req.ServerId), req.ToolName, string(req.Body.Tier))
	if err != nil {
		st, e := s.problem(err)
		return SetMcpToolTierdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	return SetMcpToolTier200JSONResponse(toMcpTool(got)), nil
}

// SetMcpToolSink implements StrictServerInterface.
func (s *Server) SetMcpToolSink(ctx context.Context, req SetMcpToolSinkRequestObject) (SetMcpToolSinkResponseObject, error) {
	if s.mcp == nil {
		st, e := s.noMCP()
		return SetMcpToolSinkdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	p, actor := s.actor(ctx)
	b := req.Body
	got, err := s.mcp.SetToolDeclaration(ctx, p.Org, actor, uid(req.ServerId), req.ToolName, b.ContentHash, sinkclass.Declaration{
		Class: sinkclass.Class(b.Sink.SinkClass), Derivation: sinkclass.Derivation(b.Sink.Derivation), AddressArgs: b.Sink.AddressArgs,
		ResultUntrustedInput: b.Sink.ResultUntrustedInput, ResultPrivateData: b.Sink.ResultPrivateData,
	})
	if err != nil {
		st, e := s.problem(err)
		return SetMcpToolSinkdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	return SetMcpToolSink200JSONResponse(toMcpTool(got)), nil
}

// SetMcpToolTwoApprovers implements StrictServerInterface.
func (s *Server) SetMcpToolTwoApprovers(ctx context.Context, req SetMcpToolTwoApproversRequestObject) (SetMcpToolTwoApproversResponseObject, error) {
	if s.mcp == nil {
		st, e := s.noMCP()
		return SetMcpToolTwoApproversdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	p, actor := s.actor(ctx)
	got, err := s.mcp.SetTwoApprovers(ctx, p.Org, actor, uid(req.ServerId), req.ToolName, req.Body.Required)
	if err != nil {
		st, e := s.problem(err)
		return SetMcpToolTwoApproversdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	return SetMcpToolTwoApprovers200JSONResponse(toMcpTool(got)), nil
}

// AcknowledgeMcpToolReview implements StrictServerInterface.
func (s *Server) AcknowledgeMcpToolReview(ctx context.Context, req AcknowledgeMcpToolReviewRequestObject) (AcknowledgeMcpToolReviewResponseObject, error) {
	if s.mcp == nil {
		st, e := s.noMCP()
		return AcknowledgeMcpToolReviewdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	p, actor := s.actor(ctx)
	got, err := s.mcp.AcknowledgeReview(ctx, p.Org, actor, uid(req.ServerId), req.ToolName)
	if err != nil {
		st, e := s.problem(err)
		return AcknowledgeMcpToolReviewdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	return AcknowledgeMcpToolReview200JSONResponse(toMcpTool(got)), nil
}

func toAgentMcp(list []mcpservers.Allowance) AgentMcpServers {
	out := AgentMcpServers{Servers: []struct {
		ServerId openapi_types.UUID `json:"serverId"`
		Tools    *[]string          `json:"tools,omitempty"`
	}{}}
	for _, a := range list {
		e := struct {
			ServerId openapi_types.UUID `json:"serverId"`
			Tools    *[]string          `json:"tools,omitempty"`
		}{ServerId: openapi_types.UUID(pgUUID(a.ServerID).Bytes)}
		if a.Tools != nil {
			t := a.Tools
			e.Tools = &t
		}
		out.Servers = append(out.Servers, e)
	}
	return out
}

// GetAgentMcpServers implements StrictServerInterface.
func (s *Server) GetAgentMcpServers(ctx context.Context, req GetAgentMcpServersRequestObject) (GetAgentMcpServersResponseObject, error) {
	if s.mcp == nil {
		st, e := s.noMCP()
		return GetAgentMcpServersdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	p, _ := s.actor(ctx)
	list, err := s.mcp.AgentServers(ctx, p.Org, pgUUID(uid(req.AgentId)))
	if err != nil {
		st, e := s.problem(err)
		return GetAgentMcpServersdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	return GetAgentMcpServers200JSONResponse(toAgentMcp(list)), nil
}

// SetAgentMcpServers implements StrictServerInterface.
func (s *Server) SetAgentMcpServers(ctx context.Context, req SetAgentMcpServersRequestObject) (SetAgentMcpServersResponseObject, error) {
	if s.mcp == nil {
		st, e := s.noMCP()
		return SetAgentMcpServersdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	p, actor := s.actor(ctx)
	allow := make([]mcpservers.Allowance, 0, len(req.Body.Servers))
	for _, a := range req.Body.Servers {
		al := mcpservers.Allowance{ServerID: uid(a.ServerId)}
		if a.Tools != nil {
			al.Tools = append([]string{}, *a.Tools...)
		}
		allow = append(allow, al)
	}
	list, err := s.mcp.SetAgentServers(ctx, p.Org, actor, pgUUID(uid(req.AgentId)), allow)
	if err != nil {
		st, e := s.problem(err)
		return SetAgentMcpServersdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	return SetAgentMcpServers200JSONResponse(toAgentMcp(list)), nil
}

// SetMcpServerDataClass implements StrictServerInterface.
func (s *Server) SetMcpServerDataClass(ctx context.Context, req SetMcpServerDataClassRequestObject) (SetMcpServerDataClassResponseObject, error) {
	if s.mcp == nil {
		st, e := s.noMCP()
		return SetMcpServerDataClassdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	p, actor := s.actor(ctx)
	d := mcpservers.Declaration{Kind: string(req.Body.Kind), Legs: req.Body.DataClass}
	if req.Body.CodeHostConnectionId != nil {
		d.CodeHostConnection = uid(*req.Body.CodeHostConnectionId)
	}
	got, err := s.mcp.SetDataClass(ctx, p.Org, actor, uid(req.ServerId), d)
	if err != nil {
		st, e := s.problem(err)
		return SetMcpServerDataClassdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	return SetMcpServerDataClass200JSONResponse(toMcpServer(got)), nil
}
