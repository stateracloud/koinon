// SPDX-License-Identifier: Apache-2.0

package gatewaytest

import (
	"context"
	"net/http"
	"net/http/httptest"
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

	// #254, ADR 0097 step 1 exit criterion 2: what the real gateway shows the guard of a reply that
	// is not a successful result. The guard (internal/mcpguard) fails closed on a call it passed whose
	// result it never saw; guard_test there drives its own Server and shows the legs it then sets.
	// These specs show the trigger on the real gateway: an error reply reaches the agent and the
	// guard is not asked about it.
	Describe(env.Name+": replies the MCP leak guard never sees (#254)", Ordered, func() {
		const faulty = "acme-faulty"
		var (
			url, key string
			logs     func(context.Context) string
			guard    *FakeGuard
		)
		BeforeAll(func(ctx SpecContext) {
			srv := faultyRemote(faulty, &headerLog{})
			DeferCleanup(func() { srv.CloseClientConnections(); go srv.Close() })
			guard = startGuard()
			k, err := gateway.NewKey(agent)
			Expect(err).NotTo(HaveOccurred())
			key = k.Material
			state := gateway.State{MCP: []gateway.MCPTarget{{Name: faulty, Org: "acme", URL: "http://" + env.Host + ":" + itoa(port(srv)) + "/mcp"}}}
			state.AddKey(k.Key)
			state.GrantTools(agent, gateway.ToolGrant{Target: faulty, Tools: []string{"read_item", "fail_item", "stray_item"}})
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

		responsesFor := func(tool string) []GuardCall {
			return slices.DeleteFunc(guard.Responses(), func(c GuardCall) bool { return c.Tool != tool })
		}

		It("passes a JSON-RPC error reply to the agent without asking the guard about it", func(ctx SpecContext) {
			text, failed := call(ctx, url, key, faulty+"_fail_item")
			Expect(failed).To(BeTrue(), text)
			Expect(text).To(ContainSubstring(replyCanary), "the error reached the agent")
			Expect(guard.Requests()).To(ContainElement(HaveField("Tool", "fail_item")), "the guard passed the call")
			Consistently(func() []GuardCall { return responsesFor("fail_item") }).WithTimeout(2*time.Second).Should(BeEmpty(),
				"the guard was never asked about the error reply: the gap the ledger closes")
			for _, c := range guard.Responses() {
				Expect(c.Body).NotTo(ContainSubstring(replyCanary))
			}
		})

		// Expected upstream behaviour (agentgateway#4245): the response hook runs on every response
		// on the call's stream, whatever its id. A server that appends a result of its own after its
		// error gets the guard asked about that result, never the error, so the guard closes the call
		// (internal/mcpguard opencalls.go). This flips when the gateway hooks only the terminal reply.
		It("asks the guard about a result a server appends after its error, and never about the error (agentgateway#4245)", func(ctx SpecContext) {
			text, failed := call(ctx, url, key, faulty+"_stray_item")
			Expect(failed).To(BeTrue(), text)
			Expect(text).To(ContainSubstring(replyCanary))
			Eventually(func() []GuardCall { return responsesFor("stray_item") }).WithTimeout(10*time.Second).Should(HaveLen(1),
				"one CheckResponse, for the appended result")
			Consistently(func() []GuardCall { return responsesFor("stray_item") }).WithTimeout(2 * time.Second).Should(ConsistOf(
				HaveField("Body", SatisfyAll(ContainSubstring("stray result"), Not(ContainSubstring(replyCanary))))))
		})
	})

	// #254, ADR 0097 step 1 exit criterion 2 as one case: the real gateway, the real leak guard
	// (internal/mcpguard.Server on Postgres) and a server that answers a guarded call with a JSON-RPC
	// error. The error reaches the agent unseen by the guard, and the guard then holds the session
	// fail-closed. Runs in both modes: the guard is an ExtMcp server at Env.Host either way.
	Describe(env.Name+": the real MCP leak guard holds a session fail-closed after a reply it never saw (#254)", Ordered, func() {
		const (
			wiki = "wiki" // private data, answers with an error
			sink = "sink" // sets no leg; its tools are external-high
		)
		var (
			url, key string
			logs     func(context.Context) string
			rg       *RealGuard
		)
		BeforeAll(func(ctx SpecContext) {
			rg = startRealGuard(ctx)
			wikiSrv := faultyRemote(rg.Target(wiki), &headerLog{})
			DeferCleanup(func() { wikiSrv.CloseClientConnections(); go wikiSrv.Close() })
			sinkSrv := fakeRemote(rg.Target(sink), &headerLog{}, "Authorization")
			DeferCleanup(func() { sinkSrv.CloseClientConnections(); go sinkSrv.Close() })
			rg.Server(ctx, wiki, []string{"private_data"}, "read_item", "fail_item")
			rg.Server(ctx, sink, nil, "write_item")

			k, err := gateway.NewKey(rg.AgentRef())
			Expect(err).NotTo(HaveOccurred())
			key = k.Material
			host := func(s *httptest.Server) string { return "http://" + env.Host + ":" + itoa(port(s)) + "/mcp" }
			state := gateway.State{MCP: []gateway.MCPTarget{
				{Name: rg.Target(wiki), Org: rg.Slug, URL: host(wikiSrv)},
				{Name: rg.Target(sink), Org: rg.Slug, URL: host(sinkSrv)},
			}}
			state.AddKey(k.Key)
			state.GrantTools(rg.AgentRef(),
				gateway.ToolGrant{Target: rg.Target(wiki), Tools: []string{"read_item", "fail_item"}},
				gateway.ToolGrant{Target: rg.Target(sink), Tools: []string{"write_item"}})
			gw, err := env.Start(ctx, []int{port(wikiSrv), port(sinkSrv), rg.Port()}, nil, rg.URL(env.Host))
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(gw.Stop)
			url, logs = gw.URL, gw.Logs
			_, err = gw.Driver.Apply(ctx, state)
			Expect(err).NotTo(HaveOccurred())
		}, NodeTimeout(10*time.Minute))

		AfterEach(func(ctx SpecContext) {
			if CurrentSpecReport().Failed() && logs != nil {
				AddReportEntry("gateway logs", logs(ctx))
			}
		})

		It("lets the error reach the agent, then needs an approval, with both legs, for the next clean external-high call", func(ctx SpecContext) {
			text, failed := call(ctx, url, key, rg.Target(sink)+"_write_item")
			Expect(failed).To(BeFalse(), "a clean session passes a clean external-high call: "+text)

			text, failed = call(ctx, url, key, rg.Target(wiki)+"_fail_item")
			Expect(failed).To(BeTrue(), text)
			Expect(text).To(ContainSubstring(replyCanary), "the error reply reached the agent, which the guard never saw")

			text, failed = call(ctx, url, key, rg.Target(sink)+"_write_item")
			Expect(failed).To(BeTrue(), text)
			Expect(text).To(ContainSubstring("approval requested: id="))
			risk := rg.Risk(ctx)
			Expect(risk).To(ContainSubstring(`"reply_unseen"`))
			Expect(risk).To(ContainSubstring(`"private_data": true`))
			Expect(risk).To(ContainSubstring(`"untrusted_input": true`), "a reply never seen sets both legs")
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
