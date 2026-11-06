// SPDX-License-Identifier: Apache-2.0

package connectors

import (
	"errors"
	"fmt"
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
// A connection on its own host (GitHub Enterprise Server, every Gitea) accepts only that host: a
// hosted MCP server of the default host does not act on it. Hosts are compared by name, without
// the port: one host name is one operator. The table is the connector type's data, so a new
// connector declares its own MCP hosts in its connector.yaml.

// MCPHosts returns the host names an MCP server's URL may name to act through a connection at
// loc, lower-case and without duplicates.
func (t *Type) MCPHosts(loc Location) []string {
	hosts := []string{normalHost(loc.Web.Host), normalHost(loc.API.Host)}
	if loc.Default && t.Spec.Site != nil {
		for _, h := range t.Spec.Site.MCPHosts {
			hosts = append(hosts, normalHost(h))
		}
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
// error says which hosts would be accepted and is safe to show.
func (t *Type) ServesMCP(loc Location, serverURL string) error {
	u, err := url.Parse(serverURL)
	if err != nil || u.Hostname() == "" {
		return fmt.Errorf("the MCP server URL %q has no host", serverURL)
	}
	host := normalHost(u.Hostname())
	allowed := t.MCPHosts(loc)
	if slices.Contains(allowed, host) {
		return nil
	}
	name := t.Metadata.DisplayName
	if c := t.Spec.Capabilities; c != nil && c.DisplayName != "" {
		name = c.DisplayName
	}
	return fmt.Errorf("an MCP server at %s does not act on the %s connection at %s; link it to the connection of its own code host (this connection accepts MCP servers at %s)",
		host, name, loc.BaseURL(), strings.Join(allowed, ", "))
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
