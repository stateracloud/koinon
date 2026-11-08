// SPDX-License-Identifier: Apache-2.0

package mcpservers

import (
	"context"
	"slices"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
)

// Allowance is one server an agent may use, optionally narrowed to some of its tools.
type Allowance struct {
	ServerID string `json:"serverId"`
	// Tools narrows the allowance; nil allows every tool of the server, including tools it adds later
	// (which enter the ask tier).
	Tools []string `json:"tools,omitempty"`
}

// AgentServers returns the servers the agent may use.
func (s *Service) AgentServers(ctx context.Context, org, agent pgtype.UUID) ([]Allowance, error) {
	q := store.New(s.pool)
	if _, err := q.GetAgent(ctx, store.GetAgentParams{OrgID: org, ID: agent}); err != nil {
		return nil, mapErr(err)
	}
	rows, err := q.ListAgentMCPServersOfAgent(ctx, store.ListAgentMCPServersOfAgentParams{OrgID: org, AgentID: agent})
	if err != nil {
		return nil, err
	}
	out := make([]Allowance, 0, len(rows))
	for _, r := range rows {
		out = append(out, Allowance{ServerID: r.ServerID.String(), Tools: r.Tools})
	}
	return out, nil
}

// SetAgentServers replaces the servers the agent may use. It takes effect on the reconciler's next
// pass. A server must exist in the org; a tool name must be well formed (a tool the server does not
// list yet is allowed, for a server that adds it later).
func (s *Service) SetAgentServers(ctx context.Context, org pgtype.UUID, actor audit.Actor, agent pgtype.UUID, allow []Allowance) ([]Allowance, error) {
	seen := map[string]bool{}
	ids := make([]pgtype.UUID, len(allow))
	for i, a := range allow {
		id, err := parseID(a.ServerID)
		if err != nil {
			return nil, invalid("server %q does not exist", a.ServerID)
		}
		if seen[a.ServerID] {
			return nil, invalid("server %q is listed twice", a.ServerID)
		}
		seen[a.ServerID] = true
		ids[i] = id
		for _, t := range a.Tools {
			if !toolPattern.MatchString(t) {
				return nil, invalid("%q is not a tool name", t)
			}
		}
		if a.Tools != nil && len(a.Tools) == 0 {
			return nil, invalid("a server narrowed to no tools allows nothing; leave the server out instead")
		}
	}
	err := s.tx(ctx, func(q *store.Queries, b audit.Batch) error {
		ag, err := q.GetAgent(ctx, store.GetAgentParams{OrgID: org, ID: agent})
		if err != nil {
			return err
		}
		names := map[string]string{}
		for i, a := range allow {
			srv, err := q.GetMCPServer(ctx, store.GetMCPServerParams{OrgID: org, ID: ids[i]})
			if err != nil {
				return invalid("server %q does not exist", a.ServerID)
			}
			names[a.ServerID] = srv.Name
		}
		before, err := q.ListAgentMCPServersOfAgent(ctx, store.ListAgentMCPServersOfAgentParams{OrgID: org, AgentID: agent})
		if err != nil {
			return err
		}
		if err := q.DeleteAgentMCPServers(ctx, store.DeleteAgentMCPServersParams{OrgID: org, AgentID: agent}); err != nil {
			return err
		}
		for i, a := range allow {
			tools := a.Tools
			if tools != nil {
				tools = slices.Clone(tools)
				slices.Sort(tools)
				tools = slices.Compact(tools)
			}
			if err := q.InsertAgentMCPServer(ctx, store.InsertAgentMCPServerParams{OrgID: org, AgentID: agent, ServerID: ids[i], Tools: tools}); err != nil {
				return err
			}
		}
		summary := func(rows []store.AgentMcpServer) []any {
			var out []any
			for _, r := range rows {
				out = append(out, map[string]any{"server": r.ServerID.String(), "tools": r.Tools})
			}
			return out
		}
		var after []any
		for i, a := range allow {
			after = append(after, map[string]any{"server": names[a.ServerID], "serverId": ids[i].String(), "tools": a.Tools})
		}
		b.Org(audit.Event{
			Org: org, Actor: actor, Action: "agent.mcp_servers", TargetKind: "agent", TargetID: ag.ID.String(),
			Before: map[string]any{"servers": summary(before)}, After: map[string]any{"servers": after},
		})
		return nil
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return s.AgentServers(ctx, org, agent)
}
