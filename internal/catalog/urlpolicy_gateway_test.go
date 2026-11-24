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

var _ = Describe("URLPolicy for the gateway's egress routes (#1086)", func() {
	// The operator allowed the whole cluster range, so only the name rules keep Koinon's Services out.
	var looked []string
	pol := func() catalog.URLPolicy {
		looked = nil
		return catalog.URLPolicy{
			Namespace: "koinon", AllowedHosts: []string{"10.96.0.0/12"},
			Resolver: func(_ context.Context, h string) ([]netip.Addr, error) {
				looked = append(looked, h)
				return []netip.Addr{netip.MustParseAddr("10.96.0.10")}, nil
			},
		}
	}

	DescribeTable("refuses by name, before any lookup",
		func(ctx SpecContext, addr string) {
			p := pol()
			_, err := p.GatewayDialContext(&net.Dialer{Timeout: 1})(ctx, "tcp", addr)
			var inv *catalog.InvalidError
			Expect(errors.As(err, &inv)).To(BeTrue(), "%v", err)
			Expect(looked).To(BeEmpty())
		},
		Entry("a Service in Koinon's namespace, short", "koinon-api.koinon:443"),
		Entry("a Service in Koinon's namespace, .svc", "koinon-api.koinon.svc:443"),
		Entry("a Service in Koinon's namespace, in full", "koinon-api.koinon.svc.cluster.local:443"),
		Entry("the same with a trailing dot and upper case", "Koinon-API.Koinon.svc.cluster.local.:443"),
		Entry("a single label", "koinon-api:443"),
		Entry("a .svc short name", "gitea.dev.svc:443"),
		Entry("localhost", "localhost:443"),
		Entry("a name under localhost", "x.localhost:443"),
		Entry("a numeric host that resolvers read in different ways", "0x7f.1:443"),
	)

	It("looks a name up once, as an absolute name, so no search domain expands it", func(ctx SpecContext) {
		p := pol()
		_, _ = p.GatewayDialContext(&net.Dialer{Timeout: 1})(ctx, "tcp", "git.corp.example:443")
		Expect(looked).To(Equal([]string{"git.corp.example."}))
	})

	It("still applies the address rules to a name outside the allowed hosts", func(ctx SpecContext) {
		p := catalog.URLPolicy{Namespace: "koinon", Resolver: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
		}}
		_, err := p.GatewayDialContext(&net.Dialer{Timeout: 1})(ctx, "tcp", "git.example.com:443")
		Expect(err).To(MatchError(ContainSubstring("egress policy denies")))
	})
})
