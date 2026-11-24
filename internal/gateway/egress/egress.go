// SPDX-License-Identifier: Apache-2.0

// Package egress serves the forge and registry L7 routes of Koinon's gateway (ADR 0097 step 2 as
// amended by ADR 0119 D1 and D11, ADR 0120 section 2). It is #1086 part A: the gateway binary serves
// the route table of package route. The route set comes from the State stream in part B, and the
// credential of a route from the secret store in part C.
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
//   - Default deny. No table, no token, a bad token or no route refuses the request. There is no
//     audit-only mode.
//   - The upstream leg is HTTPS only, to the exact host and port of the route, through the dialer the
//     caller gives (the URL policy of package catalog in the binary: no loopback, link-local,
//     metadata or private address unless the platform allowed the host).
//   - The actor token, Authorization, Cookie, Proxy-* and Forwarded headers of the guest never reach
//     the upstream. The gateway alone attaches a credential to a route (part C). Until part C, a route
//     that names a credential is refused.
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

	"github.com/stateracloud/koinon/internal/gateway/actortoken"
	"github.com/stateracloud/koinon/internal/gateway/route"
)

// PathPrefix is where the egress routes are served. The segment after it is the upstream authority.
const PathPrefix = "/egress/"

// RefusalHeader names the reason of a refusal on the response, so an agent can say why a fetch
// failed. The value is a fixed reason, never request data.
const RefusalHeader = "Koinon-Egress-Refusal"

// Reasons that the handler adds to the ones of package route. The set is fixed (ADR 0023).
const (
	// ReasonNoCredential: the route names a credential and the gateway cannot attach it yet.
	ReasonNoCredential route.Reason = "credential_unavailable" //nolint:gosec // a refusal reason, not a credential
	// ReasonUpstream: the upstream could not be reached or failed before it answered.
	ReasonUpstream route.Reason = "upstream_error"
	// ReasonDraining: the gateway stops and takes no new request.
	ReasonDraining route.Reason = "draining"
)

// reasons is every reason the metric can carry, so each series exists from the start.
var reasons = []route.Reason{
	route.ReasonAllowed, route.ReasonNoToken, route.ReasonBadToken, route.ReasonNoTable, route.ReasonNoRoute,
	route.ReasonMethod, route.ReasonPath, route.ReasonQuery, route.ReasonBadRequest, route.ReasonOrgMismatch,
	ReasonNoCredential, ReasonUpstream, ReasonDraining,
}

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
	// Verifier checks actor tokens. Nil refuses every request.
	Verifier *actortoken.Verifier
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
}

// Gateway serves the egress routes. The zero value is not usable; call New.
type Gateway struct {
	opts     Options
	table    atomic.Pointer[route.Table]
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
		ModifyResponse: modifyResponse,
		ErrorHandler:   g.upstreamError,
		ErrorLog:       slog.NewLogLogger(opts.Log.Handler(), slog.LevelWarn),
		// Stream as the upstream sends: a git fetch or a package download can be large.
		FlushInterval: -1,
	}
	return g, nil
}

// Apply installs the route table. Nil removes it, and then every request is refused.
func (g *Gateway) Apply(t *route.Table) { g.table.Store(t) }

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
}

type targetKey struct{}

func (g *Gateway) serve(w http.ResponseWriter, r *http.Request) {
	if g.draining.Load() {
		g.refuse(w, r, route.Decision{Reason: ReasonDraining}, "")
		return
	}
	authority, path, ok := split(r)
	if !ok {
		g.refuse(w, r, route.Decision{Reason: route.ReasonBadRequest}, "")
		return
	}
	d := route.NewGate(g.opts.Verifier, g.table.Load()).Check(route.Request{
		// The upstream leg is always HTTPS: the gateway dials nothing else.
		Scheme:    "https",
		Token:     r.Header.Get(route.TokenHeader),
		Authority: authority,
		Method:    r.Method,
		Path:      path,
		RawQuery:  r.URL.RawQuery,
	})
	if !d.Allowed {
		g.refuse(w, r, d, authority)
		return
	}
	if d.Route.Credential != "" {
		// Part C attaches the credential from the secret store. Until then the route is closed, never
		// served without its credential and never served with one from another source.
		d.Allowed, d.Reason = false, ReasonNoCredential
		g.refuse(w, r, d, authority)
		return
	}
	host, port := hostPort(authority)
	t := target{authority: canonicalAuthority(host, port), path: path, rawQuery: r.URL.RawQuery, route: d.Route}
	g.log(r, d, host, http.StatusOK)
	g.opts.Metrics.count(d.Route.Class, route.ReasonAllowed)
	g.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), targetKey{}, t)))
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
	// The guest's User-Agent stays (registries use it); nothing names the gateway or the actor.
}

// modifyResponse rewrites a redirect so the guest follows it through the gateway again. The gateway
// never follows it itself: the gate decides on the next request.
func modifyResponse(resp *http.Response) error {
	loc := resp.Header.Get("Location")
	if loc == "" || resp.StatusCode < 300 || resp.StatusCode > 399 {
		return nil
	}
	t, _ := resp.Request.Context().Value(targetKey{}).(target)
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
	reason := "upstream"
	if errors.Is(err, context.Canceled) {
		reason = "canceled"
	}
	// The error can name the upstream address; it holds no token and no credential (none is attached).
	g.opts.Log.Warn("egress upstream failed", "route", t.route.Name, "class", string(t.route.Class), "kind", reason, "err", err)
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
	case route.ReasonNoTable, ReasonNoCredential, ReasonDraining:
		status = http.StatusServiceUnavailable
	}
	g.opts.Metrics.count(d.Route.Class, d.Reason)
	host, _ := hostPort(authority)
	g.log(r, d, host, status)
	w.Header().Set(RefusalHeader, string(d.Reason))
	http.Error(w, "koinon-gateway: egress refused: "+string(d.Reason), status)
}

// log writes one line per request: who, which route, the host and the result. Never the token, the
// path or the query: they can hold data the agent sends out.
func (g *Gateway) log(r *http.Request, d route.Decision, host string, status int) {
	g.opts.Log.Info("egress request",
		"result", string(d.Reason), "status", status, "method", r.Method, "host", host,
		"route", d.Route.Name, "class", string(d.Route.Class),
		"org", d.Actor.Org, "agent", d.Actor.Agent, "run", d.Actor.Run)
}
