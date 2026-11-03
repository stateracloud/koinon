// SPDX-License-Identifier: Apache-2.0

package gatewaytest

import (
	"context"
	"net/http"
	"slices"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2" //nolint:staticcheck // the Ginkgo DSL is dot-imported, as in every spec file
	. "github.com/onsi/gomega"    //nolint:staticcheck // see above

	"github.com/stateracloud/koinon/internal/gateway"
)

// guardSpec is the MCP leak guard as the gateway applies it (ADR 0091): every tools/call goes to
// the guard before the server sees it and again with the result, the guard's refusal is final, and
// a guard that does not answer refuses the call (failClosed). Without a guard address the gateway
// refuses every call to an org's server and keeps Koinon's own tools working.
func guardSpec(env Env) {
	const target = "acme-guarded"
	agent := gateway.AgentRef{Org: "acme", Name: "guarded"}

	// call returns the text the agent reads, and whether the call failed.
	call := func(ctx context.Context, url, key, tool string) (string, bool) {
		client := mcp.NewClient(&mcp.Implementation{Name: "koinon-test", Version: "0.0.1"}, nil)
		s, err := client.Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint: url + "/mcp", HTTPClient: &http.Client{Transport: bearer{key: key, base: http.DefaultTransport}},
			DisableStandaloneSSE: true, MaxRetries: -1,
		}, nil)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = s.Close() }()
		res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: tool})
		if err != nil {
			return err.Error(), true
		}
		var text string
		for _, c := range res.Content {
			if t, ok := c.(*mcp.TextContent); ok {
				text += t.Text
			}
		}
		return text, res.IsError
	}
	toolCalls := func(l *headerLog) int {
		return len(slices.DeleteFunc(l.values("Mcp-Method"), func(m string) bool { return m != "tools/call" }))
	}

	Describe(env.Name+": the MCP leak guard (ADR 0091)", Ordered, func() {
		var (
			url, key string
			logs     func(context.Context) string
			guard    *FakeGuard
			srvLog   = &headerLog{}
		)
		BeforeAll(func(ctx SpecContext) {
			srv := fakeRemote(target, srvLog, "Authorization")
			DeferCleanup(func() { srv.CloseClientConnections(); go srv.Close() })
			guard = startGuard()
			k, err := gateway.NewKey(agent)
			Expect(err).NotTo(HaveOccurred())
			key = k.Material
			state := gateway.State{MCP: []gateway.MCPTarget{{Name: target, Org: "acme", URL: "http://" + env.Host + ":" + itoa(port(srv)) + "/mcp"}}}
			state.AddKey(k.Key)
			state.GrantTools(agent, gateway.ToolGrant{Target: target, Tools: []string{"read_item", "write_item"}})
			gw, err := env.Start(ctx, []int{port(srv), guard.Port()}, nil, guard.URL(env.Host))
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(gw.Stop)
			url, logs = gw.URL, gw.Logs
			_, err = gw.Driver.Apply(ctx, state)
			Expect(err).NotTo(HaveOccurred())
		}, NodeTimeout(5*time.Minute))

		AfterEach(func(ctx SpecContext) {
			if CurrentSpecReport().Failed() && logs != nil {
				AddReportEntry("gateway logs", logs(ctx))
			}
		})

		It("asks the guard before the call and with its result, naming the agent and the tool, without the agent's key", func(ctx SpecContext) {
			text, failed := call(ctx, url, key, target+"_read_item")
			Expect(failed).To(BeFalse(), text)
			Expect(text).To(Equal(target + " saw "))

			reqs := guard.Requests()
			Expect(reqs).To(ContainElement(SatisfyAll(
				HaveField("Targets", ConsistOf(target)), HaveField("Method", gateway.GuardMethod),
				HaveField("Agent", agent.String()), HaveField("Tool", "read_item"),
				HaveField("Body", ContainSubstring(`"read_item"`)))))
			for _, r := range reqs {
				for _, h := range r.Headers {
					Expect(gateway.GuardDisallowedHeaders).NotTo(ContainElement(h), "the gateway forwarded "+h+" to the guard")
				}
			}
			Expect(guard.Responses()).To(ContainElement(SatisfyAll(
				HaveField("Targets", ConsistOf(target)), HaveField("Agent", agent.String()), HaveField("Tool", "read_item"),
				HaveField("Body", ContainSubstring(target+" saw ")))))
		})

		It("refuses a call the guard refuses, before the server sees it, with the guard's reason", func(ctx SpecContext) {
			guard.Refuse("write_item", "refused by the leak guard: koinon test")
			before := toolCalls(srvLog)
			text, failed := call(ctx, url, key, target+"_write_item")
			Expect(failed).To(BeTrue(), text)
			Expect(text).To(ContainSubstring("refused by the leak guard: koinon test"))
			Expect(toolCalls(srvLog)).To(Equal(before), "the server never saw the call")
		})

		It("withholds a result the guard withholds", func(ctx SpecContext) {
			guard.Withhold("read_item", "withheld by the leak guard: koinon test")
			text, failed := call(ctx, url, key, target+"_read_item")
			Expect(failed).To(BeTrue(), text)
			Expect(text).NotTo(ContainSubstring(target + " saw"))
			Expect(text).To(ContainSubstring("withheld by the leak guard: koinon test"))
		})

		// Last: it stops the guard.
		It("refuses every call when the guard does not answer (failClosed)", func(ctx SpecContext) {
			guard.Stop()
			before := toolCalls(srvLog)
			text, failed := call(ctx, url, key, target+"_read_item")
			Expect(failed).To(BeTrue(), text)
			Expect(text).NotTo(ContainSubstring(target + " saw"))
			Expect(toolCalls(srvLog)).To(Equal(before), "the server never saw the call")
		})
	})

	Describe(env.Name+": no MCP leak guard configured", Ordered, func() {
		var (
			url, key string
			srvLog   = &headerLog{}
		)
		BeforeAll(func(ctx SpecContext) {
			srv := fakeRemote(target, srvLog, "Authorization")
			DeferCleanup(func() { srv.CloseClientConnections(); go srv.Close() })
			tools := fakeTools()
			DeferCleanup(func() { tools.CloseClientConnections(); go tools.Close() })
			k, err := gateway.NewKey(agent)
			Expect(err).NotTo(HaveOccurred())
			key = k.Material
			host := func(p int) string { return "http://" + env.Host + ":" + itoa(p) + "/mcp" }
			state := gateway.State{MCP: []gateway.MCPTarget{
				{Name: "github", URL: host(port(tools))},
				{Name: target, Org: "acme", URL: host(port(srv))},
			}}
			state.AddKey(k.Key)
			state.GrantTools(agent,
				gateway.ToolGrant{Target: "github", Tools: []string{"create_pull_request"}},
				gateway.ToolGrant{Target: target, Tools: []string{"read_item"}},
			)
			gw, err := env.Start(ctx, []int{port(srv), port(tools)}, nil, "")
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(gw.Stop)
			url = gw.URL
			_, err = gw.Driver.Apply(ctx, state)
			Expect(err).NotTo(HaveOccurred())
		}, NodeTimeout(5*time.Minute))

		It("refuses a call to an org's server and still serves Koinon's own tools", func(ctx SpecContext) {
			before := toolCalls(srvLog)
			text, failed := call(ctx, url, key, target+"_read_item")
			Expect(failed).To(BeTrue(), text)
			Expect(text).To(ContainSubstring(gateway.GuardUnconfiguredReason))
			Expect(toolCalls(srvLog)).To(Equal(before), "the server never saw the call")

			text, failed = call(ctx, url, key, "github_create_pull_request")
			Expect(failed).To(BeFalse(), text)
			Expect(text).To(Equal("create_pull_request reached by " + agent.String()))
		})
	})
}
