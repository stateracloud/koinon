// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/procoduck/koinon/connectors"
	"github.com/procoduck/koinon/internal/agentconfig"
	"github.com/procoduck/koinon/internal/db/store"
	"github.com/procoduck/koinon/internal/execution"
	"github.com/procoduck/koinon/internal/gateway"
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
// unless its preset denies them. Ask-tier tools are listed too: calling one records an approval
// request instead of acting.
func ToolGrants(cfg agentconfig.Config, servers map[string]string) []gateway.ToolGrant {
	var out []gateway.ToolGrant
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
