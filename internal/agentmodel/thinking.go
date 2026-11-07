// SPDX-License-Identifier: Apache-2.0

package agentmodel

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/thinking"
)

// The agent's thinking setting (rc.8, ADR 0069 amendment 2026-10-29): thinking is on unless the
// agent turns it off. It is a mutable setting like the model, changed without a new version and
// audited with the old and new value. The precedence is the agent's setting, then the version's
// (a template's spec.model.thinking), then the default, on (thinking.Choose). What it does is the
// model's catalog capability (thinking.Of): a model that does not think ignores it, and a model
// that always thinks refuses a new "off" and runs a stored one with thinking on (old issue 316).

// ThinkingView is an agent's thinking setting and what it resolves to.
type ThinkingView struct {
	// Setting is the agent's own setting: "" (not set), "on" or "off".
	Setting thinking.Setting
	// VersionSetting is the active version's value, "" when it names none.
	VersionSetting thinking.Setting
	// Effective is what the layers resolve to, whether or not the model can think.
	Effective thinking.Setting
	// Capability is the effective model's catalog capability. It does not think when the model
	// does not or no model is resolved.
	Capability thinking.Capability
}

// Supported reports whether the effective model can think. When it cannot, the setting is kept
// but has no effect, and the UI says "not supported by this model".
func (t ThinkingView) Supported() bool { return t.Capability.Supported() }

// AlwaysThinks reports whether the effective model thinks whatever the setting: the UI then offers
// no off choice and says thinking.AlwaysThinksMsg.
func (t ThinkingView) AlwaysThinks() bool { return t.Capability.AlwaysThinks() }

// OffOverridden reports whether the setting resolves to off and the model thinks anyway. The
// stored value is kept for a model that can stop; runs think.
func (t ThinkingView) OffOverridden() bool { return t.Capability.OffOverridden(t.Effective) }

// Notice is what the agent page says about the setting: thinking.OffOverriddenMsg when the off
// setting does not apply, "" otherwise.
func (t ThinkingView) Notice() string {
	if t.OffOverridden() {
		return thinking.OffOverriddenMsg
	}
	return ""
}

// Mode is the form a run sends (thinking.Capability.Effective).
func (t ThinkingView) Mode() thinking.Mode { return t.Capability.Effective(t.Effective) }

// parseVersion reads a version's stored value; a value that is not on or off counts as unset, so a
// config written by a later Koinon never fails a run.
func parseVersion(v string) thinking.Setting {
	s, err := thinking.ParseSetting(v)
	if err != nil {
		return ""
	}
	return s
}

// CapabilityOf returns the catalog capability of the org's model with this plain name. A model the
// org does not serve (or "") cannot think.
func CapabilityOf(ctx context.Context, q *store.Queries, org pgtype.UUID, model string) (thinking.Capability, error) {
	if model == "" {
		return thinking.Capability{}, nil
	}
	m, err := prices.Lookup(ctx, q, org, model)
	if errors.Is(err, prices.ErrUnpriced) {
		return thinking.Capability{}, nil
	} else if err != nil {
		return thinking.Capability{}, err
	}
	return thinking.Of(m.ProviderType, m.Name, m.Format), nil
}

// ChooseThinking applies the precedence to the agent's own setting and the version's value, both
// as stored; a value that is not on or off counts as unset.
func ChooseThinking(agent, version string) thinking.Setting {
	return thinking.Choose(parseVersion(agent), parseVersion(version))
}

// describeThinking resolves the thinking setting of agent a for the model that run would use.
func describeThinking(ctx context.Context, q *store.Queries, a store.Agent, versionThinking, effectiveModel string) (ThinkingView, error) {
	own := parseVersion(a.Thinking.String)
	ver := parseVersion(versionThinking)
	c, err := CapabilityOf(ctx, q, a.OrgID, effectiveModel)
	if err != nil {
		return ThinkingView{}, err
	}
	return ThinkingView{Setting: own, VersionSetting: ver, Effective: ChooseThinking(a.Thinking.String, versionThinking), Capability: c}, nil
}

// SetThinking sets the agent's thinking setting; "" clears it, so the version's value and then the
// default apply. The change and its audit event, with the old and new value, commit together.
// Setting the value it already has is not a change and writes no event. It returns the updated
// agent. "off" is refused when the model the agent's next run would use always thinks
// (thinking.AlwaysThinksMsg). Nothing else is checked against the catalog: a model that cannot think
// ignores the setting, and the view says so; and a model change is never refused for the setting,
// because a stored "off" on a model that always thinks runs with thinking on (old issue 316).
func (s *Service) SetThinking(ctx context.Context, org, agent pgtype.UUID, actor audit.Actor, value string) (store.Agent, error) {
	want, err := thinking.ParseSetting(NormalizeThinking(value))
	if err != nil {
		return store.Agent{}, &InvalidError{Msg: err.Error()}
	}
	var out store.Agent
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		cur, err := q.GetAgentForUpdate(ctx, store.GetAgentForUpdateParams{OrgID: org, ID: agent})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if cur.Status == store.AgentStatusArchived {
			return &InvalidError{Msg: "the agent is archived"}
		}
		if want == thinking.Off {
			c, err := s.nextRunCapability(ctx, q, cur)
			if err != nil {
				return err
			}
			if c.AlwaysThinks() {
				return &InvalidError{Msg: thinking.AlwaysThinksMsg}
			}
		}
		if cur.Thinking.String == string(want) {
			out = cur
			return nil
		}
		out, err = q.SetAgentThinking(ctx, store.SetAgentThinkingParams{OrgID: org, ID: agent, Thinking: pgtype.Text{String: string(want), Valid: want != ""}})
		if err != nil {
			return err
		}
		return audit.RecordErr(ctx, q, audit.Event{
			Org: org, Actor: actor, Action: "agent.thinking.set", TargetKind: "agent", TargetID: agent.String(),
			Before: map[string]any{"thinking": thinkingLabel(cur.Thinking.String)},
			After:  map[string]any{"thinking": thinkingLabel(string(want))},
		})
	})
	return out, err
}

// nextRunCapability is the catalog capability of the model agent a's next run would use.
func (s *Service) nextRunCapability(ctx context.Context, q *store.Queries, a store.Agent) (thinking.Capability, error) {
	versionModel, _, err := versionSettings(ctx, q, a)
	if err != nil {
		return thinking.Capability{}, err
	}
	v, err := describeModel(ctx, s.prices, q, a, versionModel, s.platformDefault, time.Now())
	if err != nil {
		return thinking.Capability{}, err
	}
	return CapabilityOf(ctx, q, a.OrgID, v.Effective)
}

func thinkingLabel(s string) string {
	if s == "" {
		return Default
	}
	return s
}

// NormalizeThinking returns the stored form of a requested setting: "" when it is the default.
func NormalizeThinking(v string) string {
	if v = strings.TrimSpace(v); v == Default {
		return ""
	}
	return v
}
