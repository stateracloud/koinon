// SPDX-License-Identifier: Apache-2.0

package mcpgateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/trace"

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
	if u, ok := g.pool[key]; ok && !u.idle(g.o.IdleTTL) && !u.gt.dead() && u.cfg.Same(t) && u.acquire() {
		g.mu.Unlock()
		return u, nil
	}
	// A request that carries an old snapshot must not evict the session of the current config, nor
	// connect to a target an apply changed or removed.
	if now, ok := g.snap.Load().targets[t.Name]; !ok || !now.Same(t) {
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
	if now, ok := g.snap.Load().targets[t.Name]; !ok || !now.Same(t) {
		// The config changed while this session connected. Serve this one call on it, never pool it.
		u.retired = true
		g.mu.Unlock()
		return u, nil
	}
	if cur, ok := g.pool[key]; ok && cur.cfg.Same(t) && !cur.gt.dead() && !cur.idle(g.o.IdleTTL) && cur.acquire() { // another request connected first
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

// NewTargetHTTPClient returns the HTTP client of one upstream target for one agent: the target's CA
// bundle or skipped verification applies to this client only, the credential is set on every request
// (and only to the target's host, over https), and a redirect is never followed, so the credential
// stays with the target. base nil is http.DefaultTransport. The MCP path and the A2A outbound client
// (ADR 0123 decision 7) both build their clients here, so one rule covers both. src is a [ScrubbedSource]:
// only [CredentialScrub.Source] makes one, so every credential this client sends was recorded first (#1221).
func NewTargetHTTPClient(base http.RoundTripper, t gateway.MCPTarget, agent gateway.AgentRef, src ScrubbedSource) (*http.Client, error) {
	if src.IsZero() {
		// No default source: a caller that has none would open the credential where the scrub cannot record it.
		return nil, ErrNoCredentialSource
	}
	if base == nil {
		base = http.DefaultTransport
	}
	switch {
	case t.CAFile != "" && t.InsecureSkipVerify:
		// State validation refuses this pair; fail closed if one slips through.
		return nil, errors.New("mcpgateway: a target cannot have both a CA bundle and skipped verification")
	case t.CAFile != "":
		var err error
		if base, err = caTransport(base, t.CAFile); err != nil {
			return nil, err
		}
	case t.InsecureSkipVerify:
		var err error
		if base, err = insecureTransport(base); err != nil {
			return nil, err
		}
	}
	return &http.Client{
		Transport: &credentialTransport{base: base, target: t, agent: agent, src: src},
		Timeout:   0,
		// A target must not redirect the gateway: the credential stays with the target's host.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

func (g *Gateway) connect(ctx context.Context, key poolKey, t gateway.MCPTarget) (*upstream, error) {
	hc, err := NewTargetHTTPClient(g.o.HTTPClient.Transport, t, key.agent, g.creds)
	if err != nil {
		return nil, err
	}
	// One bound on every upstream reply (issue 1347): the SDK reads a JSON reply without a limit.
	maxResp := g.o.MaxResponseBytes
	if maxResp > 0 {
		hc.Transport = &limitTransport{base: hc.Transport, max: maxResp, onLimit: func() {
			g.o.Logger.Warn("an upstream MCP reply went over the size limit and its call failed", "target", t.Name, "limit", maxResp)
		}}
	}
	maxEvent := int(min(maxResp, int64(mcp.DefaultMaxEventSize)))
	if maxResp < 0 {
		maxEvent = -1 // the SDK reads a negative value as no cap; zero is its 16 MiB default
	}
	var inner mcp.Transport = &mcp.StreamableClientTransport{
		Endpoint: t.URL, HTTPClient: hc, DisableStandaloneSSE: true, MaxRetries: -1, MaxEventSize: maxEvent,
	}
	if t.SSE {
		inner = &mcp.SSEClientTransport{Endpoint: t.URL, HTTPClient: hc, MaxEventSize: maxEvent}
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "koinon-gateway", Version: "0"}, &mcp.ClientOptions{
		// An empty set of capabilities: no sampling, no elicitation. AddRoots below adds roots.
		Capabilities: &mcp.ClientCapabilities{},
		// A target that says its tools changed gets the same check as a tools/list (#1132): a tool whose
		// text moved is left out before the agent can call it.
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
			go func() {
				defer func() { _ = recover() }()
				g.refresh(t, key.agent)
			}()
		},
	})
	// ADR 0097 decision 5 has the gateway answer roots/list with one fixed folder. Roots are
	// deprecated from MCP 2026-07-28 but work for twelve more months; sampling stays unadvertised.
	client.AddRoots(&mcp.Root{URI: RootURI, Name: "workspace"}) //nolint:staticcheck // see above
	// The handshake has a deadline; the stream of a legacy SSE target must outlive it. The stream
	// context ends when the session retires, or when the handshake fails.
	var handed atomic.Bool
	sctx, stop := context.WithCancel(detach(ctx))
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
	// Message.Credential is the kind at connect time, kept for the life of the session (it ends at the
	// idle TTL or a config change). The credential value is read on every request, not here.
	credKind := CredentialError
	if c, err := g.creds.Credential(ctx, t, key.agent); err == nil {
		credKind = c.Kind
	}
	gt := &guardTransport{
		inner: inner, guard: g.g, target: t.Name, org: t.Org, cred: credKind.String(), agent: key.agent,
		timeout: g.o.CallTimeout, log: g.o.Logger, metrics: g.o.Metrics,
	}
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

// caTransport returns a copy of base that verifies the target with the PEM bundle at caFile only,
// as agentgateway's backendTLS.root does: the bundle replaces the system roots, and it applies to
// this target alone. A missing or unparsable file, or a base that is not an *http.Transport, fails
// closed: the call is never made with other roots.
func caTransport(base http.RoundTripper, caFile string) (http.RoundTripper, error) {
	bt, ok := base.(*http.Transport)
	if !ok {
		return nil, errors.New("mcpgateway: a target with a CA bundle needs an *http.Transport")
	}
	pem, err := os.ReadFile(caFile) //nolint:gosec // the path is the rendered CA file in the gateway Secret mount
	if err != nil {
		return nil, fmt.Errorf("mcpgateway: read the CA bundle: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("mcpgateway: the CA bundle holds no PEM certificate")
	}
	c := bt.Clone()
	cfg := c.TLSClientConfig
	if cfg == nil {
		cfg = &tls.Config{}
	}
	cfg.RootCAs, cfg.MinVersion = pool, tls.VersionTLS12
	c.TLSClientConfig = cfg
	return c, nil
}

// insecureTransport returns a copy of base that does not verify the target's certificate (the server
// record's insecureSkipVerify, as curl -k). It applies to this target alone: the copy is not shared.
func insecureTransport(base http.RoundTripper) (http.RoundTripper, error) {
	bt, ok := base.(*http.Transport)
	if !ok {
		return nil, errors.New("mcpgateway: a target with skipped verification needs an *http.Transport")
	}
	c := bt.Clone()
	cfg := c.TLSClientConfig
	if cfg == nil {
		cfg = &tls.Config{}
	}
	cfg.InsecureSkipVerify, cfg.MinVersion = true, tls.VersionTLS12
	c.TLSClientConfig = cfg
	return c, nil
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
	var changed []string
	for k, u := range g.pool {
		if t, ok := cur.targets[k.target]; !ok || !t.Same(u.cfg) {
			gone = append(gone, u)
			changed = append(changed, k.target)
			delete(g.pool, k)
		}
	}
	g.mu.Unlock()
	// The tools a list left out belong to the old target config: the next list decides again.
	for _, n := range changed {
		g.left.forget(n)
	}
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

// detach returns a context for the life of a pooled session. It keeps no cancellation and no value of
// the request that connected the session: a session serves many requests, and the snapshot and the
// trace span of the first one must not judge the messages of the later ones.
func detach(ctx context.Context) context.Context {
	ctx = context.WithoutCancel(ctx)
	ctx = context.WithValue(ctx, reqKey{}, (*snapshot)(nil))
	return trace.ContextWithSpan(ctx, trace.SpanFromContext(context.Background()))
}
