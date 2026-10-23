// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"errors"
	"slices"
	"strings"

	connect "connectrpc.com/connect/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/db/store"
	agentv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/agent/v1alpha1"
	templatev1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/template/v1alpha1"
	"github.com/stateracloud/koinon/internal/scheduler"
	"github.com/stateracloud/koinon/templates"
)

var agentStatusToProto = map[store.AgentStatus]agentv1alpha1.AgentStatus{
	store.AgentStatusDraft:    agentv1alpha1.AgentStatus_AGENT_STATUS_DRAFT,
	store.AgentStatusActive:   agentv1alpha1.AgentStatus_AGENT_STATUS_ACTIVE,
	store.AgentStatusArchived: agentv1alpha1.AgentStatus_AGENT_STATUS_ARCHIVED,
}

// agentView renders an agent with its latest and active versions.
func (s *Server) agentView(ctx context.Context, q *store.Queries, a store.Agent, slugs map[pgtype.UUID]string) (*agentv1alpha1.Agent, error) {
	out := &agentv1alpha1.Agent{
		Id:            idString(a.ID),
		OrgId:         idString(a.OrgID),
		DisplayName:   a.DisplayName,
		Status:        agentStatusToProto[a.Status],
		Flavor:        flavorProto(string(a.Flavor)),
		TemplateId:    idString(a.TemplateID),
		LatestVersion: a.LatestVersion,
		CreatedAt:     tsProto(a.CreatedAt),
		ActivatedAt:   tsProto(a.ActivatedAt),
		ArchivedAt:    tsProto(a.ArchivedAt),
	}
	if a.TemplateID.Valid {
		slug, ok := slugs[a.TemplateID]
		if !ok {
			t, err := q.GetTemplate(ctx, a.TemplateID)
			if err != nil {
				return nil, err
			}
			slug = t.Slug
			if slugs != nil {
				slugs[a.TemplateID] = slug
			}
		}
		out.TemplateSlug = slug
	}
	versions, err := q.ListAgentVersions(ctx, store.ListAgentVersionsParams{OrgID: a.OrgID, AgentID: a.ID})
	if err != nil {
		return nil, err
	}
	for _, v := range versions {
		cfg, err := agentconfig.Parse(v.Config)
		if err != nil {
			return nil, err
		}
		if v.Version == a.LatestVersion {
			out.ProposedGrants = agentGrantsProto(cfg)
			out.Instructions = cfg.Instructions
			out.Model = cfg.Model
			wall, tokens := cfg.RunCaps()
			out.RunLimits = &templatev1alpha1.RunLimits{MaxDurationSeconds: int64(wall.Seconds()), MaxTokens: tokens}
		}
		if v.ID == a.ActiveVersionID {
			out.ActiveVersion = v.Version
			out.ApprovedGrants = agentGrantsProto(cfg)
		}
	}
	return out, nil
}

// CreateAgentFromTemplate implements AgentService.
func (s *Server) CreateAgentFromTemplate(ctx context.Context, req *agentv1alpha1.CreateAgentFromTemplateRequest) (*agentv1alpha1.CreateAgentFromTemplateResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleMember)
	if err != nil {
		return nil, err
	}
	templateID, err := parseID("template_id", req.GetTemplateId())
	if err != nil {
		return nil, err
	}
	row, err := s.q.GetTemplate(ctx, templateID)
	if err != nil {
		return nil, s.dbError(err, "template")
	}
	t, err := decodeTemplate(row)
	if err != nil {
		return nil, s.dbError(err, "template")
	}
	flavor := store.AgentFlavor(t.Spec.Flavors.Default)
	if req.GetFlavor() != templatev1alpha1.Flavor_FLAVOR_UNSPECIFIED {
		f, ok := flavorFromProto(req.GetFlavor())
		if !ok || !slices.Contains(t.Spec.Flavors.Allowed, templates.Flavor(f)) {
			return nil, connect.NewError(connect.CodeInvalidArgument, "flavor is not allowed by the template")
		}
		flavor = f
	}
	name := strings.TrimSpace(req.GetDisplayName())
	if name == "" {
		name = t.Metadata.DisplayName
	}
	if len(name) > 200 {
		return nil, connect.NewError(connect.CodeInvalidArgument, "display_name is longer than 200 characters")
	}
	bindings, err := s.bindGrants(ctx, c.org, t, req.GetGrants())
	if err != nil {
		return nil, err
	}
	cfg := agentconfig.FromTemplate(t, row.ContentHash)
	cfg.Bindings = bindings
	config, err := cfg.Marshal()
	if err != nil {
		return nil, s.dbError(err, "agent config")
	}

	var out *agentv1alpha1.Agent
	err = s.inTx(ctx, func(q *store.Queries) error {
		a, err := q.CreateAgent(ctx, store.CreateAgentParams{
			OrgID: c.org, DisplayName: name, Flavor: flavor, TemplateID: row.ID, CreatedBy: c.actor(),
		})
		if err != nil {
			return err
		}
		if _, err := q.CreateAgentVersion(ctx, store.CreateAgentVersionParams{
			OrgID: c.org, AgentID: a.ID, Config: config, CreatedBy: c.actor(),
		}); err != nil {
			return err
		}
		if a, err = q.GetAgent(ctx, store.GetAgentParams{OrgID: c.org, ID: a.ID}); err != nil {
			return err
		}
		out, err = s.agentView(ctx, q, a, nil)
		return err
	})
	if err != nil {
		return nil, s.dbError(err, "agent")
	}
	return &agentv1alpha1.CreateAgentFromTemplateResponse{Agent: out}, nil
}

// GetAgent implements AgentService.
func (s *Server) GetAgent(ctx context.Context, req *agentv1alpha1.GetAgentRequest) (*agentv1alpha1.GetAgentResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleViewer)
	if err != nil {
		return nil, err
	}
	id, err := parseID("agent_id", req.GetAgentId())
	if err != nil {
		return nil, err
	}
	a, err := s.q.GetAgent(ctx, store.GetAgentParams{OrgID: c.org, ID: id})
	if err != nil {
		return nil, s.dbError(err, "agent")
	}
	out, err := s.agentView(ctx, s.q, a, nil)
	if err != nil {
		return nil, s.dbError(err, "agent")
	}
	return &agentv1alpha1.GetAgentResponse{Agent: out}, nil
}

// ListAgents implements AgentService.
func (s *Server) ListAgents(ctx context.Context, req *agentv1alpha1.ListAgentsRequest) (*agentv1alpha1.ListAgentsResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleViewer)
	if err != nil {
		return nil, err
	}
	agents, err := s.q.ListAgents(ctx, c.org)
	if err != nil {
		return nil, s.dbError(err, "agents")
	}
	res := &agentv1alpha1.ListAgentsResponse{}
	slugs := map[pgtype.UUID]string{}
	for _, a := range agents {
		v, err := s.agentView(ctx, s.q, a, slugs)
		if err != nil {
			return nil, s.dbError(err, "agent")
		}
		res.Agents = append(res.Agents, v)
	}
	return res, nil
}

// ActivateAgent implements AgentService: an admin or owner activates the
// draft's latest version and so approves the grants it proposes (ADR 0020).
// The approved grants are the active version's; a later grant change adds a
// version and returns the agent to draft.
func (s *Server) ActivateAgent(ctx context.Context, req *agentv1alpha1.ActivateAgentRequest) (*agentv1alpha1.ActivateAgentResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	id, err := parseID("agent_id", req.GetAgentId())
	if err != nil {
		return nil, err
	}
	var out *agentv1alpha1.Agent
	err = s.inTx(ctx, func(q *store.Queries) error {
		a, err := q.GetAgent(ctx, store.GetAgentParams{OrgID: c.org, ID: id})
		if err != nil {
			return err
		}
		switch a.Status {
		case store.AgentStatusActive:
			return connect.NewError(connect.CodeFailedPrecondition, "agent is already active")
		case store.AgentStatusArchived:
			return connect.NewError(connect.CodeFailedPrecondition, "agent is archived")
		}
		if req.GetVersion() != a.LatestVersion {
			return connect.NewError(connect.CodeFailedPrecondition,
				"version does not match the agent's latest version; review it again")
		}
		versions, err := q.ListAgentVersions(ctx, store.ListAgentVersionsParams{OrgID: c.org, AgentID: a.ID})
		if err != nil {
			return err
		}
		i := slices.IndexFunc(versions, func(v store.AgentVersion) bool { return v.Version == a.LatestVersion })
		if i < 0 {
			return connect.NewError(connect.CodeFailedPrecondition, "agent has no version to activate")
		}
		a, err = q.ActivateAgent(ctx, store.ActivateAgentParams{
			OrgID: c.org, ID: a.ID, VersionID: versions[i].ID, ActivatedBy: c.actor(),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return connect.NewError(connect.CodeAborted, "agent changed concurrently; retry")
		}
		if err != nil {
			return err
		}
		out, err = s.agentView(ctx, q, a, nil)
		return err
	})
	if err != nil {
		return nil, s.dbError(err, "agent")
	}
	s.log.Info("agent activated", "org", c.org.String(), "agent", out.GetId(),
		"version", out.GetActiveVersion(), "by", c.actor().String())
	return &agentv1alpha1.ActivateAgentResponse{Agent: out}, nil
}

// ArchiveAgent implements AgentService. Unfinished runs of the agent are
// canceled, each with a ticket event.
func (s *Server) ArchiveAgent(ctx context.Context, req *agentv1alpha1.ArchiveAgentRequest) (*agentv1alpha1.ArchiveAgentResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	id, err := parseID("agent_id", req.GetAgentId())
	if err != nil {
		return nil, err
	}
	var out *agentv1alpha1.Agent
	err = s.inTx(ctx, func(q *store.Queries) error {
		a, err := q.ArchiveAgent(ctx, store.ArchiveAgentParams{OrgID: c.org, ID: id})
		if errors.Is(err, pgx.ErrNoRows) {
			if _, err := q.GetAgent(ctx, store.GetAgentParams{OrgID: c.org, ID: id}); err != nil {
				return err
			}
			return connect.NewError(connect.CodeFailedPrecondition, "agent is already archived")
		}
		if err != nil {
			return err
		}
		if _, err := scheduler.CancelForAgent(ctx, q, c.org, a.ID, scheduler.Actor{UserID: c.actor()}); err != nil {
			return err
		}
		out, err = s.agentView(ctx, q, a, nil)
		return err
	})
	if err != nil {
		return nil, s.dbError(err, "agent")
	}
	return &agentv1alpha1.ArchiveAgentResponse{Agent: out}, nil
}
