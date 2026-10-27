// SPDX-License-Identifier: Apache-2.0

// Package agentconfig is the agent definition stored in agent_versions.config:
// what a run of that version executes, and the grants an admin or owner
// approves by activating it (ADR 0020).
package agentconfig

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/stateracloud/koinon/templates"
)

// Defaults for versions whose config sets no run caps (plan item A4, ADR 0049).
const (
	DefaultMaxWallClock = 30 * time.Minute
	DefaultMaxTokens    = int64(2_000_000)
)

// TemplateRef records the template content a version was made from.
type TemplateRef struct {
	Slug        string `json:"slug"`
	Version     string `json:"version"`
	ContentHash string `json:"contentHash"`
}

// Config is one agent version's definition. Treat it as immutable once
// stored: edits add a version.
type Config struct {
	Template     *TemplateRef      `json:"template,omitempty"`
	Instructions string            `json:"instructions"`
	Model        string            `json:"model,omitempty"`
	Grants       []templates.Grant `json:"grants,omitempty"`
	// Bindings tie grants to an org's connections and targets (for GitHub:
	// the repositories). The grant says what the agent may do; its binding
	// says where. Set from CreateAgentFromTemplate's grants (plan item C2).
	Bindings []Binding        `json:"bindings,omitempty"`
	Limits   templates.Limits `json:"limits"`
	// Triggers start work on the agent (ADR 0066). They take effect when the version that holds
	// them is the agent's active version; a proposed version's triggers fire nothing.
	Triggers []templates.Trigger `json:"triggers,omitempty"`
	// Harness holds the template's harness settings: harness.pluginInstalls makes agentd
	// pass --plugin-installs to OpenCode and the reconciler open the npm registry.
	Harness templates.Harness `json:"harness,omitzero"`
	// Ephemeral marks an ephemeral agent: every task runs in a fresh sandbox without a persistent
	// home, deleted when the task ends (ADR 0065 decision 13c). Checkpoints and the archive tier
	// do not apply to it; callers ask IsEphemeral.
	Ephemeral bool `json:"ephemeral,omitempty"`
}

// IsEphemeral reports whether the agent version with config c is ephemeral. It is the one
// predicate behind every ephemeral special case: the reconciler's sandbox-per-task lifecycle and
// the places that skip home checkpoints and the archive tier (ADR 0032, ADR 0065 decision 13b).
func IsEphemeral(c Config) bool { return c.Ephemeral }

// Binding ties the grant on Connector to one connection and its targets.
type Binding struct {
	Connector string `json:"connector"`
	// ConnectionID is the connections.id the grant uses.
	ConnectionID string `json:"connectionId"`
	// Targets the grant is narrowed to, in the connector type's target
	// syntax (GitHub: owner/name).
	Targets []string `json:"targets"`
}

// Grant returns the grant on connector, if any.
func (c Config) Grant(connector string) (templates.Grant, bool) {
	for _, g := range c.Grants {
		if g.Connector == connector {
			return g, true
		}
	}
	return templates.Grant{}, false
}

// Binding returns the binding of the grant on connector, if any.
func (c Config) Binding(connector string) (Binding, bool) {
	for _, b := range c.Bindings {
		if b.Connector == connector {
			return b, true
		}
	}
	return Binding{}, false
}

// FromTemplate returns the first version's config for an agent made from t.
func FromTemplate(t *templates.Template, contentHash string) Config {
	return Config{
		Template: &TemplateRef{
			Slug: t.Metadata.ID, Version: t.Metadata.Version, ContentHash: contentHash,
		},
		Instructions: t.Spec.Instructions,
		Model:        t.Spec.Model.Name,
		Grants:       t.Spec.Grants,
		Limits:       t.Spec.Limits,
		Triggers:     t.Spec.Triggers,
		Harness:      t.Spec.Harness,
		Ephemeral:    t.Spec.Ephemeral,
	}
}

// Parse decodes a stored config. Unknown fields are kept compatible: later
// versions of Koinon may add some.
func Parse(raw []byte) (Config, error) {
	var c Config
	if len(raw) == 0 {
		return c, nil
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("decode agent config: %w", err)
	}
	return c, nil
}

// Marshal encodes c for storage.
func (c Config) Marshal() ([]byte, error) { return json.Marshal(c) }

// RunCaps returns the caps a run of this version gets, with defaults for
// unset values.
func (c Config) RunCaps() (wallClock time.Duration, maxTokens int64) {
	wallClock, maxTokens = c.Limits.Run.MaxDuration.Duration, c.Limits.Run.MaxTokens
	if wallClock <= 0 {
		wallClock = DefaultMaxWallClock
	}
	if maxTokens <= 0 {
		maxTokens = DefaultMaxTokens
	}
	return wallClock, maxTokens
}
