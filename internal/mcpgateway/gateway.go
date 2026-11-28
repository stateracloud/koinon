// SPDX-License-Identifier: Apache-2.0

package mcpgateway

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/stateracloud/koinon/internal/gateway"
)

// RootURI is the one folder the gateway answers roots/list with. It names no real path: a target
// sees a workspace and nothing of the agent's sandbox (ADR 0097 decision 5).
const RootURI = "file:///workspace"

// DefaultCallTimeout is the longest one stage of a tool call runs when Options.CallTimeout is zero. A call
// has two stages, each bounded by it: the guard check, which may wait for a person's approval, and the
// upstream call. A call is never retried, so the drain timeout of the process must cover both stages
// ([MinDrain]): a call in flight at SIGTERM then finishes instead of being cut by a rollout (issue 1347).
const DefaultCallTimeout = 2 * time.Minute

// MinDrain is the shortest drain timeout that lets a call in flight finish: one wait for approval and one
// upstream call, each up to [DefaultCallTimeout].
const MinDrain = 2 * DefaultCallTimeout

// Options configures a [Gateway]. The zero value of every field but Guard is usable.
type Options struct {
	// Guard sees every message of every upstream connection. Nil means [AllowGuard]; a deployment
	// that serves org targets sets a real one (ADR 0097: the guard fails closed).
	Guard Guard
	// Credentials resolves the credential of each target. Nil means [StaticCredentials].
	Credentials CredentialSource
	// Audit gets one row per tool call. Nil drops them.
	Audit Audit
	// Tracer makes one span per tool call. Nil makes none.
	Tracer trace.Tracer
	// HTTPClient is the base of the upstream HTTP clients. Nil builds one with a pool sized for
	// many streams to few hosts (Go's default keeps 2 idle connections per host).
	HTTPClient *http.Client
	// Metrics counts what the gateway drops or refuses on its own. Nil counts nothing.
	Metrics *Metrics
	// Logger gets the gateway's own log lines. They never carry arguments, results or tokens.
	Logger *slog.Logger
	// Actors checks a bearer that is not a gateway key as a Substrate actor JWT for the MCP audience
	// (issue #1489, gateway.IdentifyActor). Nil: only gateway keys are accepted.
	Actors gateway.ActorIdentifier
	// IdleTTL is how long an unused upstream session lives. Zero means 5 minutes.
	IdleTTL time.Duration
	// CallTimeout bounds one upstream call. Zero means [DefaultCallTimeout].
	CallTimeout time.Duration
	// MaxResponseBytes bounds one upstream reply (a JSON reply as a whole, one event of a stream). Zero means
	// [DefaultMaxResponseBytes]; negative means no bound (for tests).
	MaxResponseBytes int64
	// ListTimeout bounds one target's part of a tools/list. Zero means 10 seconds.
	ListTimeout time.Duration
	// Builtins are the kinds of built-in target this process can run, by [gateway.MCPTarget.Builtin].
	// A built-in target of another kind is not served. Nil serves none. Each call of a built-in gets
	// Credentials wrapped with Scrub.Source, so a value it opens is scrubbed too ([Builtin.Call]).
	Builtins map[string]Builtin
	// Scrub removes the credential values the gateway opened from its logs, spans and the results it
	// gives to agents (#1174). New wraps Logger, Tracer and Credentials with it. Nil makes one of its
	// own; a process passes its own to scrub its other log lines with the same set.
	Scrub *CredentialScrub
}

// Gateway is the MCP path. It implements [gateway.MCPGateway]: ApplyMCP swaps its configuration,
// and Handler serves agents.
type Gateway struct {
	o     Options
	creds ScrubbedSource // o.Credentials wrapped with o.Scrub: the only source the gateway reads (#1221)
	g     failClosed     // the guard, as the gateway calls it: not loaded or panicking refuses
	snap  atomic.Pointer[snapshot]

	left leftOutSet // the tools each list left out (#1132)

	mu         sync.Mutex
	pool       map[poolKey]*upstream
	refreshing map[poolKey]bool // a list_changed relist is running
	drain      bool
}

// New returns a Gateway that serves nothing until ApplyMCP.
func New(o Options) *Gateway {
	if o.Guard == nil {
		o.Guard = AllowGuard{}
	}
	if o.Credentials == nil {
		o.Credentials = StaticCredentials{}
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.IdleTTL == 0 {
		o.IdleTTL = 5 * time.Minute
	}
	if o.CallTimeout == 0 {
		o.CallTimeout = DefaultCallTimeout
	}
	if o.MaxResponseBytes == 0 {
		o.MaxResponseBytes = DefaultMaxResponseBytes
	}
	if o.ListTimeout == 0 {
		o.ListTimeout = 10 * time.Second
	}
	if o.HTTPClient == nil {
		o.HTTPClient = newHTTPClient()
	}
	// Every opened credential is recorded before it is used, and every log line, span and agent result
	// is scrubbed of the recorded values (#1174).
	if o.Scrub == nil {
		o.Scrub = NewCredentialScrub()
	}
	o.Logger = slog.New(o.Scrub.Handler(o.Logger.Handler()))
	o.Tracer = o.Scrub.Tracer(o.Tracer)
	o.Scrub.use(o.Metrics, o.Logger)
	// o.Credentials is replaced with the wrapped source, so no line in the package can read the raw one.
	creds := o.Scrub.Source(o.Credentials)
	o.Credentials = creds
	g := &Gateway{o: o, creds: creds, g: failClosed{g: o.Guard, log: o.Logger, m: o.Metrics}, pool: map[poolKey]*upstream{}}
	g.snap.Store(&snapshot{})
	return g
}

// snapshot is one applied configuration. A request reads one snapshot, so it sees a consistent one.
type snapshot struct {
	rev     gateway.Revision
	keys    map[string]gateway.Key // by "sha256:" hash
	targets map[string]gateway.MCPTarget
	grants  map[gateway.AgentRef][]gateway.ToolGrant
	// guardless are the org targets left out because this gateway has no leak guard. They serve
	// nothing; a granted call to one is refused with gateway.GuardUnconfiguredReason, so the agent and
	// the operator learn the cause and not "not granted" (ADR 0097: the guard fails closed).
	guardless map[string]gateway.MCPTarget
}

// ErrInvalidConfig means ApplyMCP refused the config; the gateway keeps the one it had.
var ErrInvalidConfig = errors.New("mcpgateway: invalid config")

// ApplyMCP implements gateway.MCPGateway. It is idempotent and returns once the new config is live.
// A rejected config leaves the previous one serving. A target that can never be served safely (a
// credential on a URL that is not https) is left out, which closes its route; the rest applies.
func (g *Gateway) ApplyMCP(ctx context.Context, c gateway.MCPConfig) (gateway.Revision, error) {
	if err := ctx.Err(); err != nil {
		return "", err // the apply deadline passed: keep the config that serves
	}
	if err := c.Validate(); err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	seen := map[string]bool{}
	s := &snapshot{
		keys:      map[string]gateway.Key{},
		targets:   map[string]gateway.MCPTarget{},
		grants:    map[gateway.AgentRef][]gateway.ToolGrant{},
		guardless: map[string]gateway.MCPTarget{},
	}
	for _, k := range c.Keys {
		if k.ID == "" || !strings.HasPrefix(k.Hash, "sha256:") {
			return "", fmt.Errorf("%w: key %q has no sha256 hash", ErrInvalidConfig, k.ID)
		}
		s.keys[k.Hash] = k
	}
	// A target that cannot be served safely is left out, which closes its route; the rest of the
	// config applies. A key revocation must never wait on an unrelated target (ADR 0120 section 2:
	// a rule that fails to load closes the route). The log line names the target and a fixed
	// reason; it carries no org, agent or user ID and no URL. Validate has refused the shapes that
	// no config may have, for the API too; the checks here are the ones only this gateway knows.
	applied := gateway.MCPConfig{Keys: c.Keys}
	drop := func(name, reason string) {
		g.o.Logger.Warn("mcp target dropped", "target", name, "reason", reason)
		g.o.Metrics.dropped(reason)
	}
	for _, t := range c.MCP {
		switch {
		case seen[t.Name]:
			// Two entries claim one name: neither can be trusted to be the right one.
			delete(s.targets, t.Name)
			delete(s.guardless, t.Name)
			drop(t.Name, dropDuplicateName)
		case t.Name == "" || strings.Contains(t.Name, "_"):
			drop("", dropInvalidName)
		case t.Builtin != "" && g.o.Builtins[t.Builtin] == nil:
			// This process has no code for the kind: the target is not served (ADR 0123 decision 10).
			drop(t.Name, dropBuiltinUnsupported)
		case t.URL == "" && t.Builtin == "":
			drop(t.Name, dropNoURL)
		case isAllowGuard(g.o.Guard) && t.Org != "":
			drop(t.Name, dropOrgNeedsGuard)
			s.guardless[t.Name] = t
		case t.Builtin == "" && t.CredentialHeader != "" && !credentialSafeURL(t.URL):
			drop(t.Name, dropCredentialNeedsHTTPS)
		default:
			s.targets[t.Name] = t
		}
		seen[t.Name] = true
	}
	for _, t := range c.MCP {
		if _, ok := s.targets[t.Name]; ok {
			applied.MCP = append(applied.MCP, t)
		}
	}
	for a, gs := range c.Tools {
		cp := make([]gateway.ToolGrant, len(gs))
		for i, gr := range gs {
			cp[i] = gr
			cp[i].Tools = slices.Clone(gr.Tools)
		}
		s.grants[a] = cp
	}
	// The revision names what serves: a dropped target changes it.
	applied.Tools = c.Tools
	s.rev = revisionOf(applied)
	if err := ctx.Err(); err != nil { // the deadline passed: the old config keeps serving
		return "", err
	}
	old := g.snap.Swap(s)
	g.evictChanged(old, s)
	return s.rev, nil
}

// isAllowGuard reports the pass-all guard, by value or by pointer.
func isAllowGuard(g Guard) bool {
	switch g.(type) {
	case AllowGuard, *AllowGuard:
		return true
	}
	return false
}

// revisionOf derives the revision from the content, so equal configs give equal revisions.
func revisionOf(c gateway.MCPConfig) gateway.Revision {
	keys := make([]string, 0, len(c.Keys))
	for _, k := range c.Keys {
		keys = append(keys, k.ID+"|"+k.Agent.String()+"|"+k.Hash)
	}
	sort.Strings(keys)
	targets := slices.Clone(c.MCP)
	slices.SortFunc(targets, func(a, b gateway.MCPTarget) int { return strings.Compare(a.Name, b.Name) })
	agents := make([]gateway.AgentRef, 0, len(c.Tools))
	for a := range c.Tools {
		agents = append(agents, a)
	}
	slices.SortFunc(agents, func(a, b gateway.AgentRef) int { return strings.Compare(a.String(), b.String()) })
	grants := make([]any, 0, len(agents))
	for _, a := range agents {
		grants = append(grants, []any{a.String(), c.Tools[a]})
	}
	b, _ := json.Marshal([]any{keys, targets, grants})
	sum := sha256.Sum256(b)
	return gateway.Revision("mcp-" + hex.EncodeToString(sum[:8]))
}

// Revision is the revision of the config in force, "" before the first ApplyMCP.
func (g *Gateway) Revision() gateway.Revision { return g.snap.Load().rev }

// authenticate finds the key that matches a bearer token. It hashes the token and looks the hash up,
// then compares in constant time.
func (s *snapshot) authenticate(token string) (gateway.Key, bool) {
	if token == "" {
		return gateway.Key{}, false
	}
	h := gateway.HashKey(token)
	k, ok := s.keys[h]
	if !ok || subtle.ConstantTimeCompare([]byte(k.Hash), []byte(h)) != 1 {
		return gateway.Key{}, false
	}
	return k, true
}

// visible reports whether target t is one the agent may reach: a platform target (Org "") or a
// target of the agent's own org. A grant that names another org's target does nothing.
func visible(t gateway.MCPTarget, agent gateway.AgentRef) bool {
	return t.Org == "" || t.Org == agent.Org
}

// allowedTools returns the tools of target the agent may use: nil and all==true for every tool.
func (s *snapshot) allowedTools(agent gateway.AgentRef, target string) (tools map[string]bool, all, ok bool) {
	t, found := s.targets[target]
	if !found {
		return nil, false, false
	}
	return s.granted(agent, t)
}

// granted is allowedTools for a target the caller has found.
func (s *snapshot) granted(agent gateway.AgentRef, t gateway.MCPTarget) (tools map[string]bool, all, ok bool) {
	if !visible(t, agent) {
		return nil, false, false
	}
	for _, gr := range s.grants[agent] {
		if gr.Target != t.Name {
			continue
		}
		if gr.All {
			all = true
		}
		if tools == nil {
			tools = map[string]bool{}
		}
		for _, n := range gr.Tools {
			tools[n] = true
		}
		ok = true
	}
	return tools, all, ok
}

// needsGuard reports a call the grants allow to a target that serves nothing only because this
// gateway has no leak guard. Another org's target is never reported: its name stays hidden.
func (s *snapshot) needsGuard(agent gateway.AgentRef, target, tool string) bool {
	t, found := s.guardless[target]
	if !found {
		return false
	}
	tools, all, ok := s.granted(agent, t)
	return ok && (all || tools[tool])
}

// Drain makes the gateway refuse new requests with 503 and retire every upstream session. A call in
// flight finishes first; each session closes when its last call ends (graceful drain, ADR 0097).
func (g *Gateway) Drain() {
	g.mu.Lock()
	g.drain = true
	pool := g.pool
	g.pool = map[poolKey]*upstream{}
	g.mu.Unlock()
	for _, u := range pool {
		u.retire()
	}
}

var _ gateway.MCPGateway = (*Gateway)(nil)

// toolName is the name an agent sees: the target, an underscore, the tool.
func toolName(target, tool string) string { return target + "_" + tool }

// splitToolName is the inverse of toolName. Target names have no underscore, so the first one splits.
func splitToolName(name string) (target, tool string, ok bool) {
	target, tool, ok = strings.Cut(name, "_")
	return target, tool, ok && target != "" && tool != ""
}
