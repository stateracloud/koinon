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
	ReasonAllowed     Reason = "allowed"
	ReasonNoToken     Reason = "no_token"
	ReasonBadToken    Reason = "bad_token"
	ReasonNoTable     Reason = "no_table"
	ReasonNoRoute     Reason = "no_route"
	ReasonMethod      Reason = "method_not_allowed"
	ReasonPath        Reason = "path_not_allowed"
	ReasonQuery       Reason = "query_not_allowed"
	ReasonBadRequest  Reason = "bad_request"
	ReasonOrgMismatch Reason = "org_mismatch"
)

// Route is one allowed destination.
type Route struct {
	// Name identifies the route in audit rows. It is unique in a set.
	Name  string
	Class Class
	// Org is the org that owns the grant. Empty means a platform default every org may use.
	Org string
	// Host is one exact lower-case host name. No wildcard, no IP literal.
	Host string
	// Port is the TCP port. Zero means 443.
	Port int
	// Methods are the allowed HTTP methods, upper case.
	Methods []string
	// PathPrefixes are the allowed paths. A prefix matches a whole path or a path under it at a
	// segment boundary: "/v2" matches "/v2" and "/v2/x" and never "/v2x". Each starts with "/".
	PathPrefixes []string
	// QueryKeys are the query parameters the route allows. A key not listed refuses the request, and
	// a route with no keys allows no query at all.
	QueryKeys []string
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
	if len(r.PathPrefixes) == 0 {
		return fmt.Errorf("route %q: no path rule", r.Name)
	}
	for _, p := range r.PathPrefixes {
		if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "*?#%;") || !canonical(p) {
			return fmt.Errorf("route %q: path prefix %q must be an absolute, canonical path with no wildcard", r.Name, p)
		}
	}
	for _, k := range r.QueryKeys {
		if k == "" || strings.ContainsAny(k, "*&=") {
			return fmt.Errorf("route %q: bad query key %q", r.Name, k)
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
		if r.Credential != "" {
			if o, ok := credOwner[r.Credential]; ok && o.class != r.Class {
				return nil, fmt.Errorf("credential %q serves classes %q and %q: one credential serves one route class", r.Credential, o.class, r.Class)
			} else if ok && o.org != r.Org {
				return nil, fmt.Errorf("credential %q is used by two orgs: a credential belongs to one org", r.Credential)
			}
			credOwner[r.Credential] = owner{org: r.Org, class: r.Class}
		}
	}
	return &Table{routes: slices.Clone(routes)}, nil
}

// Routes returns a copy of the routes of the table.
func (t *Table) Routes() []Route {
	if t == nil {
		return nil
	}
	return slices.Clone(t.routes)
}

// Request is what the gate decides on. It does not depend on net/http, so both drivers and the tests
// build it the same way.
type Request struct {
	// Scheme is the scheme of the request. Only "https" is served: a credential never goes over
	// plain HTTP.
	Scheme string
	// Token is the actor identity token as the guest sent it.
	Token string
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
	scheme := r.URL.Scheme
	if scheme == "" && r.TLS != nil {
		scheme = "https"
	}
	return Request{
		Scheme:    scheme,
		Token:     r.Header.Get(TokenHeader),
		Authority: host,
		Method:    r.Method,
		Path:      r.URL.EscapedPath(),
		RawQuery:  r.URL.RawQuery,
	}
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
}

// Gate checks the token and the route of each request.
type Gate struct {
	verifier *actortoken.Verifier
	table    *Table
}

// NewGate returns a gate. A nil verifier or a nil table makes the gate refuse every request: the
// gateway never serves a route it could not load.
func NewGate(v *actortoken.Verifier, t *Table) *Gate { return &Gate{verifier: v, table: t} }

// Check decides on r. It never panics and never allows on an error.
func (g *Gate) Check(r Request) Decision {
	if g == nil || g.table == nil || g.verifier == nil {
		return Decision{Reason: ReasonNoTable}
	}
	if strings.TrimSpace(r.Token) == "" {
		return Decision{Reason: ReasonNoToken}
	}
	claims, err := g.verifier.Verify(r.Token)
	if err != nil {
		return Decision{Reason: ReasonBadToken}
	}
	d := g.table.decide(claims.Actor, r)
	d.Actor = claims.Actor
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
	// reason keeps the most specific refusal among routes of the host, so a deny says why.
	reason := ReasonNoRoute
	rank := map[Reason]int{ReasonNoRoute: 0, ReasonOrgMismatch: 1, ReasonMethod: 2, ReasonPath: 3, ReasonQuery: 4}
	// An org's own route wins over a platform route, so the order of the set never decides.
	ordered := make([]Route, 0, len(t.routes))
	for _, rt := range t.routes {
		if rt.Org != "" {
			ordered = append(ordered, rt)
		}
	}
	for _, rt := range t.routes {
		if rt.Org == "" {
			ordered = append(ordered, rt)
		}
	}
	for _, rt := range ordered {
		if rt.Host != host || rt.port() != port {
			continue
		}
		var miss Reason
		switch {
		case rt.Org != "" && rt.Org != actor.Org:
			miss = ReasonOrgMismatch
		case !slices.Contains(rt.Methods, strings.ToUpper(r.Method)):
			miss = ReasonMethod
		case !pathAllowed(rt.PathPrefixes, p):
			miss = ReasonPath
		case !queryAllowed(rt.QueryKeys, q):
			miss = ReasonQuery
		default:
			return Decision{Allowed: true, Reason: ReasonAllowed, Route: rt}
		}
		if rank[miss] > rank[reason] {
			reason = miss
		}
	}
	return Decision{Reason: reason}
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

func queryAllowed(keys []string, q url.Values) bool {
	for k := range q {
		if !slices.Contains(keys, k) {
			return false
		}
	}
	return true
}
