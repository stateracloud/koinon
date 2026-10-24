// SPDX-License-Identifier: Apache-2.0

package agentd

import (
	"context"
	"fmt"
	"strings"

	"github.com/stateracloud/koinon/internal/db/store"
)

// mcpNotice tells the agent, when a session starts, which of the MCP servers it may use are down or
// disabled (plan item MC1): their tools are withdrawn from its tool list until the server recovers,
// and an agent that is not told may keep looking for them. The text is Koinon's own; what a server
// said never goes in it.
func (r *runner) mcpNotice(ctx context.Context) (string, error) {
	rows, err := r.q.ListUnavailableMCPServersOfAgent(ctx, store.ListUnavailableMCPServersOfAgentParams{
		OrgID: r.run.OrgID, AgentID: r.run.AgentID,
	})
	if err != nil {
		return "", err
	}
	return mcpNoticeText(rows), nil
}

func mcpNoticeText(rows []store.ListUnavailableMCPServersOfAgentRow) string {
	if len(rows) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Note from Koinon: some of the MCP servers you are allowed are not available right now, so their tools are missing from your tool list this session. Do not look for them or try to work around it; carry on without them, and say in your answer what you could not do.")
	for _, s := range rows {
		reason := s.HealthReason.String
		if s.DisabledReason.Valid {
			reason = s.DisabledReason.String
		}
		if reason == "" {
			reason = "it is not responding"
		}
		fmt.Fprintf(&b, "\n- %s: %s", s.Name, reason)
	}
	return b.String()
}
