// SPDX-License-Identifier: Apache-2.0

package approvals

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/mcpask"
	"github.com/stateracloud/koinon/internal/mcpservers"
)

// executeMCP makes the approved call to the remote MCP server through the gateway, with the org's
// executor key (ADR 0042, 0057): the gateway applies its egress policy and injects the server's
// credential, which this process never holds. Everything the approval was bound to is checked again:
// the server (ID and URL), the tool (still offered) and its published content, the tier and the
// agent's allowance. The arguments are sent exactly as approved.
//
// The tier check refuses only the never tier. An approval of an ask-tier call whose tool has since
// moved to auto still runs, as does a leak guard card of an auto-tier call (ADR 0091) whose tool has
// since moved to ask: a person approved this exact call, which is more than either tier asks. The
// gateway opens the executor's key to the auto and ask tiers alike (gatewayplan, applyMCP).
//
// Only a failure before the call was sent is retried. After that the outcome may be unknown and the
// request fails; it is never run twice.
func (s *Service) executeMCP(ctx context.Context, row store.ApprovalRequest) (map[string]any, error) {
	name, ok := strings.CutPrefix(row.Target, mcpask.ConnectorID+":")
	if !ok {
		return nil, permanent("target %q is not an MCP server", row.Target)
	}
	if s.gwURL == "" || s.execKey == nil {
		return nil, permanent("remote MCP calls are not configured: no gateway endpoint or executor keys")
	}
	var bound mcpservers.CallBinding
	if err := json.Unmarshal(row.CallBinding, &bound); err != nil || bound.ServerID == "" || bound.ContentHash == "" {
		return nil, permanent("the request has no binding to a server and tool version")
	}
	srv, err := s.q.GetServedMCPServerByName(ctx, store.GetServedMCPServerByNameParams{OrgID: row.OrgID, Name: name})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, permanent("the MCP server %s no longer exists or is disabled", name)
	}
	if err != nil {
		return nil, err
	}
	if srv.ID.String() != bound.ServerID {
		return nil, permanent("the MCP server %s is not the one the request was made for", name)
	}
	if mcpservers.URLHash(srv.Url) != bound.URLHash {
		return nil, permanent("the URL of the MCP server %s changed since the request was made", name)
	}
	tool, err := s.q.GetMCPTool(ctx, store.GetMCPToolParams{OrgID: row.OrgID, ServerID: srv.ID, Name: row.Tool})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, permanent("the server %s has no tool %s", name, row.Tool)
	}
	if err != nil {
		return nil, err
	}
	if !tool.Present {
		return nil, permanent("the server %s no longer offers %s", name, row.Tool)
	}
	if tool.ContentHash != bound.ContentHash {
		return nil, permanent("the description or input schema of %s changed since the request was made; ask again", row.Tool)
	}
	if tool.Tier == store.McpTierNever {
		return nil, permanent("an admin has since moved %s of %s to the never tier", row.Tool, name)
	}
	allowed, err := s.q.GetAgentMCPServer(ctx, store.GetAgentMCPServerParams{OrgID: row.OrgID, AgentID: row.AgentID, ServerID: srv.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, permanent("the agent is no longer allowed to use the server %s", name)
	}
	if err != nil {
		return nil, err
	}
	if allowed.Tools != nil && !slices.Contains(allowed.Tools, row.Tool) {
		return nil, permanent("the agent is no longer allowed to use %s of %s", row.Tool, name)
	}
	slug, err := s.q.GetOrgSlug(ctx, row.OrgID)
	if err != nil {
		return nil, err
	}
	key, err := s.execKey.Key(ctx, row.OrgID)
	if err != nil {
		return nil, err // transient: the reconciler may not have written the key yet
	}
	res, err := mcpservers.CallTool(ctx, mcpservers.CallRequest{
		GatewayMCPURL: s.gwURL, Key: key, Target: mcpservers.TargetName(slug, name), Tool: row.Tool,
		Arguments: json.RawMessage(row.ArgsJson), Idempotency: row.ID.String(),
	})
	switch {
	case err == nil:
		// The call left the org: count its bytes (lab run evidence, #926). Evidence, not a control: a
		// failed write is logged and the result is returned.
		if err := s.q.InsertApprovedExecutionDecision(ctx, store.InsertApprovedExecutionDecisionParams{
			OrgID: row.OrgID, AgentID: row.AgentID, RunID: row.RunID, Server: name, Tool: row.Tool,
		}); err != nil {
			s.log.Error("approvals: could not record the approved call's bytes", append(attrs(row), "err", err)...)
		}
		if s.onMCP != nil {
			raw, err := json.Marshal(res)
			if err == nil {
				err = s.onMCP(ctx, row, srv, raw)
			}
			if err != nil {
				s.log.Error("approvals: the leak guard could not record an MCP result", append(attrs(row), "err", err)...)
				return nil, permanent("the call ran, but the leak guard could not record its result, so the result is withheld")
			}
		}
		return res, nil
	case errors.Is(err, mcpservers.ErrNotSent):
		return nil, err // retried by the sweeper
	default:
		return nil, permanentError{err}
	}
}
