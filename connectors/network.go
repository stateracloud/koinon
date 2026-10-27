// SPDX-License-Identifier: Apache-2.0

package connectors

import (
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/stateracloud/koinon/templates"
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
	return t.NetworkAt(t.DefaultLocation(), preset, scopes, targets)
}

// HostPort is a host and the port the sandbox reaches it on.
type HostPort struct {
	Host string
	Port int
}

// Location is where one connection's code host lives: the web and git host, and the REST API.
// On the default host the API has its own host; on any other host it is served from the web
// host under APIPath (GitHub Enterprise Server: /api/v3).
type Location struct {
	Web, API HostPort
	// APIPath is prepended to the paths of the API's network rules.
	APIPath string
	// Default is true for the connector's default host.
	Default bool
}

// BaseURL is the web base URL, https://host[:port].
func (l Location) BaseURL() string { return "https://" + l.Web.authority() }

// APIBaseURL is the REST API's base URL, with no trailing slash.
func (l Location) APIBaseURL() string { return "https://" + l.API.authority() + l.APIPath }

func (h HostPort) authority() string {
	if h.Port == 443 {
		return h.Host
	}
	return h.Host + ":" + strconv.Itoa(h.Port)
}

// Authority is the host, with the port when it is not 443; it is what a git config URL section
// and a credential request name.
func (h HostPort) Authority() string { return h.authority() }

var hostnameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)

// parseHostPort reads an https URL that is only a host and an optional port.
func parseHostPort(raw string) (HostPort, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return HostPort{}, fmt.Errorf("%q is not a URL", raw)
	}
	if u.Scheme != "https" {
		return HostPort{}, fmt.Errorf("%q must be an https URL", raw)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return HostPort{}, fmt.Errorf("%q must be only a host and an optional port", raw)
	}
	host := strings.ToLower(u.Hostname())
	if !hostnameRE.MatchString(host) || strings.Contains(host, "..") {
		return HostPort{}, fmt.Errorf("%q has no valid host name", raw)
	}
	port := 443
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return HostPort{}, fmt.Errorf("%q has a bad port", raw)
		}
		port = n
	}
	return HostPort{Host: host, Port: port}, nil
}

// DefaultLocation is the connector's default host, the one spec.hosts and the network rules
// name. A connector with no spec.site has none: the first host serves as both.
func (t *Type) DefaultLocation() Location {
	if t.Spec.Site != nil {
		web, werr := parseHostPort(t.Spec.Site.BaseURL)
		api, aerr := parseHostPort(t.Spec.Site.APIURL)
		if werr == nil && aerr == nil {
			return Location{Web: web, API: api, APIPath: "", Default: true}
		}
	}
	var hp HostPort
	if len(t.Spec.Hosts) > 0 {
		hp = HostPort{Host: t.Spec.Hosts[0].Host, Port: t.Spec.Hosts[0].Port}
	}
	return Location{Web: hp, API: hp, Default: true}
}

// Locate resolves a connection's base URL. Empty, or the default's own URL, is the default
// location. Anything else must be an https URL of only a host and an optional port; its REST
// API is served from the same host under spec.site.apiPath, and a connector without one cannot
// be moved to another host.
func (t *Type) Locate(baseURL string) (Location, error) {
	def := t.DefaultLocation()
	if strings.TrimSpace(baseURL) == "" {
		return def, nil
	}
	hp, err := parseHostPort(strings.TrimSpace(baseURL))
	if err != nil {
		return Location{}, fmt.Errorf("base URL: %w", err)
	}
	if hp == def.Web {
		return def, nil
	}
	if t.Spec.Site == nil || t.Spec.Site.APIPath == "" {
		return Location{}, fmt.Errorf("base URL: the %s connector cannot be used on another host", t.Metadata.ID)
	}
	return Location{Web: hp, API: hp, APIPath: t.Spec.Site.APIPath}, nil
}

// NetworkAt is Network for a connection at loc: rules written for the default hosts are
// rendered at loc's web and API hosts, and the API's paths get loc's API path prefix. A host the
// connector does not treat as its web or API host is left as written.
func (t *Type) NetworkAt(loc Location, preset templates.Preset, scopes, targets []string) ([]HostAccess, error) {
	type split struct{ owner, name string }
	var parsed []split
	for _, target := range targets {
		o, n, err := t.SplitTarget(target)
		if err != nil {
			return nil, err
		}
		parsed = append(parsed, split{o, n})
	}

	// dest maps a host named in the connector's rules to where loc puts it and the prefix its
	// paths get.
	def := t.DefaultLocation()
	dest := func(host string) (HostPort, string) {
		switch host {
		case def.Web.Host:
			return loc.Web, ""
		case def.API.Host:
			return loc.API, loc.APIPath
		}
		for _, h := range t.Spec.Hosts {
			if h.Host == host {
				return HostPort(h), ""
			}
		}
		return HostPort{Host: host, Port: 443}, ""
	}

	byHost := map[HostPort]*HostAccess{}
	seen := map[string]bool{}
	add := func(host string, deny bool, r HTTPRule) {
		hp, prefix := dest(host)
		r.Path = prefix + r.Path
		h, ok := byHost[hp]
		if !ok {
			h = &HostAccess{Host: hp.Host, Port: hp.Port}
			byHost[hp] = h
		}
		k := fmt.Sprintf("%s|%t|%s|%s|%v", hp.Authority(), deny, r.Method, r.Path, r.Query)
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
	done := map[HostPort]bool{}
	for _, h := range t.Spec.Hosts {
		hp, _ := dest(h.Host)
		a, ok := byHost[hp]
		if done[hp] || !ok || len(a.Allow) == 0 {
			continue
		}
		done[hp] = true
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
