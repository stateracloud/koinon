// SPDX-License-Identifier: Apache-2.0

package connectors

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"
)

// Which MCP servers may act through a connection (#117, ADR 0091). An MCP server linked to a
// code-host connection has its code-host tools classified against that connection's repository
// list, so a server linked to the wrong connection (a GitHub MCP server to a Gitea connection)
// would make calls to one host count as inside the list of another. A link is therefore allowed
// only when the server's URL names a host known to act on the connection's host:
//
//   - the connection's own web host and API host, for every connector and location;
//   - for a connection on the connector's default host only, spec.site.mcpHosts (for GitHub:
//     api.githubcopilot.com, where GitHub runs its MCP server for github.com).
//
//   - the extra hosts an org admin added to that one connection (#240), on any location.
//
// A connection on its own host (GitHub Enterprise Server, every Gitea) accepts only that host and
// the admin's extra hosts: a hosted MCP server of the default host does not act on it. Hosts are compared by name, without
// the port: one host name is one operator. The table is the connector type's data, so a new
// connector declares its own MCP hosts in its connector.yaml.

// MaxExtraMCPHosts is how many extra hosts an admin may add to one connection (#240).
const MaxExtraMCPHosts = 20

// MCPHosts returns the host names an MCP server's URL may name to act through a connection at
// loc, lower-case and without duplicates. extra are the hosts an org admin added to this one
// connection (#240; ParseMCPHost made them); they apply on every location, the default host too.
func (t *Type) MCPHosts(loc Location, extra []string) []string {
	hosts := []string{normalHost(loc.Web.Host), normalHost(loc.API.Host)}
	if loc.Default && t.Spec.Site != nil {
		for _, h := range t.Spec.Site.MCPHosts {
			hosts = append(hosts, normalHost(h))
		}
	}
	for _, h := range extra {
		hosts = append(hosts, normalHost(h))
	}
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		if h != "" && !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	return out
}

// ServesMCP checks that an MCP server at serverURL may act through a connection at loc. The
// error says which hosts would be accepted and is safe to show. extra are the connection's admin-added
// hosts (see MCPHosts).
func (t *Type) ServesMCP(loc Location, extra []string, serverURL string) error {
	u, err := url.Parse(serverURL)
	if err != nil || u.Hostname() == "" {
		return fmt.Errorf("the MCP server URL %q has no host", serverURL)
	}
	// Over plain http the server's host is not authenticated: anything on the path could answer as
	// the code host and its calls would count as inside.
	if u.Scheme != "https" {
		return fmt.Errorf("an MCP server linked to a code-host connection must use https, not %q", u.Scheme)
	}
	host := normalHost(u.Hostname())
	allowed := t.MCPHosts(loc, extra)
	if slices.Contains(allowed, host) {
		return nil
	}
	name := t.Metadata.DisplayName
	if c := t.Spec.Capabilities; c != nil && c.DisplayName != "" {
		name = c.DisplayName
	}
	msg := fmt.Sprintf("an MCP server at %s does not act on the %s connection at %s; link it to the connection of its own code host (this connection accepts MCP servers at %s)",
		host, name, loc.BaseURL(), strings.Join(allowed, ", "))
	if privateLooking(host) {
		// Two lists decide for a private server: this one (who may act through the connection) and the
		// platform's egress list (whether the platform may reach it at all).
		msg += "; to add the host to this connection, use the connection's MCP hosts. A private host also needs to be on the platform egress list (egress.allowedInternalHosts) before the platform can reach it"
	}
	return errors.New(msg)
}

// privateLooking says whether a host name looks internal: a single label, or a suffix that is not a
// public DNS name. A name cannot show that its address is private; this only decides whether the
// refusal also names the egress list.
func privateLooking(host string) bool {
	if !strings.Contains(host, ".") {
		return true
	}
	for _, suffix := range []string{".local", ".internal", ".lan", ".home", ".corp", ".intranet", ".localdomain", ".svc", ".cluster.local"} {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// ReservedMCPHost returns the display name of the embedded connector type whose default web host, API
// host or MCP host is host, and true. Such a host is never an admin's extra MCP host: it would let
// calls to that code host count as inside the list of another connection (#117). The connector
// types are the embedded ones; a connection's own host is checked by the caller.
func ReservedMCPHost(host string) (string, bool) {
	host = normalHost(host)
	types, err := All()
	if err != nil {
		return "embedded connector types", true // fail closed
	}
	for _, t := range types {
		if t.Spec.Site != nil && t.Spec.Site.SelfHosted {
			continue
		}
		if slices.Contains(t.MCPHosts(t.DefaultLocation(), nil), host) {
			return t.Metadata.DisplayName, true
		}
	}
	return "", false
}

// ParseMCPHost validates a host name an admin adds to a connection's MCP hosts (#240) and returns it
// as ServesMCP compares it: lower-case, without a trailing dot. Only a plain DNS host name is
// accepted: no scheme, port, path, wildcard, user info or IP address, so a host entry never matches
// more than the one operator it names (the match stays exact).
func ParseMCPHost(s string) (string, error) {
	h := normalHost(s)
	if h == "" {
		return "", errors.New("the host is empty")
	}
	if len(h) > 253 {
		return "", errors.New("the host is longer than 253 characters")
	}
	if net.ParseIP(h) != nil || strings.Trim(h, "0123456789.") == "" {
		return "", fmt.Errorf("%q is an IP address; name the MCP server by its DNS host name", s)
	}
	if h == "localhost" || h == "localhost.localdomain" || strings.HasSuffix(h, ".localhost") {
		return "", fmt.Errorf("%q is a loopback name (RFC 6761); name the MCP server by its DNS host name", s)
	}
	labels := strings.Split(h, ".")
	// hostnameRE accepts 127.1, 0x7f.1 and 2130706433, which a URL parser reads as IP addresses (WHATWG).
	// No public top-level label is all digits or a 0x number, so refuse a name that ends in one.
	if last := labels[len(labels)-1]; isNumberLabel(last) {
		return "", fmt.Errorf("%q ends in a number, so it reads as an IP address; name the MCP server by its DNS host name", s)
	}
	for _, label := range labels {
		// An internationalised name is shown to the admin in Unicode but compared as punycode; refuse
		// the xn-- form, so no entry can hide a look-alike of another operator's name.
		if strings.HasPrefix(label, "xn--") {
			return "", fmt.Errorf("%q has an internationalised (xn--) label, which is not accepted", s)
		}
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("%q is not a host name", s)
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return "", fmt.Errorf("%q is not a plain host name (letters, digits, hyphens and dots only; no port, scheme, path or wildcard)", s)
			}
		}
	}
	return h, nil
}

// isNumberLabel is true for a label of only digits, or a 0x hexadecimal number.
func isNumberLabel(l string) bool {
	if l == "" {
		return false
	}
	if rest, ok := strings.CutPrefix(l, "0x"); ok {
		return strings.Trim(rest, "0123456789abcdef") == ""
	}
	return strings.Trim(l, "0123456789") == ""
}

// normalHost is a host name as ServesMCP compares it: lower-case, without a trailing dot.
func normalHost(h string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
}

// checkMCPHosts enforces the rules on spec.site.mcpHosts the JSON Schema cannot express.
func (t *Type) checkMCPHosts() error {
	st := t.Spec.Site
	if st == nil || len(st.MCPHosts) == 0 {
		return nil
	}
	if st.SelfHosted {
		return errors.New("site: a selfHosted connector has no default host, so it cannot have mcpHosts")
	}
	for _, h := range st.MCPHosts {
		if h != normalHost(h) || strings.ContainsAny(h, ":/") {
			return fmt.Errorf("site: mcpHosts: %q must be a lower-case host name without a port", h)
		}
	}
	return nil
}
