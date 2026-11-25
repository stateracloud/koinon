// SPDX-License-Identifier: Apache-2.0

package gateway_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/gateway"
)

// Issue #1315: an ask target is Koinon's mcpask proxy for the ask-tier tools of one org's server. It
// gets the agent header; an org's own server never does (ADR 0097 decision 5).
var _ = Describe("an ask MCP target", func() {
	ok := func() gateway.MCPTarget {
		return gateway.MCPTarget{Name: "acme-wiki-ask", URL: "http://koinon-api:8080/mcp-ask/wiki", Org: "acme", Ask: true}
	}
	state := func(t gateway.MCPTarget) gateway.State {
		return gateway.State{MCPState: gateway.MCPState{MCP: []gateway.MCPTarget{t}}}
	}

	It("is valid with an org and a URL", func() {
		Expect(state(ok()).Validate()).To(Succeed())
	})

	DescribeTable("refuses",
		func(mut func(*gateway.MCPTarget), want string) {
			t := ok()
			mut(&t)
			Expect(state(t).Validate()).To(MatchError(ContainSubstring(want)))
		},
		Entry("no org", func(t *gateway.MCPTarget) { t.Org = "" }, "belongs to an org"),
		Entry("no URL", func(t *gateway.MCPTarget) { t.URL = "" }, "URL"),
		Entry("a credential", func(t *gateway.MCPTarget) {
			t.CredentialHeader, t.CredentialFile = "X-Api-Key", "/etc/koinon/mcp/wiki"
		}, "no credential"),
		Entry("a CA file", func(t *gateway.MCPTarget) { t.URL, t.CAFile = "https://koinon-api/mcp-ask/wiki", "/etc/koinon/mcp/ca" }, "no credential"),
		Entry("a built-in kind", func(t *gateway.MCPTarget) { t.URL, t.Builtin = "", gateway.BuiltinA2A }, "built-in"),
	)

	It("trusts the agent header for platform and ask targets only", func() {
		Expect(gateway.MCPTarget{Name: "koinon-tools", URL: "http://tools/mcp"}.TrustsAgentHeader()).To(BeTrue())
		Expect(ok().TrustsAgentHeader()).To(BeTrue())
		Expect(gateway.MCPTarget{Name: "acme-wiki", URL: "https://wiki.example.com/mcp", Org: "acme"}.TrustsAgentHeader()).To(BeFalse())
	})
})
