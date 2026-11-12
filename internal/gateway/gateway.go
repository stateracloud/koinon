// SPDX-License-Identifier: Apache-2.0

// Package gateway is Koinon's boundary to the model and MCP gateway (ADR 0040).
//
// Callers describe what the gateway should serve as a [State] and hand it to a
// [ModelGateway] driver with Apply. The standalone driver (package
// gateway/standalone) renders agentgateway's local config file; the Kubernetes-mode
// driver (package gateway/kubernetes, plan item G2) renders agentgateway CRDs from the
// same State.
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
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/internal/money"
	"github.com/stateracloud/koinon/internal/thinking"
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
	// ModelState.SetLimit).
	//
	// Apply converges both halves of s (the model path and the MCP path), as the agentgateway
	// drivers serve both from one config. The per-half model call, next to [MCPGateway.ApplyMCP],
	// is [ModelPathGateway.ApplyModel].
	//
	// A driver whose gateway judges objects one by one (Kubernetes mode) may return the
	// revision together with a *RejectedError: everything but the named objects is live.
	Apply(ctx context.Context, s State) (Revision, error)

	// Usage returns the per-request usage source for the spend ledger (L1).
	Usage() UsageReader
}

// MCPGateway is implemented by a driver that serves the MCP path (ADR 0097 decision 2.1). It is the
// MCP counterpart of [ModelGateway], so the two paths are built, applied and acknowledged apart and
// a driver may serve one of them only. The standalone and Kubernetes drivers serve both paths from
// one config and implement neither half separately yet; Koinon's own gateway (ADR 0097 step 2) is
// the first driver of this interface.
type MCPGateway interface {
	// ApplyMCP converges the gateway's MCP path to c and returns once the gateway serves it, or
	// with an error when the gateway rejected it or ctx expired. ApplyMCP is idempotent. It takes
	// an [MCPConfig], not an MCPState, because the MCP half reads the keys to know which agent a
	// key belongs to. It returns the same [Revision] type as [ModelGateway.Apply]; the revision
	// covers the MCP path only.
	ApplyMCP(ctx context.Context, c MCPConfig) (Revision, error)
}

// ModelPathGateway is the model counterpart of [MCPGateway]: a driver that serves the model path and
// takes it apart from the MCP path. Koinon's own gateway (ADR 0097 step 2) is its first driver, and the
// State stream client (package gateway/stateclient) applies and acknowledges each half through it.
type ModelPathGateway interface {
	// ApplyModel converges the gateway's model path to c and returns once the gateway serves it, or
	// with an error when the gateway rejected it or ctx expired. ApplyModel is idempotent. The
	// revision covers the model path only.
	ApplyModel(ctx context.Context, c ModelConfig) (Revision, error)
}

// Rejection is one object the gateway refused, with the org it belongs to ("" for a platform
// object).
type Rejection struct {
	Org    string
	Kind   string
	Name   string
	Reason string
}

func (r Rejection) String() string {
	who := r.Org
	if who == "" {
		who = "platform"
	}
	return fmt.Sprintf("%s: %s %s rejected: %s", who, r.Kind, r.Name, r.Reason)
}

// RejectedError reports objects the gateway refused while it serves the rest. The Revision
// returned with it is live for everything else, so a caller records it as applied and reports
// each rejection on the owning org.
type RejectedError struct {
	Rejected []Rejection
}

func (e *RejectedError) Error() string {
	parts := make([]string, len(e.Rejected))
	for i, r := range e.Rejected {
		parts[i] = r.String()
	}
	return "gateway: " + strings.Join(parts, "; ")
}

// Orgs returns the orgs with a rejected object, sorted; "" stands for the platform.
func (e *RejectedError) Orgs() []string {
	var out []string
	for _, r := range e.Rejected {
		out = append(out, r.Org)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// ErrConfigRejected means the gateway failed to load the whole config it was given and keeps
// serving its previous one (standalone mode). Unlike a RejectedError, nothing in the new config
// is live.
var ErrConfigRejected = errors.New("standalone: agentgateway rejected the config")

// ConfigRejectedError is ErrConfigRejected with the revision and the gateway's own reason
// ("USD budget limits support at most 9 fractional digits"), so an operator does not have to read
// the gateway's log to learn it. errors.Is(err, ErrConfigRejected) matches it.
type ConfigRejectedError struct {
	Revision Revision
	// Reason is the gateway's reload error, or empty when it could not be read.
	Reason string
}

func (e *ConfigRejectedError) Error() string {
	msg := fmt.Sprintf("%s (revision %s)", ErrConfigRejected.Error(), e.Revision)
	if e.Reason != "" {
		msg += ": " + e.Reason
	}
	return msg
}

// Is makes errors.Is(err, ErrConfigRejected) true.
func (e *ConfigRejectedError) Is(target error) bool { return target == ErrConfigRejected }

// LedgerBudgets is implemented by a driver whose gateway cannot enforce a spend limit itself: the
// only limit it honours is an amount of zero, which it enforces by revoking the agent's keys (the
// Kubernetes driver). A caller that cannot compute a budget must then fail closed to zero, because a
// positive amount is not enforced by anything between two ledger passes.
type LedgerBudgets interface {
	LedgerEnforcesBudgets() bool
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

// OrgSlugPattern is the rule for org slugs: a DNS label of at most 40
// characters. It is the orgs.slug CHECK of migration 00001_initial.sql, so the
// gateway accepts exactly the orgs Postgres can hold.
const OrgSlugPattern = `^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`

var (
	orgSlug = regexp.MustCompile(OrgSlugPattern)
	// dnsLabel matches agent names (agent IDs in practice).
	dnsLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

// Validate reports whether a is safe to embed in gateway config and CEL.
func (a AgentRef) Validate() error {
	if !orgSlug.MatchString(a.Org) {
		return fmt.Errorf("gateway: invalid agent ref %q: org must be an org slug (%s)", a.String(), OrgSlugPattern)
	}
	if !dnsLabel.MatchString(a.Name) {
		return fmt.Errorf("gateway: invalid agent ref %q: name must be a DNS label", a.String())
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

// AgentFromHeader reads the agent the gateway set in AgentHeader. It refuses unless the header
// is present exactly once and that value parses: Go keeps repeated header lines apart and Get
// returns the first, which could be a client's copy that a driver appended to instead of
// overwriting. Tool servers must use this, not Header.Get.
func AgentFromHeader(h http.Header) (AgentRef, error) {
	v := h.Values(AgentHeader)
	if len(v) != 1 {
		return AgentRef{}, fmt.Errorf("gateway: want exactly one %s header, got %d", AgentHeader, len(v))
	}
	return ParseAgentRef(v[0])
}

// OrgHeader is the request header agentgateway sets on every LLM request from the
// authenticated key's org. Drivers render model entries that match on it, so an org's
// plain model name resolves to that org's model only. Clients cannot choose it: the
// gateway overwrites whatever they send.
const OrgHeader = "x-koinon-org"

// AgentHeader is the request header the gateway (agentgateway today, Koinon's MCP path
// when it takes over, ADR 0097 decision 5) sets on every MCP request it forwards to a target: the authenticated key's agent (AgentRef.String).
// Drivers render it so that a client-supplied value is overwritten; MCP
// targets trust it only because they are reachable from the gateway alone.
const AgentHeader = "X-Koinon-Agent"

// Names of the agents Koinon itself runs to use MCP servers through the gateway: a prober per
// server and an executor per org for approved calls. They hold keys like agents do, ask for no
// model, and are not rows in the agents table.
const (
	MCPProbeAgentPrefix = "mcp-probe-"
	MCPExecAgentName    = "mcp-exec"
)

// IsInternalAgent reports whether a is one of Koinon's own MCP agents. Internal agents request no
// model.
func IsInternalAgent(a AgentRef) bool {
	return a.Name == MCPExecAgentName || strings.HasPrefix(a.Name, MCPProbeAgentPrefix)
}

// ModelCheckAgentName names the org's system principal for the provider echo check (ADR 0056
// amendment): one key per org that may request only the models in its State.Models entry, with a
// small daily budget. It is not a row in the agents table; the spend ledger records its requests
// with the principal "system" and no run.
const ModelCheckAgentName = "model-check"

// IsSystemAgent reports whether a is a system principal: a key of Koinon's own that requests
// models (unlike the internal MCP agents) but belongs to no agent. Its models are exactly the ones
// State.Models lists for it, none by default.
func IsSystemAgent(a AgentRef) bool { return a.Name == ModelCheckAgentName }

// ReadyPrompt is the MCP prompt Koinon's MCP server publishes to every agent.
// A harness that turns MCP prompts into commands (OpenCode v2) announces it
// once the server is connected and its tools are registered; koinon-agentd
// waits for it before the first prompt (ADR 0052). Drivers must let every
// authenticated key list and get it.
const ReadyPrompt = "koinon-ready"

// ReadyPromptExposed is the prompt as agents see it: agentgateway prefixes every prompt and tool with
// the name of its target (prefixMode always), and Koinon's MCP server is the "github" target.
const ReadyPromptExposed = "github_" + ReadyPrompt

// KoinonTarget is the MCP target of Koinon's own agent tools (ask_user, plan item TK3), offered to
// every active agent; the agent sees "koinon_ask_user". The tool server is koinon-api's.
const KoinonTarget = "koinon"

// ToolAskUser is the name of the ask_user tool on KoinonTarget.
const ToolAskUser = "ask_user"

// ToolCancelTask is the name of the cancel_task tool on KoinonTarget: the agent closes its own task
// as canceled (ADR 0065 decision 10, plan item TK4).
const ToolCancelTask = "cancel_task"

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

// USD limits spend priced by the gateway's model cost catalog. It is the only unit a Limit
// carries: Koinon budgets money, and the amount is an exact money.USD (old issue 245).
const USD LimitUnit = "USD"

// BudgetFractionDigits is how many fraction digits of a USD limit the gateway accepts: standalone
// agentgateway v1.7.0 rejects its whole config for a budget with more ("USD budget limits support
// at most 9 fractional digits"). A gateway upgrade revisits it (ADR 0050, amendment 2026-10-29).
const BudgetFractionDigits = 9

// Limit is a per-agent spend limit enforced in the request path. Spend is
// charged after a response completes, so a limit can be overshot by up to one
// in-flight response per concurrent call: N calls in flight when the limit is
// reached can each finish and be charged (ADR 0097 records this bound; ADR 0041 decision 6 says "one response").
type Limit struct {
	Unit LimitUnit
	// Amount is exact, in USD with at most BudgetFractionDigits fraction digits (USDLimit rounds
	// down to it; Validate rejects more).
	Amount money.USD
	// Window is the accounting window. agentgateway aligns windows to the Unix
	// epoch (24h starts at midnight UTC). Zero means no window: since agentgateway
	// v1.7 the counter then accumulates for the lifetime of its name and never
	// resets, so only a new Generation starts a fresh one. Koinon does not use
	// that yet: agentgateway deletes only expired windowed counters at startup,
	// so every unwindowed generation would stay in its table (and in memory) forever.
	Window time.Duration
	// Generation names the gateway's spend counter for this limit. agentgateway
	// keys budget usage by key and budget name, so a new Generation starts a
	// fresh counter. The spend ledger pushes limits net of recorded spend and
	// changes Generation with every new net amount, so spend is never counted
	// both in the amount and in the gateway's counter (plan item L1). Empty
	// keeps one counter per key.
	Generation string
}

// USDLimit returns a USD limit of amount per window. It is the edge where Koinon's exact money
// type (package money) meets the gateway's precision: the amount is rounded down to
// BudgetFractionDigits, which never hands out more than the remainder. The generation is not
// derived from the amount, so the rounding does not change which counter the gateway uses.
func USDLimit(amount money.USD, window time.Duration, generation string) Limit {
	return Limit{Unit: USD, Amount: amount.Floor(BudgetFractionDigits), Window: window, Generation: generation}
}

// Validate reports why the gateway cannot take the limit, or nil. A limit that fails it is
// quarantined with its key by drivers that can (standalone: one bad limit must not freeze the file).
// AppliedWindow is the window a gateway applies: agentgateway takes whole seconds at the smallest, so a
// positive window below that rounds up to the next second and 0 stays "no window". Rendering and the
// State snapshot (internal/gateway/staterev) both use it, so what is stored is what is applied.
func (l Limit) AppliedWindow() time.Duration {
	if l.Window <= 0 {
		return 0
	}
	return (l.Window + time.Second - 1) / time.Second * time.Second
}

func (l Limit) Validate() error {
	switch {
	case l.Unit != USD:
		return fmt.Errorf("unknown unit %q", l.Unit)
	case l.Amount < 0 || l.Window < 0:
		return errors.New("amount and window must be >= 0")
	case !l.Amount.HasFractionDigits(BudgetFractionDigits):
		return fmt.Errorf("amount %s has more than %d fraction digits", l.Amount.Numeric(), BudgetFractionDigits)
	case !generation.MatchString(l.Generation):
		return errors.New("generation must be at most 64 of [a-z0-9-]")
	}
	return nil
}

// Provider is one OpenAI-compatible upstream of an org (ADR 0056, plan item PV1). Drivers
// serve each of its models as "<org>/<model>" to the org's own keys only.
type Provider struct {
	// Org is the owning org's slug.
	Org string
	// Name is unique within the org; it names the provider in logs and key files.
	Name string
	// Type names the upstream for the cost catalog and telemetry (agentgateway's
	// providerOverride), for example "openrouter" or "ollama".
	Type string
	// BaseURL is the OpenAI-compatible base, for example https://openrouter.ai/api/v1.
	BaseURL string
	// APIKeyFile is the path, inside the gateway, of the mounted provider key.
	// Empty means the upstream needs no key.
	APIKeyFile string
	// AnthropicWorkspaceID, when set, is sent as the anthropic-workspace-id header on the requests of
	// this provider's anthropic-format models (ADR 0069 decision 5). It is an identifier, not a
	// credential.
	AnthropicWorkspaceID string
	Models               []Model
}

// Model is one model clients may request.
type Model struct {
	// Name is the plain name agents use; it is unique within the org.
	Name string
	// Format is the model's wire format (ADR 0069); the drivers render it through one table
	// (Rendering). It is required.
	Format modelformat.Format
	// Thinking is what the catalog says about the model's thinking (internal/thinking). The
	// rendering pins the anthropic-beta its row needs and never passes the client's header
	// through (ADR 0069 amendment 2026-10-29). The zero value is a model that does not think.
	Thinking thinking.Capability
	// Guard turns on the routing guard (ADR 0048): the gateway forces GuardOverrides
	// into every request body.
	Guard bool
	// Price is per million tokens in USD. agentgateway charges zero for models
	// it cannot price (ADR 0041), so drivers do not serve models without a
	// price: requests for them are rejected as unknown models (plan item L1).
	// A free model needs an explicit zero price.
	Price *Price
}

// GuardOverrides are the request fields the routing guard forces: OpenRouter's provider
// preferences for zero data retention and no training on prompts (ADR 0048). They win over
// client values.
func GuardOverrides() map[string]any {
	return map[string]any{"provider": map[string]any{"zdr": true, "data_collection": "deny"}}
}

// Price is a per-million-token price in USD, as decimal strings.
type Price struct {
	Input      string
	Output     string
	CacheRead  string
	CacheWrite string
	Reasoning  string
	// Tiers raise or lower rates once the input side of a request (input, cache
	// reads and cache writes together) exceeds ContextOver tokens. A tier overlays
	// the base rates: unset fields keep the base rate (agentgateway's model catalog).
	Tiers []PriceTier
}

// PriceTier is one context-length tier of a Price.
type PriceTier struct {
	ContextOver int64
	Input       string
	Output      string
	CacheRead   string
	CacheWrite  string
	Reasoning   string
}

// Unpriced returns "<org>/<model>" for each model without a price; drivers do not serve them.
func (s ModelState) Unpriced() []string {
	var out []string
	for _, p := range s.Providers {
		for _, m := range p.Models {
			if m.Price == nil {
				out = append(out, p.Org+"/"+m.Name)
			}
		}
	}
	return out
}

// ModelRef is the gateway-wide name of an org's model.
func ModelRef(org, model string) string { return org + "/" + model }

// MCPTarget is an upstream MCP server.
type MCPTarget struct {
	// Name prefixes the target's tools when several targets are multiplexed. It is a DNS-label style
	// name without underscores (agentgateway reserves them as delimiters).
	Name string
	URL  string
	// SSE says the server speaks the legacy SSE transport; the default is Streamable HTTP.
	SSE bool
	// Org, when set, makes the target an org's own server (ADR 0057): only keys of this org see it.
	// Drivers render a per-target condition for it, besides the agents' own allow rules.
	Org string
	// CredentialHeader, CredentialPrefix and CredentialFile inject a static credential into every
	// request the gateway sends to the target: the header's value is CredentialPrefix followed by
	// the content of the file, a path inside the gateway. Agents never see it. All empty means the
	// target needs no credential.
	CredentialHeader string
	CredentialPrefix string
	CredentialFile   string
}

// ToolGrant allows an agent to see and call some tools of one MCP target.
type ToolGrant struct {
	Target string
	Tools  []string
	// All grants every tool of the target, whatever it lists. Only Koinon's own probe keys have it
	// (the prober lists a server's tools through the gateway); an agent's grants name tools.
	All bool
}

// State is everything a gateway serves. The zero value serves nothing. It is the model half and
// the MCP half side by side (ADR 0097 decision 2.1), with the one thing both need, the keys.
// Fields of the halves are promoted, so s.Providers and s.MCP read as before; a composite literal
// may name the half (State{ModelState: ModelState{...}}) or set the promoted fields flat. ModelConfig
// and MCPConfig give each path its view.
type State struct {
	// Keys are shared by the halves: a key is how a caller of either path is authenticated, and
	// its agent is what the model half limits and the MCP half grants tools to. Keeping them on
	// State (not copied into each half) leaves one source of truth for them.
	Keys []Key
	ModelState
	MCPState
}

// ModelConfig is what the model path is handed: the keys and the model half of a State. A driver
// that serves only the model path takes this and ignores the MCP half.
type ModelConfig struct {
	Keys []Key
	ModelState
}

// MCPConfig is what the MCP path is handed: the keys and the MCP half of a State. A driver that
// serves only the MCP path takes this and ignores the model half.
type MCPConfig struct {
	Keys []Key
	MCPState
}

// ModelConfig returns the model view of s: its keys and its model half.
func (s State) ModelConfig() ModelConfig { return ModelConfig{Keys: s.Keys, ModelState: s.ModelState} }

// MCPConfig returns the MCP view of s: its keys and its MCP half.
func (s State) MCPConfig() MCPConfig { return MCPConfig{Keys: s.Keys, MCPState: s.MCPState} }

// ModelState is the half of a State the model path serves (ADR 0097 decision 2.1): spend limits,
// providers with their models, and each agent's model allowlist. A driver that serves only the
// model path is handed this half (with the keys) and ignores the MCP half.
type ModelState struct {
	Limits    map[AgentRef]Limit
	Providers []Provider
	// Models narrows a key's models to the listed names, which must be served and priced for the
	// key's org to count. An agent without an entry may request all of its org's models; a system
	// agent (IsSystemAgent) without one may request none.
	Models map[AgentRef][]string
}

// MCPState is the half of a State the MCP path serves (ADR 0097 decision 2.1): the upstream MCP
// servers and each agent's tool allowlist.
type MCPState struct {
	MCP []MCPTarget
	// Tools are per-agent MCP allowlists. An agent without grants sees no tools.
	Tools map[AgentRef][]ToolGrant
}

// WithoutInvalidLimits returns s without the keys and limits of agents whose Limit fails
// Limit.Validate, and one Rejection per dropped key. A driver that serves the rest of the state
// uses it so that one agent's bad limit fails that agent's key (it is not served: fail closed)
// and not the whole config. A key is shared by both halves, so the dropped key also takes the agent
// off the MCP path (its tool grants stay, but name no key); a spec pins this. ADR 0097 does not say
// otherwise.
func (s State) WithoutInvalidLimits() (State, []Rejection) {
	var bad map[AgentRef]error
	for a, l := range s.Limits {
		if err := l.Validate(); err != nil {
			if bad == nil {
				bad = map[AgentRef]error{}
			}
			bad[a] = err
		}
	}
	if bad == nil {
		return s, nil
	}
	out := s
	out.Limits = map[AgentRef]Limit{}
	for a, l := range s.Limits {
		if _, ok := bad[a]; !ok {
			out.Limits[a] = l
		}
	}
	out.Keys = nil
	var rej []Rejection
	for _, k := range s.Keys {
		if err, ok := bad[k.Agent]; ok {
			rej = append(rej, Rejection{Org: k.Agent.Org, Kind: "Key", Name: k.ID, Reason: "limit: " + err.Error()})
			continue
		}
		out.Keys = append(out.Keys, k)
	}
	slices.SortFunc(rej, func(a, b Rejection) int { return strings.Compare(a.Name, b.Name) })
	return out, rej
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
func (s *ModelState) SetLimit(agent AgentRef, l *Limit) {
	if l == nil {
		delete(s.Limits, agent)
		return
	}
	if s.Limits == nil {
		s.Limits = map[AgentRef]Limit{}
	}
	s.Limits[agent] = *l
}

// RestrictModels limits agent's keys to the named models; no names clears the entry.
func (s *ModelState) RestrictModels(agent AgentRef, names ...string) {
	if s.Models == nil {
		s.Models = map[AgentRef][]string{}
	}
	if len(names) == 0 {
		delete(s.Models, agent)
		return
	}
	s.Models[agent] = names
}

// AllowedModels returns the names agent's keys may request out of served, the plain names its org
// serves: all of them, the State.Models entry's, or none for an internal agent and for a system
// agent without an entry. The result is sorted.
func (s ModelState) AllowedModels(agent AgentRef, served []string) []string {
	out := []string{}
	if IsInternalAgent(agent) {
		return out
	}
	narrow, ok := s.Models[agent]
	if !ok && IsSystemAgent(agent) {
		return out
	}
	for _, n := range served {
		if !ok || slices.Contains(narrow, n) {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// ServedModels returns the plain names of the priced models org serves in s, sorted.
func (s ModelState) ServedModels(org string) []string {
	var out []string
	for _, p := range s.Providers {
		if p.Org != org {
			continue
		}
		for _, m := range p.Models {
			if m.Price != nil {
				out = append(out, m.Name)
			}
		}
	}
	slices.Sort(out)
	return out
}

// Restricted reports whether agent's keys carry a model list of their own.
func (s ModelState) Restricted(agent AgentRef) bool {
	_, ok := s.Models[agent]
	return ok || IsSystemAgent(agent)
}

// GrantTools replaces agent's MCP allowlist.
func (s *MCPState) GrantTools(agent AgentRef, grants ...ToolGrant) {
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
	// betaValue is one anthropic-beta value, a dated feature name such as thinking-binding-controls-2026-08-01.
	betaValue = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
	// providerType is an agentgateway providerOverride.
	providerType = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	keyPath      = regexp.MustCompile(`^/[A-Za-z0-9_./-]{1,256}$`)
	// targetName is agentgateway's SectionName rule for MCP targets (lowercase letters, digits, '-'
	// and '.'; '_' and '+' are its delimiters).
	targetName = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,126}[a-z0-9])?$`)
	// headerName is an HTTP token.
	headerName = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+.^_|~-]{1,128}$`)
)

// loaderDollar ends the error for an org-supplied string with '$' that a driver writes into the
// gateway's config: agentgateway's file loader reads $NAME and ${NAME} as environment variables
// (#668). The org-facing validators (catalog, mcpservers) refuse it first; this is the backstop
// every driver passes.
const loaderDollar = ", which the gateway's config loader reads as an environment variable"

// ToolRules renders one CEL allow rule per agent for agentgateway's MCP authorization. The gateway
// allows a tool when any rule matches, and filters tools/list the same way, so an agent sees only
// its granted tools. The last rule admits ReadyPrompt for every key: the gateway filters
// prompts/list by the same rules, and the tool rules never match a prompt. With no grants at all
// the prompt rule alone keeps the policy deny-all for tools instead of absent.
func ToolRules(tools map[AgentRef][]ToolGrant) []string {
	agents := make([]AgentRef, 0, len(tools))
	for a := range tools {
		agents = append(agents, a)
	}
	slices.SortFunc(agents, func(a, b AgentRef) int { return strings.Compare(a.String(), b.String()) })

	var rules []string
	for _, a := range agents {
		var alts []string
		for _, g := range tools[a] {
			if g.All {
				alts = append(alts, fmt.Sprintf("(mcp.tool.target == %s)", strconv.Quote(g.Target)))
				continue
			}
			if len(g.Tools) == 0 {
				continue
			}
			names := make([]string, len(g.Tools))
			for i, t := range g.Tools {
				names[i] = strconv.Quote(t)
			}
			alts = append(alts, fmt.Sprintf("(mcp.tool.target == %s && mcp.tool.name in [%s])",
				strconv.Quote(g.Target), strings.Join(names, ", ")))
		}
		if len(alts) == 0 {
			continue
		}
		rules = append(rules, fmt.Sprintf("apiKey.agent == %s && (%s)", strconv.Quote(a.String()), strings.Join(alts, " || ")))
	}
	return append(rules, "mcp.prompt.name == "+strconv.Quote(ReadyPrompt))
}

// Validate checks s for values that would be unsafe or ambiguous in gateway
// config. Drivers call it before rendering. The joined error lists the faults in a fixed order:
// keys, then the model half (limits, providers, models), then the MCP half (targets, tools).
func (s State) Validate() error {
	errs := validateKeys(s.Keys)
	errs = append(errs, s.ModelState.validate()...)
	errs = append(errs, s.MCPState.validate()...)
	return invalidState(errs)
}

// Validate checks the keys and the model half, with the same errors State.Validate reports for them.
func (c ModelConfig) Validate() error {
	return invalidState(append(validateKeys(c.Keys), c.validate()...))
}

// Validate checks the keys and the MCP half, with the same errors State.Validate reports for them.
func (c MCPConfig) Validate() error {
	return invalidState(append(validateKeys(c.Keys), c.validate()...))
}

func invalidState(errs []error) error {
	if len(errs) > 0 {
		return fmt.Errorf("gateway: invalid state: %w", errors.Join(errs...))
	}
	return nil
}

// validateKeys checks the keys both halves share.
func validateKeys(keys []Key) []error {
	var errs []error
	seenID := map[string]bool{}
	seenHash := map[string]bool{}
	for _, k := range keys {
		if !keyID.MatchString(k.ID) {
			errs = append(errs, fmt.Errorf("key %q: invalid id", k.ID))
		} else if k.ID == ReloadProbeKeyName {
			errs = append(errs, fmt.Errorf("key %q: reserved for the reload probe", k.ID))
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
	return errs
}

// validate checks the model half; State.Validate runs it after the keys.
func (s ModelState) validate() []error {
	var errs []error
	for a, l := range s.Limits {
		if err := a.Validate(); err != nil {
			errs = append(errs, err)
		}
		if err := l.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("limit for %s: %w", a, err))
		}
	}
	seenModel := map[string]bool{}
	seenProvider := map[string]bool{}
	for _, p := range s.Providers {
		ref := p.Org + "/" + p.Name
		switch {
		case !orgSlug.MatchString(p.Org):
			errs = append(errs, fmt.Errorf("provider %q: org must be an org slug", ref))
		case !dnsLabel.MatchString(p.Name):
			errs = append(errs, fmt.Errorf("provider %q: name must be a DNS label", ref))
		case seenProvider[ref]:
			errs = append(errs, fmt.Errorf("provider %q: duplicate", ref))
		}
		seenProvider[ref] = true
		if !providerType.MatchString(p.Type) {
			errs = append(errs, fmt.Errorf("provider %q: invalid type %q", ref, p.Type))
		}
		if p.BaseURL == "" {
			errs = append(errs, fmt.Errorf("provider %q: base URL is required", ref))
		}
		if strings.Contains(p.BaseURL, "$") {
			errs = append(errs, fmt.Errorf("provider %q: base URL contains '$'%s", ref, loaderDollar))
		}
		if p.APIKeyFile != "" && !keyPath.MatchString(p.APIKeyFile) {
			errs = append(errs, fmt.Errorf("provider %q: invalid key file path", ref))
		}
		if p.AnthropicWorkspaceID != "" && !modelformat.ValidWorkspaceID(p.AnthropicWorkspaceID) {
			errs = append(errs, fmt.Errorf("provider %q: invalid anthropic workspace id", ref))
		}
		for _, m := range p.Models {
			if !modelformat.Known(m.Format) {
				errs = append(errs, fmt.Errorf("model %q of provider %q: unknown or unset format %q", m.Name, ref, m.Format))
			} else if _, err := RenderingOf(m); err != nil {
				errs = append(errs, fmt.Errorf("model %q of provider %q: %w", m.Name, ref, err))
			}
			if err := m.Thinking.Validate(); err != nil {
				errs = append(errs, fmt.Errorf("model %q of provider %q: %w", m.Name, ref, err))
			}
			for _, b := range m.Thinking.Betas {
				// A beta becomes a header value the gateway sets.
				if !betaValue.MatchString(b) {
					errs = append(errs, fmt.Errorf("model %q of provider %q: invalid anthropic beta %q", m.Name, ref, b))
				}
			}
			if !modelName.MatchString(m.Name) {
				errs = append(errs, fmt.Errorf("model %q of provider %q: invalid name", m.Name, ref))
			}
			if seenModel[p.Org+"/"+m.Name] {
				errs = append(errs, fmt.Errorf("model %q: served twice in org %q", m.Name, p.Org))
			}
			seenModel[p.Org+"/"+m.Name] = true
		}
	}
	for a, names := range s.Models {
		if err := a.Validate(); err != nil {
			errs = append(errs, err)
		}
		for _, n := range names {
			if !modelName.MatchString(n) {
				errs = append(errs, fmt.Errorf("models for %s: invalid name %q", a, n))
			}
		}
	}
	return errs
}

// validate checks the MCP half; State.Validate runs it after the model half.
func (s MCPState) validate() []error {
	var errs []error
	targets := map[string]bool{}
	for _, t := range s.MCP {
		switch {
		case !targetName.MatchString(t.Name) || t.URL == "":
			errs = append(errs, fmt.Errorf("mcp target %q: invalid name or empty URL", t.Name))
		case targets[t.Name]:
			errs = append(errs, fmt.Errorf("mcp target %q: duplicate", t.Name))
		}
		if t.Org != "" && !orgSlug.MatchString(t.Org) {
			errs = append(errs, fmt.Errorf("mcp target %q: org must be an org slug", t.Name))
		}
		if (t.CredentialHeader == "") != (t.CredentialFile == "") {
			errs = append(errs, fmt.Errorf("mcp target %q: a credential needs both a header and a file", t.Name))
		}
		if t.CredentialHeader != "" && !headerName.MatchString(t.CredentialHeader) {
			errs = append(errs, fmt.Errorf("mcp target %q: invalid credential header name", t.Name))
		}
		if t.CredentialFile != "" && !keyPath.MatchString(t.CredentialFile) {
			errs = append(errs, fmt.Errorf("mcp target %q: invalid credential file path", t.Name))
		}
		if strings.ContainsAny(t.CredentialPrefix, "\r\n") {
			errs = append(errs, fmt.Errorf("mcp target %q: invalid credential prefix", t.Name))
		}
		for _, f := range [...]struct{ name, v string }{{"URL", t.URL}, {"credential header", t.CredentialHeader}, {"credential prefix", t.CredentialPrefix}} {
			if strings.Contains(f.v, "$") {
				errs = append(errs, fmt.Errorf("mcp target %q: %s contains '$'%s", t.Name, f.name, loaderDollar))
			}
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
	return errs
}

const (
	// ReloadProbeKeyName is the ID of the key a driver puts in each
	// config to detect its reload (standalone.ReloadProbeKey). Its requests
	// never reach a model; the spend ledger drops them.
	ReloadProbeKeyName = "koinon-reload-probe"
)

// UsageRecord is the Koinon usage record (ADR 0097 decision 3): one per gateway request, whichever
// gateway served it. The spend ledger reads only this and never a gateway's own fields; each driver
// maps what its gateway logs into it (for agentgateway, package agentgateway).
type UsageRecord struct {
	// RequestID is unique per request and is the ledger's idempotency key.
	RequestID string
	// KeyID is the gateway key that made the request.
	KeyID string
	// Agent is the key's agent: the org slug and the agent ID. A run is found from it and Time
	// (the per-run key of ADR 0073 will name the run directly).
	Agent AgentRef
	// Time is when the gateway logged the request.
	Time  time.Time
	Model string
	// Format is the API format the model was served in (ADR 0069); Stream says the response
	// streamed.
	Format modelformat.Format
	Stream bool
	// Status is the HTTP status the gateway answered (0 when not recorded). UpstreamKnown says the
	// record tells whether the provider answered; Upstream is that answer, so Status is the
	// provider's. Without it the origin of a status is not known.
	Status                  int64
	UpstreamKnown, Upstream bool
	// Error is why the gateway failed the request, empty when it did not ("Upstream" for a stream
	// the upstream cut).
	Error string

	// The token buckets are disjoint (ADR 0097 decision 3): InputTokens excludes CacheReadTokens and
	// CacheWriteTokens, OutputTokens excludes ReasoningTokens, and every bucket is priced as it
	// stands. A driver whose gateway reports totals subtracts the parts when it maps its log. When
	// ReasoningKnown is false the reasoning is not split out and OutputTokens is the whole output.
	// InputKnown and OutputKnown say the gateway reported the side.
	InputTokens, OutputTokens         int64
	InputKnown, OutputKnown           bool
	CacheReadTokens, CacheWriteTokens int64
	// ReasoningTokens is the provider-reported reasoning part of the output. ReasoningKnown says the
	// split was reported; without it the 0 means "not reported", not "no thinking".
	ReasoningTokens int64
	ReasoningKnown  bool

	// MaxTokens is the request's max_tokens (0 when not recorded). RequestBytes and ResponseBytes
	// are -1 when not recorded. They feed the estimate and the reserve (ADR 0056).
	MaxTokens                   int64
	RequestBytes, ResponseBytes int64
	// ResponseID is the provider's response ID (an OpenRouter generation ID), empty when none.
	ResponseID string

	// CostUSD is the gateway's own cost as a decimal string, empty when the driver has none or
	// could not price the request. It is compared with Koinon's own computation, never charged.
	CostUSD string

	// Cut is set by the ledger, never by a driver: a response with output usage that was cut
	// anyway, so its usage is partial, not final.
	Cut bool
}

// UsageReader yields usage records, for drivers whose gateway can be polled.
// The spend ledger does not poll: agentgateway pushes its access log over OTLP
// to koinon-api (package agentgateway maps it to usage records), so drivers return NoUsage.
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
