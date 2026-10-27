// SPDX-License-Identifier: Apache-2.0

// Package triggers is the read side of agent triggers (ADR 0066): which triggers are in effect.
// A trigger is in effect when it belongs to the active version of an active agent; a proposed
// version's triggers, a draft agent's and an archived agent's are not. The schedule (TR2) and
// the direct-message dispatcher (TR3) read from here and fire nothing in this package.
package triggers

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/templates"
)

// Active is one trigger in effect.
type Active struct {
	OrgID   pgtype.UUID
	AgentID pgtype.UUID
	// Version is the agent version that defines the trigger.
	Version int32
	templates.Trigger
}

// ForOrg lists the triggers in effect in one org, of the given types (all types when none).
func ForOrg(ctx context.Context, q *store.Queries, org pgtype.UUID, types ...templates.TriggerType) ([]Active, error) {
	rows, err := q.ListActiveAgentVersions(ctx, org)
	if err != nil {
		return nil, fmt.Errorf("list active agent versions: %w", err)
	}
	var out []Active
	for _, r := range rows {
		a, err := collect(r.OrgID, r.AgentID, r.Version, r.Config, types)
		if err != nil {
			return nil, err
		}
		out = append(out, a...)
	}
	return out, nil
}

// All lists the triggers in effect in every live org.
func All(ctx context.Context, q *store.Queries, types ...templates.TriggerType) ([]Active, error) {
	rows, err := q.ListActiveAgentVersionsAllOrgs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list active agent versions: %w", err)
	}
	var out []Active
	for _, r := range rows {
		a, err := collect(r.OrgID, r.AgentID, r.Version, r.Config, types)
		if err != nil {
			return nil, err
		}
		out = append(out, a...)
	}
	return out, nil
}

// ForAgent lists the triggers in effect on one agent: empty when it is not active.
func ForAgent(ctx context.Context, q *store.Queries, org, agent pgtype.UUID, types ...templates.TriggerType) ([]Active, error) {
	all, err := ForOrg(ctx, q, org, types...)
	if err != nil {
		return nil, err
	}
	var out []Active
	for _, a := range all {
		if a.AgentID == agent {
			out = append(out, a)
		}
	}
	return out, nil
}

func collect(org, agent pgtype.UUID, version int32, raw []byte, types []templates.TriggerType) ([]Active, error) {
	cfg, err := agentconfig.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("agent %s version %d: %w", agent, version, err)
	}
	var out []Active
	for _, tr := range cfg.Triggers {
		if len(types) > 0 && !contains(types, tr.Type) {
			continue
		}
		out = append(out, Active{OrgID: org, AgentID: agent, Version: version, Trigger: tr})
	}
	return out, nil
}

func contains(ts []templates.TriggerType, t templates.TriggerType) bool {
	for _, x := range ts {
		if x == t {
			return true
		}
	}
	return false
}
