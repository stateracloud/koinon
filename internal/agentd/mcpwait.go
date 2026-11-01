// SPDX-License-Identifier: Apache-2.0

package agentd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/stateracloud/koinon/internal/db/store"
)

// DefaultMCPRetryDelay is how long a run waits before it checks again for an allowed MCP server that
// was missing: a probe interval (60 s) plus slack, so the prober has had one more pass.
const DefaultMCPRetryDelay = 75 * time.Second

// Run events for an allowed MCP server that is missing when the harness is about to start (issue
// #326). The real-model-test validity gate reads them to mark a run invalid instead of counting the
// missing tools as a model miss.
const (
	// mcpMissingKind is recorded at each miss. Payload: {attempt, servers: [{name, reason}], reason,
	// retry_in_seconds (first miss only)}.
	mcpMissingKind = "run.mcp_server_missing"
	// mcpRecoveredKind is recorded when the check after a miss finds every server serving. Payload:
	// {servers: [name]}.
	mcpRecoveredKind = "run.mcp_server_recovered"
)

type missingMCPServer struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// missingMCPServers lists the servers the agent may use that Koinon has found not serving tools (see
// ListMissingMCPServersOfAgent). It reads the control plane's health record, which the reconciler's
// prober refreshes every 60 s, so a server that drops between two probes (agentgateway runs failOpen
// and leaves it out silently) is not caught until the next probe marks it.
func (r *runner) missingMCPServers(ctx context.Context) ([]missingMCPServer, error) {
	rows, err := r.q.ListMissingMCPServersOfAgent(ctx, store.ListMissingMCPServersOfAgentParams{
		OrgID: r.run.OrgID, AgentID: r.run.AgentID,
	})
	if err != nil {
		return nil, err
	}
	out := make([]missingMCPServer, 0, len(rows))
	for _, s := range rows {
		reason := s.HealthReason.String
		if reason == "" {
			reason = "it is not responding"
		}
		if s.Health != string(store.McpHealthUnhealthy) {
			reason = fmt.Sprintf("its last %d probe(s) failed: %s", s.ConsecutiveFailures, reason)
		}
		out = append(out, missingMCPServer{Name: s.Name, Reason: reason})
	}
	return out, nil
}

func mcpServerNames(ss []missingMCPServer) []string {
	names := make([]string, len(ss))
	for i, s := range ss {
		names[i] = s.Name
	}
	return names
}

// waitMCPServers is the check before the harness starts, so the harness never connects to a gateway
// that is missing a server's tools without anyone knowing (issue #326; decision 2026-10-31). When an
// MCP server the agent may use is not serving tools it records run.mcp_server_missing and a trace
// attribute, waits MCPRetryDelay, and looks once more. If the servers are back it records
// run.mcp_server_recovered and the harness starts, connecting to a gateway that has them. On a second
// miss the run fails with a reason that names each server. A server an admin disabled or removed is
// not a miss: the note at session start (mcpnotice.go) already tells the agent.
func (r *runner) waitMCPServers(ctx context.Context) error {
	missing, err := r.missingMCPServers(ctx)
	if err != nil || len(missing) == 0 {
		return err
	}
	delay := r.p.cfg.MCPRetryDelay
	r.log.Warn("an MCP server the agent may use is not serving tools; checking again before the harness starts",
		"servers", mcpServerNames(missing), "retry_in", delay)
	r.tr.noteMCPMissing(mcpServerNames(missing), 1)
	if err := r.record(ctx, mcpMissingKind, map[string]any{
		"attempt": 1, "servers": missing, "retry_in_seconds": int(delay.Seconds()),
		"reason": "an MCP server the agent may use is not serving tools: " + describeMCPServers(missing),
	}); err != nil {
		return err
	}
	if !sleep(ctx, delay) {
		return ctx.Err()
	}
	again, err := r.missingMCPServers(ctx)
	if err != nil {
		return err
	}
	if len(again) == 0 {
		r.log.Info("the MCP servers are serving tools again", "servers", mcpServerNames(missing))
		return r.record(ctx, mcpRecoveredKind, map[string]any{"servers": mcpServerNames(missing)})
	}
	r.tr.noteMCPMissing(mcpServerNames(again), 2)
	reason := "an MCP server the agent may use is not serving tools, and still was not after " + delay.String() +
		": " + describeMCPServers(again) + ". Check the servers on the MCP servers page, then run the task again"
	if err := r.record(ctx, mcpMissingKind, map[string]any{"attempt": 2, "servers": again, "reason": reason}); err != nil {
		return err
	}
	return &failure{reason: reason}
}

func describeMCPServers(ss []missingMCPServer) string {
	parts := make([]string, len(ss))
	for i, s := range ss {
		parts[i] = fmt.Sprintf("%s (%s)", s.Name, s.Reason)
	}
	return strings.Join(parts, "; ")
}
