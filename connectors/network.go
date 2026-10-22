// SPDX-License-Identifier: Apache-2.0

package connectors

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/procoduck/koinon/templates"
)

// HostAccess is what a grant lets the sandbox send straight to one host (plan item C2): allow
// rules with the grant's targets filled in, and the deny rules that win over them.
type HostAccess struct {
	Host  string
	Port  int
	Allow []HTTPRule
	Deny  []HTTPRule
}

// HTTPRule matches requests by method, path glob and query parameters, with OpenShell's REST
// matcher semantics.
type HTTPRule struct {
	Method string
	Path   string
	// Query maps a parameter name to the glob each of its values must match.
	Query map[string]string
}

func hasTargetPlaceholder(path string) bool {
	return strings.Contains(path, "{owner}") || strings.Contains(path, "{repo}")
}

// SplitTarget splits a repository target into owner and name. It rejects anything the type's
// target pattern rejects, and the names "." and "..", which would change a rendered path.
func (t *Type) SplitTarget(target string) (owner, name string, err error) {
	if !t.ValidTarget(target) {
		return "", "", fmt.Errorf("target %q does not match %s", target, t.Spec.Targets.Pattern)
	}
	owner, name, ok := strings.Cut(target, "/")
	if !ok || owner == "" || name == "" || name == "." || name == ".." {
		return "", "", fmt.Errorf("target %q must be owner/name", target)
	}
	return owner, name, nil
}

// Network returns the direct network access a grant of scopes under preset gives for targets,
// one entry per host in spec.hosts order. Only scopes the preset allows contribute: anything that
// would need a decision goes through tools (ADR 0042). Rules whose paths name {owner} or {repo}
// are repeated per target; a host with no allow rule left is omitted, deny rules included, so an
// empty result means no direct access at all.
func (t *Type) Network(preset templates.Preset, scopes, targets []string) ([]HostAccess, error) {
	type split struct{ owner, name string }
	var parsed []split
	for _, target := range targets {
		o, n, err := t.SplitTarget(target)
		if err != nil {
			return nil, err
		}
		parsed = append(parsed, split{o, n})
	}

	byHost := map[string]*HostAccess{}
	seen := map[string]bool{}
	add := func(host string, deny bool, r HTTPRule) {
		h, ok := byHost[host]
		if !ok {
			h = &HostAccess{Host: host}
			byHost[host] = h
		}
		k := fmt.Sprintf("%s|%t|%s|%s|%v", host, deny, r.Method, r.Path, r.Query)
		if seen[k] {
			return
		}
		seen[k] = true
		if deny {
			h.Deny = append(h.Deny, r)
		} else {
			h.Allow = append(h.Allow, r)
		}
	}
	for _, id := range scopes {
		if t.Decide(preset, id) != Allow {
			continue
		}
		s, _ := t.Scope(id)
		for _, n := range s.Network {
			for _, method := range n.Methods {
				for _, path := range n.Paths {
					if !hasTargetPlaceholder(path) {
						add(n.Host, n.Deny, HTTPRule{Method: method, Path: path, Query: maps.Clone(n.Query)})
						continue
					}
					for _, tg := range parsed {
						p := strings.NewReplacer("{owner}", tg.owner, "{repo}", tg.name).Replace(path)
						add(n.Host, n.Deny, HTTPRule{Method: method, Path: p, Query: maps.Clone(n.Query)})
					}
				}
			}
		}
	}

	var out []HostAccess
	for _, h := range t.Spec.Hosts {
		a, ok := byHost[h.Host]
		if !ok || len(a.Allow) == 0 {
			continue
		}
		a.Port = h.Port
		out = append(out, *a)
	}
	return out, nil
}

// HasNetwork reports whether any of scopes carries network rules the preset allows.
func (t *Type) HasNetwork(preset templates.Preset, scopes []string) bool {
	return slices.ContainsFunc(scopes, func(id string) bool {
		s, ok := t.Scope(id)
		return ok && len(s.Network) > 0 && t.Decide(preset, id) == Allow
	})
}
