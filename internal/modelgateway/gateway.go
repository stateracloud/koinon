// SPDX-License-Identifier: Apache-2.0

// Package modelgateway is Koinon's own model path (ADR 0097 step 2): a reverse proxy for the two
// native formats, OpenAI chat completions and Anthropic Messages. It builds every upstream request
// from a header allowlist, taps the response for usage without keeping a body, and hands one usage
// record per request to a sink. It does no format translation (ADR 0069).
//
// It stops an agent whose spend reached its limit and charges each call after it ends (budget.go).
// It does not retry or fail over: the Koinon config has no retry rule today (ADR 0097 decision 5).
package modelgateway

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/credkind"
	"github.com/stateracloud/koinon/pricing"
)

// UsageSink gets one record per request that reached the model path's proxy stage. It must not block.
type UsageSink func(gateway.UsageRecord)

// Options configures a [Gateway]. The zero value is usable.
type Options struct {
	// Sink gets the usage records. Nil drops them.
	Sink UsageSink
	// Tracer makes one span per model call. Nil makes none.
	Tracer trace.Tracer
	// HTTPClient is the upstream transport. Nil builds one with a pool sized for many streams to few
	// hosts, no compression and no redirects.
	HTTPClient *http.Client
	// Metrics counts requests and failures with fixed label sets. Nil counts nothing.
	Metrics *Metrics
	// Logger gets the gateway's own log lines. They never carry bodies, keys or IDs of agents.
	Logger *slog.Logger
	// KeyTTL is how long a provider key file is cached. Zero means 30 seconds.
	KeyTTL time.Duration
	// Scrub, when set, gets each provider key the gateway reads, with its kind and the provider's org, before
	// it is sent, so the scrubbed logger and spans mask it. The binary passes the Record of the gateway's
	// credential scrub, which reads the rule of the kind from the one list (credkind, #1221).
	Scrub func(kind credkind.Kind, org string, values ...string)
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
	// Actors checks a bearer that is not a gateway key as a Substrate actor JWT for the model audience
	// (issue #1489, gateway.IdentifyActor). The request then counts against the agent's own gateway key
	// (its budget and usage records). Nil: only gateway keys are accepted.
	Actors gateway.ActorIdentifier
}

// Gateway is the model path. It implements [gateway.ModelPathGateway].
type Gateway struct {
	o    Options
	snap atomic.Pointer[snapshot]
	bud  *budgets

	applyMu sync.Mutex // keeps the live counters and the snapshot in step

	mu    sync.Mutex
	drain bool
	keys  map[string]cachedKey
}

type cachedKey struct {
	val string
	at  time.Time
}

// New returns a Gateway that serves nothing until ApplyModel.
func New(o Options) *Gateway {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.KeyTTL == 0 {
		o.KeyTTL = 30 * time.Second
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.HTTPClient == nil {
		o.HTTPClient = newHTTPClient()
	}
	g := &Gateway{o: o, bud: newBudgets(), keys: map[string]cachedKey{}}
	g.snap.Store(&snapshot{})
	return g
}

func newHTTPClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DisableCompression = true // the tap reads plain bytes, and the client's own encoding is not forwarded
	t.MaxIdleConns, t.MaxIdleConnsPerHost = 256, 64
	t.ResponseHeaderTimeout = 0 // a model may think for minutes before its first byte
	return &http.Client{Transport: t, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// route is one model an org serves.
type route struct {
	p     gateway.Provider
	m     gateway.Model
	price pricing.Price // the model's price, parsed once: the charge after each call
}

// snapshot is one applied configuration. A request reads one snapshot.
type snapshot struct {
	rev  gateway.Revision
	keys map[string]gateway.Key // by "sha256:" hash
	// byAgent is the one key of each agent, for a request that an actor JWT identifies. An agent with
	// more than one key has none here: such a request is refused (fail closed).
	byAgent map[gateway.AgentRef]gateway.Key
	state   gateway.ModelState
	routes  map[string]map[string]route // org -> model name -> route (priced models only)
}

// ErrInvalidConfig means ApplyModel refused the config; the gateway keeps the one it had.
var ErrInvalidConfig = errors.New("modelgateway: invalid config")

// ApplyModel implements gateway.ModelPathGateway. It is idempotent and returns once the new config
// serves: a key of the config authenticates from the next request on.
func (g *Gateway) ApplyModel(ctx context.Context, c gateway.ModelConfig) (gateway.Revision, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := c.Validate(); err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	s := &snapshot{keys: map[string]gateway.Key{}, state: c.ModelState, routes: map[string]map[string]route{}}
	// The snapshot owns its maps: a caller that changes its config later must not change the one in force.
	s.state.Limits = maps.Clone(c.Limits)
	s.state.Models = maps.Clone(c.Models)
	for _, k := range c.Keys {
		if k.ID == "" || !strings.HasPrefix(k.Hash, "sha256:") {
			return "", fmt.Errorf("%w: key %q has no sha256 hash", ErrInvalidConfig, k.ID)
		}
		s.keys[k.Hash] = k
	}
	s.byAgent = keysByAgent(c.Keys)
	for _, p := range c.Providers {
		u, err := url.Parse(p.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			g.o.Logger.Warn("model provider dropped", "provider", p.Name, "reason", "base_url")
			g.o.Metrics.dropped("base_url")
			continue
		}
		for _, m := range p.Models {
			// A model without a price is not served: its spend cannot be charged (ADR 0041).
			if m.Price == nil {
				continue
			}
			price, err := pricing.Spec{Prices: wireOf(m.Price.Input, m.Price.Output, m.Price.CacheRead, m.Price.CacheWrite, m.Price.Reasoning), Tiers: tiersOf(m.Price.Tiers)}.Parse()
			if err != nil {
				// A price that cannot be parsed cannot be charged, so the model is not served.
				g.o.Logger.Warn("model dropped", "provider", p.Name, "reason", "price")
				g.o.Metrics.dropped("price")
				continue
			}
			if _, err := gateway.RenderingOf(m); err != nil {
				g.o.Logger.Warn("model dropped", "provider", p.Name, "reason", "format")
				g.o.Metrics.dropped("format")
				continue
			}
			if s.routes[p.Org] == nil {
				s.routes[p.Org] = map[string]route{}
			}
			s.routes[p.Org][m.Name] = route{p: p, m: m, price: price}
		}
	}
	s.rev = revisionOf(c)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// One apply at a time. The live counters and the snapshot change together, under the lock of the
	// counters: a request that reads the new snapshot finds its counters live, and a check that
	// finds its counters retired finds the new snapshot published.
	g.applyMu.Lock()
	defer g.applyMu.Unlock()
	g.bud.swap(s.state.Limits, func() { g.snap.Store(s) })
	return s.rev, nil
}

func wireOf(in, out, cr, cw, re string) pricing.Wire {
	return pricing.Wire{Input: in, Output: out, CacheRead: cr, CacheWrite: cw, Reasoning: re}
}

func tiersOf(ts []gateway.PriceTier) []pricing.WireTier {
	var out []pricing.WireTier
	for _, t := range ts {
		out = append(out, pricing.WireTier{ContextOver: t.ContextOver, Prices: wireOf(t.Input, t.Output, t.CacheRead, t.CacheWrite, t.Reasoning)})
	}
	return out
}

// revisionOf derives the revision from the content, so equal configs give equal revisions.
func revisionOf(c gateway.ModelConfig) gateway.Revision {
	keys := make([]string, 0, len(c.Keys))
	for _, k := range c.Keys {
		keys = append(keys, k.ID+"|"+k.Agent.String()+"|"+k.Hash)
	}
	sort.Strings(keys)
	// A map keyed by AgentRef does not encode as JSON, so the maps become lists sorted by agent.
	limits := make([]any, 0, len(c.Limits))
	for a, l := range c.Limits {
		limits = append(limits, []any{a.String(), l})
	}
	models := make([]any, 0, len(c.Models))
	for a, m := range c.Models {
		models = append(models, []any{a.String(), m})
	}
	byAgent := func(x []any) {
		sort.Slice(x, func(i, j int) bool { return x[i].([]any)[0].(string) < x[j].([]any)[0].(string) })
	}
	byAgent(limits)
	byAgent(models)
	b, err := json.Marshal([]any{keys, limits, c.Providers, models})
	if err != nil {
		// Every field is plain data: a failure is a bug, and a constant revision would hide it.
		panic("modelgateway: revision: " + err.Error())
	}
	sum := sha256.Sum256(b)
	return gateway.Revision("model-" + hex.EncodeToString(sum[:8]))
}

// Revision is the revision of the config in force, "" before the first ApplyModel.
func (g *Gateway) Revision() gateway.Revision { return g.snap.Load().rev }

// Drain makes the gateway refuse new requests with 503; a request in flight finishes.
func (g *Gateway) Drain() {
	g.mu.Lock()
	g.drain = true
	g.mu.Unlock()
}

func (g *Gateway) draining() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.drain
}

// keysByAgent maps each agent with exactly one key to that key.
func keysByAgent(keys []gateway.Key) map[gateway.AgentRef]gateway.Key {
	out := make(map[gateway.AgentRef]gateway.Key, len(keys))
	many := map[gateway.AgentRef]bool{}
	for _, k := range keys {
		if _, seen := out[k.Agent]; seen {
			many[k.Agent] = true
		}
		out[k.Agent] = k
	}
	for a := range many {
		delete(out, a)
	}
	return out
}

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

// resolve finds the route of a plain model name for an agent: the model must be served in the
// agent's org and allowed to the agent.
func (s *snapshot) resolve(agent gateway.AgentRef, model string) (route, bool) {
	served := s.routes[agent.Org]
	if served == nil {
		return route{}, false
	}
	names := make([]string, 0, len(served))
	for n := range served {
		names = append(names, n)
	}
	if !slices.Contains(s.state.AllowedModels(agent, names), model) {
		return route{}, false
	}
	return served[model], true
}

// providerKey returns the provider key of p, "" when it needs none. A key that is configured but
// cannot be read fails closed: the request is not sent without it.
func (g *Gateway) providerKey(p gateway.Provider) (string, error) {
	if p.APIKeyFile == "" {
		return "", nil
	}
	now := g.o.Now()
	g.mu.Lock()
	c, ok := g.keys[p.APIKeyFile]
	g.mu.Unlock()
	if ok && now.Sub(c.at) < g.o.KeyTTL {
		return c.val, nil
	}
	b, err := os.ReadFile(p.APIKeyFile)
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(string(b))
	// The key is recorded before it is cached, so no request sends a key that the scrub does not know. A
	// cached key was recorded when it was read; the scrub keeps it for a day after that (ADR 0097).
	if g.o.Scrub != nil && v != "" {
		g.o.Scrub(credkind.ProviderKey, p.Org, v)
	}
	g.mu.Lock()
	g.keys[p.APIKeyFile] = cachedKey{val: v, at: now}
	g.mu.Unlock()
	return v, nil
}

var _ gateway.ModelPathGateway = (*Gateway)(nil)
