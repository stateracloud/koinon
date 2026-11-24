// SPDX-License-Identifier: Apache-2.0

package mcpgateway_test

import (
	"net/http"
	"net/http/httptest"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gstruct"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/mcpgateway"
)

var _ = Describe("the MCP path", func() {
	var (
		acmeLog, globexLog, openLog *upstreamLog
		guard                       *recordingGuard
		audit                       *auditLog
		gw                          *mcpgateway.Gateway
		cfg                         gateway.MCPConfig
		front                       *httptest.Server
		keyAcme, keyNoGrant, keyX   gateway.IssuedKey
		agentAcme                   = gateway.AgentRef{Org: "acme", Name: "wiki-reader"}
		agentNone                   = gateway.AgentRef{Org: "acme", Name: "no-mcp"}
		agentCross                  = gateway.AgentRef{Org: "acme", Name: "cross"}
	)

	BeforeEach(func(ctx SpecContext) {
		acmeLog, globexLog, openLog = &upstreamLog{}, &upstreamLog{}, &upstreamLog{}
		acme, globex, open := fakeUpstream("acme-wiki", acmeLog), fakeUpstream("globex-wiki", globexLog), fakeUpstream("acme-open", openLog)
		DeferCleanup(func() { acme.CloseClientConnections(); acme.Close() })
		DeferCleanup(func() { globex.CloseClientConnections(); globex.Close() })
		DeferCleanup(func() { open.CloseClientConnections(); open.Close() })

		guard = &recordingGuard{refuse: map[string]string{}}
		audit = &auditLog{}
		gw = mcpgateway.New(mcpgateway.Options{Guard: guard, Audit: audit})
		DeferCleanup(gw.Drain)
		mux := http.NewServeMux()
		mux.Handle("/mcp", gw.Handler())
		front = httptest.NewServer(mux)
		DeferCleanup(func() { front.CloseClientConnections(); front.Close() })

		keyAcme, keyNoGrant, keyX = mustKey(agentAcme), mustKey(agentNone), mustKey(agentCross)
		cfg = gateway.MCPConfig{
			Keys: []gateway.Key{keyAcme.Key, keyNoGrant.Key, keyX.Key},
			MCPState: gateway.MCPState{
				MCP: []gateway.MCPTarget{
					{Name: "acme-wiki", Org: "acme", URL: acme.URL + "/mcp", CredentialHeader: "X-Api-Key", CredentialFile: writeFile("acme-key", fakeToken+"\n")},
					{Name: "globex-wiki", Org: "globex", URL: globex.URL + "/mcp", CredentialHeader: "X-Api-Key", CredentialFile: writeFile("globex-key", fakeToken+"-globex")},
					{Name: "acme-open", Org: "acme", URL: open.URL + "/mcp"},
				},
				Tools: map[gateway.AgentRef][]gateway.ToolGrant{
					agentAcme: {
						{Target: "acme-wiki", Tools: []string{"read_item", "write_item", "fail", "roots", "sample", "log"}},
						{Target: "acme-open", Tools: []string{"read_item"}},
					},
					// A grant that names another org's target does nothing.
					agentCross: {{Target: "globex-wiki", Tools: []string{"read_item"}}},
				},
			},
		}
		_, err := gw.ApplyMCP(ctx, cfg)
		Expect(err).NotTo(HaveOccurred())
	})

	It("lists only the granted tools of the targets the agent's org may see, with the target as prefix", func(ctx SpecContext) {
		Expect(toolNames(ctx, connect(ctx, front.URL, keyAcme.Material))).To(ConsistOf(
			"acme-wiki_read_item", "acme-wiki_write_item", "acme-wiki_fail", "acme-wiki_roots", "acme-wiki_sample", "acme-wiki_log",
			"acme-open_read_item"))
		Expect(toolNames(ctx, connect(ctx, front.URL, keyNoGrant.Material))).To(BeEmpty())
		Expect(toolNames(ctx, connect(ctx, front.URL, keyX.Material))).To(BeEmpty())
	})

	It("never contacts another org's target", func(ctx SpecContext) {
		s := connect(ctx, front.URL, keyX.Material)
		res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "globex-wiki_read_item"})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.IsError).To(BeTrue())
		Expect(globexLog.headers).To(BeEmpty())
	})

	It("refuses a tool that is not granted, without contacting the target", func(ctx SpecContext) {
		s := connect(ctx, front.URL, keyAcme.Material)
		res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "acme-open_write_item"})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.IsError).To(BeTrue())
		Expect(openLog.header("Mcp-Method")).NotTo(ContainElement("tools/call"))
	})

	It("sets the credential itself and ignores one the agent sends", func(ctx SpecContext) {
		c := mcp.NewClient(&mcp.Implementation{Name: "koinon-test", Version: "0"}, nil)
		s, err := c.Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint: front.URL + "/mcp", DisableStandaloneSSE: true, MaxRetries: -1,
			HTTPClient: &http.Client{Transport: headerSetter{key: keyAcme.Material, name: "X-Api-Key", value: "agent-chose-this"}},
		}, nil)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = s.Close() })
		res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "acme-wiki_read_item", Arguments: map[string]any{"text": "x"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(text(res)).To(Equal("acme-wiki read x"))
		Expect(acmeLog.header("X-Api-Key")).To(HaveEach(fakeToken))
		Expect(acmeLog.header("Authorization")).To(BeEmpty(), "the gateway key never goes upstream")
		// A target without a credential gets none, and not the agent's key.
		_, err = s.CallTool(ctx, &mcp.CallToolParams{Name: "acme-open_read_item"})
		Expect(err).NotTo(HaveOccurred())
		Expect(openLog.header("X-Api-Key")).To(BeEmpty())
		Expect(openLog.header("Authorization")).To(BeEmpty())
		// The agent header is for Koinon's own tool servers only: an org's server never learns the agent.
		Expect(acmeLog.header(gateway.AgentHeader)).To(BeEmpty())
		Expect(openLog.header(gateway.AgentHeader)).To(BeEmpty())
	})

	It("sends the agent header to a platform target (Koinon's own tool servers) only", func(ctx SpecContext) {
		toolsLog := &upstreamLog{}
		tools := fakeUpstream("koinon-tools", toolsLog)
		DeferCleanup(func() { tools.CloseClientConnections(); tools.Close() })
		cfg.MCP = append(cfg.MCP, gateway.MCPTarget{Name: "koinon-tools", URL: tools.URL + "/mcp"})
		cfg.Tools[agentAcme] = append(cfg.Tools[agentAcme], gateway.ToolGrant{Target: "koinon-tools", Tools: []string{"read_item"}})
		_, err := gw.ApplyMCP(ctx, cfg)
		Expect(err).NotTo(HaveOccurred())

		s := connect(ctx, front.URL, keyAcme.Material)
		_, err = s.CallTool(ctx, &mcp.CallToolParams{Name: "koinon-tools_read_item", Arguments: map[string]any{"text": "x"}})
		Expect(err).NotTo(HaveOccurred())
		_, err = s.CallTool(ctx, &mcp.CallToolParams{Name: "acme-wiki_read_item", Arguments: map[string]any{"text": "x"}})
		Expect(err).NotTo(HaveOccurred())
		// The platform target learns the agent the key check found (ADR 0097 decision 5); the org's server
		// on the same session does not (#1235).
		Expect(toolsLog.header(gateway.AgentHeader)).NotTo(BeEmpty())
		Expect(toolsLog.header(gateway.AgentHeader)).To(HaveEach(agentAcme.String()))
		Expect(acmeLog.header(gateway.AgentHeader)).To(BeEmpty())
	})

	It("refuses an unknown key, a missing key and a key outside the Authorization header", func(ctx SpecContext) {
		do := func(h map[string]string, path string) *http.Response {
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, front.URL+path, nil)
			for k, v := range h {
				req.Header.Set(k, v)
			}
			res, err := http.DefaultClient.Do(req)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(res.Body.Close)
			return res
		}
		Expect(do(nil, "/mcp").StatusCode).To(Equal(http.StatusUnauthorized))
		Expect(do(map[string]string{"Authorization": "Bearer kgw_unknown"}, "/mcp").StatusCode).To(Equal(http.StatusUnauthorized))
		res := do(map[string]string{"Authorization": "Bearer " + keyAcme.Material, "User-Agent": keyAcme.Material}, "/mcp")
		Expect(res.StatusCode).To(Equal(http.StatusForbidden))
		Expect(gateway.DoNotRetry(res.Header)).To(BeTrue())
		Expect(do(map[string]string{"Authorization": "Bearer " + keyAcme.Material}, "/mcp?k="+keyAcme.Material).StatusCode).To(Equal(http.StatusForbidden))
	})

	It("refuses a method that is not on the allowlist", func(ctx SpecContext) {
		s := connect(ctx, front.URL, keyAcme.Material)
		_, err := s.ListResources(ctx, nil)
		Expect(err).To(HaveOccurred())
		_, err = s.ListPrompts(ctx, nil)
		Expect(err).To(HaveOccurred())
	})

	It("answers roots/list with one fixed folder", func(ctx SpecContext) {
		s := connect(ctx, front.URL, keyAcme.Material)
		res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "acme-wiki_roots"})
		Expect(err).NotTo(HaveOccurred())
		Expect(text(res)).To(Equal(mcpgateway.RootURI))
	})

	It("refuses sampling, reports it to the guard and still shows the guard the request", func(ctx SpecContext) {
		s := connect(ctx, front.URL, keyAcme.Material)
		res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "acme-wiki_sample"})
		Expect(err).NotTo(HaveOccurred())
		Expect(text(res)).To(Equal("sampling: error"))
		Eventually(func() []mcpgateway.Violation {
			guard.mu.Lock()
			defer guard.mu.Unlock()
			return append([]mcpgateway.Violation(nil), guard.violations...)
		}).Should(ContainElement(mcpgateway.ViolationSampling))
		Expect(guard.sawText("canary-sampling")).To(BeTrue())
	})

	It("shows the guard every message kind: results, errors, notifications and server requests", func(ctx SpecContext) {
		s := connect(ctx, front.URL, keyAcme.Material)
		_, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "acme-wiki_log"})
		Expect(err).NotTo(HaveOccurred())
		_, err = s.CallTool(ctx, &mcp.CallToolParams{Name: "acme-wiki_sample"})
		Expect(err).NotTo(HaveOccurred())
		_, err = s.ListTools(ctx, nil)
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() bool {
			return guard.sawKind(mcpgateway.FromUpstream, mcpgateway.KindNotification, "notifications/progress")
		}).Should(BeTrue())
		Expect(guard.sawKind(mcpgateway.FromUpstream, mcpgateway.KindResult, "tools/call")).To(BeTrue())
		Expect(guard.sawKind(mcpgateway.FromUpstream, mcpgateway.KindResult, "tools/list")).To(BeTrue())
		Expect(guard.sawKind(mcpgateway.FromUpstream, mcpgateway.KindRequest, "sampling/createMessage")).To(BeTrue())
		Expect(guard.sawKind(mcpgateway.ToUpstream, mcpgateway.KindRequest, "tools/call")).To(BeTrue())
		Expect(guard.sawKind(mcpgateway.ToUpstream, mcpgateway.KindNotification, "notifications/initialized")).To(BeTrue())
		Expect(guard.sawKind(mcpgateway.FromUpstream, mcpgateway.KindResult, "initialize")).To(BeTrue())
	})

	It("withholds a result the guard refuses and tells the agent why", func(ctx SpecContext) {
		guard.refuse["acme-wiki read secret"] = "refused: the result leaks"
		s := connect(ctx, front.URL, keyAcme.Material)
		res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "acme-wiki_read_item", Arguments: map[string]any{"text": "secret"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.IsError).To(BeTrue())
		Expect(text(res)).To(Equal("refused: the result leaks"))
		Expect(text(res)).NotTo(ContainSubstring("secret"))
	})

	It("writes one audit row per call and none of it holds arguments, results or the token", func(ctx SpecContext) {
		s := connect(ctx, front.URL, keyAcme.Material)
		_, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "acme-wiki_read_item", Arguments: map[string]any{"text": "canary-args"}})
		Expect(err).NotTo(HaveOccurred())
		_, err = s.CallTool(ctx, &mcp.CallToolParams{Name: "acme-wiki_fail"})
		Expect(err).NotTo(HaveOccurred())
		_, err = s.CallTool(ctx, &mcp.CallToolParams{Name: "acme-open_write_item"})
		Expect(err).NotTo(HaveOccurred())
		audit.mu.Lock()
		defer audit.mu.Unlock()
		Expect(audit.rows).To(HaveLen(3))
		Expect(audit.rows[0]).To(gstruct.MatchFields(gstruct.IgnoreExtras, gstruct.Fields{
			"Agent": Equal(agentAcme), "Target": Equal("acme-wiki"), "Tool": Equal("read_item"),
			"Outcome": Equal(mcpgateway.OutcomeOK), "CredKind": Equal(mcpgateway.CredentialStatic),
		}))
		Expect(audit.rows[1].Outcome).To(Equal(mcpgateway.OutcomeToolError))
		Expect(audit.rows[2].Outcome).To(Equal(mcpgateway.OutcomeRefusedGrant))
		Expect(audit.rows[2].CredKind).To(Equal(mcpgateway.CredentialNone))
		for _, r := range audit.rows {
			Expect(r).NotTo(WithTransform(func(r mcpgateway.AuditRow) string { return r.Verdict + r.Tool + r.Target }, ContainSubstring("canary")))
			Expect(r).NotTo(WithTransform(func(r mcpgateway.AuditRow) string { return r.Verdict }, ContainSubstring(fakeToken)))
		}
	})

	It("applies a new config at once, drops the sessions of a changed target, and refuses a bad one", func(ctx SpecContext) {
		s := connect(ctx, front.URL, keyAcme.Material)
		Expect(toolNames(ctx, s)).NotTo(BeEmpty())
		rev, err := gw.ApplyMCP(ctx, gateway.MCPConfig{Keys: cfg.Keys})
		Expect(err).NotTo(HaveOccurred())
		Expect(gw.Revision()).To(Equal(rev))
		Expect(gw.OpenSessions()).To(BeZero())
		Expect(toolNames(ctx, connect(ctx, front.URL, keyAcme.Material))).To(BeEmpty())
		_, err = gw.ApplyMCP(ctx, gateway.MCPConfig{MCPState: gateway.MCPState{MCP: []gateway.MCPTarget{{Name: "bad_name", URL: "http://x"}}}})
		Expect(err).To(MatchError(mcpgateway.ErrInvalidConfig))
		Expect(gw.Revision()).To(Equal(rev), "a rejected config leaves the old one in force")
	})

	It("answers 503 after Drain", func(ctx SpecContext) {
		gw.Drain()
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, front.URL+"/mcp", nil)
		req.Header.Set("Authorization", "Bearer "+keyAcme.Material)
		res, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(res.Body.Close)
		Expect(res.StatusCode).To(Equal(http.StatusServiceUnavailable))
		Expect(res.Header.Get("Retry-After")).NotTo(BeEmpty())
	})
})

type headerSetter struct{ key, name, value string }

func (h headerSetter) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+h.key)
	r.Header.Set(h.name, h.value)
	return http.DefaultTransport.RoundTrip(r)
}
