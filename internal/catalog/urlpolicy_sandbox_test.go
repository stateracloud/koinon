// SPDX-License-Identifier: Apache-2.0

package catalog_test

import (
	"context"
	"errors"
	"net"
	"net/netip"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/catalog"
)

var _ = Describe("URLPolicy for sandbox egress (#1054)", func() {
	resolver := func(_ context.Context, _ string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("203.0.113.9")}, nil
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
		Entry("single label, trailing dot", "https://intranet./", "intranet.<namespace>.svc.cluster.local"),
		Entry("single label, uppercase", "https://INTRANET", "intranet.<namespace>.svc.cluster.local"),
		Entry("single label, userinfo", "https://user:pw@intranet", "intranet.<namespace>.svc.cluster.local"),
		Entry("allow-listed single label", "http://intranet:80", "intranet.<namespace>.svc.cluster.local"),
		Entry("Service name with .svc", "https://gitea.ns.svc", "gitea.ns.svc.cluster.local"),
		Entry("Service name with .svc and port 443", "https://gitea.ns.svc:443", "gitea.ns.svc.cluster.local"),
		Entry("Service name, trailing dot", "https://gitea.ns.svc.", "gitea.ns.svc.cluster.local"),
		Entry("Service name, uppercase", "https://Gitea.NS.SVC", "gitea.ns.svc.cluster.local"),
		Entry("Service name, userinfo", "https://u@gitea.ns.svc", "gitea.ns.svc.cluster.local"),
		Entry("allow-listed Service name", "http://gitea.ns.svc:3000", "gitea.ns.svc.cluster.local"),
		Entry("Service name with .svc.cluster", "https://gitea.ns.svc.cluster", "gitea.ns.svc.cluster.local"),
	)

	DescribeTable("passes a full name, a public name or an IP literal",
		func(ctx SpecContext, u string) {
			for name, check := range checks {
				Expect(check(ctx, u)).To(Succeed(), name+" "+u)
			}
		},
		Entry("full cluster name", "https://gitea.ns.svc.cluster.local"),
		Entry("full cluster name with port", "http://gitea.ns.svc.cluster.local:3000"),
		Entry("full name with a custom cluster domain", "https://gitea.ns.svc.corp.example"),
		Entry("trailing dot", "https://git.example.com./"),
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

	It("keeps Check unchanged for a model provider: a short name that the platform allows passes", func(ctx SpecContext) {
		Expect(p.Check(ctx, "http://intranet")).To(Succeed())
		Expect(p.Check(ctx, "http://gitea.ns.svc:3000")).To(Succeed())
	})

	DescribeTable("ShortName",
		func(host string, short bool) {
			Expect(catalog.ShortName(host) != "").To(Equal(short), host)
		},
		Entry("empty", "", false),
		Entry("label", "gitea", true),
		Entry("label, uppercase, trailing dot", "GITEA.", true),
		Entry("name.ns.svc", "gitea.ns.svc", true),
		Entry("full", "gitea.ns.svc.cluster.local", false),
		Entry("IPv4", "10.0.0.1", false),
		Entry("short IPv4 form", "127.1", false),
		Entry("IPv6", "::1", false),
		Entry("public name", "git.example.com", false),
	)

	It("SandboxDialContext refuses a short name before it dials, and passes a full name on", func(ctx SpecContext) {
		dial := p.SandboxDialContext(&net.Dialer{})
		for _, addr := range []string{"intranet:80", "gitea.ns.svc:443", "INTRANET.:80"} {
			_, err := dial(ctx, "tcp", addr)
			var inv *catalog.InvalidError
			Expect(errors.As(err, &inv)).To(BeTrue(), addr)
		}
		// A full name reaches the policy dial: this one resolves to a denied address.
		deny := catalog.URLPolicy{Resolver: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		}}
		_, err := deny.SandboxDialContext(&net.Dialer{})(ctx, "tcp", "gitea.ns.svc.cluster.local:443")
		Expect(err).To(MatchError(ContainSubstring("egress policy denies")))
	})
})
