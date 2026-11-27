// SPDX-License-Identifier: Apache-2.0

// Package route is the route model of the forge and registry L7 routes of Koinon's gateway (ADR 0120
// section 2, ADR 0119 D1 and D2, ADR 0080 and ADR 0113 as amended).
//
// A route says which host, method, path and query a credential class may reach. Egress hosts and
// registry rules are routes, so there is one list for both execution drivers. The gateway is the only
// L7 enforcement point:
//
//   - Default deny. A request that matches no route is refused.
//   - No audit-only mode. There is no switch that logs a violation and lets it pass.
//   - A rule that fails to load closes the route. [Compile] refuses the whole set when one route is
//     wrong, and a [Gate] without a table refuses every request.
//   - No wildcard hosts, and no route without a method and a path rule.
//   - One credential serves one route class (ADR 0120 section 1.5). A route names its credential,
//     never its value; the gateway looks the value up in the secret store after the decision.
//
// A [Gate] checks the actor token (package actortoken) first and the route second, so a request with
// no valid token never reaches a credential.
package route

import (
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/stateracloud/koinon/internal/gateway/actortoken"
)

// Class is the kind of a route. One credential serves one class.
type Class string

// Route classes of this slice. Model and MCP routes keep their own paths (ADR 0097 step 2).
const (
	ClassForge    Class = "forge"
	ClassRegistry Class = "registry"
)

// Reason is why a request was allowed or refused. The set is fixed, so it is safe as a metric label
// (ADR 0023).
type Reason string

// Reasons.
const (
	ReasonAllowed    Reason = "allowed"
	ReasonNoToken    Reason = "no_token"
	ReasonBadToken   Reason = "bad_token"
	ReasonNoTable    Reason = "no_table"
	ReasonNoRoute    Reason = "no_route"
	ReasonMethod     Reason = "method_not_allowed"
	ReasonPath       Reason = "path_not_allowed"
	ReasonQuery      Reason = "query_not_allowed"
	ReasonBadRequest Reason = "bad_request"
	// ReasonOrgMismatch: a route of the host belongs to another org, or to another agent of the org.
	ReasonOrgMismatch Reason = "org_mismatch"
	// ReasonDenied: a deny route matches the request.
	ReasonDenied Reason = "denied"
	// ReasonUnknownActor: the Substrate token verified, but Koinon's record has no such actor (an
	// unknown actor, another UID, or a deleted or archived sandbox; issue #1386).
	ReasonUnknownActor Reason = "unknown_actor"
	// ReasonIdentityUnavailable: the gateway cannot check the token now (the Substrate issuer keys cannot
	// be read). The request is refused, never allowed.
	ReasonIdentityUnavailable Reason = "identity_unavailable"
)

// Reasons are all reasons, in a fixed order, for metrics that create every series at start.
func Reasons() []Reason {
	return []Reason{
		ReasonAllowed, ReasonNoToken, ReasonBadToken, ReasonNoTable, ReasonNoRoute, ReasonMethod, ReasonPath,
		ReasonQuery, ReasonBadRequest, ReasonOrgMismatch, ReasonDenied, ReasonUnknownActor, ReasonIdentityUnavailable,
	}
}

// Route is one allowed destination.
type Route struct {
	// Name identifies the route in audit rows. It is unique in a set.
	Name  string
	Class Class
	// Org is the org that owns the grant. Empty means a platform default every org may use.
	Org string
	// Agent narrows an org route to one agent: the agent of the actor token, which is the agent's
	// name at the gateway (gateway.AgentRef.Name). Empty means every agent of Org. A route with an
	// agent needs an org.
	Agent string
	// Host is one exact lower-case host name. No wildcard, no IP literal.
	Host string
	// Port is the TCP port. Zero means 443.
	Port int
	// Methods are the allowed HTTP methods, upper case.
	Methods []string
	// PathPrefixes are the allowed paths. A prefix matches a whole path or a path under it at a
	// segment boundary: "/v2" matches "/v2" and "/v2/x" and never "/v2x". Each starts with "/".
	PathPrefixes []string
	// Paths are path patterns, for rules that a prefix cannot say (a connector's network rules). A
	// pattern is "/"-separated segments: the segment "*" matches one segment, the segment "**" matches
	// any number of segments (zero included), and every other segment matches exactly the decoded
	// segment of the request. No other wildcard exists. A route needs PathPrefixes or Paths.
	Paths []string
	// QueryKeys are the query parameters the route allows. A key not listed refuses the request, and
	// a route with no keys allows no query at all.
	QueryKeys []string
	// QueryValues are required query parameters: each one must be present, and each of its values must
	// equal the given value exactly. A key listed here is also allowed. In a deny route the parameter
	// must be present and one value equal is enough (compared without case).
	QueryValues map[string]string
	// AnyQuery allows every query parameter. QueryValues still apply.
	AnyQuery bool
	// Deny makes the route a refusal: a request that it matches is refused, also when another route
	// allows it. A deny route of an org or an agent applies only to that org or agent. Methods, paths
	// and query values of a deny route are compared without case, so a deny never misses on a spelling.
	// A deny route has no credential, no QueryKeys and no AnyQuery.
	Deny bool
	// Credential names the secret the gateway attaches on this route. Empty means none. It is a name,
	// never a value.
	Credential string
}

func (r Route) port() int {
	if r.Port == 0 {
		return 443
	}
	return r.Port
}

func (r Route) validate() error {
	if r.Name == "" {
		return errors.New("a route needs a name")
	}
	if r.Class != ClassForge && r.Class != ClassRegistry {
		return fmt.Errorf("route %q: unknown class %q", r.Name, r.Class)
	}
	if err := validHost(r.Host); err != nil {
		return fmt.Errorf("route %q: %w", r.Name, err)
	}
	if r.Port < 0 || r.Port > 65535 {
		return fmt.Errorf("route %q: port out of range", r.Name)
	}
	if len(r.Methods) == 0 {
		return fmt.Errorf("route %q: no method rule", r.Name)
	}
	for _, m := range r.Methods {
		if m == "" || m != strings.ToUpper(m) || strings.ContainsAny(m, " *") {
			return fmt.Errorf("route %q: method %q must be one upper-case HTTP method", r.Name, m)
		}
	}
	if len(r.PathPrefixes) == 0 && len(r.Paths) == 0 {
		return fmt.Errorf("route %q: no path rule", r.Name)
	}
	for _, p := range r.PathPrefixes {
		if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "*?#%;[]") || !canonical(p) {
			return fmt.Errorf("route %q: path prefix %q must be an absolute, canonical path with no wildcard", r.Name, p)
		}
	}
	for _, p := range r.Paths {
		if err := validPattern(p); err != nil {
			return fmt.Errorf("route %q: path %q: %w", r.Name, p, err)
		}
	}
	for _, k := range r.QueryKeys {
		if !validQueryKey(k) {
			return fmt.Errorf("route %q: bad query key %q", r.Name, k)
		}
	}
	for k, v := range r.QueryValues {
		if !validQueryKey(k) {
			return fmt.Errorf("route %q: bad query key %q", r.Name, k)
		}
		if v == "" || strings.ContainsAny(v, "*?[]&=") {
			return fmt.Errorf("route %q: query %q needs one exact value, not %q", r.Name, k, v)
		}
	}
	return r.validateScope()
}

// validateScope checks the agent scope and the rules of a deny route.
func (r Route) validateScope() error {
	if r.Agent != "" && r.Org == "" {
		return fmt.Errorf("route %q: a route with an agent needs an org", r.Name)
	}
	if r.Deny && (r.Credential != "" || len(r.QueryKeys) > 0 || r.AnyQuery) {
		return fmt.Errorf("route %q: a deny route has no credential, no query keys and no any-query", r.Name)
	}
	// A deny route matches decoded segments only: a prefix compares the escaped path, so an encoded
	// letter ("/%61dmin") would pass it.
	if r.Deny && r.PathPrefixes != nil {
		return fmt.Errorf("route %q: a deny route takes path patterns (paths), not path prefixes", r.Name)
	}
	return nil
}

func validQueryKey(k string) bool { return k != "" && !strings.ContainsAny(k, "*?[]&=") }

// validPattern checks a path pattern of [Route.Paths].
func validPattern(p string) error {
	if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "?#%;[]\\") || !canonical(p) {
		return errors.New("must be an absolute, canonical path")
	}
	for _, seg := range strings.Split(p[1:], "/") {
		if strings.Contains(seg, "*") && seg != "*" && seg != "**" {
			return errors.New(`a wildcard is a whole segment, "*" or "**"`)
		}
	}
	return nil
}

func validHost(h string) error {
	switch {
	case h == "":
		return errors.New("no host")
	case strings.HasSuffix(h, "."):
		return fmt.Errorf("host %q must not end with a dot", h)
	case h != strings.ToLower(h):
		return fmt.Errorf("host %q must be lower case", h)
	case strings.ContainsAny(h, "*?[]/:@ "):
		return fmt.Errorf("host %q must be one exact name (no wildcard, port, path or userinfo)", h)
	case net.ParseIP(h) != nil:
		return fmt.Errorf("host %q is an IP literal", h)
	case !strings.Contains(h, "."):
		return fmt.Errorf("host %q is not a fully qualified name", h)
	}
	return nil
}

// canonical reports whether p has no empty, "." or ".." segment (a trailing slash is fine).
func canonical(p string) bool {
	segs := strings.Split(strings.TrimPrefix(p, "/"), "/")
	for i, s := range segs {
		if s == "." || s == ".." || (s == "" && i != len(segs)-1) {
			return false
		}
	}
	return !strings.ContainsRune(p, 0)
}

// Table is a validated, immutable set of routes.
type Table struct {
	routes []Route
}

// Compile validates routes and returns the table. It refuses the whole set when one route is wrong,
// so a rule that fails to load closes every route in the set, never only that one (ADR 0120 2.2). It
// also refuses a credential that two route classes share, and two routes with one name.
func Compile(routes []Route) (*Table, error) {
	seen := map[string]bool{}
	type owner struct {
		org   string
		class Class
	}
	credOwner := map[string]owner{}
	for _, r := range routes {
		if err := r.validate(); err != nil {
			return nil, err
		}
		if seen[r.Name] {
			return nil, fmt.Errorf("route name %q is used twice", r.Name)
		}
		seen[r.Name] = true
		if r.Deny {
			continue
		}
		if r.Credential != "" && r.Org == "" {
			return nil, fmt.Errorf("route %q: a platform route (no org) cannot carry a credential: one secret would serve every org", r.Name)
		}
		if r.Credential != "" {
			if o, ok := credOwner[r.Credential]; ok && o.class != r.Class {
				return nil, fmt.Errorf("credential %q serves classes %q and %q: one credential serves one route class", r.Credential, o.class, r.Class)
			} else if ok && o.org != r.Org {
				return nil, fmt.Errorf("credential %q is used by two orgs: a credential belongs to one org", r.Credential)
			}
			credOwner[r.Credential] = owner{org: r.Org, class: r.Class}
		}
	}
	cp := make([]Route, len(routes))
	for i, r := range routes {
		cp[i] = r.clone()
	}
	return &Table{routes: cp}, nil
}

// Routes returns a copy of the routes of the table.
func (t *Table) Routes() []Route {
	if t == nil {
		return nil
	}
	cp := make([]Route, len(t.routes))
	for i, r := range t.routes {
		cp[i] = r.clone()
	}
	return cp
}

// Request is what the gate decides on. It does not depend on net/http, so both drivers and the tests
// build it the same way.
type Request struct {
	// Scheme is the scheme of the request. Only "https" is served: a credential never goes over
	// plain HTTP.
	Scheme string
	// Token is the actor identity token as the request carried it: the first value of [TokenHeader].
	Token string
	// RepeatedToken is set when the request carried more than one value of [TokenHeader]. The gate
	// refuses it (ADR 0120, amendment for issue #1386, item 4): no value is chosen.
	RepeatedToken bool
	// Authority is the Host of the request: host or host:port.
	Authority string
	Method    string
	// Path is the escaped path as it arrived (u.EscapedPath), and RawQuery the query string.
	Path     string
	RawQuery string
}

// FromHTTP builds a Request from an incoming proxy request. The token header is [TokenHeader].
func FromHTTP(r *http.Request) Request {
	host := r.Host
	if host == "" {
		host = r.URL.Host
	}
	// The scheme comes from the connection, never from the absolute-form URL, which a client
	// can write as https over a plain connection. A proxy that terminates TLS for a CONNECT
	// tunnel sets Request.Scheme itself.
	scheme := ""
	if r.TLS != nil {
		scheme = "https"
	}
	tok, repeated := TokenOf(r.Header)
	return Request{
		Scheme:        scheme,
		Token:         tok,
		RepeatedToken: repeated,
		Authority:     host,
		Method:        r.Method,
		Path:          r.URL.EscapedPath(),
		RawQuery:      r.URL.RawQuery,
	}
}

// TokenOf returns the first value of [TokenHeader] in h, and whether h holds more than one value.
func TokenOf(h http.Header) (token string, repeated bool) {
	v := h.Values(TokenHeader)
	if len(v) == 0 {
		return "", false
	}
	return v[0], len(v) > 1
}

// TokenHeader carries the actor token from the guest to the gateway. The gateway removes it before
// it forwards the request.
const TokenHeader = "Koinon-Actor-Token" //nolint:gosec // a header name, not a credential

// Decision is the result of a check.
type Decision struct {
	Allowed bool
	Reason  Reason
	// Route is the matching route; it is set only when Allowed.
	Route Route
	// Actor is the verified actor; it is set whenever the token verified.
	Actor actortoken.Actor
	// TokenReason says why the token was refused (actortoken.Reason): a fixed, low-cardinality text for
	// the log, never token data. It is empty when the token was not refused.
	TokenReason string
}

// Gate checks the token and the route of each request.
type Gate struct {
	ident Identifier
	table *Table
}

// NewGate returns a gate. A nil identifier or a nil table makes the gate refuse every request: the
// gateway never serves a route it could not load.
func NewGate(id Identifier, t *Table) *Gate { return &Gate{ident: id, table: t} }

// Check decides on r. It never panics and never allows on an error.
func (g *Gate) Check(r Request) Decision {
	if g == nil || g.table == nil || g.ident == nil {
		return Decision{Reason: ReasonNoTable}
	}
	if r.RepeatedToken {
		return Decision{Reason: ReasonBadToken, TokenReason: actortoken.Reason(actortoken.ErrMultipleTokens)}
	}
	if strings.TrimSpace(r.Token) == "" {
		return Decision{Reason: ReasonNoToken}
	}
	// The guest's placeholder is never a token, for both identifiers: it means the egress layer did not
	// replace it.
	if strings.TrimSpace(r.Token) == actortoken.Placeholder {
		return Decision{Reason: ReasonBadToken, TokenReason: actortoken.Reason(actortoken.ErrPlaceholder)}
	}
	actor, err := g.ident.Identify(r.Token)
	if err != nil {
		reason := ReasonBadToken
		switch {
		case errors.Is(err, actortoken.ErrUnavailable):
			reason = ReasonIdentityUnavailable
		case errors.Is(err, actortoken.ErrUnknownActor):
			reason = ReasonUnknownActor
		}
		return Decision{Reason: reason, TokenReason: actortoken.Reason(err)}
	}
	if actor.Validate() != nil {
		return Decision{Reason: ReasonBadToken, TokenReason: actortoken.Reason(actortoken.ErrInvalid)}
	}
	d := g.table.decide(actor, r)
	d.Actor = actor
	return d
}

func (t *Table) decide(actor actortoken.Actor, r Request) Decision {
	if r.Scheme != "https" {
		return Decision{Reason: ReasonBadRequest}
	}
	host, port, ok := splitAuthority(r.Authority)
	if !ok {
		return Decision{Reason: ReasonBadRequest}
	}
	p, ok := cleanPath(r.Path)
	if !ok {
		return Decision{Reason: ReasonBadRequest}
	}
	q, err := url.ParseQuery(r.RawQuery)
	if err != nil {
		return Decision{Reason: ReasonBadRequest}
	}
	segs, ok := decodedSegments(p)
	if !ok {
		return Decision{Reason: ReasonBadRequest}
	}
	// A deny route of the actor's scope wins over every allow.
	for i := range t.routes {
		rt := &t.routes[i]
		if rt.Deny && rt.Host == host && rt.port() == port && rt.inScope(actor) && rt.denies(r.Method, p, segs, q) {
			return Decision{Reason: ReasonDenied}
		}
	}
	// reason keeps the most specific refusal among routes of the host, so a deny says why.
	reason := ReasonNoRoute
	rank := map[Reason]int{ReasonNoRoute: 0, ReasonOrgMismatch: 1, ReasonMethod: 2, ReasonPath: 3, ReasonQuery: 4}
	// Among the routes that allow the request, an agent's route wins over an org's route, an org's
	// route over a platform route, then the longest literal path, then the route name. The order of
	// the set never decides.
	var best *Route
	bestLen := -1
	for i := range t.routes {
		rt := &t.routes[i]
		if rt.Deny || rt.Host != host || rt.port() != port {
			continue
		}
		var miss Reason
		switch {
		case !rt.inScope(actor):
			miss = ReasonOrgMismatch
		case !slices.Contains(rt.Methods, r.Method):
			miss = ReasonMethod
		case !rt.pathMatches(p, segs, false):
			miss = ReasonPath
		case !rt.queryAllowed(q):
			miss = ReasonQuery
		default:
			l := rt.matchLen(p, segs)
			if best == nil || beats(rt, l, best, bestLen) {
				best, bestLen = rt, l
			}
			continue
		}
		if rank[miss] > rank[reason] {
			reason = miss
		}
	}
	if best != nil {
		return Decision{Allowed: true, Reason: ReasonAllowed, Route: best.clone()}
	}
	return Decision{Reason: reason}
}

// inScope reports whether the route serves actor: a platform route serves every actor, an org route
// the agents of its org, and an agent route that agent only.
func (r *Route) inScope(actor actortoken.Actor) bool {
	return (r.Org == "" || r.Org == actor.Org) && (r.Agent == "" || r.Agent == actor.Agent)
}

// scopeRank orders the scopes for the choice among allowing routes.
func (r *Route) scopeRank() int {
	switch {
	case r.Agent != "":
		return 2
	case r.Org != "":
		return 1
	}
	return 0
}

// denies reports whether the deny route r matches the request. It compares without case.
func (r *Route) denies(method, p string, segs []string, q url.Values) bool {
	if !slices.ContainsFunc(r.Methods, func(m string) bool { return strings.EqualFold(m, method) }) {
		return false
	}
	// A trailing slash adds an empty segment; the forge treats "/x/" as "/x", so a deny pattern
	// ignores it.
	if n := len(segs); n > 0 && segs[n-1] == "" {
		segs = segs[:n-1]
	}
	if !r.pathMatches(p, segs, true) {
		return false
	}
	for k, want := range r.QueryValues {
		if !slices.ContainsFunc(q[k], func(v string) bool { return strings.EqualFold(v, want) }) {
			return false
		}
	}
	return true
}

// pathMatches reports whether a prefix or a pattern of r matches the path p (escaped) or its decoded
// segments. fold compares without case, for deny routes, which have no prefixes.
func (r *Route) pathMatches(p string, segs []string, fold bool) bool {
	if !fold && pathAllowed(r.PathPrefixes, p) {
		return true
	}
	for _, pat := range r.Paths {
		if patternMatches(strings.Split(pat[1:], "/"), segs, fold) {
			return true
		}
	}
	return false
}

// patternMatches matches pattern segments against path segments: "*" is one segment, "**" any number
// of segments.
func patternMatches(pat, segs []string, fold bool) bool {
	if len(pat) == 0 {
		return len(segs) == 0
	}
	switch pat[0] {
	case "**":
		for i := 0; i <= len(segs); i++ {
			if patternMatches(pat[1:], segs[i:], fold) {
				return true
			}
		}
		return false
	case "*":
		return len(segs) > 0 && segs[0] != "" && patternMatches(pat[1:], segs[1:], fold)
	}
	if len(segs) == 0 {
		return false
	}
	if fold && !strings.EqualFold(pat[0], segs[0]) || !fold && pat[0] != segs[0] {
		return false
	}
	return patternMatches(pat[1:], segs[1:], fold)
}

// decodedSegments splits a canonical escaped path into its decoded segments. cleanPath has already
// refused an encoded slash, backslash, dot or NUL, so decoding cannot change the segment structure.
func decodedSegments(p string) ([]string, bool) {
	raw := strings.Split(p[1:], "/")
	out := make([]string, len(raw))
	for i, s := range raw {
		d, err := url.PathUnescape(s)
		if err != nil {
			return nil, false
		}
		out[i] = d
	}
	return out, true
}

// queryAllowed applies QueryValues, then AnyQuery or QueryKeys.
func (r *Route) queryAllowed(q url.Values) bool {
	for k, want := range r.QueryValues {
		vs := q[k]
		if len(vs) == 0 {
			return false
		}
		for _, v := range vs {
			if v != want {
				return false
			}
		}
	}
	if r.AnyQuery {
		return true
	}
	for k := range q {
		if _, ok := r.QueryValues[k]; !ok && !slices.Contains(r.QueryKeys, k) {
			return false
		}
	}
	return true
}

// matchLen is how specific the match of r on the path is: the longest matching prefix, or the
// literal length of the longest matching pattern up to its first wildcard.
func (r *Route) matchLen(p string, segs []string) int {
	n := 0
	for _, pre := range r.PathPrefixes {
		if pathAllowed([]string{pre}, p) && len(pre) > n {
			n = len(pre)
		}
	}
	for _, pat := range r.Paths {
		parts := strings.Split(pat[1:], "/")
		if !patternMatches(parts, segs, false) {
			continue
		}
		l := 0
		for _, part := range parts {
			if part == "*" || part == "**" {
				break
			}
			l += 1 + len(part)
		}
		n = max(n, l)
	}
	return n
}

// beats reports whether route a (matching with length al) is preferred over b.
func beats(a *Route, al int, b *Route, bl int) bool {
	if a.scopeRank() != b.scopeRank() {
		return a.scopeRank() > b.scopeRank()
	}
	if al != bl {
		return al > bl
	}
	return a.Name < b.Name
}

func (r Route) clone() Route {
	r.Methods = slices.Clone(r.Methods)
	r.PathPrefixes = slices.Clone(r.PathPrefixes)
	r.QueryKeys = slices.Clone(r.QueryKeys)
	r.Paths = slices.Clone(r.Paths)
	r.QueryValues = maps.Clone(r.QueryValues)
	return r
}

func splitAuthority(a string) (string, int, bool) {
	for i := 0; i < len(a); i++ {
		c := a[i]
		if c >= 0x80 || c <= 0x20 || strings.IndexByte("/@\\", c) >= 0 {
			return "", 0, false
		}
	}
	a = strings.ToLower(a)
	if a == "" {
		return "", 0, false
	}
	host, ps, err := net.SplitHostPort(a)
	if err != nil {
		if strings.Contains(a, ":") {
			return "", 0, false
		}
		return strings.TrimSuffix(a, "."), 443, true
	}
	if len(ps) == 0 || len(ps) > 5 || strings.Trim(ps, "0123456789") != "" {
		return "", 0, false
	}
	port, err := strconv.Atoi(ps)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, false
	}
	return strings.TrimSuffix(host, "."), port, true
}

// cleanPath refuses a path that is not canonical: dot segments, empty segments, an encoded slash or
// backslash, or a NUL. A request with such a path is refused, never rewritten, so the path the gate
// checked is the path the forge sees.
func cleanPath(raw string) (string, bool) {
	if raw == "" || !strings.HasPrefix(raw, "/") {
		return "", false
	}
	low := strings.ToLower(raw)
	if strings.Contains(low, "%2f") || strings.Contains(low, "%5c") || strings.Contains(low, "%00") ||
		strings.Contains(low, "%2e") || strings.ContainsAny(raw, "\\\x00;") {
		return "", false
	}
	if !canonical(raw) {
		return "", false
	}
	return raw, true
}

func pathAllowed(prefixes []string, p string) bool {
	for _, pre := range prefixes {
		pre = strings.TrimSuffix(pre, "/")
		if p == pre || p == pre+"/" || strings.HasPrefix(p, pre+"/") {
			return true
		}
	}
	return false
}
