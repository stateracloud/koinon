// SPDX-License-Identifier: Apache-2.0

package catalog_test

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/catalog"
)

var _ = Describe("URLPolicy for sandbox egress (#1054)", func() {
	// podResolver models the resolver of a pod (ClusterFirst, search list below). A name with a trailing
	// dot is absolute: it resolves only if the zone has it. Any other name is tried as given and then
	// with each search domain, whatever its number of dots (ndots only changes the order).
	zone := map[string]bool{
		"git.example.com": true, "gitea.ns.svc.cluster.local": true, "a.b.c.d.example.org": true,
		"gitea.ns.svc.corp.example": true, "x.y.z.w.v.svc.cluster.local": true, "x.y.z.w.v.u.svc.cluster.local": true,
		"git.example.com.svc.cluster.local": false,
	}
	search := []string{"koinon.svc.cluster.local", "svc.cluster.local", "cluster.local"}
	resolver := func(_ context.Context, h string) ([]netip.Addr, error) {
		ok := netip.MustParseAddr("203.0.113.9")
		if abs, found := strings.CutSuffix(h, "."); found {
			if zone[abs] {
				return []netip.Addr{ok}, nil
			}
			return nil, errors.New("no such host")
		}
		if zone[h] {
			return []netip.Addr{ok}, nil
		}
		for _, d := range search {
			if zone[h+"."+d] {
				return []netip.Addr{ok}, nil
			}
		}
		// Every other name resolves in this model, as a public name would.
		if strings.Contains(h, ".") && !strings.HasSuffix(h, ".ns") && !strings.HasSuffix(h, ".v") && !strings.HasSuffix(h, ".u") {
			return []netip.Addr{ok}, nil
		}
		return nil, errors.New("no such host")
	}
	// The allow list names two short names: a connector still refuses them.
	p := catalog.URLPolicy{
		Namespace:    "koinon",
		AllowedHosts: []string{"intranet", "gitea.ns.svc:3000"},
		Resolver:     resolver,
	}
	checks := map[string]func(context.Context, string) error{
		"CheckSandbox":       p.CheckSandbox,
		"CheckSandboxStrict": p.CheckSandboxStrict,
	}

	DescribeTable("refuses a short name, whatever the spelling",
		func(ctx SpecContext, u, full string) {
			for name, check := range checks {
				err := check(ctx, u)
				var inv *catalog.InvalidError
				Expect(errors.As(err, &inv)).To(BeTrue(), name+" "+u)
				Expect(err.Error()).To(ContainSubstring("short name"), name+" "+u)
				Expect(err.Error()).To(ContainSubstring(full), name+" "+u)
			}
		},
		Entry("single label", "http://intranet", "intranet.<namespace>.svc.cluster.local"),
		Entry("single label with a port", "http://intranet:8080/x", "intranet.<namespace>.svc.cluster.local"),
		Entry("single label, two trailing dots", "https://intranet../", "empty label"),
		Entry("Service name, two trailing dots", "https://gitea.ns.svc..", "empty label"),
		Entry("single label, uppercase", "https://INTRANET", "intranet.<namespace>.svc.cluster.local"),
		Entry("single label, userinfo", "https://user:pw@intranet", "intranet.<namespace>.svc.cluster.local"),
		Entry("allow-listed single label", "http://intranet:80", "intranet.<namespace>.svc.cluster.local"),
		Entry("Service name with .svc", "https://gitea.ns.svc", "gitea.ns.svc.cluster.local"),
		Entry("Service name with .svc and port 443", "https://gitea.ns.svc:443", "gitea.ns.svc.cluster.local"),
		Entry("Service name, uppercase", "https://Gitea.NS.SVC", "gitea.ns.svc.cluster.local"),
		Entry("Service name, userinfo", "https://u@gitea.ns.svc", "gitea.ns.svc.cluster.local"),
		Entry("allow-listed Service name", "http://gitea.ns.svc:3000", "gitea.ns.svc.cluster.local"),
		Entry("Service name with .svc.cluster", "https://gitea.ns.svc.cluster", "gitea.ns.svc.cluster.local"),
		Entry("single label, one trailing dot", "http://intranet./", "intranet.<namespace>.svc.cluster.local"),
		Entry("Service name, one trailing dot", "https://gitea.ns.svc./", "gitea.ns.svc.cluster.local"),
		Entry("namespace-qualified name, one trailing dot", "https://gitea.ns./", "gitea.ns.svc.cluster.local"),
		Entry("namespace-qualified name, search domains expand it", "https://gitea.ns", "gitea.ns.svc.cluster.local"),
		Entry("namespace-qualified name with a port", "https://Gitea.NS:3000/x", "gitea.ns.svc.cluster.local"),
		Entry("four dots, below ndots 5", "https://x.y.z.w.v", "fully qualified name"),
		Entry("five dots, at ndots 5: the search fallback still expands it", "https://x.y.z.w.v.u", "fully qualified name"),
	)

	DescribeTable("passes a full name, a public name or an IP literal",
		func(ctx SpecContext, u string) {
			for name, check := range checks {
				Expect(check(ctx, u)).To(Succeed(), name+" "+u)
			}
		},
		Entry("full cluster name", "https://gitea.ns.svc.cluster.local"),
		Entry("full cluster name with port", "http://gitea.ns.svc.cluster.local:3000"),
		Entry("trailing dot", "https://git.example.com./"),
		Entry("absolute namespace-qualified name", "https://gitea.ns.svc.cluster.local./"),
		Entry("five dots that resolve as given", "https://a.b.c.d.example.org"),
		Entry("uppercase", "https://GIT.Example.COM"),
		Entry("userinfo on a dotted name", "https://intranet@git.example.com"),
		Entry("port", "https://git.example.com:8443"),
		Entry("public IPv4 literal", "https://203.0.113.9:3000"),
		Entry("public IPv6 literal", "https://[2001:db8::1]:3000"),
		Entry("decimal IPv4 form", "https://3405803785"),
	)

	It("still applies the platform rules after the short-name rule", func(ctx SpecContext) {
		for name, check := range checks {
			Expect(check(ctx, "https://127.0.0.1")).To(HaveOccurred(), name)
			Expect(check(ctx, "https://x.koinon.svc.cluster.local")).To(HaveOccurred(), name)
		}
	})

	It("refuses in strict mode when the absolute lookup fails after the platform check resolved the name", func(ctx SpecContext) {
		calls := 0
		flaky := catalog.URLPolicy{Resolver: func(_ context.Context, h string) ([]netip.Addr, error) {
			calls++
			if calls == 1 {
				return []netip.Addr{netip.MustParseAddr("203.0.113.9")}, nil
			}
			return nil, errors.New("SERVFAIL")
		}}
		Expect(flaky.CheckSandboxStrict(ctx, "https://git.example.com")).To(MatchError(ContainSubstring("does not resolve as an absolute name")))
	})

	It("applies the address rules to the addresses of the absolute lookup", func(ctx SpecContext) {
		split := catalog.URLPolicy{Resolver: func(_ context.Context, h string) ([]netip.Addr, error) {
			if strings.HasSuffix(h, ".") { // what the sandbox resolves
				return []netip.Addr{netip.MustParseAddr("10.1.2.3")}, nil
			}
			return []netip.Addr{netip.MustParseAddr("203.0.113.9")}, nil
		}}
		Expect(split.Check(ctx, "https://git.example.com")).To(Succeed())
		Expect(split.CheckSandbox(ctx, "https://git.example.com")).To(MatchError(ContainSubstring("private")))
	})

	It("keeps Check unchanged for a model provider: a short name that the platform allows passes", func(ctx SpecContext) {
		Expect(p.Check(ctx, "http://intranet")).To(Succeed())
		Expect(p.Check(ctx, "http://gitea.ns.svc:3000")).To(Succeed())
		Expect(p.Check(ctx, "http://gitea.ns")).To(Succeed())
	})

	It("takes the cluster domain from the parameter", func(ctx SpecContext) {
		custom := p
		custom.Search = &catalog.ResolverSearch{Domains: catalog.SubstrateSearch.Domains, Ndots: 5, ClusterDomain: "corp.example"}
		Expect(custom.CheckSandbox(ctx, "https://gitea.ns.svc.corp.example")).To(Succeed())
		Expect(custom.CheckSandbox(ctx, "https://gitea.ns.svc.cluster.local")).To(HaveOccurred())
		Expect(p.CheckSandbox(ctx, "https://gitea.ns.svc.corp.example")).To(MatchError(ContainSubstring("short name")))
	})

	It("reads the search list and ndots from the parameter", func() {
		none := catalog.URLPolicy{Search: &catalog.ResolverSearch{}}
		one := catalog.URLPolicy{Search: &catalog.ResolverSearch{Domains: []string{"cluster.local"}, Ndots: 1, ClusterDomain: "cluster.local"}}
		for host, want := range map[string][3]bool{ // default, no search domains, ndots 1
			"gitea.ns":      {true, false, false},
			"gitea.ns.svc":  {true, true, true},
			"gitea":         {true, true, true},
			"gitea.ns.":     {false, false, false},
			"a.b.c.d.e.f":   {false, false, false},
			"a.b.c.corp":    {true, false, false},
			"gitea.ns.corp": {true, false, false},
		} {
			Expect(p.ShortName(host) != "").To(Equal(want[0]), "default "+host)
			Expect(none.ShortName(host) != "").To(Equal(want[1]), "no search domains "+host)
			Expect(one.ShortName(host) != "").To(Equal(want[2]), "ndots 1 "+host)
		}
	})

	It("refuses <service>.<namespace> at any ndots when a search domain makes it a Service name", func() {
		low := catalog.URLPolicy{Search: &catalog.ResolverSearch{Domains: catalog.SubstrateSearch.Domains, Ndots: 1, ClusterDomain: "cluster.local"}}
		Expect(low.ShortName("gitea.ns")).NotTo(BeEmpty())
		Expect(low.ShortName("kubernetes.default")).NotTo(BeEmpty())
		Expect(low.ShortName("git.example.com")).To(BeEmpty())
		Expect(low.ShortName("git.corp.local")).To(BeEmpty()) // three labels: not a Service name
	})

	DescribeTable("ShortName",
		func(host string, short bool) {
			Expect(p.ShortName(host) != "").To(Equal(short), host)
		},
		Entry("empty", "", false),
		Entry("label", "gitea", true),
		Entry("label, uppercase", "GITEA", true),
		Entry("label with one trailing dot", "gitea.", true),
		Entry("two trailing dots are an empty label", "gitea..", true),
		Entry("name.ns.svc", "gitea.ns.svc", true),
		Entry("full", "gitea.ns.svc.cluster.local", false),
		Entry("IPv4", "10.0.0.1", false),
		Entry("short IPv4 form", "127.1", false),
		Entry("IPv6", "::1", false),
		Entry("public name", "git.example.com", false),
		Entry("two labels, not a public suffix", "gitea.ns", true),
		Entry("two labels, public suffix", "gitea.dev", false),
		Entry("three labels, not a public suffix", "git.corp.local", true),
		Entry("a private-section suffix", "foo.github.io", false),
		Entry("four dots, not a public suffix", "a.b.c.d.local", true),
		Entry("five dots, at ndots", "a.b.c.d.e.local", false),
		Entry("one trailing dot is absolute", "gitea.ns.", false),
		Entry("full Service name for another cluster domain", "gitea.ns.svc.corp.example", true),
	)

	It("SandboxDialContext refuses a short name before it dials, and passes a full name on", func(ctx SpecContext) {
		dial := p.SandboxDialContext(&net.Dialer{})
		for _, addr := range []string{"intranet:80", "gitea.ns.svc:443", "intranet..:443", "gitea.ns:443", "x.y.z.w.v.u:443"} {
			_, err := dial(ctx, "tcp", addr)
			var inv *catalog.InvalidError
			Expect(errors.As(err, &inv)).To(BeTrue(), addr)
		}
		By("looking the host up once, as an absolute name")
		var looked []string
		spy := catalog.URLPolicy{Resolver: func(_ context.Context, h string) ([]netip.Addr, error) {
			looked = append(looked, h)
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		}}
		_, _ = spy.SandboxDialContext(&net.Dialer{})(ctx, "tcp", "git.example.com:443")
		Expect(looked).To(Equal([]string{"git.example.com."}))
		By("refusing a name that only the search domains resolve")
		_, err := p.SandboxDialContext(&net.Dialer{})(ctx, "tcp", "gitea.dev:443")
		Expect(err).To(MatchError(ContainSubstring("absolute name")))
		// A full name reaches the policy dial: this one resolves to a denied address.
		deny := catalog.URLPolicy{Resolver: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		}}
		_, err = deny.SandboxDialContext(&net.Dialer{})(ctx, "tcp", "gitea.ns.svc.cluster.local:443")
		Expect(err).To(MatchError(ContainSubstring("egress policy denies")))
	})
})
