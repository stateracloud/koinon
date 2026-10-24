// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"context"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"
)

// URLPolicy decides which upstream URLs a provider may name. Providers are entered by org admins
// (and by anyone who can write a labelled Secret), and the gateway then sends requests, carrying the
// provider's key, to the URL: without a policy that is a server-side request forgery primitive
// against the node and the cluster. The policy denies, unless the host is in AllowedHosts:
//
//   - loopback, unspecified and link-local addresses (127/8, ::1, 169.254/16, fe80::/10) and the
//     cloud metadata addresses, whether written as the host or reached by resolving its name;
//   - single-label names (they resolve to Services in the gateway's own namespace through the search
//     path) and `localhost`;
//   - names in Koinon's own namespace: <name>.<namespace>.svc[.cluster.local] and <name>.<namespace>.
//
// It checks the URL when a provider is saved and again, with a fresh lookup, when the reconciler
// renders it, which narrows DNS rebinding but cannot close it: the gateway resolves the name itself
// at request time. Put a NetworkPolicy on the gateway's egress for a hard limit.
//
// AllowedHosts is a platform-level setting (chart value providers.allowedInternalHosts): exact
// hostnames or IPs, each optionally with a port, such as "ollama.lab.example.com:11434".
type URLPolicy struct {
	// Namespace is Koinon's namespace.
	Namespace string
	// AllowedHosts are exempt from every rule above.
	AllowedHosts []string
	// Resolver looks a name up; default net.DefaultResolver.
	Resolver func(ctx context.Context, host string) ([]netip.Addr, error)
}

var metadataAddrs = []netip.Addr{
	netip.MustParseAddr("169.254.169.254"), netip.MustParseAddr("fd00:ec2::254"), netip.MustParseAddr("100.100.100.200"),
}

func deniedAddr(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsLoopback() || a.IsUnspecified() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsMulticast() || slices.Contains(metadataAddrs, a)
}

func (p URLPolicy) allowed(host, hostport string) bool {
	for _, a := range p.AllowedHosts {
		a = strings.ToLower(strings.TrimSpace(a))
		if a != "" && (a == host || a == hostport) {
			return true
		}
	}
	return false
}

func (p URLPolicy) lookup(ctx context.Context, host string) ([]netip.Addr, error) {
	if p.Resolver != nil {
		return p.Resolver(ctx, host)
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	return ips, err
}

// Check returns an *InvalidError when rawURL names a host the policy denies. A name that does not
// resolve passes: nothing is reachable through it now, and the check runs again at render.
func (p URLPolicy) Check(ctx context.Context, rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return invalid("url must be an http(s) URL of an OpenAI-compatible API")
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if p.allowed(host, strings.ToLower(u.Host)) {
		return nil
	}
	deny := func(why string) error {
		return invalid("the url's host %q is not allowed: %s (a platform admin can allow it with providers.allowedInternalHosts)", host, why)
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if deniedAddr(ip) {
			return deny("it is a loopback, link-local or metadata address")
		}
		return nil
	}
	switch {
	case host == "localhost" || strings.HasSuffix(host, ".localhost"):
		return deny("it is the gateway itself")
	case !strings.Contains(host, "."):
		return deny("a name without a dot resolves to a Service in Koinon's namespace")
	}
	if ns := p.Namespace; ns != "" {
		for _, suffix := range []string{"." + ns + ".svc", "." + ns + ".svc.cluster.local", "." + ns} {
			if strings.HasSuffix(host, suffix) {
				return deny("it is in Koinon's own namespace")
			}
		}
	}
	addrs, err := p.lookup(ctx, host)
	if err != nil {
		return nil
	}
	for _, a := range addrs {
		if deniedAddr(a) {
			return deny("it resolves to a loopback, link-local or metadata address")
		}
	}
	return nil
}
