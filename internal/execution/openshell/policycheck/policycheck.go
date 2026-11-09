// SPDX-License-Identifier: Apache-2.0

// Package policycheck evaluates rendered OpenShell network policy against HTTP requests, for
// tests that assert what a sandbox can and cannot reach without a real gateway. It follows the
// OpenShell policy schema reference (docs/how-it-works/policies/schema.mdx, identical at tags v0.1.5
// and v0.1.7):
//
//   - A sandbox's effective policy is its network_policies plus one entry per attached provider
//     profile, made of the profile's endpoints and binaries.
//   - A request needs an entry whose binaries include the caller and whose endpoints match the
//     host (case-insensitive) and port. An endpoint without a protocol allows at L4.
//   - On an inspected (rest) endpoint, a deny rule wins over every allow rule; otherwise an allow
//     rule or the access preset must match. With enforcement audit OpenShell only logs, so the
//     request is allowed.
//   - REST paths are case-sensitive globs split at "/": "*" and "?" stay within a segment, a
//     whole-segment "**" matches one or more segments. Query globs split at ".".
//   - An allow rule's query matcher needs every value of the parameter to match; a deny rule's
//     needs the parameter present with one matching value. An allow rule whose parameter is
//     absent is treated as matching, the more permissive reading of the schema, so a test that
//     expects a denial is not satisfied by accident.
//
// It is deliberately small: it does not resolve DNS, follow binary ancestry, select endpoints by
// their path field (Koinon renders none) or model GraphQL, MCP and WebSocket rules. The escape
// suite (test/escape) checks the same expectations against a real gateway.
package policycheck

import (
	"net/url"
	"path"
	"strings"

	sandboxv1 "github.com/stateracloud/koinon/internal/gen/openshell/sandbox/v1"
	openshellv1 "github.com/stateracloud/koinon/internal/gen/openshell/v1"
)

// Request is one HTTP request a sandboxed binary makes.
type Request struct {
	Binary string
	Host   string
	Port   uint32
	Method string
	// Path and Query are the request target, for example "/o/r.git/info/refs" and
	// "service=git-upload-pack".
	Path  string
	Query string
}

// Verdict is the outcome of a request and why.
type Verdict struct {
	Allowed bool
	Reason  string
}

type entry struct {
	name      string
	endpoints []*sandboxv1.NetworkEndpoint
	binaries  []string
}

// Evaluate decides r under policy with the given provider profiles attached.
func Evaluate(policy *sandboxv1.SandboxPolicy, profiles []*openshellv1.ProviderProfile, r Request) Verdict {
	var entries []entry
	for name, rule := range policy.GetNetworkPolicies() {
		e := entry{name: name, endpoints: rule.GetEndpoints()}
		for _, b := range rule.GetBinaries() {
			e.binaries = append(e.binaries, b.GetPath())
		}
		entries = append(entries, e)
	}
	for _, p := range profiles {
		e := entry{name: "_provider_" + p.GetId(), endpoints: p.GetEndpoints()}
		for _, b := range p.GetBinaries() {
			e.binaries = append(e.binaries, b.GetPath())
		}
		entries = append(entries, e)
	}

	reqPath := path.Clean("/" + r.Path)
	query, err := url.ParseQuery(r.Query)
	if err != nil {
		return Verdict{Reason: "invalid query"}
	}

	var candidates []*sandboxv1.NetworkEndpoint
	for _, e := range entries {
		if !matchesBinary(e.binaries, r.Binary) {
			continue
		}
		for _, ep := range e.endpoints {
			if hostMatches(ep.GetHost(), r.Host) && portMatches(ep, r.Port) {
				candidates = append(candidates, ep)
			}
		}
	}
	if len(candidates) == 0 {
		return Verdict{Reason: "no endpoint for this binary, host and port"}
	}

	allowed, audit := false, false
	for _, ep := range candidates {
		if ep.GetProtocol() == "" || ep.GetProtocol() == "tcp" {
			return Verdict{Allowed: true, Reason: "L4 endpoint"}
		}
		enforced := ep.GetEnforcement() == sandboxv1.NetworkEnforcementMode_NETWORK_ENFORCEMENT_MODE_ENFORCE
		for _, d := range ep.GetDenyRules() {
			if methodMatches(d.GetMethod(), r.Method) && globMatch(d.GetPath(), reqPath, "/") && denyQueryMatches(d.GetQuery(), query) {
				if enforced {
					return Verdict{Reason: "deny rule " + d.GetMethod() + " " + d.GetPath()}
				}
				audit = true
			}
		}
		if presetAllows(ep.GetAccess(), r.Method) {
			allowed = true
		}
		for _, rule := range ep.GetRules() {
			a := rule.GetAllow()
			if methodMatches(a.GetMethod(), r.Method) && globMatch(a.GetPath(), reqPath, "/") && allowQueryMatches(a.GetQuery(), query) {
				allowed = true
			}
		}
		if !allowed && !enforced {
			audit = true
		}
	}
	switch {
	case allowed && !audit:
		return Verdict{Allowed: true, Reason: "allow rule"}
	case audit:
		return Verdict{Allowed: true, Reason: "audit only"}
	default:
		return Verdict{Reason: "no allow rule matches"}
	}
}

func matchesBinary(binaries []string, bin string) bool {
	for _, b := range binaries {
		if globMatch(b, bin, "/") {
			return true
		}
	}
	return false
}

func hostMatches(pattern, host string) bool {
	return globMatch(strings.ToLower(pattern), strings.ToLower(host), ".")
}

func portMatches(ep *sandboxv1.NetworkEndpoint, port uint32) bool {
	if len(ep.GetPorts()) > 0 {
		for _, p := range ep.GetPorts() {
			if p == port {
				return true
			}
		}
		return false
	}
	return ep.GetPort() == port
}

func methodMatches(pattern, method string) bool {
	return pattern == "*" || strings.EqualFold(pattern, method)
}

func presetAllows(a sandboxv1.NetworkAccessPreset, method string) bool {
	m := strings.ToUpper(method)
	switch a {
	case sandboxv1.NetworkAccessPreset_NETWORK_ACCESS_PRESET_FULL:
		return true
	case sandboxv1.NetworkAccessPreset_NETWORK_ACCESS_PRESET_READ_ONLY:
		return m == "GET" || m == "HEAD" || m == "OPTIONS"
	case sandboxv1.NetworkAccessPreset_NETWORK_ACCESS_PRESET_READ_WRITE:
		return m == "GET" || m == "HEAD" || m == "OPTIONS" || m == "POST" || m == "PUT" || m == "PATCH"
	}
	return false
}

func allowQueryMatches(matchers map[string]*sandboxv1.L7QueryMatcher, q url.Values) bool {
	for name, m := range matchers {
		for _, v := range q[name] {
			if !queryValueMatches(m, v) {
				return false
			}
		}
	}
	return true
}

func denyQueryMatches(matchers map[string]*sandboxv1.L7QueryMatcher, q url.Values) bool {
	for name, m := range matchers {
		vs, ok := q[name]
		if !ok {
			return false
		}
		one := false
		for _, v := range vs {
			if queryValueMatches(m, v) {
				one = true
			}
		}
		if !one {
			return false
		}
	}
	return true
}

func queryValueMatches(m *sandboxv1.L7QueryMatcher, v string) bool {
	if m.GetGlob() != "" && globMatch(m.GetGlob(), v, ".") {
		return true
	}
	for _, g := range m.GetAny() {
		if globMatch(g, v, ".") {
			return true
		}
	}
	return false
}

// globMatch matches value against pattern, both split at sep: a whole-segment "**" matches
// one or more segments, other segments use path.Match ("*", "?", classes).
func globMatch(pattern, value, sep string) bool {
	return matchSegments(strings.Split(pattern, sep), strings.Split(value, sep))
}

func matchSegments(p, v []string) bool {
	if len(p) == 0 {
		return len(v) == 0
	}
	if p[0] == "**" {
		for i := 1; i <= len(v); i++ {
			if matchSegments(p[1:], v[i:]) {
				return true
			}
		}
		return false
	}
	if len(v) == 0 {
		return false
	}
	seg := strings.ReplaceAll(p[0], "**", "*") // next to other characters ** behaves like *
	ok, err := path.Match(seg, v[0])
	return err == nil && ok && matchSegments(p[1:], v[1:])
}
