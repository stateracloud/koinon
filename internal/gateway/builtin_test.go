// SPDX-License-Identifier: Apache-2.0

package gateway_test

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/gateway"
)

var _ = Describe("a built-in MCP target", func() {
	hash := strings.Repeat("a", 64)
	ok := func() gateway.MCPTarget {
		return gateway.MCPTarget{
			Name: "acme-agents", Org: "acme", Builtin: gateway.BuiltinA2A, HandleKeyFile: "/etc/koinon/mcp/a2a-key",
			RemoteAgents: []gateway.MCPRemoteAgent{{
				Name: "kagent", ID: "ra-1", CardURL: "https://agents.example.com", CardHash: hash,
				CredentialHeader: "Authorization", CredentialPrefix: "Bearer ", CredentialFile: "/etc/koinon/mcp/ra-1", Agents: []string{"scout"},
			}},
		}
	}
	state := func(t gateway.MCPTarget) gateway.State {
		return gateway.State{MCPState: gateway.MCPState{MCP: []gateway.MCPTarget{t}}}
	}

	It("accepts a target with no URL", func() {
		Expect(state(ok()).Validate()).To(Succeed())
	})

	DescribeTable("refuses",
		func(mut func(*gateway.MCPTarget), want string) {
			t := ok()
			mut(&t)
			Expect(state(t).Validate()).To(MatchError(ContainSubstring(want)))
		},
		Entry("an unknown kind", func(t *gateway.MCPTarget) { t.Builtin = "other" }, "built-in"),
		Entry("a URL on the target", func(t *gateway.MCPTarget) { t.URL = "https://x.example.com" }, "URL"),
		Entry("no org", func(t *gateway.MCPTarget) { t.Org = "" }, "org"),
		Entry("a bad card hash", func(t *gateway.MCPTarget) { t.RemoteAgents[0].CardHash = "abc" }, "hash"),
		Entry("a card URL that is not http", func(t *gateway.MCPTarget) { t.RemoteAgents[0].CardURL = "ftp://x" }, "URL"),
		Entry("a credential header without a file", func(t *gateway.MCPTarget) { t.RemoteAgents[0].CredentialFile = "" }, "credential"),
		Entry("a long remote agent name", func(t *gateway.MCPTarget) { t.RemoteAgents[0].Name = strings.Repeat("a", 25) }, "name"),
		Entry("a duplicate remote agent name", func(t *gateway.MCPTarget) { t.RemoteAgents = append(t.RemoteAgents, t.RemoteAgents[0]) }, "duplicate"),
		Entry("a CA file with insecureSkipVerify", func(t *gateway.MCPTarget) {
			t.RemoteAgents[0].CAFile, t.RemoteAgents[0].InsecureSkipVerify = "/etc/ca.pem", true
		}, "CA"),
		Entry("a handle key path with a bad character", func(t *gateway.MCPTarget) { t.HandleKeyFile = "/etc/koinon/key file" }, "key"),
	)

	It("refuses remote agents on an ordinary target", func() {
		t := gateway.MCPTarget{Name: "acme-x", URL: "https://x.example.com/mcp", Org: "acme", RemoteAgents: ok().RemoteAgents}
		Expect(state(t).Validate()).NotTo(Succeed())
	})

	It("turns a remote agent into the target the client dials", func() {
		t := ok().RemoteAgents[0].Target("acme")
		Expect(t.URL).To(Equal("https://agents.example.com"))
		Expect(t.Org).To(Equal("acme"))
		Expect(t.CredentialFile).To(Equal("/etc/koinon/mcp/ra-1"))
	})

	It("is dropped, with its grants and one rejection, by a driver that cannot run it", func() {
		plain := gateway.MCPTarget{Name: "acme-x", URL: "https://x.example.com/mcp", Org: "acme"}
		s := gateway.MCPState{
			MCP: []gateway.MCPTarget{ok(), plain},
			Tools: map[gateway.AgentRef][]gateway.ToolGrant{agent: {
				{Target: "acme-agents", Tools: []string{"send_task"}}, {Target: "acme-x", Tools: []string{"t"}},
			}},
		}
		out, rej := s.WithoutBuiltin()
		Expect(out.MCP).To(HaveLen(1))
		Expect(out.MCP[0].Name).To(Equal("acme-x"))
		Expect(out.Tools[agent]).To(ConsistOf(gateway.ToolGrant{Target: "acme-x", Tools: []string{"t"}}))
		Expect(rej).To(HaveLen(1))
		Expect(rej[0].Org).To(Equal("acme"))
		Expect(rej[0].Name).To(Equal("acme-agents"))
	})

	It("compares by value", func() {
		Expect(ok().Same(ok())).To(BeTrue())
		b := ok()
		b.RemoteAgents[0].CardHash = strings.Repeat("b", 64)
		Expect(ok().Same(b)).To(BeFalse())
	})
})
