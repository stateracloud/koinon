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

	"golang.org/x/net/publicsuffix"
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
//   - names in Koinon's own namespace: <name>.<namespace>.svc[.<cluster domain>] and <name>.<namespace>.
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
	// Search is the resolver of a sandbox guest, for CheckSandbox, CheckSandboxStrict and
	// SandboxDialContext. Nil means SubstrateSearch. A value with no Domains is a guest with no search
	// domains.
	Search *ResolverSearch
}

// ResolverSearch is the resolv.conf of a sandbox guest: what its resolver does to a name that has no
// trailing dot (#1054).
type ResolverSearch struct {
	// Domains is the search list. Empty means the guest has no search domains: it resolves every name as
	// written, and the sandbox refuses a name that does not resolve as written.
	Domains []string
	// Ndots is the "options ndots" value: a name with fewer dots is tried with each search domain first.
	Ndots int
	// ClusterDomain is the cluster's DNS domain (the chart value clusterDomain). A Service name in full is
	// <service>.<namespace>.svc.<ClusterDomain>.
	ClusterDomain string
}

// SubstrateSearch is the resolv.conf measured in a Substrate gVisor actor on kind: the search list and
// ndots come from the worker pod, so the first domain is the worker namespace, not the connector's
// (Substrate internal/ateworker/actornet/etcfiles.go:24-38). The actor's DNS relay answers NXDOMAIN for a
// name that its egress policy does not allow (Substrate internal/atunnel/dns/dnshandler.go:120-122): the
// guest still needs an egress policy that allows the name after expansion. That is the egress layer's
// job, not this check's.
var SubstrateSearch = ResolverSearch{
	Domains:       []string{"ate-workers.svc.cluster.local", "svc.cluster.local", "cluster.local"},
	Ndots:         5,
	ClusterDomain: "cluster.local",
}

// DefaultClusterDomain is the cluster DNS domain when no one sets another (the chart value clusterDomain).
const DefaultClusterDomain = "cluster.local"

// SubstrateSearchFor is SubstrateSearch for a cluster with another DNS domain: the guest copies the search
// list of the worker pod, and that list is built from the cluster domain (ate-workers.svc.<domain>,
// svc.<domain>, <domain>). An empty domain means DefaultClusterDomain.
func SubstrateSearchFor(clusterDomain string) ResolverSearch {
	d := strings.Trim(strings.ToLower(strings.TrimSpace(clusterDomain)), ".")
	if d == "" {
		d = DefaultClusterDomain
	}
	return ResolverSearch{
		Domains:       []string{"ate-workers.svc." + d, "svc." + d, d},
		Ndots:         SubstrateSearch.Ndots,
		ClusterDomain: d,
	}
}

// ValidateClusterDomain refuses a cluster domain that is not a DNS name (no trailing dot, no upper case,
// no empty label), so a typo in the chart value fails at start instead of changing which names the
// connector check accepts. An empty value is valid and means DefaultClusterDomain.
func ValidateClusterDomain(domain string) error {
	if domain == "" {
		return nil
	}
	if len(domain) > 253 {
		return fmt.Errorf("clusterDomain %q is longer than 253 characters", domain)
	}
	for _, label := range strings.Split(domain, ".") {
		ok := label != "" && len(label) <= 63 && label[0] != '-' && label[len(label)-1] != '-'
		for i := 0; ok && i < len(label); i++ {
			c := label[i]
			ok = c == '-' || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
		}
		if !ok {
			return fmt.Errorf("clusterDomain %q is not a lower-case DNS name such as cluster.local (no trailing dot, no empty label)", domain)
		}
	}
	return nil
}

func (p URLPolicy) search() ResolverSearch {
	if p.Search == nil {
		return SubstrateSearch
	}
	return *p.Search
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
		e = unbracket(strings.TrimSpace(e))
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
		e = unbracket(strings.TrimSpace(e))
		if pre, err := netip.ParsePrefix(e); err == nil && pre.Contains(a) {
			return true
		}
		if ip, err := netip.ParseAddr(e); err == nil && ip.Unmap() == a {
			return true
		}
	}
	return false
}

// unbracket turns an entry such as "[fd00::1]" into "fd00::1", as the entry is written in a url.
// An entry with a port ("[fd00::1]:443") stays as it is.
func unbracket(e string) string {
	if strings.HasPrefix(e, "[") && strings.HasSuffix(e, "]") {
		return e[1 : len(e)-1]
	}
	return e
}

func (p URLPolicy) allowedName(host, hostport string) bool {
	for _, a := range p.AllowedHosts {
		a = unbracket(strings.ToLower(strings.TrimSpace(a)))
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

// endpoint reads a URL's host and port once, for every allow-list decision (#1054). host is lower case
// without a trailing dot. hostport is "host:port" with the default port of the scheme made explicit
// (http 80, https 443), so "host:443" matches both https://host and https://host:443. An allow-list entry
// without a port matches every port of the host (the documented meaning of egress.allowedInternalHosts);
// an entry with a port matches that port only. The port never changes the host decision: the host rules
// read Hostname() only.
func endpoint(u *url.URL) (host, hostport string) {
	host = strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	port := u.Port()
	if n, err := strconv.Atoi(port); err == nil {
		port = strconv.Itoa(n) // 0443 is port 443
	}
	if port == "" {
		switch strings.ToLower(u.Scheme) {
		case "http":
			port = "80"
		case "https":
			port = "443"
		}
	}
	if port == "" {
		return host, host
	}
	return host, net.JoinHostPort(host, port)
}

// checkPort refuses a port that no connection can use.
func checkPort(u *url.URL) error {
	port := u.Port()
	if port == "" {
		return nil
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return invalid("the url's port %q is not a port number from 1 to 65535", port)
	}
	return nil
}

// DialContext is a net.Dialer DialContext that connects only to addresses the policy allows. It is
// for clients that call a URL Check accepted: it closes the window between the check and the
// connection (DNS rebinding) by checking the addresses it dials. A host the platform allowed by
// name is dialled as given.
func (p URLPolicy) DialContext(d *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return p.dialer(d, false)
}

// dialer is DialContext. With absolute it looks a name up as the sandbox does, as an absolute name with
// no search domains, and dials the addresses of that one lookup, so the check and the connection agree.
func (p URLPolicy) dialer(d *net.Dialer, absolute bool) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		given := strings.ToLower(host)
		if absolute {
			if err := p.sandboxHost(given, "the host"); err != nil {
				return nil, err
			}
		}
		host = strings.TrimSuffix(given, ".")
		allowed := p.allowedName(host, net.JoinHostPort(host, port))
		if allowed && !absolute {
			return d.DialContext(ctx, network, addr)
		}
		var addrs []netip.Addr
		if ip, err := netip.ParseAddr(host); err == nil {
			addrs = []netip.Addr{ip}
		} else {
			name := host
			if absolute {
				name += "."
			}
			if addrs, err = p.lookup(ctx, name); err != nil {
				if absolute {
					return nil, invalid("the host %q does not resolve as an absolute name, and a name without a trailing dot may be expanded by a search domain: %s", host, p.fullNameHint(host))
				}
				return nil, err
			}
		}
		var last error
		for _, a := range addrs {
			if !allowed && deniedAddr(a) && !p.allowedAddr(a) {
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
// resolver cannot be used to get past the policy. A URL with '$' is refused as well: the gateway's
// config loader would read $NAME in it as an environment variable, and gateway.State.Validate
// refuses the whole state for it, so a row saved before the validators refused '$' is reported and
// left out here instead of stopping every org's config (#668).
func (p URLPolicy) CheckStrict(ctx context.Context, rawURL string) error {
	if strings.Contains(rawURL, "$") {
		return invalid("url must not contain '$': the gateway's config loader reads $NAME as an environment variable")
	}
	return p.check(ctx, rawURL, true)
}

// CheckSandbox is Check for a URL that also becomes sandbox egress, such as a connector's base URL (#1054).
// The guest resolves a name with the search list and ndots of URLPolicy.Search (default SubstrateSearch,
// measured in a Substrate guest on kind), while the API pod resolves it with its own search domains. A name
// that a search domain can turn into another name works in one place and fails or reaches a different
// Service in the other. CheckSandbox refuses such a name (ShortName) before any allow-list entry is read.
// It also looks the host up as an absolute name, as the guest does, and applies the address rules to those
// addresses; a name that only the relative lookup resolves is refused. Like Check, it passes a name that
// does not resolve at all. The guest still needs an egress policy that allows the name; that is the egress
// layer's job, not this check's.
func (p URLPolicy) CheckSandbox(ctx context.Context, rawURL string) error {
	return p.checkSandbox(ctx, rawURL, false)
}

// CheckSandboxStrict is CheckStrict with the rule of CheckSandbox, for rendering: any failure of the
// absolute lookup refuses the URL.
func (p URLPolicy) CheckSandboxStrict(ctx context.Context, rawURL string) error {
	return p.checkSandbox(ctx, rawURL, true)
}

func (p URLPolicy) checkSandbox(ctx context.Context, rawURL string, strict bool) error {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return invalid("url must be an http(s) URL")
	}
	if err := p.sandboxHost(strings.ToLower(u.Hostname()), "the url's host"); err != nil {
		return err
	}
	check := p.Check
	if strict {
		check = p.CheckStrict
	}
	if err := check(ctx, rawURL); err != nil {
		return err
	}
	host, hostport := endpoint(u)
	if _, err := netip.ParseAddr(host); err == nil {
		return nil
	}
	addrs, err := p.lookup(ctx, host+".")
	if err != nil {
		if _, relErr := p.lookup(ctx, host); relErr == nil {
			return invalid("the url's host %q is a short name: %s", host, p.fullNameHint(host))
		}
		if strict {
			return invalid("the url's host %q does not resolve as an absolute name, and a name without a trailing dot may be expanded by a search domain: %s", host, p.fullNameHint(host))
		}
		return nil
	}
	if p.allowedName(host, hostport) {
		return nil
	}
	for _, a := range addrs {
		if deniedAddr(a) && !p.allowedAddr(a) {
			return invalid("the url's host %q is not allowed: it resolves to a loopback, link-local, private, shared or metadata address (a platform admin can allow it with egress.allowedInternalHosts)", host)
		}
	}
	return nil
}

// SandboxDialContext is DialContext for clients of connector URLs. It refuses a short name, and it
// resolves the host once, as an absolute name, as the sandbox does. It dials only the addresses of that
// lookup, so a name that the API pod would expand through a search domain is never dialled.
func (p URLPolicy) SandboxDialContext(d *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return p.dialer(d, true)
}

// GatewayDialContext is the dialer of the gateway's egress routes (#1086). It resolves every name once, as
// an absolute name, so the gateway pod's search domains never turn a route host into a Service name, and
// it dials only the addresses of that lookup. Before it resolves, it refuses by name what check refuses by
// name: a single label, localhost, a numeric host that resolvers read in different ways, and a name in
// Koinon's own namespace. The address rules then apply as in DialContext, so an allowed CIDR that holds
// a ClusterIP still does not open Koinon's own Services by name.
func (p URLPolicy) GatewayDialContext(d *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	q := p
	// The gateway asks for absolute names, so no search list applies; ShortName then refuses a single
	// label and the .svc forms only.
	q.Search = &ResolverSearch{ClusterDomain: p.search().ClusterDomain}
	dial := q.dialer(d, true)
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		if why := p.ownName(host); why != "" {
			return nil, invalid("the host %q is refused: %s", host, why)
		}
		return dial(ctx, network, addr)
	}
}

// ownName returns why a host names the gateway or Koinon's own namespace, and "" for any other host.
func (p URLPolicy) ownName(host string) string {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	switch {
	case host == "localhost" || strings.HasSuffix(host, ".localhost"):
		return "it is the gateway itself"
	case p.Namespace == "":
		return ""
	}
	ns := strings.ToLower(p.Namespace)
	for _, suffix := range []string{"." + ns + ".svc", "." + ns + ".svc." + strings.Trim(strings.ToLower(p.search().ClusterDomain), "."), "." + ns + ".svc." + DefaultClusterDomain, "." + ns} {
		if strings.HasSuffix(host, suffix) {
			return "it is in Koinon's own namespace"
		}
	}
	return ""
}

// sandboxHost is the host rule of the sandbox URL check and dial: a numeric host that a resolver could read
// differently (numericHost), then a name that a search domain would expand (ShortName). host is lower case
// and as written, with its trailing dot if it has one.
func (p URLPolicy) sandboxHost(host, subject string) error {
	if why := numericHost(host); why != "" {
		return invalid("%s %q is not a plain IP address or a name: %s", subject, host, why)
	}
	if why := p.ShortName(host); why != "" {
		return invalid("%s %q is a short name: %s", subject, host, why)
	}
	return nil
}

// numericHost returns a message for a host that is written with numbers but is not an address in the one form
// that every resolver reads the same way, and "" for any other host. Resolvers disagree about numbers:
// inet_aton and the WHATWG URL parser read 0x7f.1, 2130706433 and 0177.0.0.1 as 127.0.0.1, a DNS-only resolver
// looks them up as names, and a trailing dot makes 10.0.0.1. a name for some and an address for others. A
// check that read one way and a guest that read the other would apply the address rules to the wrong
// host. So the rule is closed, not a list of forms: a host is allowed to be a number only when it is an
// IPv4 address as four decimal numbers without leading zeros (10.0.0.1), or an IPv6 address
// ([::1], [fd00::1]) without a zone, and in neither case with a trailing dot. Every other host whose last
// label is a number (decimal, 0x hex or leading-0 octal, one to four labels, with or without a trailing
// dot) is refused. A name whose last label is not a number (1.2.3.4.example.com) is a name.
func numericHost(host string) string {
	bare := strings.TrimSuffix(host, ".")
	if a, err := netip.ParseAddr(bare); err == nil {
		switch {
		case bare != host:
			return "an address with a trailing dot is a name to some resolvers; write the address without the dot"
		case a.Zone() != "":
			return "an IPv6 zone is not allowed; write the address without %zone"
		}
		return ""
	}
	if _, numeric, _ := whatwgIPv4(bare); numeric {
		return "resolvers read a numeric host in different ways (0x7f.1, 2130706433 and 0177.0.0.1 are all 127.0.0.1 for some of them); write an IPv4 address as four decimal numbers (10.0.0.1), or use a DNS name"
	}
	return ""
}

func (p URLPolicy) fullNameHint(host string) string {
	domain := strings.Trim(strings.ToLower(p.search().ClusterDomain), ".")
	if domain == "" {
		domain = DefaultClusterDomain
	}
	var full string
	switch {
	case strings.HasSuffix(host, ".svc"):
		full = "such as " + host + "." + domain
	case strings.HasSuffix(host, ".svc.cluster") && strings.HasPrefix(domain, "cluster."):
		full = "such as " + host + strings.TrimPrefix(domain, "cluster")
	case !strings.Contains(host, "."):
		full = "such as " + host + ".<namespace>.svc." + domain
	case strings.Count(host, ".") == 1:
		full = "such as " + host + ".svc." + domain
	default:
		full = "the fully qualified name of the host (for a cluster Service, <service>.<namespace>.svc." + domain + ")"
	}
	return "write the full name, " + full
}

// ShortName returns, for a host that the guest's resolver would expand into a name other than the one
// written, a message that names the full form to write; it returns "" for any other host. A single label
// ("intranet") is refused in every case. With no search domains the rest of the rule is static too: a Service
// name that ends in ".svc" or ".svc.cluster" ("gitea.ns.svc") is a search-path name in every cluster;
// CheckSandbox adds the names that only a lookup shows ("gitea.ns"). With a search list (the Substrate
// guest), the resolver tries a name with fewer dots than Ndots with each search domain first, so every such
// name is refused unless it is the full Service form (<service>.<namespace>.svc.<ClusterDomain>) or a
// public FQDN (its last label is in the public suffix list). A name with one trailing dot is absolute and
// accepted, except a single label. IP literals in any WHATWG form are not names. Case does not matter; more
// than one trailing dot is an empty label and is refused.
func (p URLPolicy) ShortName(host string) string {
	host = strings.ToLower(host)
	absolute := strings.HasSuffix(host, ".")
	host = strings.TrimSuffix(host, ".")
	switch {
	case host == "":
		return ""
	case strings.HasSuffix(host, "."):
		return "the name has an empty label; write the full name without extra dots"
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return ""
	}
	if _, numeric, _ := whatwgIPv4(host); numeric {
		return ""
	}
	if !strings.Contains(host, ".") {
		return p.fullNameHint(host)
	}
	rs := p.search()
	if len(rs.Domains) == 0 {
		if strings.HasSuffix(host, ".svc") || strings.HasSuffix(host, ".svc.cluster") {
			return p.fullNameHint(host)
		}
		return ""
	}
	if absolute {
		return ""
	}
	domain := strings.Trim(strings.ToLower(rs.ClusterDomain), ".")
	if domain != "" && strings.HasSuffix(host, ".svc."+domain) && strings.Count(strings.TrimSuffix(host, ".svc."+domain), ".") >= 1 {
		return ""
	}
	if strings.HasSuffix(host, ".svc") || strings.HasSuffix(host, ".svc.cluster") {
		return p.fullNameHint(host)
	}
	suffix, icann := publicsuffix.PublicSuffix(host)
	public := icann || strings.Contains(suffix, ".")
	// <service>.<namespace>: the resolver falls back to the search list after the absolute name fails,
	// at any ndots, so a search domain can turn it into a Service name.
	if !public && strings.Count(host, ".") == 1 && domain != "" {
		for _, d := range rs.Domains {
			d = strings.Trim(strings.ToLower(d), ".")
			if d == "svc."+domain || strings.HasSuffix(d, ".svc."+domain) {
				return p.fullNameHint(host)
			}
		}
	}
	if strings.Count(host, ".") >= rs.Ndots || public {
		return ""
	}
	return p.fullNameHint(host)
}

func (p URLPolicy) check(ctx context.Context, rawURL string, strict bool) error {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return invalid("url must be an http(s) URL")
	}
	if err := checkPort(u); err != nil {
		return err
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
	if name, hostport := endpoint(u); p.allowedName(name, hostport) {
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
		for _, suffix := range []string{"." + ns + ".svc", "." + ns + ".svc." + p.search().ClusterDomain, "." + ns + ".svc." + DefaultClusterDomain, "." + ns} {
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
