// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"net"
	"net/url"
)

// CredentialSafeURL says a credential may travel to raw. The URL must name a host and be https. A
// loopback address is the one exception: its traffic does not leave the machine (a sidecar, a test).
// The MCP gateway and the code that stores an MCP server use this one rule.
func CredentialSafeURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	if u.Scheme != "http" {
		return false
	}
	h := u.Hostname()
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}
