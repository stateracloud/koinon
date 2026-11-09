// SPDX-License-Identifier: Apache-2.0

package mcpguard

import (
	"context"
	"encoding/json"
	"log/slog"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/stateracloud/koinon/internal/gen/agentgateway/extmcp"
)

// The metadata keys the gateway's processor config sets (CEL; render.go mcpGuardrails). A CEL
// expression that fails is left out of the map by the gateway, so a missing key is a refusal.
const (
	MetaAgent = "agent" // apiKey.agent
	MetaTool  = "tool"  // mcp.tool.name
)

// MethodToolsCall is the only method the gateway sends the guard (the processor's methods map).
const MethodToolsCall = "tools/call"

// Server is the ExtMcp gRPC service of agentgateway's mcpGuardrails.
type Server struct {
	extmcp.UnimplementedExtMcpServer
	g   *Guard
	log *slog.Logger
}

// NewServer returns the gRPC service of g.
func NewServer(g *Guard) *Server { return &Server{g: g, log: g.log} }

var _ extmcp.ExtMcpServer = (*Server)(nil)

func metaString(s *structpb.Struct, key string) string {
	if s == nil {
		return ""
	}
	v, ok := s.GetFields()[key]
	if !ok {
		return ""
	}
	return v.GetStringValue()
}

func denied(text string) *extmcp.AuthorizationError {
	return &extmcp.AuthorizationError{Code: extmcp.AuthorizationError_PERMISSION_DENIED, Reason: text}
}

// CheckRequest decides one tools/call. Every answer but Pass is an error the agent reads; the
// gRPC error is reserved for a guard that cannot answer, which the gateway fails closed on.
func (s *Server) CheckRequest(ctx context.Context, req *extmcp.McpRequest) (*extmcp.McpRequestResult, error) {
	if req.GetMethod() != MethodToolsCall {
		// The processor is configured for tools/call only; anything else is a configuration
		// error, and the cautious answer is no.
		return &extmcp.McpRequestResult{Result: &extmcp.McpRequestResult_Error{Error: denied("refused by the leak guard: unexpected method " + req.GetMethod())}}, nil
	}
	if len(req.GetServiceNames()) != 1 {
		return &extmcp.McpRequestResult{Result: &extmcp.McpRequestResult_Error{Error: denied("refused by the leak guard: a tool call must target exactly one server")}}, nil
	}
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.GetMcpRequest(), &params); err != nil || params.Name == "" {
		return &extmcp.McpRequestResult{Result: &extmcp.McpRequestResult_Error{Error: denied("refused by the leak guard: the tool call's parameters do not parse")}}, nil
	}
	out := s.g.Check(ctx, Call{
		Target: req.GetServiceNames()[0], Agent: metaString(req.GetMetadataContext(), MetaAgent),
		Tool: params.Name, Arguments: params.Arguments,
	})
	if out.Verdict.Action == Pass {
		return &extmcp.McpRequestResult{Result: &extmcp.McpRequestResult_Pass{Pass: &extmcp.Pass{}}}, nil
	}
	return &extmcp.McpRequestResult{Result: &extmcp.McpRequestResult_Error{Error: denied(out.Text)}}, nil
}

// CheckResponse records a tools/call result's legs and fingerprints. If it cannot, the result is
// withheld: an unrecorded private result could leave the org unseen later.
func (s *Server) CheckResponse(ctx context.Context, resp *extmcp.McpResponse) (*extmcp.McpResponseResult, error) {
	if resp.GetMethod() != MethodToolsCall {
		// A reply the guard has no reading for: it passes, but the session takes both legs (#254).
		// The gateway is configured for tools/call only, so this is the fail-closed rule for drift.
		if len(resp.GetServiceNames()) != 1 {
			return &extmcp.McpResponseResult{Result: &extmcp.McpResponseResult_Error{Error: denied("withheld by the leak guard: a reply must come from exactly one server")}}, nil
		}
		if err := s.g.CheckUnseen(ctx, metaString(resp.GetMetadataContext(), MetaAgent), resp.GetServiceNames()[0]); err != nil {
			text, ok := RefusalText(err)
			if !ok {
				s.log.Error("mcp guard: recording an unclassified reply failed", "method", resp.GetMethod(), "err", err)
				text = "withheld by the leak guard: it could not record this reply (an internal error); try again later"
			}
			return &extmcp.McpResponseResult{Result: &extmcp.McpResponseResult_Error{Error: denied(text)}}, nil
		}
		return &extmcp.McpResponseResult{Result: &extmcp.McpResponseResult_Pass{Pass: &extmcp.Pass{}}}, nil
	}
	if len(resp.GetServiceNames()) != 1 {
		return &extmcp.McpResponseResult{Result: &extmcp.McpResponseResult_Error{Error: denied("withheld by the leak guard: a tool result must come from exactly one server")}}, nil
	}
	tool := metaString(resp.GetMetadataContext(), MetaTool)
	if tool == "" {
		return &extmcp.McpResponseResult{Result: &extmcp.McpResponseResult_Error{Error: denied("withheld by the leak guard: the gateway did not name the tool")}}, nil
	}
	err := s.g.CheckResult(ctx, Result{
		Target: resp.GetServiceNames()[0], Agent: metaString(resp.GetMetadataContext(), MetaAgent),
		Tool: tool, Result: resp.GetMcpResponse(),
	})
	if err != nil {
		text, ok := RefusalText(err)
		if !ok {
			s.log.Error("mcp guard: recording a result failed", "target", resp.GetServiceNames()[0], "tool", tool, "err", err)
			text = "withheld by the leak guard: it could not record this result (an internal error); try again later"
		}
		return &extmcp.McpResponseResult{Result: &extmcp.McpResponseResult_Error{Error: denied(text)}}, nil
	}
	return &extmcp.McpResponseResult{Result: &extmcp.McpResponseResult_Pass{Pass: &extmcp.Pass{}}}, nil
}
