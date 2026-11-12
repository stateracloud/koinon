// SPDX-License-Identifier: Apache-2.0

// Package agentresources reads and changes an agent's sandbox resource override (ADR 0083, #75).
// The override is a setting of the agent, not a new version: it lays over the template's values,
// inside the platform bounds (internal/sandboxres), and applies when the sandbox is next rendered.
package agentresources

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/sandboxres"
	"github.com/stateracloud/koinon/templates"
)

// ErrNotFound means the agent does not exist in the org.
var ErrNotFound = errors.New("not found")

// InvalidError is a refused override; the message is safe to show and names the bounds.
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string { return e.Msg }

// Service changes agents' sandbox resource overrides.
type Service struct {
	pool   *pgxpool.Pool
	policy sandboxres.Policy
}

// New returns a Service on pool that checks overrides against policy, which must be valid.
func New(pool *pgxpool.Pool, policy sandboxres.Policy) *Service {
	return &Service{pool: pool, policy: policy}
}

// View is an agent's sandbox sizing and where it comes from.
type View struct {
	Override, Template, Effective templates.Resources
	Bounds                        sandboxres.Bounds
	MemoryRequestEqualsLimit      bool
}

// OverrideOf is the override stored on the agent row.
func OverrideOf(a store.Agent) templates.Resources {
	return templates.Resources{
		CPU:    templates.Quantities{Request: a.SandboxCpuRequest.String, Limit: a.SandboxCpuLimit.String},
		Memory: templates.Quantities{Request: a.SandboxMemoryRequest.String, Limit: a.SandboxMemoryLimit.String},
	}
}

// templateOf is the sandbox sizing of agent a's active version, or of its latest version when it is
// not yet activated.
func templateOf(ctx context.Context, q *store.Queries, a store.Agent) (templates.Resources, error) {
	versions, err := q.ListAgentVersions(ctx, store.ListAgentVersionsParams{OrgID: a.OrgID, AgentID: a.ID})
	if err != nil {
		return templates.Resources{}, err
	}
	for _, v := range versions {
		if v.ID == a.ActiveVersionID || (!a.ActiveVersionID.Valid && v.Version == a.LatestVersion) {
			cfg, err := agentconfig.Parse(v.Config)
			if err != nil {
				return templates.Resources{}, err
			}
			return cfg.Resources, nil
		}
	}
	return templates.Resources{}, nil
}

func (s *Service) view(a store.Agent, tpl templates.Resources) View {
	override := OverrideOf(a)
	effective, _ := s.policy.Effective(sandboxres.Overlay(tpl, override))
	return View{Override: override, Template: tpl, Effective: effective, Bounds: s.policy.Bounds, MemoryRequestEqualsLimit: s.policy.MemoryRequestEqualsLimit}
}

// Load describes agent's sandbox sizing. ErrNotFound: no such agent in the org.
func (s *Service) Load(ctx context.Context, org, agent pgtype.UUID) (View, error) {
	q := store.New(s.pool)
	a, err := q.GetAgent(ctx, store.GetAgentParams{OrgID: org, ID: agent})
	if errors.Is(err, pgx.ErrNoRows) {
		return View{}, ErrNotFound
	} else if err != nil {
		return View{}, err
	}
	tpl, err := templateOf(ctx, q, a)
	if err != nil {
		return View{}, err
	}
	return s.view(a, tpl), nil
}

func text(v string) pgtype.Text { return pgtype.Text{String: v, Valid: v != ""} }

// Set replaces the agent's override; a value left empty is cleared. A value outside the platform
// bounds is refused with an *InvalidError that names them. The change and its audit event, with the
// old and the new override, commit together; setting what is already stored is not a change and
// writes no event. It returns the view after the change.
func (s *Service) Set(ctx context.Context, org, agent pgtype.UUID, actor audit.Actor, want templates.Resources) (View, error) {
	var out View
	_, err := audit.InTx(ctx, s.pool, func(q *store.Queries, b audit.Batch) error {
		cur, err := q.GetAgentForUpdate(ctx, store.GetAgentForUpdateParams{OrgID: org, ID: agent})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if cur.Status == store.AgentStatusArchived {
			return &InvalidError{Msg: "the agent is archived"}
		}
		tpl, err := templateOf(ctx, q, cur)
		if err != nil {
			return err
		}
		if err := s.policy.CheckOverride(tpl, want); err != nil {
			return &InvalidError{Msg: err.Error()}
		}
		before := OverrideOf(cur)
		if before == want {
			out = s.view(cur, tpl)
			return nil
		}
		next, err := q.SetAgentSandboxResources(ctx, store.SetAgentSandboxResourcesParams{
			OrgID: org, ID: agent,
			CpuRequest: text(want.CPU.Request), CpuLimit: text(want.CPU.Limit),
			MemoryRequest: text(want.Memory.Request), MemoryLimit: text(want.Memory.Limit),
		})
		if err != nil {
			return err
		}
		b.Org(audit.Event{
			Org: org, Actor: actor, Action: "agent.sandbox_resources.set", TargetKind: "agent", TargetID: agent.String(),
			Before: auditValue(before), After: auditValue(want),
		})
		out = s.view(next, tpl)
		return nil
	})
	return out, err
}

func auditValue(r templates.Resources) map[string]any {
	return map[string]any{
		"cpu_request": r.CPU.Request, "cpu_limit": r.CPU.Limit,
		"memory_request": r.Memory.Request, "memory_limit": r.Memory.Limit,
	}
}
