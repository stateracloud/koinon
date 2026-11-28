// SPDX-License-Identifier: Apache-2.0

// Package egress serves the forge and registry L7 routes of Koinon's gateway (ADR 0097 step 2 as
// amended by ADR 0119 D1 and D11, ADR 0120 section 2). The gateway binary serves the route table of
// package route (#1086 part A). The org routes come from the State stream (part B), and the credential
// of a route is a sealed file that only the gateway key opens (part C).
//
// A guest never names the gateway as an HTTP proxy, and the gateway never terminates TLS for a host
// it does not own. The guest addresses the gateway by name, and the path names the upstream:
//
//	<gateway>/egress/<host>[:<port>]/<path>?<query>
//
// The gateway sends the request to https://<host>[:<port>]/<path>?<query> when the actor token is
// valid and a route allows the host, method, path and query. Git and the package managers take such a
// base URL (git's url.<base>.insteadOf, npm's registry, pip's index-url), so the guest needs no CA
// of Koinon's and no proxy setting, and the gateway holds no key that can sign for another host.
//
// What the handler guarantees:
//
//   - Default deny. No table, no token, a bad or repeated token, the guest's placeholder, an actor that
//     Koinon's record does not hold, an identity check that cannot run, or no route refuses the request.
//     There is no audit-only mode. A held install (ADR 0107, Options.Held) refuses every request before any check.
//   - The upstream leg is HTTPS only, to the exact host and port of the route, through the dialer the
//     caller gives (the URL policy of package catalog in the binary: no loopback, link-local,
//     metadata or private address unless the platform allowed the host).
//   - The actor token, Authorization, Cookie, Proxy-* and Forwarded headers of the guest never reach
//     the upstream. The gateway alone attaches a credential, and only to the route that names it: the
//     credential's org and agent are the route's, the route matched the actor's token, and the value
//     goes to the route's own host over HTTPS. A route whose credential cannot be read is refused
//     (credential_unavailable); it is never served without it.
//   - The credential never comes back to the guest: on a credentialed route the gateway asks the
//     upstream for an uncompressed body, refuses an encoded one, and masks the value (and its basic
//     form) in the response headers and the body.
//   - The gateway does not follow redirects. A redirect to an https URL comes back rewritten to the
//     gateway form, so the guest's next request passes the gate again.
//   - Logs and metrics never hold the token, a credential, the path or the query. Metric labels are
//     the fixed route class and reason (ADR 0023).
package egress

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/stateracloud/koinon/internal/gateway/credkind"
	"github.com/stateracloud/koinon/internal/gateway/route"
	"github.com/stateracloud/koinon/internal/restorehold"
)

// PathPrefix is where the egress routes are served. The segment after it is the upstream authority.
const PathPrefix = "/egress/"

// RefusalHeader names the reason of a refusal on the response, so an agent can say why a fetch
// failed. The value is a fixed reason, never request data.
const RefusalHeader = "Koinon-Egress-Refusal"

// Reasons that the handler adds to the ones of package route. The set is fixed (ADR 0023).
const (
	// ReasonNoCredential: the route names a credential and the gateway cannot read it.
	ReasonNoCredential route.Reason = "credential_unavailable" //nolint:gosec // a refusal reason, not a credential
	// ReasonUpstream: the upstream could not be reached or failed before it answered.
	ReasonUpstream route.Reason = "upstream_error"
	// ReasonDraining: the gateway stops and takes no new request.
	ReasonDraining route.Reason = "draining"
	// ReasonHeld: the install is held for restore (ADR 0107): a held gateway is ready but deny-all.
	ReasonHeld route.Reason = "held"
)

// reasons is every reason the metric can carry, so each series exists from the start.
var reasons = append(route.Reasons(),
	ReasonNoCredential, ReasonUpstream, ReasonDraining, ReasonHeld,
)

// classNone labels a request that matched no route.
const classNone = "none"

// Metrics are the counters of the egress routes.
type Metrics struct {
	Requests *prometheus.CounterVec
}

// NewMetrics registers the egress metrics on reg (nil registers nothing).
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{Requests: prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "koinon_gateway_egress_requests_total",
		Help: "Forge and registry route requests by route class and result reason.",
	}, []string{"class", "reason"})}
	for _, c := range []string{string(route.ClassForge), string(route.ClassRegistry), classNone} {
		for _, r := range reasons {
			m.Requests.WithLabelValues(c, string(r))
		}
	}
	if reg != nil {
		reg.MustRegister(m.Requests)
	}
	return m
}

func (m *Metrics) count(class route.Class, r route.Reason) {
	if m == nil {
		return
	}
	c := string(class)
	if c == "" {
		c = classNone
	}
	m.Requests.WithLabelValues(c, string(r)).Inc()
}

// Options configure a Gateway.
type Options struct {
	// Identifier finds the actor of each request's actor token: the Ed25519 verifier (OpenShell) or the
	// Substrate identifier (ADR 0120, amendment for issue #1386, item 4). Nil refuses every request.
	Identifier route.Identifier
	// Dial opens the upstream connection. It is required: the binary passes the dialer of the URL
	// policy, so an allowed host that resolves to an internal address is still refused.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// RootCAs verify the upstream. Nil means the system roots.
	RootCAs *x509.CertPool
	// ResponseHeaderTimeout bounds the wait for the upstream's response headers. Zero means 60 s.
	ResponseHeaderTimeout time.Duration
	Metrics               *Metrics
	// Log gets one line per request. The binary passes the scrubbed logger.
	Log *slog.Logger
	// Credentials opens the credentials that routes name. Nil refuses every credentialed route until
	// SetCredentials.
	Credentials CredentialOpener
	// Scrub, when set, gets each credential value and its kind before it is sent, so the scrubbed logger
	// masks it (org is the route's org). The binary passes the Record of the gateway's credential scrub,
	// which reads the rule of the kind from the one list (credkind, #1221).
	Scrub func(kind credkind.Kind, org string, values ...string)
	// Held refuses every request with ReasonHeld, whatever routes are applied (ADR 0107, issue #1390).
	// It is read at start, as --restore-hold: a release is a rollout. It must not change after New.
	Held bool
}

// state is one installed set: the route table and the credentials its routes name. Both change
// together, so a request never pairs a route with the credential of another set.
type state struct {
	table *route.Table
	creds map[string]route.Credential
}

// Gateway serves the egress routes. The zero value is not usable; call New.
type Gateway struct {
	opts     Options
	state    atomic.Pointer[state]
	opener   atomic.Pointer[CredentialOpener]
	draining atomic.Bool
	proxy    *httputil.ReverseProxy
}

// New returns a Gateway with no route table: it refuses every request until Apply.
func New(opts Options) (*Gateway, error) {
	if opts.Dial == nil {
		return nil, errors.New("egress: a dialer is required")
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	if opts.ResponseHeaderTimeout <= 0 {
		opts.ResponseHeaderTimeout = 60 * time.Second
	}
	g := &Gateway{opts: opts}
	if opts.Credentials != nil {
		g.SetCredentials(opts.Credentials)
	}
	t := &http.Transport{
		// Never an environment proxy: the route names the host the gateway connects to.
		Proxy:                 nil,
		DialContext:           opts.Dial,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: opts.RootCAs},
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: opts.ResponseHeaderTimeout,
		ExpectContinueTimeout: time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          256,
		// Go's default of 2 idle connections per host is too small for a forge or registry that many
		// sandboxes share (ADR 0097 step 2 design notes).
		MaxIdleConnsPerHost: 32,
	}
	g.proxy = &httputil.ReverseProxy{
		Rewrite:        g.rewrite,
		Transport:      t,
		ModifyResponse: g.modifyResponse,
		ErrorHandler:   g.upstreamError,
		ErrorLog:       slog.NewLogLogger(opts.Log.Handler(), slog.LevelWarn),
		// Stream as the upstream sends: a git fetch or a package download can be large.
		FlushInterval: -1,
	}
	return g, nil
}

// Apply installs the route table with no credentials. Nil removes it, and then every request is
// refused.
func (g *Gateway) Apply(t *route.Table) { g.state.Store(&state{table: t}) }

// ApplyWithCredentials installs the route table and the credentials its routes name in one step.
func (g *Gateway) ApplyWithCredentials(t *route.Table, creds map[string]route.Credential) {
	g.state.Store(&state{table: t, creds: creds})
}

// SetCredentials sets the opener of the credentials. The binary calls it once the gateway keys load;
// until then every credentialed route is refused.
func (g *Gateway) SetCredentials(o CredentialOpener) {
	if o == nil {
		g.opener.Store(nil)
		return
	}
	g.opener.Store(&o)
}

// Drain makes the gateway refuse new requests. Requests in flight finish.
func (g *Gateway) Drain() { g.draining.Store(true) }

// Handler returns the HTTP handler of the egress routes. It serves PathPrefix only.
func (g *Gateway) Handler() http.Handler { return http.HandlerFunc(g.serve) }

// target is the upstream of an allowed request, kept on the request context for Rewrite.
type target struct {
	authority string // host or host:port as the route matched it, lower case
	path      string // the escaped path the gate checked
	rawQuery  string
	route     route.Route
	decision  route.Decision
	cred      attached // empty for a route without a credential
}

type targetKey struct{}

func (g *Gateway) serve(w http.ResponseWriter, r *http.Request) {
	if g.opts.Held {
		// Before the token check and the route match: a held install sends nothing out.
		g.refuse(w, r, route.Decision{Reason: ReasonHeld}, "")
		return
	}
	if g.draining.Load() {
		g.refuse(w, r, route.Decision{Reason: ReasonDraining}, "")
		return
	}
	authority, path, ok := split(r)
	if ok {
		// The gate checks the escaped path; rewrite sends it decoded too. A path that does not decode is
		// refused here, so rewrite never sends a request without its path.
		_, err := url.PathUnescape(path)
		ok = err == nil
	}
	if !ok {
		g.refuse(w, r, route.Decision{Reason: route.ReasonBadRequest}, "")
		return
	}
	st := g.state.Load()
	if st == nil {
		st = &state{}
	}
	tok, repeated := route.TokenOf(r.Header)
	d := route.NewGate(g.opts.Identifier, st.table).Check(route.Request{
		// The upstream leg is always HTTPS: the gateway dials nothing else.
		Scheme:        "https",
		Token:         tok,
		RepeatedToken: repeated,
		Authority:     authority,
		Method:        r.Method,
		Path:          path,
		RawQuery:      r.URL.RawQuery,
	})
	if !d.Allowed {
		g.refuse(w, r, d, authority)
		return
	}
	var cred attached
	if d.Route.Credential != "" {
		var ok bool
		if cred, ok = g.credential(st, d); !ok {
			d.Allowed, d.Reason = false, ReasonNoCredential
			g.refuse(w, r, d, authority)
			return
		}
	}
	// The upstream is the route's host and port, not the guest's text: the gate's decision and the dial
	// target come from one source. The log line and the count come with the response (modifyResponse)
	// or the failure (upstreamError), so each request counts one result.
	port := d.Route.Port
	if port == 0 {
		port = 443
	}
	t := target{authority: canonicalAuthority(d.Route.Host, port), path: path, rawQuery: r.URL.RawQuery, route: d.Route, decision: d, cred: cred}
	g.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), targetKey{}, t)))
}

// credential reads the credential that the allowed route d names. It fails closed: a credential that
// is not in the set, that belongs to another org or agent, or that cannot be opened refuses the route.
func (g *Gateway) credential(st *state, d route.Decision) (attached, bool) {
	c, ok := st.creds[d.Route.Credential]
	op := g.opener.Load()
	if !ok || op == nil || c.Org != d.Route.Org || c.Agent != d.Route.Agent || d.Route.Agent == "" {
		return attached{}, false
	}
	v, err := (*op).Open(c)
	if err == nil {
		var a attached
		if a, err = attach(c, v); err == nil {
			if g.opts.Scrub != nil {
				g.opts.Scrub(a.kind, c.Org, v, a.header)
			}
			return a, true
		}
	}
	// The error names a file or a fixed reason, never the value.
	reason := "read"
	var oe *OpenError
	if errors.As(err, &oe) {
		reason = oe.Reason
	}
	g.opts.Log.Warn("egress credential unavailable", "route", d.Route.Name, "class", string(d.Route.Class), "kind", reason)
	return attached{}, false
}

// split reads the upstream authority and the escaped upstream path from a request to the gateway. It
// refuses a request that asks for a protocol switch or a tunnel, and an authority with an escape.
func split(r *http.Request) (authority, path string, ok bool) {
	if r.Method == http.MethodConnect || r.Header.Get("Upgrade") != "" {
		return "", "", false
	}
	rest, found := strings.CutPrefix(r.URL.EscapedPath(), PathPrefix)
	if !found {
		return "", "", false
	}
	authority, p, slash := strings.Cut(rest, "/")
	if authority == "" || strings.ContainsAny(authority, "%") {
		return "", "", false
	}
	path = "/"
	if slash {
		path = "/" + p
	}
	return strings.ToLower(authority), path, true
}

// hostPort splits an authority that the gate accepted.
func hostPort(authority string) (string, int) {
	host, ps, err := net.SplitHostPort(authority)
	if err != nil {
		return strings.TrimSuffix(authority, "."), 443
	}
	p, _ := strconv.Atoi(ps)
	return strings.TrimSuffix(host, "."), p
}

func canonicalAuthority(host string, port int) string {
	if port == 443 {
		return host
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// guestOnly are request headers of the guest that never go upstream. httputil.ReverseProxy removes
// the hop-by-hop headers and, with Rewrite, the Forwarded and X-Forwarded-* headers itself.
var guestOnly = []string{route.TokenHeader, "Authorization", "Cookie", "Proxy-Authorization", "Proxy-Connection", "X-Real-Ip"}

func (g *Gateway) rewrite(pr *httputil.ProxyRequest) {
	t, _ := pr.In.Context().Value(targetKey{}).(target)
	out := pr.Out
	out.URL = &url.URL{Scheme: "https", Host: t.authority, RawQuery: t.rawQuery}
	// The path goes as the gate checked it: decoded into Path, escaped form in RawPath.
	if p, err := url.PathUnescape(t.path); err == nil {
		out.URL.Path = p
		out.URL.RawPath = t.path
	}
	out.Host = t.authority
	for _, h := range guestOnly {
		out.Header.Del(h)
	}
	if t.cred.header != "" {
		out.Header.Set("Authorization", t.cred.header)
		// No Accept-Encoding of the guest: the transport then asks for gzip itself and decodes it, so
		// the gateway sees, and scrubs, the plain body.
		out.Header.Del("Accept-Encoding")
	}
	// The guest's User-Agent stays (registries use it); nothing names the gateway or the actor.
}

// modifyResponse logs and counts the allowed request, removes a refusal header the upstream sent (only
// the gateway says why it refused), and rewrites a redirect so the guest follows it through the gateway
// again. The gateway never follows it itself: the gate decides on the next request.
func (g *Gateway) modifyResponse(resp *http.Response) error {
	t, _ := resp.Request.Context().Value(targetKey{}).(target)
	if t.cred.header != "" {
		if err := g.scrubResponse(resp, t); err != nil {
			return err
		}
	}
	g.log(resp.Request, t.decision, t.route.Host, resp.StatusCode)
	g.opts.Metrics.count(t.route.Class, route.ReasonAllowed)
	resp.Header.Del(RefusalHeader)
	loc := resp.Header.Get("Location")
	if loc == "" || resp.StatusCode < 300 || resp.StatusCode > 399 {
		return nil
	}
	u, err := url.Parse(loc)
	if err != nil {
		resp.Header.Del("Location")
		return nil
	}
	switch {
	case u.Scheme == "https" && u.Host != "" && u.User == nil:
		resp.Header.Set("Location", gatewayForm(strings.ToLower(u.Host), u))
	case u.Scheme == "" && u.Host == "" && strings.HasPrefix(u.Path, "/"):
		resp.Header.Set("Location", gatewayForm(t.authority, u))
	case u.Scheme == "" && u.Host == "":
		// A relative reference resolves against the gateway form of the request, which is right.
	default:
		// A redirect to http, to userinfo or to another scheme is not followed through the gateway.
		resp.Header.Del("Location")
	}
	return nil
}

// errEncoded refuses a response of a credentialed route that the gateway cannot scan.
var errEncoded = errors.New("egress: a credentialed route answered with an encoded body")

// scrubResponse masks the credential in the response of a credentialed route. A body with a
// Content-Encoding the transport did not decode cannot be scanned, so it is refused: the gateway asked
// for none it cannot read.
func (g *Gateway) scrubResponse(resp *http.Response, t target) error {
	if ce := resp.Header.Get("Content-Encoding"); ce != "" && !strings.EqualFold(ce, "identity") && hasBody(resp) {
		_ = resp.Body.Close()
		return errEncoded
	}
	hit := maskHeader(resp.Header, t.cred.needles)
	if maskHeader(resp.Trailer, t.cred.needles) {
		hit = true
	}
	onHit := func() {
		g.opts.Log.Warn("egress response held the credential; masked", "route", t.route.Name, "class", string(t.route.Class))
	}
	if hit {
		onHit()
	}
	resp.Body = newScrubBody(resp.Body, t.cred.needles, func() http.Header { return resp.Trailer }, onHit)
	return nil
}

// hasBody reports whether a response can carry a body. A HEAD answer, 1xx, 204 and 304 never do, so
// their Content-Encoding header describes no bytes the gateway must scan.
func hasBody(resp *http.Response) bool {
	if resp.Request != nil && resp.Request.Method == http.MethodHead {
		return false
	}
	c := resp.StatusCode
	return c >= 200 && c != http.StatusNoContent && c != http.StatusNotModified
}

func gatewayForm(authority string, u *url.URL) string {
	host, port := hostPort(authority)
	s := PathPrefix + canonicalAuthority(host, port) + u.EscapedPath()
	if u.RawQuery != "" {
		s += "?" + u.RawQuery
	}
	return s
}

func (g *Gateway) upstreamError(w http.ResponseWriter, r *http.Request, err error) {
	t, _ := r.Context().Value(targetKey{}).(target)
	g.opts.Metrics.count(t.route.Class, ReasonUpstream)
	// The request line names the actor for a failed upstream too, so the audit shows who tried.
	d := t.decision
	d.Allowed, d.Reason = false, ReasonUpstream
	g.log(r, d, t.route.Host, http.StatusBadGateway)
	reason := "upstream"
	if errors.Is(err, context.Canceled) {
		reason = "canceled"
	}
	if errors.Is(err, errEncoded) {
		reason = "encoded"
	}
	// The error can name the upstream address. The credential goes in a header, which no transport
	// error quotes; the text is masked all the same.
	msg := []byte(err.Error())
	mask(msg, t.cred.needles)
	g.opts.Log.Warn("egress upstream failed", "route", t.route.Name, "class", string(t.route.Class), "kind", reason, "err", string(msg))
	w.Header().Set(RefusalHeader, string(ReasonUpstream))
	http.Error(w, "koinon-gateway: the upstream could not be reached", http.StatusBadGateway)
}

func (g *Gateway) refuse(w http.ResponseWriter, r *http.Request, d route.Decision, authority string) {
	status := http.StatusForbidden
	switch d.Reason {
	case route.ReasonNoToken, route.ReasonBadToken:
		status = http.StatusUnauthorized
	case route.ReasonBadRequest:
		status = http.StatusBadRequest
	case route.ReasonNoTable, route.ReasonIdentityUnavailable, ReasonNoCredential, ReasonDraining, ReasonHeld:
		status = http.StatusServiceUnavailable
	}
	g.opts.Metrics.count(d.Route.Class, d.Reason)
	// The host goes to the log only for a verified actor, so a caller without a token cannot write text
	// into the gateway's log.
	host := ""
	if d.Actor.Org != "" {
		host, _ = hostPort(authority)
	}
	g.log(r, d, host, status)
	// The guest never learns that another org has a route to the host: org_mismatch is no_route on the
	// wire. The log and the metric keep the true reason.
	wire := d.Reason
	if wire == route.ReasonOrgMismatch {
		wire = route.ReasonNoRoute
	}
	w.Header().Set(RefusalHeader, string(wire))
	msg := "koinon-gateway: egress refused: " + string(wire)
	if wire == ReasonHeld {
		msg += ": " + restorehold.Message
	}
	http.Error(w, msg, status)
}

// log writes one line per request: who, which route, the host and the result. Never the token, the
// path or the query: they can hold data the agent sends out.
func (g *Gateway) log(r *http.Request, d route.Decision, host string, status int) {
	g.opts.Log.Info("egress request",
		"result", string(d.Reason), "status", status, "method", r.Method, "host", host,
		"route", d.Route.Name, "class", string(d.Route.Class), "token_reason", d.TokenReason,
		"org", d.Actor.Org, "agent", d.Actor.Agent, "run", d.Actor.Run)
}
