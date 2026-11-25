// SPDX-License-Identifier: Apache-2.0

package reconciler_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/reconciler"
)

// The sandbox profile with each MCP driver (ADR 0097 decision 5, #1239; ADR 0120: every inspected route
// is enforced at L7, and nothing else of a gateway is open).
var _ = Describe("GatewayProfile", func() {
	f := reconciler.Harness{Binaries: []string{"/usr/local/bin/opencode"}}
	agw := reconciler.Endpoint{Host: "koinon-gateway.koinon.svc", Port: 8080}
	kgw := reconciler.Endpoint{Host: "koinon-mcp-gateway.koinon.svc", Port: 8080}
	mcpRules := []execution.L7Rule{
		{Method: "POST", Path: "/mcp"},
		{Method: "GET", Path: "/mcp"},
		{Method: "DELETE", Path: "/mcp"},
	}
	modelRules := func() []execution.L7Rule {
		var out []execution.L7Rule
		for _, p := range gateway.RoutePaths() {
			out = append(out, execution.L7Rule{Method: "POST", Path: p})
		}
		return out
	}

	It("puts the model routes and /mcp on agentgateway with driver agentgateway (no MCP gateway endpoint)", func() {
		p := reconciler.GatewayProfile(harness.OpenCode, f, agw, reconciler.Endpoint{}, true)
		Expect(p.Endpoints).To(Equal([]execution.Endpoint{{
			Host: agw.Host, Port: agw.Port, Protocol: "rest", Allow: append(modelRules(), mcpRules...),
		}}))
	})

	It("puts /mcp on koinon-gateway, and not on agentgateway, with driver koinon", func() {
		p := reconciler.GatewayProfile(harness.OpenCode, f, agw, kgw, true)
		Expect(p.Endpoints).To(Equal([]execution.Endpoint{
			{Host: agw.Host, Port: agw.Port, Protocol: "rest", Allow: modelRules()},
			{Host: kgw.Host, Port: kgw.Port, Protocol: "rest", Allow: mcpRules},
		}))
		for _, e := range p.Endpoints {
			Expect(e.Protocol).NotTo(BeEmpty(), "every endpoint is inspected and so enforced")
			Expect(e.Access).To(Equal(execution.AccessNone), "no preset opens other paths")
		}
	})

	It("opens no /mcp anywhere without tool servers, whatever the driver", func() {
		for _, mcp := range []reconciler.Endpoint{{}, kgw} {
			p := reconciler.GatewayProfile(harness.OpenCode, f, agw, mcp, false)
			Expect(p.Endpoints).To(Equal([]execution.Endpoint{{
				Host: agw.Host, Port: agw.Port, Protocol: "rest", Allow: modelRules(),
			}}))
		}
	})

	It("keeps the one bearer credential and the profile's identity with both drivers", func() {
		a := reconciler.GatewayProfile(harness.OpenCode, f, agw, reconciler.Endpoint{}, true)
		k := reconciler.GatewayProfile(harness.OpenCode, f, agw, kgw, true)
		Expect(k.ID).To(Equal(a.ID))
		Expect(k.Credentials).To(Equal(a.Credentials))
		Expect(k.Credentials).To(HaveLen(1))
		Expect(k.Credentials[0].EnvVars).To(Equal([]string{reconciler.GatewayKeyEnvVar}))
		Expect(k.Binaries).To(Equal(f.Binaries))
	})
})
