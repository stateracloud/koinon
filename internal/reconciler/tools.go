// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/stateracloud/koinon/connectors"
	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/gateway"
)

// mcpTargets renders the configured tool servers as agentgateway MCP targets, sorted by name.
func (r *Reconciler) mcpTargets() []gateway.MCPTarget {
	var out []gateway.MCPTarget
	for name, url := range r.cfg.ToolServers {
		out = append(out, gateway.MCPTarget{Name: name, URL: url})
	}
	slices.SortFunc(out, func(a, b gateway.MCPTarget) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// activeConfig reads the agent's active version: its grants and their bindings.
func (r *Reconciler) activeConfig(ctx context.Context, a store.Agent) (agentconfig.Config, error) {
	if !a.ActiveVersionID.Valid {
		return agentconfig.Config{}, nil
	}
	v, err := r.q.GetAgentVersion(ctx, store.GetAgentVersionParams{OrgID: a.OrgID, ID: a.ActiveVersionID})
	if err != nil {
		return agentconfig.Config{}, fmt.Errorf("reconciler: read active version of agent %s: %w", execution.UUIDString(a.ID.Bytes), err)
	}
	cfg, err := agentconfig.Parse(v.Config)
	if err != nil {
		return agentconfig.Config{}, fmt.Errorf("reconciler: agent %s: %w", execution.UUIDString(a.ID.Bytes), err)
	}
	return cfg, nil
}

// ToolGrants is an agent's MCP allowlist, which agentgateway enforces as CEL (ADR 0008, 0042):
// for every approved grant whose connector type has a tool server, the tools its scopes expose
// unless its preset denies them, and Koinon's own ask_user when that server is configured. Ask-tier tools are listed too: calling one records an approval
// request instead of acting.
func ToolGrants(cfg agentconfig.Config, servers map[string]string) []gateway.ToolGrant {
	var out []gateway.ToolGrant
	if _, ok := servers[gateway.KoinonTarget]; ok {
		// Koinon's own tools need no grant: ask_user parks the run and puts a question in a person's
		// inbox, and costs nothing; cancel_task closes the agent's own task as canceled (ADR 0065).
		out = append(out, gateway.ToolGrant{Target: gateway.KoinonTarget, Tools: []string{gateway.ToolAskUser, gateway.ToolCancelTask}})
	}
	for _, g := range cfg.Grants {
		if _, ok := servers[g.Connector]; !ok {
			continue
		}
		ct, ok := connectors.Get(g.Connector)
		if !ok {
			continue
		}
		var names []string
		for _, td := range ct.Tools(g.Preset, g.Scopes) {
			names = append(names, td.Tool)
		}
		if len(names) > 0 {
			out = append(out, gateway.ToolGrant{Target: g.Connector, Tools: names})
		}
	}
	return out
}
