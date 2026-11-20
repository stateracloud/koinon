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
		"gitea.ns.svc.corp.example": true, "1.2.3.4.example.com": true, "x.y.z.w.v.svc.cluster.local": true, "x.y.z.w.v.u.svc.cluster.local": true,
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
		Entry("IPv4 with the default port written", "https://203.0.113.9:443"),
		Entry("a name that starts with a number", "https://1.2.3.4.example.com"),
		Entry("a name with a number inside", "https://git.example.com:08443"),
	)

	DescribeTable("refuses a numeric host that a resolver could read as another address (numeric hosts, #1054)",
		func(ctx SpecContext, u string) {
			for name, check := range checks {
				err := check(ctx, u)
				var inv *catalog.InvalidError
				Expect(errors.As(err, &inv)).To(BeTrue(), name+" "+u)
				Expect(err.Error()).To(ContainSubstring("not a plain IP address or a name"), name+" "+u)
			}
		},
		Entry("decimal IPv4 form", "https://3405803785"),
		Entry("hex, one number", "https://0xcb007109"),
		Entry("hex, two numbers", "https://0x7f.1"),
		Entry("hex, four numbers", "https://0x7f.0x0.0x0.0x1"),
		Entry("octal", "https://0177.0.0.1"),
		Entry("leading zero in a decimal-looking part", "https://010.0.0.1"),
		Entry("short form", "https://127.1"),
		Entry("three parts", "https://10.1.2"),
		Entry("trailing dot after four numbers", "https://10.0.0.1./"),
		Entry("trailing dot after a hex form", "https://0x7f.1./"),
		Entry("two trailing dots after a number", "https://10.0.0.1../"),
		Entry("public address in a short form", "https://203.0.113"),
		Entry("uppercase hex", "https://0X7F.1"),
		Entry("number with a port", "https://3405803785:8443"),
		Entry("number with userinfo", "https://u@0x7f.1"),
		Entry("IPv6 with a zone", "https://[fe80::1%25eth0]/"),
	)

	It("refuses a numeric host in the dial too", func(ctx SpecContext) {
		dial := p.SandboxDialContext(&net.Dialer{})
		for _, addr := range []string{"3405803785:443", "0x7f.1:443", "10.0.0.1.:443", "0177.0.0.1:443", "127.1:80"} {
			_, err := dial(ctx, "tcp", addr)
			Expect(err).To(MatchError(ContainSubstring("not a plain IP address or a name")), addr)
		}
	})

	It("reads the port once: it never changes the host decision (ports, #1054)", func(ctx SpecContext) {
		for _, port := range []string{"", ":80", ":443", ":3000", ":8443", ":0443"} {
			for name, check := range checks {
				for _, host := range []string{"intranet", "gitea.ns", "gitea.ns.svc", "x.y.z.w.v", "0x7f.1", "10.0.0.1."} {
					Expect(check(ctx, "https://"+host+port)).To(HaveOccurred(), name+" "+host+port)
				}
				for _, host := range []string{"git.example.com", "gitea.ns.svc.cluster.local", "203.0.113.9", "[2001:db8::1]"} {
					Expect(check(ctx, "https://"+host+port)).To(Succeed(), name+" "+host+port)
				}
			}
		}
	})

	DescribeTable("refuses a port that no connection can use",
		func(ctx SpecContext, u string) {
			for name, check := range checks {
				Expect(check(ctx, u)).To(MatchError(ContainSubstring("port number from 1 to 65535")), name+" "+u)
			}
			Expect(p.Check(ctx, u)).To(MatchError(ContainSubstring("port number from 1 to 65535")), u)
		},
		Entry("zero", "https://git.example.com:0"),
		Entry("too large", "https://git.example.com:65536"),
		Entry("far too large", "https://git.example.com:99999999999999999999"),
	)

	Describe("the default port in the allow list", func() {
		// 10.5.0.9 is private: only an allow-list entry lets it through.
		allow := func(entries ...string) catalog.URLPolicy {
			return catalog.URLPolicy{AllowedHosts: entries, Resolver: func(context.Context, string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr("10.5.0.9")}, nil
			}}
		}
		DescribeTable("host:port entries",
			func(ctx SpecContext, entry, u string, ok bool) {
				pol := allow(entry)
				for name, check := range map[string]func(context.Context, string) error{
					"Check": pol.Check, "CheckStrict": pol.CheckStrict, "CheckSandbox": pol.CheckSandbox, "CheckSandboxStrict": pol.CheckSandboxStrict,
				} {
					if ok {
						Expect(check(ctx, u)).To(Succeed(), name+" "+entry+" "+u)
					} else {
						Expect(check(ctx, u)).To(HaveOccurred(), name+" "+entry+" "+u)
					}
				}
			},
			Entry("https default port matches the explicit entry", "git.example.com:443", "https://git.example.com/x", true),
			Entry("https written port matches", "git.example.com:443", "https://git.example.com:443/x", true),
			Entry("http default port matches", "git.example.com:80", "http://git.example.com", true),
			Entry("a leading zero is the same port", "git.example.com:443", "https://git.example.com:0443", true),
			Entry("trailing dot and upper case", "git.example.com:443", "https://GIT.example.com.:443", true),
			Entry("another port does not match", "git.example.com:443", "https://git.example.com:8443", false),
			Entry("the http default is not the https entry", "git.example.com:443", "http://git.example.com", false),
			Entry("an entry without a port allows every port", "git.example.com", "https://git.example.com:8443", true),
			Entry("an entry without a port allows the default port", "git.example.com", "https://git.example.com", true),
			Entry("an IPv6 entry with a port", "[fd00::1]:8443", "https://[fd00::1]:8443", true),
		)

		It("a port never turns a refused short name into an accepted one", func(ctx SpecContext) {
			pol := allow("gitea.ns.svc:443", "gitea.ns.svc", "intranet:80", "intranet", "gitea.ns:443", "gitea.ns")
			for _, u := range []string{"https://gitea.ns.svc", "https://gitea.ns.svc:443", "http://intranet", "http://intranet:80", "https://gitea.ns", "https://gitea.ns:443"} {
				Expect(pol.CheckSandbox(ctx, u)).To(MatchError(ContainSubstring("short name")), u)
				Expect(pol.CheckSandboxStrict(ctx, u)).To(MatchError(ContainSubstring("short name")), u)
			}
		})

		It("the dial and the check agree on an entry with the default port", func(ctx SpecContext) {
			pol := allow("git.example.com:443")
			Expect(pol.CheckSandbox(ctx, "https://git.example.com")).To(Succeed())
			// The check let the private address through by the entry; the dial must not refuse it as denied.
			_, err := pol.SandboxDialContext(&net.Dialer{Timeout: 1})(ctx, "tcp", "git.example.com:443")
			Expect(err).NotTo(MatchError(ContainSubstring("egress policy denies")))
		})
	})

	Describe("the cluster domain", func() {
		It("SubstrateSearchFor builds the worker pod's search list from the domain", func() {
			Expect(catalog.SubstrateSearchFor("cluster.local")).To(Equal(catalog.SubstrateSearch))
			Expect(catalog.SubstrateSearchFor("")).To(Equal(catalog.SubstrateSearch))
			Expect(catalog.SubstrateSearchFor("corp.example")).To(Equal(catalog.ResolverSearch{
				Domains:       []string{"ate-workers.svc.corp.example", "svc.corp.example", "corp.example"},
				Ndots:         5,
				ClusterDomain: "corp.example",
			}))
		})

		It("a policy built for another domain accepts that domain's full name only", func(ctx SpecContext) {
			s := catalog.SubstrateSearchFor("corp.example")
			pol := catalog.URLPolicy{Search: &s, Resolver: resolver}
			Expect(pol.CheckSandbox(ctx, "https://gitea.ns.svc.corp.example")).To(Succeed())
			Expect(pol.CheckSandbox(ctx, "https://gitea.ns.svc.cluster.local")).To(MatchError(ContainSubstring("short name")))
			Expect(pol.CheckSandbox(ctx, "https://gitea.ns.svc")).To(MatchError(ContainSubstring("gitea.ns.svc.corp.example")))
		})

		DescribeTable("ValidateClusterDomain",
			func(domain string, ok bool) {
				if ok {
					Expect(catalog.ValidateClusterDomain(domain)).To(Succeed(), domain)
				} else {
					Expect(catalog.ValidateClusterDomain(domain)).To(HaveOccurred(), domain)
				}
			},
			Entry("empty means the default", "", true),
			Entry("default", "cluster.local", true),
			Entry("another domain", "corp-1.example", true),
			Entry("one label", "local", true),
			Entry("trailing dot", "cluster.local.", false),
			Entry("empty label", "cluster..local", false),
			Entry("upper case", "Cluster.Local", false),
			Entry("underscore", "cluster_x.local", false),
			Entry("leading hyphen", "-a.local", false),
			Entry("space", "a b.local", false),
			Entry("label of 64 characters", strings.Repeat("a", 64)+".local", false),
		)
	})

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
