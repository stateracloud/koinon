// SPDX-License-Identifier: Apache-2.0

package mcpask_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/mcpask"
	"github.com/stateracloud/koinon/internal/mcpgateway"
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/mcpservers/mcpserverstest"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

// passGuard lets every message through. koinon-gateway serves an org's target only with a real guard
// (an AllowGuard drops it), so the spec gives it one that inspects nothing.
type passGuard struct{}

func (passGuard) Inspect(context.Context, mcpgateway.Message) mcpgateway.Verdict {
	return mcpgateway.Verdict{}
}

// headerLog records the agent header of each request an upstream gets.
type headerLog struct {
	mu   sync.Mutex
	seen []string
}

func (l *headerLog) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		l.seen = append(l.seen, r.Header.Values(gateway.AgentHeader)...)
		l.mu.Unlock()
		h.ServeHTTP(w, r)
	})
}

func (l *headerLog) values() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.seen...)
}

// Issue #1315: with driver koinon, the ask-tier tools of an org's server reach the agent through
// koinon-gateway. The gateway plan gives the ask target the org slug (org scoping, tool pins), and
// marks it as Koinon's own proxy (MCPTarget.Ask). koinon-gateway sends that target the agent header,
// so mcpask finds the caller. An org's own server still gets no agent header (#1235, ADR 0097
// decision 5). The kind quickstart step 6b proves the same on a cluster.
var _ = Describe("the ask-tier proxy behind koinon-gateway", func() {
	var (
		slug              string
		agentRef          gateway.AgentRef
		key               gateway.IssuedKey
		proxyURL, orgURL  string
		askLog, serverLog *headerLog
		front             *httptest.Server
	)

	BeforeEach(func(ctx SpecContext) {
		org, user, s := pgtest.NewOrg(ctx, pool)
		slug = s
		a, err := q.CreateAgent(ctx, store.CreateAgentParams{OrgID: org, DisplayName: "helper", Harness: harness.OpenCode, CreatedBy: user})
		Expect(err).NotTo(HaveOccurred())
		v, err := q.CreateAgentVersion(ctx, store.CreateAgentVersionParams{OrgID: org, AgentID: a.ID, Config: []byte(`{"instructions":"x"}`), CreatedBy: user})
		Expect(err).NotTo(HaveOccurred())
		agent, err := q.ActivateAgent(ctx, store.ActivateAgentParams{OrgID: org, ID: a.ID, VersionID: v.ID, ActivatedBy: user})
		Expect(err).NotTo(HaveOccurred())
		agentRef = gateway.AgentRef{Org: slug, Name: agent.ID.String()}

		svc := mcpservers.New(pool, ring, nil).WithURLPolicy(catalog.URLPolicy{Namespace: "koinon"})
		server, err := svc.Create(ctx, org, audit.System("koinon-test:spec"), user, mcpservers.Input{
			Name: "tracker", URL: "https://mcp.example.com/mcp", CredentialHeader: "X-Api-Key", CredentialValue: "koinon_test_not-a-real-token",
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = svc.SyncTools(ctx, org, uuidOf(server.ID), []mcpservers.ListedTool{
			{Name: "write_item", Description: "Write an item", InputSchema: json.RawMessage(`{"type":"object"}`)},
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = svc.SetTier(ctx, org, audit.System("koinon-test:spec"), server.ID, "write_item", "ask", mcpserverstest.PinOf(ctx, svc, org, server.ID, "write_item"))
		Expect(err).NotTo(HaveOccurred())
		Expect(q.InsertAgentMCPServer(ctx, store.InsertAgentMCPServerParams{OrgID: org, AgentID: agent.ID, ServerID: uuidOf(server.ID)})).To(Succeed())

		// Koinon's mcpask proxy, as koinon-api serves it.
		askLog = &headerLog{}
		mux := http.NewServeMux()
		mux.Handle(mcpask.Path, mcpask.New(pool, mcpask.Options{}).Handler())
		proxy := httptest.NewServer(askLog.wrap(mux))
		DeferCleanup(func() { proxy.CloseClientConnections(); proxy.Close() })
		proxyURL = proxy.URL + mcpask.Path + "tracker"

		// The org's own server: it serves one auto-tier tool and records the agent header it gets.
		serverLog = &headerLog{}
		orgSrv := mcp.NewServer(&mcp.Implementation{Name: "tracker", Version: "0"}, nil)
		orgSrv.AddTool(&mcp.Tool{Name: "read_item", InputSchema: json.RawMessage(`{"type":"object"}`)},
			func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "read"}}}, nil
			})
		upstream := httptest.NewServer(serverLog.wrap(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return orgSrv }, nil)))
		DeferCleanup(func() { upstream.CloseClientConnections(); upstream.Close() })
		orgURL = upstream.URL

		key, err = gateway.NewKey(agentRef)
		Expect(err).NotTo(HaveOccurred())
	})

	// apply starts koinon-gateway with the org's server and its ask target, as the gateway plan
	// renders them (gatewayplan: mcpTarget and the ask target).
	apply := func(ctx SpecContext, ask bool) {
		GinkgoHelper()
		gw := mcpgateway.New(mcpgateway.Options{Guard: passGuard{}})
		DeferCleanup(gw.Drain)
		server, askTarget := mcpservers.TargetName(slug, "tracker"), mcpservers.AskTargetName(slug, "tracker")
		_, err := gw.ApplyMCP(ctx, gateway.MCPConfig{
			Keys: []gateway.Key{key.Key},
			MCPState: gateway.MCPState{
				MCP: []gateway.MCPTarget{
					{Name: server, URL: orgURL, Org: slug},
					{Name: askTarget, URL: proxyURL, Org: slug, Ask: ask},
				},
				Tools: map[gateway.AgentRef][]gateway.ToolGrant{agentRef: {
					{Target: server, Tools: []string{"read_item"}},
					{Target: askTarget, Tools: []string{"write_item", mcpservers.AskStatusTool}},
				}},
			},
		})
		Expect(err).NotTo(HaveOccurred())
		mux := http.NewServeMux()
		mux.Handle("/mcp", gw.Handler())
		front = httptest.NewServer(mux)
		DeferCleanup(func() { front.CloseClientConnections(); front.Close() })
	}

	toolNames := func(ctx SpecContext) []string {
		GinkgoHelper()
		c := mcp.NewClient(&mcp.Implementation{Name: "koinon-test", Version: "0"}, nil)
		s, err := c.Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint: front.URL + "/mcp", DisableStandaloneSSE: true, MaxRetries: -1,
			HTTPClient: &http.Client{Transport: bearer{key: key.Material}},
		}, nil)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = s.Close() })
		res, err := s.ListTools(ctx, nil)
		Expect(err).NotTo(HaveOccurred())
		var out []string
		for _, t := range res.Tools {
			out = append(out, t.Name)
		}
		return out
	}

	It("lists the ask-tier tool for the caller, and the org's server gets no agent header", func(ctx SpecContext) {
		apply(ctx, true)
		askTarget := mcpservers.AskTargetName(slug, "tracker")
		Expect(toolNames(ctx)).To(ConsistOf(
			mcpservers.TargetName(slug, "tracker")+"_read_item",
			askTarget+"_write_item", askTarget+"_"+mcpservers.AskStatusTool))
		Expect(askLog.values()).To(HaveEach(agentRef.String()))
		Expect(askLog.values()).NotTo(BeEmpty())
		Expect(serverLog.values()).To(BeEmpty(), "an org's own server never learns the agent")
	})

	It("lists no ask-tier tool when the target is not marked as Koinon's proxy (the bug of #1315)", func(ctx SpecContext) {
		apply(ctx, false)
		Expect(toolNames(ctx)).To(ConsistOf(mcpservers.TargetName(slug, "tracker") + "_read_item"))
		Expect(askLog.values()).To(BeEmpty())
		Expect(serverLog.values()).To(BeEmpty())
	})
})

// bearer sends the gateway key as an agent does.
type bearer struct{ key string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.key)
	return http.DefaultTransport.RoundTrip(r)
}
