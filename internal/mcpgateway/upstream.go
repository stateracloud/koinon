// SPDX-License-Identifier: Apache-2.0

package mcpgateway

import (
	"context"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
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
	stop    context.CancelFunc // ends the stream context of the session
	gt      *guardTransport    // its liveness tells that the connection is lost

	mu       sync.Mutex
	lastUsed time.Time
	refs     int  // calls in flight
	retired  bool // no new call may use it; it closes when refs reaches 0
}

// retire closes the session now, or when the last call that uses it ends.
func (u *upstream) retire() {
	u.mu.Lock()
	u.retired = true
	closeNow := u.refs == 0
	u.mu.Unlock()
	if closeNow {
		_ = u.session.Close()
		u.stop()
	}
}

// release ends one use of the session.
func (u *upstream) release() {
	u.mu.Lock()
	u.refs--
	u.lastUsed = time.Now()
	closeNow := u.retired && u.refs == 0
	u.mu.Unlock()
	if closeNow {
		_ = u.session.Close()
		u.stop()
	}
}

// acquire takes one use of the session, or reports false when it is retired.
func (u *upstream) acquire() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.retired {
		return false
	}
	u.refs++
	u.lastUsed = time.Now()
	return true
}

func (u *upstream) idle(ttl time.Duration) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.refs == 0 && time.Since(u.lastUsed) > ttl
}

// session returns the live upstream session for (target, agent), connecting when there is none. The
// caller must call release on it. The
// client advertises no sampling and no elicitation, and answers roots/list with RootURI.
func (g *Gateway) session(ctx context.Context, t gateway.MCPTarget, agent gateway.AgentRef) (*upstream, error) {
	key := poolKey{target: t.Name, agent: agent}
	g.mu.Lock()
	if g.drain {
		g.mu.Unlock()
		return nil, errDraining
	}
	if u, ok := g.pool[key]; ok && !u.idle(g.o.IdleTTL) && !u.gt.dead() && u.cfg == t && u.acquire() {
		g.mu.Unlock()
		return u, nil
	}
	// A request that carries an old snapshot must not evict the session of the current config, nor
	// connect to a target an apply changed or removed.
	if now, ok := g.snap.Load().targets[t.Name]; !ok || now != t {
		g.mu.Unlock()
		return nil, errConfigChanged
	}
	stale := g.pool[key]
	delete(g.pool, key)
	g.mu.Unlock()
	if stale != nil {
		stale.retire()
	}

	u, err := g.connect(ctx, key, t)
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	if g.drain {
		g.mu.Unlock()
		u.retire()
		return nil, errDraining
	}
	u.refs = 1
	if now, ok := g.snap.Load().targets[t.Name]; !ok || now != t {
		// The config changed while this session connected. Serve this one call on it, never pool it.
		u.retired = true
		g.mu.Unlock()
		return u, nil
	}
	if cur, ok := g.pool[key]; ok && cur.cfg == t && !cur.gt.dead() && !cur.idle(g.o.IdleTTL) && cur.acquire() { // another request connected first
		g.mu.Unlock()
		u.refs, u.retired = 0, true
		u.retire()
		return cur, nil
	}
	displaced := g.pool[key]
	g.pool[key] = u
	g.mu.Unlock()
	if displaced != nil {
		displaced.retire() // outside the lock: Close can wait on the network
	}
	return u, nil
}

func (g *Gateway) connect(ctx context.Context, key poolKey, t gateway.MCPTarget) (*upstream, error) {
	base := g.o.HTTPClient.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	hc := &http.Client{
		Transport: &credentialTransport{base: base, target: t, agent: key.agent, src: g.o.Credentials},
		Timeout:   0,
		// A target must not redirect the gateway: the credential stays with the target's host.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
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
	// The handshake has a deadline; the stream of a legacy SSE target must outlive it. The stream
	// context ends when the session retires, or when the handshake fails.
	var handed atomic.Bool
	sctx, stop := context.WithCancel(context.WithoutCancel(ctx))
	timer := time.AfterFunc(g.o.CallTimeout, stop)
	go func() {
		select {
		case <-ctx.Done():
			// The caller gave up before the handshake ended.
			if handed.Load() || !timer.Stop() {
				return
			}
			stop()
		case <-sctx.Done():
		}
	}()
	gt := &guardTransport{inner: inner, guard: g.o.Guard, target: t.Name, agent: key.agent}
	s, err := client.Connect(sctx, gt, nil)
	if err != nil {
		timer.Stop()
		stop()
		return nil, err
	}
	handed.Store(true)
	if !timer.Stop() || sctx.Err() != nil || ctx.Err() != nil {
		// The deadline or the caller ended the stream as the handshake finished: do not pool it.
		_ = s.Close()
		stop()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, context.DeadlineExceeded
	}
	return &upstream{key: key, cfg: t, session: s, lastUsed: time.Now(), stop: stop, gt: gt}, nil
}

// evict drops the upstream session of key, so the next call connects again.
// Calls in flight on it finish first.
func (g *Gateway) evict(u *upstream) {
	g.mu.Lock()
	if g.pool[u.key] == u {
		delete(g.pool, u.key)
	}
	g.mu.Unlock()
	u.retire()
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
		u.retire()
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
		u.retire()
	}
}

// OpenSessions is the number of upstream sessions, for a gauge with no labels.
func (g *Gateway) OpenSessions() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.pool)
}
