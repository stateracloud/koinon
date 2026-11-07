// SPDX-License-Identifier: Apache-2.0

package agentd

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/mcpservers"
)

// MCPTools lists the MCP tools the gateway offers an agent right now, as the agent itself sees them
// (names with the gateway's <target>_ prefix). Its implementation holds or reaches the agent's own
// gateway key; koinon-agentd never sees that key (old issue 488, ADR 0057 amendment 2026-11-01), so
// the interface returns names only. mcpcheck.Remote asks koinon-api.
type MCPTools interface {
	Tools(ctx context.Context, org, agent pgtype.UUID) ([]string, error)
}

// DefaultMCPRetryDelay is how long a run waits before it checks again for an allowed MCP server that
// was missing: a probe interval (60 s) plus slack, so the prober has had one more pass.
const DefaultMCPRetryDelay = 75 * time.Second

// Run events for an allowed MCP server that is missing when the harness is about to start (issue
// old issue 326). The real-model-test validity gate reads them to mark a run invalid instead of counting the
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

// reasonNotListed is the reason of a server the gateway does not list to the agent. The gateway runs
// failOpen, so it does not say whether the server is down, rejected its credential, or the agent's
// key lacks the grant.
const reasonNotListed = "the gateway does not list its tools to this agent: the server is unreachable or the agent's key lacks the grant (the gateway does not say which)"

// missingMCPServers lists the servers the agent may use that are not serving tools: the control
// plane's health record (healthRecordMisses) and, when the live check is configured, the gateway's
// own list for the agent (liveMisses). The health record is refreshed every 60 s, so a server that
// drops between two probes (agentgateway runs failOpen and leaves it out silently) is caught by the
// live check only.
func (r *runner) missingMCPServers(ctx context.Context) ([]missingMCPServer, error) {
	out, err := r.healthRecordMisses(ctx)
	if err != nil {
		return nil, err
	}
	live, err := r.liveMisses(ctx)
	if err != nil {
		return nil, err
	}
	for _, m := range live {
		if !slices.ContainsFunc(out, func(o missingMCPServer) bool { return o.Name == m.Name }) {
			out = append(out, m)
		}
	}
	slices.SortFunc(out, func(a, b missingMCPServer) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// liveMisses asks the gateway, as the agent (the agent's own key, held by koinon-api, never by
// agentd), which tools it lists, and returns each allowed, served server of which none of its tools
// appear. A check that cannot be made (koinon-api or the gateway unreachable, no key yet) is logged
// and counts as no miss: the harness's own connection would fail the same way, and the health record
// still applies.
func (r *runner) liveMisses(ctx context.Context) ([]missingMCPServer, error) {
	if r.p.cfg.MCPTools == nil {
		return nil, nil
	}
	rows, err := r.q.ListMCPServersToCheckLive(ctx, store.ListMCPServersToCheckLiveParams{
		OrgID: r.run.OrgID, AgentID: r.run.AgentID,
	})
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	listed, err := r.p.cfg.MCPTools.Tools(ctx, r.run.OrgID, r.run.AgentID)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		r.log.Warn("the live check of the agent's MCP tools could not be made; relying on the health record", "err", err)
		return nil, nil
	}
	return liveMissing(rows, listed), nil
}

// liveMissing compares what the gateway listed with the tools each server should offer. Tools carry
// the gateway's prefix: <org>-<server>_<tool> (auto tier) or <org>-<server>-ask_<tool> (ask tier).
//
// Reachability is judged by the auto target only: it points at the remote server, so its tools are
// listed only when the server answered. The ask target points at koinon-api's /mcp-ask proxy, which
// lists the tools from Postgres and never contacts the remote server, so a listed ask tool proves
// the grant but not that the server is up. A server with auto-tier tools therefore needs at least one
// of them listed. A server with ask-tier tools only gets the ask tools as a grant check; it is not
// proven reachable (the health record still applies).
func liveMissing(rows []store.ListMCPServersToCheckLiveRow, listed []string) []missingMCPServer {
	var out []missingMCPServer
	for _, s := range rows {
		var auto, ask []string
		for _, t := range s.AutoTools {
			if mcpservers.ToolNameProblem(s.OrgSlug, s.Name, t) == "" {
				auto = append(auto, mcpservers.TargetName(s.OrgSlug, s.Name)+"_"+t)
			}
		}
		for _, t := range s.AskTools {
			if mcpservers.ToolNameProblem(s.OrgSlug, s.Name, t) == "" {
				ask = append(ask, mcpservers.AskTargetName(s.OrgSlug, s.Name)+"_"+t)
			}
		}
		want := auto
		if len(auto) == 0 {
			want = ask
		}
		if len(want) == 0 || slices.ContainsFunc(want, func(w string) bool { return slices.Contains(listed, w) }) {
			continue
		}
		out = append(out, missingMCPServer{Name: s.Name, Reason: reasonNotListed})
	}
	return out
}

// healthRecordMisses lists the servers the control plane's health record says are not serving tools
// (see ListMissingMCPServersOfAgent).
func (r *runner) healthRecordMisses(ctx context.Context) ([]missingMCPServer, error) {
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
// that is missing a server's tools without anyone knowing (old issue 326; decision 2026-10-31). When an
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
	if err := r.own.Record(ctx, mcpMissingKind, map[string]any{
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
		return r.own.Record(ctx, mcpRecoveredKind, map[string]any{"servers": mcpServerNames(missing)})
	}
	r.tr.noteMCPMissing(mcpServerNames(again), 2)
	reason := "an MCP server the agent may use is not serving tools, and still was not after " + delay.String() +
		": " + describeMCPServers(again) + ". Check the servers on the MCP servers page, then run the task again"
	if err := r.own.Record(ctx, mcpMissingKind, map[string]any{"attempt": 2, "servers": again, "reason": reason}); err != nil {
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
