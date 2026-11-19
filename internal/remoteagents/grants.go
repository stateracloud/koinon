// SPDX-License-Identifier: Apache-2.0

package remoteagents

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
)

// AgentGrants returns the ids of the remote agents the Koinon agent may call. An agent with no grant calls
// no remote agent.
func (s *Service) AgentGrants(ctx context.Context, org, agent pgtype.UUID) ([]string, error) {
	q := store.New(s.pool)
	if _, err := q.GetAgent(ctx, store.GetAgentParams{OrgID: org, ID: agent}); err != nil {
		return nil, mapErr(err)
	}
	rows, err := q.ListAgentRemoteAgentsOfAgent(ctx, store.ListAgentRemoteAgentsOfAgentParams{OrgID: org, AgentID: agent})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.RemoteAgentID.String())
	}
	return out, nil
}

// SetAgentGrants replaces the remote agents the Koinon agent may call. It takes effect on the reconciler's
// next pass. Every remote agent must exist in the org.
func (s *Service) SetAgentGrants(ctx context.Context, org pgtype.UUID, actor audit.Actor, agent pgtype.UUID, remoteAgentIDs []string) ([]string, error) {
	seen := map[string]bool{}
	ids := make([]pgtype.UUID, len(remoteAgentIDs))
	for i, raw := range remoteAgentIDs {
		id, err := parseID(raw)
		if err != nil {
			return nil, invalid("remote agent %q does not exist", raw)
		}
		if seen[id.String()] {
			return nil, invalid("remote agent %q is listed twice", raw)
		}
		seen[id.String()] = true
		ids[i] = id
	}
	err := s.tx(ctx, func(q *store.Queries, b audit.Batch) error {
		ag, err := q.GetAgent(ctx, store.GetAgentParams{OrgID: org, ID: agent})
		if err != nil {
			return err
		}
		names := make([]any, 0, len(ids))
		for _, id := range ids {
			ra, err := q.GetRemoteAgent(ctx, store.GetRemoteAgentParams{OrgID: org, ID: id})
			if err != nil {
				return invalid("remote agent %q does not exist", id.String())
			}
			names = append(names, map[string]any{"remoteAgent": ra.Name, "remoteAgentId": id.String()})
		}
		before, err := q.ListAgentRemoteAgentsOfAgent(ctx, store.ListAgentRemoteAgentsOfAgentParams{OrgID: org, AgentID: agent})
		if err != nil {
			return err
		}
		if err := q.DeleteAgentRemoteAgents(ctx, store.DeleteAgentRemoteAgentsParams{OrgID: org, AgentID: agent}); err != nil {
			return err
		}
		for _, id := range ids {
			if err := q.InsertAgentRemoteAgent(ctx, store.InsertAgentRemoteAgentParams{OrgID: org, AgentID: agent, RemoteAgentID: id}); err != nil {
				return err
			}
		}
		was := make([]any, 0, len(before))
		for _, r := range before {
			was = append(was, r.RemoteAgentID.String())
		}
		b.Org(audit.Event{
			Org: org, Actor: actor, Action: "agent.remote_agents.set", TargetKind: "agent", TargetID: ag.ID.String(),
			Before: map[string]any{"remoteAgents": was}, After: map[string]any{"remoteAgents": names},
		})
		return nil
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return s.AgentGrants(ctx, org, agent)
}
