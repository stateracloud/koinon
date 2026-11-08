// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	connect "connectrpc.com/connect/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/agentmodel"
	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	agentv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/agent/v1alpha1"
	templatev1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/template/v1alpha1"
	"github.com/stateracloud/koinon/internal/money"
	"github.com/stateracloud/koinon/internal/runlimits"
	"github.com/stateracloud/koinon/internal/runoutcome"
	"github.com/stateracloud/koinon/templates"
)

var agentStatusToProto = map[store.AgentStatus]agentv1alpha1.AgentStatus{
	store.AgentStatusDraft:    agentv1alpha1.AgentStatus_AGENT_STATUS_DRAFT,
	store.AgentStatusActive:   agentv1alpha1.AgentStatus_AGENT_STATUS_ACTIVE,
	store.AgentStatusArchived: agentv1alpha1.AgentStatus_AGENT_STATUS_ARCHIVED,
}

// agentView renders an agent with its latest and active versions.
func (s *Server) agentView(ctx context.Context, q *store.Queries, a store.Agent, slugs map[pgtype.UUID]string, archived map[pgtype.UUID]pgtype.Timestamptz) (*agentv1alpha1.Agent, error) {
	out := &agentv1alpha1.Agent{
		Id:              idString(a.ID),
		OrgId:           idString(a.OrgID),
		DisplayName:     a.DisplayName,
		Status:          agentStatusToProto[a.Status],
		Flavor:          flavorProto(string(a.Flavor)),
		TemplateId:      idString(a.TemplateID),
		LatestVersion:   a.LatestVersion,
		CreatedAt:       tsProto(a.CreatedAt),
		ActivatedAt:     tsProto(a.ActivatedAt),
		ArchivedAt:      tsProto(a.ArchivedAt),
		UnhealthyAt:     tsProto(a.UnhealthyAt),
		UnhealthyReason: a.UnhealthyReason.String,
		ModelSetting:    a.Model.String,
	}
	if archived != nil {
		out.SandboxArchivedAt = tsProto(archived[a.ID])
	} else if sb, err := q.GetAgentSandbox(ctx, store.GetAgentSandboxParams{OrgID: a.OrgID, AgentID: a.ID}); err == nil {
		out.SandboxArchivedAt = tsProto(sb.ArchivedAt)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
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
			out.ProposedTriggers = triggersProto(cfg.Triggers)
			out.Instructions = cfg.Instructions
			out.Model = cfg.Model
			out.Ephemeral = agentconfig.IsEphemeral(cfg)
			// The limits a run queued now gets: the version's, with the admin's override on top.
			o, err := runlimits.GetOverride(ctx, q, a.OrgID, a.ID)
			if err != nil {
				return nil, err
			}
			out.RunLimits = runLimitsProto(o.Apply(cfg.RunLimits()))
		}
		if v.ID == a.ActiveVersionID {
			out.ActiveVersion = v.Version
			out.ApprovedGrants = agentGrantsProto(cfg)
			out.ApprovedTriggers = triggersProto(cfg.Triggers)
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
	if req.Ephemeral != nil {
		cfg.Ephemeral = req.GetEphemeral()
	}
	config, err := cfg.Marshal()
	if err != nil {
		return nil, s.dbError(err, "agent config")
	}

	var out *agentv1alpha1.Agent
	err = s.inAuditTx(ctx, func(q *store.Queries, b audit.Batch) error {
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
		// The template's spendUSD is only the default: from here the limit is the agent's own,
		// which org admins change (plan item BG1).
		if err := q.InsertAgentBudgetDefault(ctx, store.InsertAgentBudgetDefaultParams{
			OrgID: c.org, AgentID: a.ID, DailyLimit: money.FromCents(t.Spec.Limits.SpendUSD.Cents()).Numeric(), UpdatedBy: "template",
		}); err != nil {
			return err
		}
		if a, err = q.GetAgent(ctx, store.GetAgentParams{OrgID: c.org, ID: a.ID}); err != nil {
			return err
		}
		out, err = s.agentView(ctx, q, a, nil, nil)
		if err != nil {
			return err
		}
		b.Org(audit.Event{
			Org: c.org, Actor: c.auditActor(), Action: "agent.create", TargetKind: "agent", TargetID: a.ID.String(),
			After: map[string]any{"displayName": name, "flavor": string(flavor), "templateId": row.ID.String()},
		})
		return nil
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
	out, err := s.agentView(ctx, s.q, a, nil, nil)
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
	rows, err := s.q.ListArchivedAgentSandboxes(ctx, c.org)
	if err != nil {
		return nil, s.dbError(err, "agents")
	}
	archived := map[pgtype.UUID]pgtype.Timestamptz{}
	for _, r := range rows {
		archived[r.AgentID] = r.ArchivedAt
	}
	for _, a := range agents {
		v, err := s.agentView(ctx, s.q, a, slugs, archived)
		if err != nil {
			return nil, s.dbError(err, "agent")
		}
		res.Agents = append(res.Agents, v)
	}
	return res, nil
}

// ActivateAgent implements AgentService: an admin or owner activates the
// draft's latest version and so approves the grants it proposes (ADR 0020), or moves an active
// agent to its newer proposed version (ADR 0066).
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
	restarting := 0
	err = s.inAuditTx(ctx, func(q *store.Queries, b audit.Batch) error {
		a, err := q.GetAgent(ctx, store.GetAgentParams{OrgID: c.org, ID: id})
		if err != nil {
			return err
		}
		if a.Status == store.AgentStatusArchived {
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
		newCfg, err := agentconfig.Parse(versions[i].Config)
		if err != nil {
			return err
		}
		var oldCfg agentconfig.Config
		if j := slices.IndexFunc(versions, func(v store.AgentVersion) bool { return v.ID == a.ActiveVersionID }); j >= 0 {
			if oldCfg, err = agentconfig.Parse(versions[j].Config); err != nil {
				return err
			}
		}
		// An active agent can move to a newer, proposed version (SetAgentTriggers); it cannot be
		// activated again with the version it already runs.
		if a.Status == store.AgentStatusActive && versions[i].ID == a.ActiveVersionID {
			return connect.NewError(connect.CodeFailedPrecondition, "agent is already active")
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
		if req.GetApplyToOpenTickets() {
			// Open tickets of the agent start a fresh session, with a handoff, at their next run.
			marked, err := q.RequestSessionRestartForAgent(ctx, store.RequestSessionRestartForAgentParams{OrgID: c.org, AgentID: a.ID})
			if err != nil {
				return err
			}
			restarting = len(marked)
		}
		out, err = s.agentView(ctx, q, a, nil, nil)
		if err != nil {
			return err
		}
		b.Org(audit.Event{
			Org: c.org, Actor: c.auditActor(), Action: "agent.activate", TargetKind: "agent", TargetID: a.ID.String(),
			After: map[string]any{
				"version": versions[i].Version, "triggers": triggerAudit(newCfg.Triggers, false),
				"applyToOpenTickets": req.GetApplyToOpenTickets(), "ticketsRestarting": restarting,
			},
			Before: map[string]any{"triggers": triggerAudit(oldCfg.Triggers, false)},
		})
		return nil
	})
	if err != nil {
		return nil, s.dbError(err, "agent")
	}
	s.log.Info("agent activated", "org", c.org.String(), "agent", out.GetId(),
		"version", out.GetActiveVersion(), "by", c.actor().String(),
		"apply_to_open_tickets", req.GetApplyToOpenTickets(), "tickets_restarting", restarting)
	return &agentv1alpha1.ActivateAgentResponse{Agent: out, TicketsRestarting: int32(restarting)}, nil
}

// triggerAudit renders triggers for an audit event; instruction text only when asked for.
func triggerAudit(ts []templates.Trigger, withInstruction bool) []map[string]any {
	out := make([]map[string]any, 0, len(ts))
	for _, t := range ts {
		m := map[string]any{"id": t.ID, "type": string(t.Type)}
		if t.Filter.Cron != "" {
			m["cron"], m["timezone"] = t.Filter.Cron, t.Filter.Timezone
		}
		if withInstruction {
			m["instruction"] = t.Instruction
		}
		if t.Notify != nil {
			m["notify"] = t.Notify
		}
		out = append(out, m)
	}
	return out
}

// SetAgentTriggers implements AgentService: a new version with the given triggers and everything
// else carried over from the latest one. The agent keeps running its active version; the new
// triggers take effect when ActivateAgent activates the new version (ADR 0066).
func (s *Server) SetAgentTriggers(ctx context.Context, req *agentv1alpha1.SetAgentTriggersRequest) (*agentv1alpha1.SetAgentTriggersResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	id, err := parseID("agent_id", req.GetAgentId())
	if err != nil {
		return nil, err
	}
	triggers, err := triggersFromProto(req.GetTriggers())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	var out *agentv1alpha1.Agent
	err = s.inAuditTx(ctx, func(q *store.Queries, b audit.Batch) error {
		// A person who is asked must be a member who can answer (member or higher).
		for i, t := range triggers {
			if t.Notify == nil || t.Notify.Kind != templates.NotifyUser {
				continue
			}
			m, err := q.FindOrgMemberByIdent(ctx, store.FindOrgMemberByIdentParams{OrgID: c.org, Ident: t.Notify.User})
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && m.Role == store.OrgRoleViewer) {
				return connect.NewError(connect.CodeInvalidArgument,
					fmt.Sprintf("triggers[%d]: notify: %s is not a member who can answer questions", i, t.Notify.User))
			}
			if err != nil {
				return err
			}
		}
		// The row lock makes the base_version check and the new version one step: a concurrent
		// proposal waits, then fails the check against the version this one created.
		a, err := q.GetAgentForUpdate(ctx, store.GetAgentForUpdateParams{OrgID: c.org, ID: id})
		if err != nil {
			return err
		}
		if a.Status == store.AgentStatusArchived {
			return connect.NewError(connect.CodeFailedPrecondition, "agent is archived")
		}
		if req.GetBaseVersion() != a.LatestVersion {
			return connect.NewError(connect.CodeFailedPrecondition,
				"base_version does not match the agent's latest version; review it again")
		}
		versions, err := q.ListAgentVersions(ctx, store.ListAgentVersionsParams{OrgID: c.org, AgentID: a.ID})
		if err != nil {
			return err
		}
		i := slices.IndexFunc(versions, func(v store.AgentVersion) bool { return v.Version == a.LatestVersion })
		if i < 0 {
			return connect.NewError(connect.CodeFailedPrecondition, "agent has no version to base the change on")
		}
		cfg, err := agentconfig.Parse(versions[i].Config)
		if err != nil {
			return err
		}
		before := cfg.Triggers
		cfg.Triggers = triggers
		raw, err := cfg.Marshal()
		if err != nil {
			return err
		}
		v, err := q.CreateAgentVersion(ctx, store.CreateAgentVersionParams{
			OrgID: c.org, AgentID: a.ID, Config: raw, CreatedBy: c.actor(),
		})
		if err != nil {
			return err
		}
		if a, err = q.GetAgent(ctx, store.GetAgentParams{OrgID: c.org, ID: a.ID}); err != nil {
			return err
		}
		if out, err = s.agentView(ctx, q, a, nil, nil); err != nil {
			return err
		}
		b.Org(audit.Event{
			Org: c.org, Actor: c.auditActor(), Action: "agent.triggers.propose", TargetKind: "agent", TargetID: a.ID.String(),
			Before: map[string]any{"version": versions[i].Version, "triggers": triggerAudit(before, true)},
			After:  map[string]any{"version": v.Version, "triggers": triggerAudit(triggers, true)},
		})
		return nil
	})
	if err != nil {
		return nil, s.dbError(err, "agent")
	}
	return &agentv1alpha1.SetAgentTriggersResponse{Agent: out}, nil
}

// ResumeAgent implements AgentService: an admin or owner clears an agent's unhealthy state (ADR 0054).
// The agent's stuck sandbox, if it is still there, is recreated, and the count of recreations
// starts again.
func (s *Server) ResumeAgent(ctx context.Context, req *agentv1alpha1.ResumeAgentRequest) (*agentv1alpha1.ResumeAgentResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	id, err := parseID("agent_id", req.GetAgentId())
	if err != nil {
		return nil, err
	}
	var out *agentv1alpha1.Agent
	err = s.inAuditTx(ctx, func(q *store.Queries, b audit.Batch) error {
		before, err := q.GetAgent(ctx, store.GetAgentParams{OrgID: c.org, ID: id})
		if err != nil {
			return err
		}
		a, err := q.ResumeAgent(ctx, store.ResumeAgentParams{OrgID: c.org, ID: id})
		if errors.Is(err, pgx.ErrNoRows) {
			return connect.NewError(connect.CodeFailedPrecondition, "agent is not unhealthy")
		}
		if err != nil {
			return err
		}
		// Every queued run of the agent starts a fresh sandbox wait: without this a resumed agent's
		// runs would hit the sandbox wait cap again at once (ADR 0066 amendment 2026-10-28).
		if _, err := q.ClearSandboxWaitForAgent(ctx, store.ClearSandboxWaitForAgentParams{OrgID: c.org, AgentID: id}); err != nil {
			return err
		}
		// The sandbox that kept getting stuck is recreated; the reconciler is woken below.
		if _, err := q.MarkSandboxUnhealthy(ctx, store.MarkSandboxUnhealthyParams{
			OrgID: c.org, AgentID: id, Reason: "the agent was resumed after its sandbox kept getting stuck",
		}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		out, err = s.agentView(ctx, q, a, nil, nil)
		if err != nil {
			return err
		}
		b.Org(audit.Event{
			Org: c.org, Actor: c.auditActor(), Action: "agent.resume", TargetKind: "agent", TargetID: id.String(),
			Before: map[string]any{"unhealthyReason": before.UnhealthyReason.String},
			After:  map[string]any{"unhealthy": false},
		})
		return nil
	})
	if err != nil {
		return nil, s.dbError(err, "agent")
	}
	if err := s.q.NotifyReconciler(ctx, c.org); err != nil {
		s.log.Warn("notifying the reconciler after resuming an agent failed", "err", err)
	}
	s.log.Info("agent resumed", "org", c.org.String(), "agent", id.String(), "by", c.actor().String())
	return &agentv1alpha1.ResumeAgentResponse{Agent: out}, nil
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
	err = s.inAuditTx(ctx, func(q *store.Queries, b audit.Batch) error {
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
		canceled, err := runoutcome.CancelForAgent(ctx, q, c.org, a.ID, runoutcome.Actor{UserID: c.actor()})
		if err != nil {
			return err
		}
		out, err = s.agentView(ctx, q, a, nil, nil)
		if err != nil {
			return err
		}
		b.Org(audit.Event{
			Org: c.org, Actor: c.auditActor(), Action: "agent.archive", TargetKind: "agent", TargetID: a.ID.String(),
			Before: map[string]any{"displayName": a.DisplayName},
			After:  map[string]any{"archived": true, "runsCanceled": len(canceled)},
		})
		return nil
	})
	if err != nil {
		return nil, s.dbError(err, "agent")
	}
	return &agentv1alpha1.ArchiveAgentResponse{Agent: out}, nil
}

var modelSourceProto = map[agentmodel.Source]agentv1alpha1.ModelSource{
	agentmodel.SourceAgent:      agentv1alpha1.ModelSource_MODEL_SOURCE_AGENT,
	agentmodel.SourceVersion:    agentv1alpha1.ModelSource_MODEL_SOURCE_VERSION,
	agentmodel.SourceOrgDefault: agentv1alpha1.ModelSource_MODEL_SOURCE_ORG_DEFAULT,
	agentmodel.SourceNone:       agentv1alpha1.ModelSource_MODEL_SOURCE_NONE,
}

func agentModelProto(v agentmodel.View) *agentv1alpha1.GetAgentModelResponse {
	out := &agentv1alpha1.GetAgentModelResponse{
		Setting: v.Setting, VersionModel: v.VersionModel, Effective: v.Effective,
		Source: modelSourceProto[v.Source], Problem: v.Problem,
		Thinking: &agentv1alpha1.ThinkingState{
			Setting: string(v.Thinking.Setting), VersionSetting: string(v.Thinking.VersionSetting),
			Effective: string(v.Thinking.Effective), Supported: v.Thinking.Supported(), Mode: string(v.Thinking.Mode()),
			AlwaysThinks: v.Thinking.AlwaysThinks(), OffOverridden: v.Thinking.OffOverridden(), Notice: v.Thinking.Notice(),
		},
	}
	for _, m := range v.Available {
		out.Available = append(out.Available, &agentv1alpha1.ServedModel{Name: m.Name, Provider: m.Provider})
	}
	return out
}

func (s *Server) agentModelError(err error) error {
	var inv *agentmodel.InvalidError
	switch {
	case errors.As(err, &inv):
		return connect.NewError(connect.CodeInvalidArgument, inv.Msg)
	case errors.Is(err, agentmodel.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, "agent not found")
	}
	return s.dbError(err, "agent")
}

// GetAgentModel implements AgentService. Admins and owners.
func (s *Server) GetAgentModel(ctx context.Context, req *agentv1alpha1.GetAgentModelRequest) (*agentv1alpha1.GetAgentModelResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	id, err := parseID("agent_id", req.GetAgentId())
	if err != nil {
		return nil, err
	}
	_, v, err := s.agentModels.Load(ctx, c.org, id, time.Now())
	if err != nil {
		return nil, s.agentModelError(err)
	}
	return agentModelProto(v), nil
}

// SetAgentModel implements AgentService. Admins and owners.
func (s *Server) SetAgentModel(ctx context.Context, req *agentv1alpha1.SetAgentModelRequest) (*agentv1alpha1.SetAgentModelResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	id, err := parseID("agent_id", req.GetAgentId())
	if err != nil {
		return nil, err
	}
	if _, err := s.agentModels.Set(requestCtx(ctx), c.org, id, c.auditActor(), req.GetModel(), time.Now()); err != nil {
		return nil, s.agentModelError(err)
	}
	_, v, err := s.agentModels.Load(ctx, c.org, id, time.Now())
	if err != nil {
		return nil, s.agentModelError(err)
	}
	return &agentv1alpha1.SetAgentModelResponse{Model: agentModelProto(v)}, nil
}

// SetAgentThinking implements AgentService. Admins and owners.
func (s *Server) SetAgentThinking(ctx context.Context, req *agentv1alpha1.SetAgentThinkingRequest) (*agentv1alpha1.SetAgentThinkingResponse, error) {
	c, err := authorize(ctx, req.GetOrgId(), store.OrgRoleAdmin)
	if err != nil {
		return nil, err
	}
	id, err := parseID("agent_id", req.GetAgentId())
	if err != nil {
		return nil, err
	}
	if _, err := s.agentModels.SetThinking(requestCtx(ctx), c.org, id, c.auditActor(), req.GetThinking()); err != nil {
		return nil, s.agentModelError(err)
	}
	_, v, err := s.agentModels.Load(ctx, c.org, id, time.Now())
	if err != nil {
		return nil, s.agentModelError(err)
	}
	return &agentv1alpha1.SetAgentThinkingResponse{Model: agentModelProto(v)}, nil
}
