// SPDX-License-Identifier: Apache-2.0

package mcpgateway

import (
	"context"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stateracloud/koinon/internal/gateway"
)

// The gateway serves one MCP prompt: the readiness prompt (gateway.ReadyPrompt, ADR 0052). Koinon's
// MCP server publishes it; a harness that turns MCP prompts into commands announces it once the
// server is connected, and koinon-agentd waits for it before the first prompt. The driver contract
// lets every authenticated key list and get it. The gateway asks only the platform target
// gateway.ReadyTarget for it, never an org target, and needs no grant. Every other prompt is refused.

// readyTarget returns the platform target that publishes the readiness prompt, if the config has it.
func (s *snapshot) readyTarget() (gateway.MCPTarget, bool) {
	t, ok := s.targets[gateway.ReadyTarget]
	if !ok || t.Org != "" || t.Builtin != "" {
		return gateway.MCPTarget{}, false
	}
	return t, true
}

// listPrompts answers prompts/list: the readiness prompt as gateway.ReadyPromptExposed, or no prompt
// when the target does not publish it or does not answer.
func (g *Gateway) listPrompts(ctx context.Context, agent gateway.AgentRef) *mcp.ListPromptsResult {
	out := &mcp.ListPromptsResult{Prompts: []*mcp.Prompt{}}
	t, ok := g.snapOf(ctx).readyTarget()
	if !ok {
		return out
	}
	lctx, cancel := context.WithTimeout(ctx, g.o.ListTimeout)
	defer cancel()
	var found *mcp.Prompt
	err := g.onTarget(lctx, t, agent, func(u *upstream) error {
		found = nil
		for p, err := range u.session.Prompts(scrubCtx(lctx, agent), nil) {
			if err != nil {
				return err
			}
			if p.Name == gateway.ReadyPrompt {
				found = p
			}
		}
		return nil
	})
	if err != nil {
		g.o.Logger.WarnContext(scrubCtx(ctx, agent), "prompts/list: target skipped", "target", t.Name, "error", errKind(err))
		return out
	}
	if found != nil {
		// Only the fields a harness needs. The prompt takes no arguments.
		out.Prompts = append(out.Prompts, &mcp.Prompt{
			Name:        gateway.ReadyPromptExposed,
			Title:       g.o.Scrub.TextFor(agent.Org, found.Title),
			Description: g.o.Scrub.TextFor(agent.Org, found.Description),
		})
	}
	return out
}

// getPrompt answers prompts/get for the readiness prompt and refuses every other name.
func (g *Gateway) getPrompt(ctx context.Context, agent gateway.AgentRef, p *mcp.GetPromptParams) (*mcp.GetPromptResult, error) {
	t, ok := g.snapOf(ctx).readyTarget()
	if p == nil || p.Name != gateway.ReadyPromptExposed || !ok {
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "refused: no such prompt"}
	}
	lctx, cancel := context.WithTimeout(ctx, g.o.ListTimeout)
	defer cancel()
	var res *mcp.GetPromptResult
	err := g.onTarget(lctx, t, agent, func(u *upstream) error {
		var err error
		res, err = u.session.GetPrompt(scrubCtx(lctx, agent), &mcp.GetPromptParams{Name: gateway.ReadyPrompt})
		return err
	})
	if err != nil {
		g.o.Logger.WarnContext(scrubCtx(ctx, agent), "prompts/get: target failed", "target", t.Name, "error", errKind(err))
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "the prompt is not available"}
	}
	// Text messages only, scrubbed with the values of the agent's org.
	out := &mcp.GetPromptResult{Description: g.o.Scrub.TextFor(agent.Org, res.Description), Messages: []*mcp.PromptMessage{}}
	for _, m := range res.Messages {
		if c, ok := m.Content.(*mcp.TextContent); ok {
			out.Messages = append(out.Messages, &mcp.PromptMessage{Role: m.Role, Content: &mcp.TextContent{Text: g.o.Scrub.TextFor(agent.Org, c.Text)}})
		}
	}
	return out, nil
}

// onTarget runs fn on the upstream session of (t, agent). A failed session is evicted and fn runs once
// more on a new one, as for a tools/list. A JSON-RPC error is the target's answer and is returned.
func (g *Gateway) onTarget(ctx context.Context, t gateway.MCPTarget, agent gateway.AgentRef, fn func(*upstream) error) error {
	var lastErr error
	for range 2 {
		u, err := g.session(ctx, t, agent)
		if err != nil {
			return err
		}
		lastErr = fn(u)
		u.release()
		if lastErr == nil {
			return nil
		}
		var je *jsonrpc.Error
		if ctx.Err() != nil || errors.As(lastErr, &je) {
			break // the target answered, or the caller gave up: the session is not at fault
		}
		g.evict(u)
	}
	return lastErr
}
