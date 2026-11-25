// SPDX-License-Identifier: Apache-2.0

package gatewaytest

import (
	"context"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2" //nolint:staticcheck // the Ginkgo DSL is dot-imported, as in every spec file
	. "github.com/onsi/gomega"    //nolint:staticcheck // see above

	"github.com/stateracloud/koinon/internal/gateway"
)

// readyPromptSpec is the readiness prompt contract (gateway.ReadyPrompt, ADR 0052): every
// authenticated key lists and gets the one prompt gateway.ReadyPromptExposed, with or without a
// grant, and no other prompt. koinon-agentd waits for the command a harness makes from it, so a
// driver that does not serve it fails every run (#1239). Both [Register] and [RegisterMCP] run it.
func readyPromptSpec(env Env) {
	Describe(env.Name+": the readiness prompt (ADR 0052)", Ordered, func() {
		var (
			url                  string
			keyGrant, keyNoGrant gateway.IssuedKey
		)
		agentGrant := gateway.AgentRef{Org: "acme", Name: "granted"}
		agentNoGrant := gateway.AgentRef{Org: "globex", Name: "no-grant"}

		connect := func(ctx context.Context, key string) *mcp.ClientSession {
			client := mcp.NewClient(&mcp.Implementation{Name: "koinon-test", Version: "0.0.1"}, nil)
			s, err := client.Connect(ctx, &mcp.StreamableClientTransport{
				Endpoint:             url + "/mcp",
				HTTPClient:           &http.Client{Transport: bearer{key: key, base: http.DefaultTransport}},
				DisableStandaloneSSE: true,
				MaxRetries:           -1,
			}, nil)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { _ = s.Close() })
			return s
		}

		BeforeAll(func(ctx SpecContext) {
			tools := fakeTools()
			DeferCleanup(func() { tools.CloseClientConnections(); go tools.Close() })
			var err error
			keyGrant, err = gateway.NewKey(agentGrant)
			Expect(err).NotTo(HaveOccurred())
			keyNoGrant, err = gateway.NewKey(agentNoGrant)
			Expect(err).NotTo(HaveOccurred())
			host := func(p int) string { return "http://" + env.Host + ":" + itoa(p) + "/mcp" }
			state := gateway.State{MCPState: gateway.MCPState{MCP: []gateway.MCPTarget{
				{Name: gateway.ReadyTarget, URL: host(port(tools))},
			}}}
			state.AddKey(keyGrant.Key)
			state.AddKey(keyNoGrant.Key)
			state.GrantTools(agentGrant, gateway.ToolGrant{Target: gateway.ReadyTarget, Tools: []string{"get_approval_status"}})
			gw, err := env.Start(ctx, []int{port(tools)}, nil, Guard{})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(gw.Stop)
			url = gw.URL
			_, err = gw.Driver.Apply(ctx, state)
			Expect(err).NotTo(HaveOccurred())
		}, NodeTimeout(5*time.Minute))

		It("lists the readiness prompt to every agent, and no other prompt", func(ctx SpecContext) {
			for _, key := range []string{keyGrant.Material, keyNoGrant.Material} {
				s := connect(ctx, key)
				res, err := s.ListPrompts(ctx, nil)
				Expect(err).NotTo(HaveOccurred())
				Expect(res.Prompts).To(ConsistOf(HaveField("Name", gateway.ReadyPromptExposed)))
			}
		})

		It("gets the readiness prompt for every agent, and refuses every other prompt", func(ctx SpecContext) {
			for _, key := range []string{keyGrant.Material, keyNoGrant.Material} {
				s := connect(ctx, key)
				res, err := s.GetPrompt(ctx, &mcp.GetPromptParams{Name: gateway.ReadyPromptExposed})
				Expect(err).NotTo(HaveOccurred())
				Expect(res.Messages).NotTo(BeEmpty())
				_, err = s.GetPrompt(ctx, &mcp.GetPromptParams{Name: gateway.ReadyTarget + "_hidden_prompt"})
				Expect(err).To(HaveOccurred())
			}
		})
	})
}
