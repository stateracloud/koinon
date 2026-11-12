// SPDX-License-Identifier: Apache-2.0

package ratelimit

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// maxHops bounds how many hops of a forwarding header are read from the right. A header longer than
// this that is still all trusted proxies is a loop or an attack; the hop reached is used.
const maxHops = 32

// TrustedProxies is the set of proxy networks whose forwarding headers are believed. The zero value
// and nil trust nothing: the client address is the connection's peer, as when no list is configured.
type TrustedProxies struct {
	nets []netip.Prefix
}

// ParseTrustedProxies parses CIDRs or bare addresses (a bare address is a single-host network).
// Blank entries are skipped; an entry that is neither a CIDR nor an address is an error, so a typo
// fails the start-up instead of silently trusting nothing (or, with a wrong mask, too much).
func ParseTrustedProxies(entries []string) (*TrustedProxies, error) {
	t := &TrustedProxies{}
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		p, err := netip.ParsePrefix(e)
		if err != nil {
			a, aerr := netip.ParseAddr(e)
			if aerr != nil {
				return nil, fmt.Errorf("trusted proxy %q is neither a CIDR nor an IP address", e)
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		a := p.Addr().WithZone("")
		if a.Is4In6() { // ::ffff:10.0.0.0/104 names the IPv4 network 10.0.0.0/8
			if p.Bits() < 96 {
				return nil, fmt.Errorf("trusted proxy %q: an IPv4-mapped network needs a prefix of at least 96 bits", e)
			}
			p = netip.PrefixFrom(a.Unmap(), p.Bits()-96)
		} else {
			p = netip.PrefixFrom(a, p.Bits())
		}
		t.nets = append(t.nets, p.Masked())
	}
	return t, nil
}

// Enabled reports whether any proxy is trusted.
func (t *TrustedProxies) Enabled() bool { return t != nil && len(t.nets) > 0 }

func (t *TrustedProxies) trusts(a netip.Addr) bool {
	a = a.WithZone("").Unmap()
	for _, p := range t.nets {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// Resolve is the client address of r, as an address without port: the connection's peer, unless the
// peer is a trusted proxy. Then the forwarding header is walked from the right, and the first hop that
// is not itself a trusted proxy is the client (the hops to its left are written by the client or by
// proxies it passed through, and are not believed). The walk stops at the nearest hop it can read: a
// malformed or unknown hop ends it with the hop to its right, never with a value the client typed.
// Both X-Forwarded-For and Forwarded are read; when both are present and disagree the peer is used,
// since a proxy that sets only one of them leaves the other to the client.
func (t *TrustedProxies) Resolve(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, perr := netip.ParseAddr(host)
	if perr != nil || !t.Enabled() || !t.trusts(peer) {
		return host
	}
	xff, hasXFF := r.Header["X-Forwarded-For"]
	fwd, hasFwd := r.Header["Forwarded"]
	var cx, cf netip.Addr
	if hasXFF {
		cx = t.walk(peer, xffHops(xff))
	}
	if hasFwd {
		cf = t.walk(peer, forwardedHops(fwd))
	}
	switch {
	case hasXFF && hasFwd && cx != cf:
		return peer.String()
	case hasXFF:
		return cx.String()
	case hasFwd:
		return cf.String()
	}
	return peer.String()
}

// walk goes right to left over hops starting from the trusted peer, and returns the first untrusted
// address, or the last readable one.
func (t *TrustedProxies) walk(peer netip.Addr, hops []string) netip.Addr {
	cur := peer
	for i := len(hops) - 1; i >= 0 && len(hops)-1-i < maxHops; i-- {
		a, ok := parseHop(hops[i])
		if !ok {
			return cur
		}
		cur = a
		if !t.trusts(a) {
			return a
		}
	}
	return cur
}

// xffHops splits the X-Forwarded-For header lines into hops, in order. Several header lines are one
// list, as RFC 9110 says.
func xffHops(lines []string) []string {
	var hops []string
	for _, l := range lines {
		hops = append(hops, strings.Split(l, ",")...)
	}
	return hops
}

// forwardedHops is the "for" parameter of each element of the Forwarded header lines (RFC 7239), in
// order. An element without one yields "", which parseHop refuses, so the walk stops there.
func forwardedHops(lines []string) []string {
	var hops []string
	for _, l := range lines {
		for _, elem := range strings.Split(l, ",") {
			v := ""
			for _, pair := range strings.Split(elem, ";") {
				k, val, ok := strings.Cut(strings.TrimSpace(pair), "=")
				if ok && strings.EqualFold(k, "for") {
					v = strings.Trim(val, `"`)
					break
				}
			}
			hops = append(hops, v)
		}
	}
	return hops
}

// parseHop reads one hop: an IPv4 or IPv6 address, with an optional port and, for IPv6 in Forwarded,
// brackets. Anything else (a name, "unknown", an obfuscated "_x", an empty string) is not an address.
func parseHop(s string) (netip.Addr, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return netip.Addr{}, false
	}
	if h, _, err := net.SplitHostPort(s); err == nil {
		s = h
	}
	s = strings.TrimSuffix(strings.TrimPrefix(s, "["), "]")
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, false
	}
	return a.WithZone(""), true
}

type resolvedKey struct{}

// Middleware resolves the client address once per request and carries it in the request context, where
// ClientAddr finds it. With no trusted proxy it returns next unchanged, so the headers are never read.
func (t *TrustedProxies) Middleware(next http.Handler) http.Handler {
	if !t.Enabled() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), resolvedKey{}, t.Resolve(r))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
