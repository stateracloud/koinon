// SPDX-License-Identifier: Apache-2.0

package mcpgateway

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stateracloud/koinon/internal/gateway"
)

// Builtin is a target that the gateway runs in its own process, with no upstream MCP server (ADR 0123
// decision 10). Its tools are fixed in code. The gateway checks the grants of the agent, runs the
// request-side call check (the ask tier, the leak guard) and writes the span and the audit row, as it
// does for any tool. It also gives the result to the guard as the result of an upstream tool, so a
// result of a built-in tool is as untrusted as any other.
type Builtin interface {
	// Tools returns the fixed tool list. It never changes, and no remote text is part of it.
	Tools() []*mcp.Tool
	// Call runs one tool for agent. t is the target as applied, so a call sees the config of the
	// snapshot that authenticated the agent. It never returns an error: a failure is a result with
	// IsError, whose text is fixed and names no URL, no card text and no remote error body.
	//
	// creds is the gateway's credential source, wrapped with its credential scrub: every value it gives is
	// recorded before the built-in can use it, and is removed from the logs, spans and results of the
	// gateway (#1174, ADR 0097). A built-in opens each credential through creds and through no source of
	// its own. The outbound clients ([NewTargetHTTPClient], a2aout) take only a [ScrubbedSource], so a
	// built-in that sends a credential from a source of its own through them does not compile (#1221). An
	// HTTP client of the built-in's own is not covered: the test of each built-in kind catches that.
	Call(ctx context.Context, creds ScrubbedSource, t gateway.MCPTarget, agent gateway.AgentRef, tool string, args json.RawMessage) BuiltinResult
}

// BuiltinResult is the answer of a [Builtin] tool.
type BuiltinResult struct {
	Result *mcp.CallToolResult
	// Outcome is the audit outcome when it is not the one the result says (ok, or tool_error when
	// Result.IsError). Empty keeps that.
	Outcome Outcome
	// Credential is the kind of credential the tool used (see [CredentialNone]). Empty means none.
	Credential string
}

// callBuiltin runs one tool of a built-in target. The grant check and the call check are done.
func (g *Gateway) callBuiltin(ctx context.Context, agent gateway.AgentRef, t gateway.MCPTarget, tool string, args json.RawMessage, row *AuditRow, finish func(Outcome)) *mcp.CallToolResult {
	cctx, cancel := context.WithTimeout(ctx, g.o.CallTimeout)
	defer cancel()
	b := g.o.Builtins[t.Builtin]
	if b == nil { // ApplyMCP leaves such a target out; fail closed if one is here
		finish(OutcomeUpstreamError)
		return refusedResult("the target is not available")
	}
	r := b.Call(cctx, g.creds, t, agent, tool, args)
	// A built-in can quote a credential it opened. The result is scrubbed with the values of the org as it
	// enters, so the guard, the audit and the agent see one text.
	r.Result = g.o.Scrub.Result(agent.Org, r.Result)
	row.CredKind = CredentialNone
	if r.Credential != "" {
		row.CredKind = r.Credential
	}
	if r.Result == nil {
		finish(OutcomeUpstreamError)
		return refusedResult("the target did not answer")
	}
	// The result is the text of another party. It goes through the same leg handling as the result
	// of a tool on an upstream connection (ADR 0123 decision 12): the guard sees it as a result that
	// came from the target, and the run is tainted as after any other untrusted result.
	raw, err := json.Marshal(struct {
		JSONRPC string              `json:"jsonrpc"`
		ID      int                 `json:"id"`
		Result  *mcp.CallToolResult `json:"result"`
	}{"2.0", 1, r.Result})
	if err != nil {
		finish(OutcomeUpstreamError)
		return refusedResult("the target did not answer")
	}
	v := g.g.Inspect(cctx, Message{
		Target: row.Target, Org: t.Org, Credential: row.CredKind, Agent: agent,
		Dir: FromUpstream, Kind: KindResult, Method: "tools/call", Tool: tool, Raw: raw,
	})
	if v.Refuse {
		row.Verdict = v.Reason
		finish(OutcomeRefusedGuard)
		return refusedResult(v.Reason)
	}
	switch {
	case r.Outcome != "":
		finish(r.Outcome)
	case r.Result.IsError:
		finish(OutcomeToolError)
	default:
		finish(OutcomeOK)
	}
	return r.Result
}
