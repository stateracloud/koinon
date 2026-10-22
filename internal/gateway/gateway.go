// SPDX-License-Identifier: Apache-2.0

// Package gateway is Koinon's boundary to the model and MCP gateway (ADR 0040).
//
// Callers describe what the gateway should serve as a [State] and hand it to a
// [ModelGateway] driver with Apply. The standalone driver (package
// gateway/standalone) renders agentgateway's local config file; a Kubernetes-mode
// driver (plan item G2) renders agentgateway CRDs from the same State.
//
// Key material never enters a State. IssueKey returns it exactly once; the caller
// stores it in a Kubernetes Secret (and the OpenShell provider record) and keeps
// only the [Key] record, which carries the hash.
package gateway

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// ModelGateway is implemented by each gateway driver.
type ModelGateway interface {
	// IssueKey mints a new API key for agent. The returned material is shown
	// once and is not retained by the driver. The key takes effect when a State
	// containing issued.Key is applied.
	IssueKey(ctx context.Context, agent AgentRef) (IssuedKey, error)

	// Apply converges the gateway to s and returns once the gateway serves it,
	// or with an error when the gateway rejected it or ctx expired. Apply is
	// idempotent; revoking a key or changing a limit is an Apply of a State
	// without that key or with the new limit (see State.RevokeKey and
	// State.SetLimit).
	Apply(ctx context.Context, s State) (Revision, error)

	// Usage returns the per-request usage source for the spend ledger (L1).
	Usage() UsageReader
}

// Revision identifies an applied configuration. Drivers derive it from the
// rendered content, so equal States give equal revisions.
type Revision string

// AgentRef names an agent within its org.
type AgentRef struct {
	Org  string
	Name string
}

func (a AgentRef) String() string { return a.Org + "/" + a.Name }

// dnsLabel matches the slugs Koinon uses for org and agent names.
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// Validate reports whether a is safe to embed in gateway config and CEL.
func (a AgentRef) Validate() error {
	if !dnsLabel.MatchString(a.Org) || !dnsLabel.MatchString(a.Name) {
		return fmt.Errorf("gateway: invalid agent ref %q: org and name must be DNS labels", a.String())
	}
	return nil
}

// ParseAgentRef parses the "org/name" form String returns.
func ParseAgentRef(s string) (AgentRef, error) {
	org, name, _ := strings.Cut(s, "/")
	a := AgentRef{Org: org, Name: name}
	if err := a.Validate(); err != nil {
		return AgentRef{}, err
	}
	return a, nil
}

// AgentHeader is the request header agentgateway sets on every MCP request
// it forwards to a target: the authenticated key's agent (AgentRef.String).
// Drivers render it so that a client-supplied value is overwritten; MCP
// targets trust it only because they are reachable from the gateway alone.
const AgentHeader = "X-Koinon-Agent"

// Key is the gateway-side record of an API key: its ID, owner and hash. It
// holds no key material and is safe to persist and to render into config.
type Key struct {
	// ID is unique per key and stable for its lifetime. Gateways that keep
	// per-key state (budgets) key it by this ID, so a rotated key starts with a
	// fresh budget window.
	ID string
	// Agent owns the key. Limits and tool grants apply to every key of an agent.
	Agent AgentRef
	// Hash is "sha256:<hex>" of the key material.
	Hash string
}

// IssuedKey is a new key with its material. Material must only be written to
// a Kubernetes Secret or an OpenShell provider record, never logged.
type IssuedKey struct {
	Key
	Material string
}

// KeyPrefix starts every Koinon gateway key, so leaked keys are recognisable
// by secret scanners.
const KeyPrefix = "kgw_"

// NewKey generates key material for agent with 256 bits of entropy.
func NewKey(agent AgentRef) (IssuedKey, error) {
	if err := agent.Validate(); err != nil {
		return IssuedKey{}, err
	}
	var secret [32]byte
	var id [6]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return IssuedKey{}, err
	}
	if _, err := rand.Read(id[:]); err != nil {
		return IssuedKey{}, err
	}
	material := KeyPrefix + base64.RawURLEncoding.EncodeToString(secret[:])
	return IssuedKey{
		Key: Key{
			ID:    "k-" + hex.EncodeToString(id[:]),
			Agent: agent,
			Hash:  HashKey(material),
		},
		Material: material,
	}, nil
}

// HashKey returns the "sha256:<hex>" form agentgateway's keyHash expects.
func HashKey(material string) string {
	sum := sha256.Sum256([]byte(material))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// LimitUnit is the unit of a spend limit.
type LimitUnit string

const (
	// USD limits spend priced by the gateway's model cost catalog.
	USD LimitUnit = "USD"
	// Tokens limits input plus output tokens.
	Tokens LimitUnit = "Tokens"
)

// Limit is a per-agent spend limit enforced in the request path. Spend is
// charged after a response completes, so a limit can be overshot by up to one
// in-flight response (ADR 0041).
type Limit struct {
	Unit   LimitUnit
	Amount float64
	// Window is the accounting window. agentgateway aligns windows to the Unix
	// epoch (24h starts at midnight UTC).
	Window time.Duration
	// Generation names the gateway's spend counter for this limit. agentgateway
	// keys budget usage by key and budget name, so a new Generation starts a
	// fresh counter. The spend ledger pushes limits net of recorded spend and
	// changes Generation with every new net amount, so spend is never counted
	// both in the amount and in the gateway's counter (plan item L1). Empty
	// keeps one counter per key.
	Generation string
}

// ModelUpstream is the one OpenAI-compatible upstream of v0.1 (ADR 0049).
type ModelUpstream struct {
	// Provider names the upstream for the cost catalog and telemetry, for
	// example "openrouter" or "ollama".
	Provider string
	// BaseURL is the OpenAI-compatible base, for example https://openrouter.ai/api/v1.
	BaseURL string
	// APIKeyFile is the path, inside the gateway, of the mounted provider key.
	// Empty means the upstream needs no key.
	APIKeyFile string
	Models     []Model
	// Overrides are set on every request body and win over client values, for
	// example OpenRouter's provider routing guard (ADR 0048).
	Overrides map[string]any
}

// Model is one model clients may request.
type Model struct {
	Name string
	// Price is per million tokens in USD. agentgateway charges zero for models
	// it cannot price (ADR 0041), so drivers do not serve models without a
	// price: requests for them are rejected as unknown models (plan item L1).
	// A free model needs an explicit zero price.
	Price *Price
}

// Price is a per-million-token price in USD, as decimal strings.
type Price struct {
	Input     string
	Output    string
	CacheRead string
}

// Unpriced returns the names of the models without a price, which drivers do
// not serve.
func (u ModelUpstream) Unpriced() []string {
	var out []string
	for _, m := range u.Models {
		if m.Price == nil {
			out = append(out, m.Name)
		}
	}
	return out
}

// MCPTarget is an upstream MCP server (Streamable HTTP).
type MCPTarget struct {
	// Name prefixes the target's tools when several targets are multiplexed.
	Name string
	URL  string
}

// ToolGrant allows an agent to see and call some tools of one MCP target.
type ToolGrant struct {
	Target string
	Tools  []string
}

// State is everything a gateway serves. The zero value serves nothing.
type State struct {
	Keys   []Key
	Limits map[AgentRef]Limit
	Models ModelUpstream
	MCP    []MCPTarget
	// Tools are per-agent MCP allowlists. An agent without grants sees no tools.
	Tools map[AgentRef][]ToolGrant
}

// AddKey adds k to the State.
func (s *State) AddKey(k Key) { s.Keys = append(s.Keys, k) }

// RevokeKey removes the key with id; it reports whether one was removed.
func (s *State) RevokeKey(id string) bool {
	n := len(s.Keys)
	s.Keys = slices.DeleteFunc(s.Keys, func(k Key) bool { return k.ID == id })
	return len(s.Keys) != n
}

// SetLimit sets agent's limit; nil clears it.
func (s *State) SetLimit(agent AgentRef, l *Limit) {
	if l == nil {
		delete(s.Limits, agent)
		return
	}
	if s.Limits == nil {
		s.Limits = map[AgentRef]Limit{}
	}
	s.Limits[agent] = *l
}

// GrantTools replaces agent's MCP allowlist.
func (s *State) GrantTools(agent AgentRef, grants ...ToolGrant) {
	if s.Tools == nil {
		s.Tools = map[AgentRef][]ToolGrant{}
	}
	if len(grants) == 0 {
		delete(s.Tools, agent)
		return
	}
	s.Tools[agent] = grants
}

var (
	toolName   = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
	generation = regexp.MustCompile(`^[a-z0-9-]{0,64}$`)
	keyID      = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	keyHash    = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	modelName  = regexp.MustCompile(`^[A-Za-z0-9_.:/@-]{1,256}$`)
)

// Validate checks s for values that would be unsafe or ambiguous in gateway
// config. Drivers call it before rendering.
func (s State) Validate() error {
	var errs []error
	seenID := map[string]bool{}
	seenHash := map[string]bool{}
	for _, k := range s.Keys {
		if !keyID.MatchString(k.ID) {
			errs = append(errs, fmt.Errorf("key %q: invalid id", k.ID))
		}
		if !keyHash.MatchString(k.Hash) {
			errs = append(errs, fmt.Errorf("key %q: hash must be sha256:<64 hex>", k.ID))
		}
		if err := k.Agent.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("key %q: %w", k.ID, err))
		}
		if seenID[k.ID] || seenHash[k.Hash] {
			errs = append(errs, fmt.Errorf("key %q: duplicate id or hash", k.ID))
		}
		seenID[k.ID], seenHash[k.Hash] = true, true
	}
	for a, l := range s.Limits {
		if err := a.Validate(); err != nil {
			errs = append(errs, err)
		}
		if l.Unit != USD && l.Unit != Tokens {
			errs = append(errs, fmt.Errorf("limit for %s: unknown unit %q", a, l.Unit))
		}
		if l.Amount < 0 || l.Window <= 0 {
			errs = append(errs, fmt.Errorf("limit for %s: amount must be >= 0 and window > 0", a))
		}
		if !generation.MatchString(l.Generation) {
			errs = append(errs, fmt.Errorf("limit for %s: generation must be at most 64 of [a-z0-9-]", a))
		}
	}
	for _, m := range s.Models.Models {
		if !modelName.MatchString(m.Name) {
			errs = append(errs, fmt.Errorf("model %q: invalid name", m.Name))
		}
	}
	if len(s.Models.Models) > 0 && (s.Models.BaseURL == "" || s.Models.Provider == "") {
		errs = append(errs, errors.New("models: provider and base URL are required"))
	}
	targets := map[string]bool{}
	for _, t := range s.MCP {
		if !toolName.MatchString(t.Name) || t.URL == "" {
			errs = append(errs, fmt.Errorf("mcp target %q: invalid name or empty URL", t.Name))
		}
		targets[t.Name] = true
	}
	for a, grants := range s.Tools {
		if err := a.Validate(); err != nil {
			errs = append(errs, err)
		}
		for _, g := range grants {
			if !targets[g.Target] {
				errs = append(errs, fmt.Errorf("tools for %s: unknown mcp target %q", a, g.Target))
			}
			for _, t := range g.Tools {
				if !toolName.MatchString(t) {
					errs = append(errs, fmt.Errorf("tools for %s: invalid tool name %q", a, t))
				}
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("gateway: invalid state: %w", errors.Join(errs...))
	}
	return nil
}

// Access-log attributes every driver adds to agentgateway's access log. The
// spend ledger (package ledger) attributes requests by them.
const (
	// LogFieldKey is the gateway key's ID (Key.ID).
	LogFieldKey = "koinon.key"
	// LogFieldAgent is the key's agent (AgentRef.String: "<org slug>/<agent>").
	LogFieldAgent = "koinon.agent"
	// LogFieldRequest is a per-request UUID. agentgateway logs its own request
	// id only at debug level, so drivers add one; the ledger is idempotent on it.
	LogFieldRequest = "koinon.request"
)

// UsageRecord is one charged request, as the spend ledger (L1) needs it.
type UsageRecord struct {
	RequestID    string
	KeyID        string
	Agent        AgentRef
	Model        string
	InputTokens  int64
	OutputTokens int64
	CostUSD      float64
	Time         time.Time
}

// UsageReader yields usage records, for drivers whose gateway can be polled.
// The spend ledger does not poll: agentgateway pushes its access log over OTLP
// to koinon-api (package ledger), so drivers return NoUsage.
type UsageReader interface {
	ReadUsage(ctx context.Context, since time.Time) ([]UsageRecord, error)
}

// ErrUsageUnsupported is returned by a UsageReader that has no source yet.
var ErrUsageUnsupported = errors.New("gateway: usage reading not implemented")

// NoUsage is a UsageReader that always returns ErrUsageUnsupported.
type NoUsage struct{}

// ReadUsage implements UsageReader.
func (NoUsage) ReadUsage(context.Context, time.Time) ([]UsageRecord, error) {
	return nil, ErrUsageUnsupported
}
