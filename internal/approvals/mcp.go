// SPDX-License-Identifier: Apache-2.0

package approvals

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/mcpask"
	"github.com/stateracloud/koinon/internal/mcpservers"
)

// executeMCP calls the remote MCP server with the exact arguments a human approved (ADR 0042, plan
// item MC1). The credential is opened here, in koinon-api, and sent to the server's URL only; the
// agent never holds it. The server and the tool are looked up again: a server deleted or disabled
// since, or a tool an admin moved to the never tier since, is not called.
func (s *Service) executeMCP(ctx context.Context, row store.ApprovalRequest) (map[string]any, error) {
	name, ok := strings.CutPrefix(row.Target, mcpask.ConnectorID+":")
	if !ok {
		return nil, permanent("target %q is not an MCP server", row.Target)
	}
	srv, err := s.q.GetServedMCPServerByName(ctx, store.GetServedMCPServerByNameParams{OrgID: row.OrgID, Name: name})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, permanent("the MCP server %s no longer exists or is disabled", name)
	}
	if err != nil {
		return nil, err
	}
	tool, err := s.q.GetMCPTool(ctx, store.GetMCPToolParams{OrgID: row.OrgID, ServerID: srv.ID, Name: row.Tool})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, permanent("the server %s has no tool %s", name, row.Tool)
	}
	if err != nil {
		return nil, err
	}
	if tool.Tier == store.McpTierNever {
		return nil, permanent("an admin has since moved %s of %s to the never tier", row.Tool, name)
	}
	if err := s.policy.CheckStrict(ctx, srv.Url); err != nil {
		return nil, permanentError{err}
	}
	var secret string
	if len(srv.Credential) > 0 {
		if s.keys == nil {
			return nil, permanent("no key-encryption keys are configured, so the server's credential cannot be opened")
		}
		if secret, err = mcpservers.OpenCredential(s.keys, row.OrgID, srv.ID, srv.Credential); err != nil {
			return nil, permanentError{err}
		}
	}
	res, err := mcpservers.CallTool(ctx, s.policy, mcpservers.CallRequest{
		URL: srv.Url, Transport: string(srv.Transport), Header: srv.CredentialHeader.String, Prefix: srv.CredentialPrefix,
		Value: secret, Tool: row.Tool, Arguments: []byte(row.ArgsJson), Idempotency: row.ID.String(),
	})
	if errors.Is(err, mcpservers.ErrPermanent) {
		return nil, permanentError{err}
	}
	return res, err
}
