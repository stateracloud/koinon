// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// URLPolicy decides which upstream URLs a provider or a remote MCP server may name. Providers are entered by org admins
// (and by anyone who can write a labelled Secret), and the gateway then sends requests, carrying the
// provider's key, to the URL: without a policy that is a server-side request forgery primitive
// against the node and the cluster. The policy denies, unless the host is in AllowedHosts:
//
//   - loopback, unspecified, link-local, private and shared-address ranges (127/8, ::1, 0/8,
//     169.254/16, fe80::/10, RFC 1918, fc00::/7, 100.64/10) and the cloud metadata addresses,
//     whether written as the host or reached by resolving its name. Private addresses include
//     Kubernetes Services' ClusterIPs;
//   - single-label names (they resolve to Services in the gateway's own namespace through the search
//     path) and `localhost`;
//   - names in Koinon's own namespace: <name>.<namespace>.svc[.cluster.local] and <name>.<namespace>.
//
// It checks the URL when a provider is saved and again, with a fresh lookup, when the reconciler
// renders it, which narrows DNS rebinding but cannot close it: the gateway resolves the name itself
// at request time. Put a NetworkPolicy on the gateway's egress for a hard limit.
//
// Hosts are read the way agentgateway's URL parser (WHATWG) reads them: a host whose last label is a
// number (decimal, 0x hex or leading-0 octal) is an IPv4 address in any of its short forms, so
// 0177.0.0.1, 127.1, 0x7f.0.0.1 and 2130706433 are all 127.0.0.1; one that is not a valid address is
// refused.
//
// AllowedHosts is a platform-level setting (chart value egress.allowedInternalHosts; providers.allowedInternalHosts is a deprecated alias): exact
// hostnames, IPs or CIDRs, each name optionally with a port, such as "ollama.lab.example.com:11434"
// or "10.7.0.0/24". The gateway pod's egress NetworkPolicy is a separate allow list (see the chart).
type URLPolicy struct {
	// Namespace is Koinon's namespace.
	Namespace string
	// AllowedHosts are exempt from every rule above.
	AllowedHosts []string
	// Resolver looks a name up; default net.DefaultResolver.
	Resolver func(ctx context.Context, host string) ([]netip.Addr, error)
}

var (
	metadataAddrs = []netip.Addr{
		netip.MustParseAddr("169.254.169.254"), netip.MustParseAddr("fd00:ec2::254"), netip.MustParseAddr("100.100.100.200"),
	}
	deniedPrefixes = []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("fc00::/7"),
		// IPv6 ranges that carry an IPv4 address inside: refused outright rather than unpacked.
		netip.MustParsePrefix("::/96"),          // IPv4-compatible (::127.0.0.1, ::7f00:1)
		netip.MustParsePrefix("64:ff9b::/96"),   // NAT64
		netip.MustParsePrefix("64:ff9b:1::/48"), // local-use NAT64
		netip.MustParsePrefix("2002::/16"),      // 6to4
	}
)

func deniedAddr(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsLoopback() || a.IsUnspecified() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsMulticast() || a.IsPrivate() || slices.Contains(metadataAddrs, a) ||
		slices.ContainsFunc(deniedPrefixes, func(p netip.Prefix) bool { return p.Contains(a) })
}

// ValidateAllowedHosts refuses an AllowedHosts entry that is a pattern. Entries are matched
// exactly, so a wildcard never matched anything; failing at startup tells the operator instead of
// leaving a setting that looks like it allows a whole domain (ADR 0075, decision 13). A bracketed
// IPv6 literal, with or without a port ("[fd00::1]:11434", as written in a URL), is exact.
func ValidateAllowedHosts(entries []string) error {
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if strings.ContainsAny(withoutIPv6Literal(e), "*?[]{}") {
			return fmt.Errorf("egress.allowedInternalHosts entry %q is a pattern; list each host exactly (an IP or a CIDR is fine)", e)
		}
	}
	return nil
}

// withoutIPv6Literal returns e without a leading bracketed IPv6 address, so that the brackets of
// "[fd00::1]" or "[fd00::1]:11434" are not read as a glob class. Anything else is returned as is.
func withoutIPv6Literal(e string) string {
	if !strings.HasPrefix(e, "[") {
		return e
	}
	end := strings.IndexByte(e, ']')
	if end < 0 {
		return e
	}
	if a, err := netip.ParseAddr(e[1:end]); err != nil || !a.Is6() {
		return e
	}
	return e[end+1:]
}

// allowedAddr reports whether an AllowedHosts entry (an IP or a CIDR) covers a.
func (p URLPolicy) allowedAddr(a netip.Addr) bool {
	a = a.Unmap()
	for _, e := range p.AllowedHosts {
		e = strings.TrimSpace(e)
		if pre, err := netip.ParsePrefix(e); err == nil && pre.Contains(a) {
			return true
		}
		if ip, err := netip.ParseAddr(e); err == nil && ip.Unmap() == a {
			return true
		}
	}
	return false
}

func (p URLPolicy) allowedName(host, hostport string) bool {
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
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// whatwgIPv4 reads host as the WHATWG URL standard's IPv4 parser does. numeric is true when the
// host's last label is a number, which makes it an IPv4 address or invalid; ok is false for an
// invalid one.
func whatwgIPv4(host string) (addr netip.Addr, numeric, ok bool) {
	parts := strings.Split(host, ".")
	if parts[len(parts)-1] == "" && len(parts) > 1 {
		parts = parts[:len(parts)-1]
	}
	isNum := func(s string) bool {
		if s == "" {
			return false
		}
		digits, valid := s, "0123456789"
		if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
			digits, valid = s[2:], "0123456789abcdefABCDEF"
		}
		return strings.Trim(digits, valid) == ""
	}
	if !isNum(parts[len(parts)-1]) {
		return netip.Addr{}, false, false
	}
	if len(parts) > 4 {
		return netip.Addr{}, true, false
	}
	nums := make([]uint64, len(parts))
	for i, s := range parts {
		var v uint64
		var err error
		switch {
		case s == "":
			return netip.Addr{}, true, false
		case strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X"):
			if len(s) == 2 {
				v = 0
			} else if v, err = strconv.ParseUint(s[2:], 16, 64); err != nil {
				return netip.Addr{}, true, false
			}
		case len(s) > 1 && s[0] == '0':
			if v, err = strconv.ParseUint(s[1:], 8, 64); err != nil {
				return netip.Addr{}, true, false
			}
		default:
			if v, err = strconv.ParseUint(s, 10, 64); err != nil {
				return netip.Addr{}, true, false
			}
		}
		nums[i] = v
	}
	for _, v := range nums[:len(nums)-1] {
		if v > 255 {
			return netip.Addr{}, true, false
		}
	}
	last := nums[len(nums)-1]
	if last >= 1<<(8*(5-uint(len(nums)))) {
		return netip.Addr{}, true, false
	}
	ip := last
	for i, v := range nums[:len(nums)-1] {
		ip += v << (8 * (3 - uint(i)))
	}
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(ip)) //nolint:gosec // ip < 2^32 was checked above
	return netip.AddrFrom4(b), true, true
}

// DialContext is a net.Dialer DialContext that connects only to addresses the policy allows. It is
// for clients that call a URL Check accepted: it closes the window between the check and the
// connection (DNS rebinding) by checking the addresses it dials. A host the platform allowed by
// name is dialled as given.
func (p URLPolicy) DialContext(d *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		host = strings.ToLower(strings.TrimSuffix(host, "."))
		if p.allowedName(host, host+":"+port) {
			return d.DialContext(ctx, network, addr)
		}
		var addrs []netip.Addr
		if ip, err := netip.ParseAddr(host); err == nil {
			addrs = []netip.Addr{ip}
		} else if addrs, err = p.lookup(ctx, host); err != nil {
			return nil, err
		}
		var last error
		for _, a := range addrs {
			if deniedAddr(a) && !p.allowedAddr(a) {
				last = invalid("the host %q resolves to an address the egress policy denies", host)
				continue
			}
			c, err := d.DialContext(ctx, network, net.JoinHostPort(a.String(), port))
			if err == nil {
				return c, nil
			}
			last = err
		}
		if last == nil {
			last = errors.New("no address")
		}
		return nil, last
	}
}

// Check returns an *InvalidError when rawURL names a host the policy denies. A name that does not
// resolve passes: nothing is reachable through it now, and the check runs again at render.
func (p URLPolicy) Check(ctx context.Context, rawURL string) error {
	return p.check(ctx, rawURL, false)
}

// CheckStrict is Check for rendering: a name that cannot be resolved is refused too, so a failing
// resolver cannot be used to get past the policy.
func (p URLPolicy) CheckStrict(ctx context.Context, rawURL string) error {
	return p.check(ctx, rawURL, true)
}

func (p URLPolicy) check(ctx context.Context, rawURL string, strict bool) error {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return invalid("url must be an http(s) URL")
	}
	host := strings.ToLower(u.Hostname())
	for i := 0; i < len(host); i++ {
		if host[i] >= 0x80 {
			return invalid("the url's host must be ASCII; write an internationalised name in its xn-- form")
		}
	}
	if strings.ContainsAny(host, "*?[]{}") {
		// Never an exact host, and never allow-listable: a pattern is how the DNS exfiltration
		// channel of a wildcard egress rule opens (ADR 0075, decision 13).
		return invalid("the url's host %q is a pattern; name the host exactly, because wildcard hosts reopen a DNS exfiltration channel", host)
	}
	if p.allowedName(strings.TrimSuffix(host, "."), strings.ToLower(u.Host)) {
		return nil
	}
	deny := func(why string) error {
		return invalid("the url's host %q is not allowed: %s (a platform admin can allow it with egress.allowedInternalHosts)", host, why)
	}
	check := func(a netip.Addr, how string) error {
		if deniedAddr(a) && !p.allowedAddr(a) {
			return deny(how + " a loopback, link-local, private, shared or metadata address")
		}
		return nil
	}
	if ip, err := netip.ParseAddr(host); err == nil { // an IPv6 literal (brackets already stripped)
		return check(ip, "it is")
	}
	if ip, numeric, ok := whatwgIPv4(host); numeric {
		if !ok {
			return deny("it is not a valid IP address, and agentgateway would not read it as a name")
		}
		return check(ip, "it is")
	}
	host = strings.TrimSuffix(host, ".")
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
		if strict {
			return deny("it cannot be resolved right now, and the policy fails closed")
		}
		return nil
	}
	for _, a := range addrs {
		if err := check(a, "it resolves to"); err != nil {
			return err
		}
	}
	return nil
}
