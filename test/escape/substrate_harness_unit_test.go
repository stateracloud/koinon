// SPDX-License-Identifier: Apache-2.0

package escape_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/execution/substrate"
)

// The parsing of the gateway routes input of the Substrate harness (issue #1508). It needs no cluster.
var _ = Describe("Substrate harness gateway routes", Label("unit"), func() {
	It("parses a list of host:port routes", func() {
		routes, err := parseGatewayRoutes(" koinon-gateway.koinon-system.svc:8080, koinon-gateway.koinon-system.svc:8081 ,,")
		Expect(err).NotTo(HaveOccurred())
		Expect(routes).To(Equal([]substrate.GatewayRoute{
			{Host: "koinon-gateway.koinon-system.svc", Port: 8080},
			{Host: "koinon-gateway.koinon-system.svc", Port: 8081},
		}))
	})

	DescribeTable("refuses a bad input",
		func(raw, want string) {
			_, err := parseGatewayRoutes(raw)
			Expect(err).To(MatchError(ContainSubstring(want)))
		},
		Entry("empty", "", "no gateway route"),
		Entry("only separators", " , ,", "no gateway route"),
		Entry("no port", "gateway.svc", "want host:port"),
		Entry("port is not a number", "gateway.svc:http", "not a number"),
		Entry("port is zero", "gateway.svc:0", "port must be 1-65535"),
		Entry("port is too large", "gateway.svc:70000", "port must be 1-65535"),
		Entry("IP address", "10.0.0.1:8080", "IP address"),
		Entry("upper case", "Gateway.svc:8080", "lowercase"),
	)

	It("names the missing gateway input of a driver run", func() {
		Expect(missingSubstrateEnv(func(string) string { return "x" })).To(BeEmpty())
		get := func(n string) string {
			if n == "KOINON_ESCAPE_SUBSTRATE_GATEWAY_ROUTES" {
				return ""
			}
			return "x"
		}
		Expect(missingSubstrateEnv(get)).To(Equal([]string{"KOINON_ESCAPE_SUBSTRATE_GATEWAY_ROUTES"}))
	})
})
