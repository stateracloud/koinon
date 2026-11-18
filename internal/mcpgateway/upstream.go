// SPDX-License-Identifier: Apache-2.0

package mcpgateway

import (
	"context"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stateracloud/koinon/internal/gateway"
)

// newHTTPClient is the base of the upstream clients. Go's default transport keeps 2 idle
// connections per host, too few for many agents on a few targets.
func newHTTPClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = 512
	t.MaxIdleConnsPerHost = 64
	t.IdleConnTimeout = 90 * time.Second
	t.DialContext = (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	t.ResponseHeaderTimeout = 0 // a streaming reply is slow to finish, not to start; CallTimeout bounds it
	return &http.Client{Transport: t}
}

// poolKey names one upstream session: the target and the agent whose credential it carries. The
// guard needs the agent on each message, and a passport differs per agent.
type poolKey struct {
	target string
	agent  gateway.AgentRef
}

// upstream is one client session to a target.
type upstream struct {
	key     poolKey
	cfg     gateway.MCPTarget
	session *mcp.ClientSession

	mu       sync.Mutex
	lastUsed time.Time
}

func (u *upstream) close() { _ = u.session.Close() }

func (u *upstream) touch() {
	u.mu.Lock()
	u.lastUsed = time.Now()
	u.mu.Unlock()
}

func (u *upstream) idle(ttl time.Duration) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return time.Since(u.lastUsed) > ttl
}

// session returns the live upstream session for (target, agent), connecting when there is none. The
// client advertises no sampling and no elicitation, and answers roots/list with RootURI.
func (g *Gateway) session(ctx context.Context, t gateway.MCPTarget, agent gateway.AgentRef) (*upstream, error) {
	key := poolKey{target: t.Name, agent: agent}
	g.mu.Lock()
	if g.drain {
		g.mu.Unlock()
		return nil, errDraining
	}
	if u, ok := g.pool[key]; ok && !u.idle(g.o.IdleTTL) && u.cfg == t {
		g.mu.Unlock()
		u.touch()
		return u, nil
	}
	stale := g.pool[key]
	delete(g.pool, key)
	g.mu.Unlock()
	if stale != nil {
		stale.close()
	}

	u, err := g.connect(ctx, key, t)
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	if cur, ok := g.pool[key]; ok { // another request connected first
		g.mu.Unlock()
		u.close()
		cur.touch()
		return cur, nil
	}
	g.pool[key] = u
	g.mu.Unlock()
	return u, nil
}

func (g *Gateway) connect(ctx context.Context, key poolKey, t gateway.MCPTarget) (*upstream, error) {
	hc := &http.Client{
		Transport: &credentialTransport{base: g.o.HTTPClient.Transport, target: t, agent: key.agent, src: g.o.Credentials},
		Timeout:   0,
	}
	var inner mcp.Transport = &mcp.StreamableClientTransport{
		Endpoint: t.URL, HTTPClient: hc, DisableStandaloneSSE: true, MaxRetries: -1,
	}
	if t.SSE {
		inner = &mcp.SSEClientTransport{Endpoint: t.URL, HTTPClient: hc}
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "koinon-gateway", Version: "0"}, &mcp.ClientOptions{
		// An empty set of capabilities: no sampling, no elicitation. AddRoots below adds roots.
		Capabilities: &mcp.ClientCapabilities{},
	})
	// ADR 0097 decision 5 has the gateway answer roots/list with one fixed folder. Roots are
	// deprecated from MCP 2026-07-28 but work for twelve more months; sampling stays unadvertised.
	client.AddRoots(&mcp.Root{URI: RootURI, Name: "workspace"}) //nolint:staticcheck // see above
	cctx, cancel := context.WithTimeout(ctx, g.o.CallTimeout)
	defer cancel()
	s, err := client.Connect(cctx, &guardTransport{inner: inner, guard: g.o.Guard, target: t.Name, agent: key.agent}, nil)
	if err != nil {
		return nil, err
	}
	u := &upstream{key: key, cfg: t, session: s}
	u.touch()
	return u, nil
}

// evict drops the upstream session of key, so the next call connects again.
func (g *Gateway) evict(u *upstream) {
	g.mu.Lock()
	if g.pool[u.key] == u {
		delete(g.pool, u.key)
	}
	g.mu.Unlock()
	u.close()
}

// evictChanged closes the sessions whose target is gone or changed in the new snapshot.
func (g *Gateway) evictChanged(old, cur *snapshot) {
	g.mu.Lock()
	var gone []*upstream
	for k, u := range g.pool {
		if t, ok := cur.targets[k.target]; !ok || t != u.cfg {
			gone = append(gone, u)
			delete(g.pool, k)
		}
	}
	g.mu.Unlock()
	for _, u := range gone {
		u.close()
	}
	_ = old
}

// Sweep closes the upstream sessions that sat idle past IdleTTL. A replica calls it on a timer.
func (g *Gateway) Sweep() {
	g.mu.Lock()
	var idle []*upstream
	for k, u := range g.pool {
		if u.idle(g.o.IdleTTL) {
			idle = append(idle, u)
			delete(g.pool, k)
		}
	}
	g.mu.Unlock()
	for _, u := range idle {
		u.close()
	}
}

// OpenSessions is the number of upstream sessions, for a gauge with no labels.
func (g *Gateway) OpenSessions() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.pool)
}
