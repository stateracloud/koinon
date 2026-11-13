// SPDX-License-Identifier: Apache-2.0

// Package agentconfig is the agent definition stored in agent_versions.config:
// what a run of that version executes, and the grants an admin or owner
// approves by activating it (ADR 0020).
package agentconfig

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/stateracloud/koinon/internal/money"
	"github.com/stateracloud/koinon/templates"
)

// Defaults for versions whose config sets no run limits (plan item A4, ADR 0049, ADR 0081): the
// versions made before run limits existed. Shipped templates set their own, calibrated per
// template (ADR 0081).
const (
	DefaultMaxWallClock     = 30 * time.Minute
	DefaultMaxModelRequests = int64(200)
)

// DefaultMaxContinuations is the number of continuation runs a task gets after a time cap when
// neither the template nor the agent sets one (issue #99).
const DefaultMaxContinuations = 3

// MaxContinuations returns the continuation runs a task of this version gets: the template's
// value, or the platform default when it sets none. 0 is a value: no continuation.
func (c Config) MaxContinuations() int {
	if m := c.Limits.Run.MaxContinuations; m != nil {
		return *m
	}
	return DefaultMaxContinuations
}

// DefaultMaxSpend is the run spend limit of a version that sets none: $5.
var DefaultMaxSpend = money.FromCents(500)

// TemplateRef records the template content a version was made from.
type TemplateRef struct {
	Slug        string `json:"slug"`
	Version     string `json:"version"`
	ContentHash string `json:"contentHash"`
}

// Config is one agent version's definition. Treat it as immutable once
// stored: edits add a version.
type Config struct {
	Template     *TemplateRef `json:"template,omitempty"`
	Instructions string       `json:"instructions"`
	Model        string       `json:"model,omitempty"`
	// Thinking is the template's starting value for the thinking setting, "on" or "off", "" when
	// it names none (internal/thinking.Choose reads it after the agent's own setting).
	Thinking string            `json:"thinking,omitempty"`
	Grants   []templates.Grant `json:"grants,omitempty"`
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
	Harness templates.HarnessSettings `json:"harness,omitzero"`
	// Ephemeral marks an ephemeral agent: every task runs in a fresh sandbox without a persistent
	// home, deleted when the task ends (ADR 0065 decision 13c). Checkpoints and the archive tier
	// do not apply to it; callers ask IsEphemeral.
	Ephemeral bool `json:"ephemeral,omitempty"`
	// Questions sets the reminder and expiry of the questions the agent asks (ADR 0065 decision
	// 10); unset values take the defaults, see QuestionTimers.
	Questions templates.QuestionTimers `json:"questions,omitzero"`
	// Resources are the template's sandbox CPU and memory requests and limits (ADR 0083). Unset
	// values, and every value of a version stored before this field existed, take the platform
	// default in the reconciler (internal/sandboxres).
	Resources templates.Resources `json:"resources,omitzero"`
}

// Defaults for the questions an agent asks (ADR 0065 decision 10).
const (
	DefaultQuestionReminder = 24 * time.Hour
	DefaultQuestionExpiry   = 7 * 24 * time.Hour
)

// QuestionTimers returns when a question this version's agent asks gets its reminder and when it
// expires, both from the moment it is asked, with the defaults for unset values. A reminder that
// would not come before the expiry is dropped (reminder 0 means none).
func (c Config) QuestionTimers() (reminder, expiry time.Duration) {
	reminder, expiry = c.Questions.ReminderAfter.Duration, c.Questions.ExpireAfter.Duration
	if expiry <= 0 {
		expiry = DefaultQuestionExpiry
	}
	if reminder <= 0 {
		reminder = DefaultQuestionReminder
	}
	if reminder >= expiry {
		reminder = 0
	}
	return reminder, expiry
}

// WebEgress reports whether a sandbox running this version may reach arbitrary web hosts, which
// is what a harness's built-in web fetch needs. It is the one predicate behind the tool set
// matching the egress (ADR 0080): koinon-agentd offers the built-in web tools only when it is
// true, and says so in the instructions it writes. No grant opens arbitrary hosts today (a
// connector grant opens its connection's host, the npm registry follows harness.pluginInstalls,
// and web research goes through an MCP tool the gateway runs), so it is false for every version;
// a grant type that opens the web adds its case here.
func (c Config) WebEgress() bool { return false }

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
		Thinking:     t.Spec.Model.Thinking,
		Grants:       t.Spec.Grants,
		Limits:       t.Spec.Limits,
		Triggers:     t.Spec.Triggers,
		Harness:      t.Spec.Harness.HarnessSettings,
		Ephemeral:    t.Spec.Ephemeral,
		Questions:    t.Spec.Questions,
		Resources:    t.Spec.Resources,
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

// RunLimits are the limits one run gets (ADR 0081): the first reached ends the run.
type RunLimits struct {
	// WallClock is the time a run may be owned.
	WallClock time.Duration
	// MaxModelRequests counts every model request through the agent's key.
	MaxModelRequests int64
	// MaxSpend is the run's spend by the ledger's settled cost.
	MaxSpend money.USD
}

// RunLimits returns the limits a run of this version gets, with defaults for unset values. An
// admin's override of the agent (package budget's run-limit overrides) is applied on top of it.
func (c Config) RunLimits() RunLimits {
	l := RunLimits{
		WallClock:        c.Limits.Run.MaxDuration.Duration,
		MaxModelRequests: c.Limits.Run.MaxModelRequests,
		MaxSpend:         money.FromCents(c.Limits.Run.MaxSpendUSD.Cents()),
	}
	if l.WallClock <= 0 {
		l.WallClock = DefaultMaxWallClock
	}
	if l.MaxModelRequests <= 0 {
		l.MaxModelRequests = DefaultMaxModelRequests
	}
	if l.MaxSpend <= 0 {
		l.MaxSpend = DefaultMaxSpend
	}
	return l
}
