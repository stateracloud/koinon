// SPDX-License-Identifier: Apache-2.0

package standalone_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/testcontainers/testcontainers-go"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/gatewaytest"
)

// An org MCP server's approval-gated tools are offered on a second gateway target,
// "<org>-<server>-ask", that points at koinon-api's proxy and holds no credential (ADR 0042,
// package mcpask). The kind e2e step for MC1 relies on it end to end. This guards the gateway layer
// only: it builds the state and the grants by hand, in the shape the reconciler renders
// (internal/reconciler/mcp.go), so it does not check that the reconciler computes them. Already covered for this driver by the shared
// G1 suite (internal/gateway/gatewaytest): the credential injected at the gateway and not
// settable by the agent, the agent's key never forwarded, the key refused in any header but
// Authorization; and by the reload specs above: the prober key never reaching an upstream.
var _ = Describe("agentgateway v1.7.0 ask-tier target (standalone)", Ordered, func() {
	const (
		server    = "acme-tracker"
		askServer = server + "-ask"
	)
	var (
		r        *running
		guard    *gatewaytest.FakeGuard
		llm      string
		key      gateway.IssuedKey
		agent    = gateway.AgentRef{Org: "acme", Name: "tracker-user"}
		realLog  = &requestLog{}
		askLog   = &requestLog{}
		seenCall = func(l *requestLog, tool string) bool { return l.called(tool) }
	)

	session := func(ctx context.Context) *mcp.ClientSession {
		client := mcp.NewClient(&mcp.Implementation{Name: "koinon-test", Version: "0.0.1"}, nil)
		s, err := client.Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint: llm + "/mcp",
			HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				req = req.Clone(req.Context())
				req.Header.Set("Authorization", "Bearer "+key.Material)
				return http.DefaultTransport.RoundTrip(req)
			})},
			DisableStandaloneSSE: true, MaxRetries: -1,
		}, nil)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = s.Close() })
		return s
	}

	BeforeAll(func(ctx SpecContext) {
		real := newToolServer(server, realLog, "X-Api-Key")
		ask := newToolServer(askServer, askLog, "X-Api-Key")
		DeferCleanup(func() { real.CloseClientConnections(); real.Close() })
		DeferCleanup(func() { ask.CloseClientConnections(); ask.Close() })

		var err error
		guard, err = gatewaytest.NewFakeGuard()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(guard.Stop)
		r, err = startStandalone(ctx, []int{portOf(real), portOf(ask), guard.Port()}, map[string]string{"tracker-key": "koinon_test_not-a-real-token-tracker"},
			guard.URL(testcontainers.HostInternal))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(r.Stop)
		llm = r.URL
		key, err = gateway.NewKey(agent)
		Expect(err).NotTo(HaveOccurred())

		host := func(s *httptest.Server) string {
			return "http://" + testcontainers.HostInternal + ":" + itoaPort(s) + "/mcp"
		}
		state := gateway.State{MCP: []gateway.MCPTarget{
			{Name: server, Org: "acme", URL: host(real), CredentialHeader: "X-Api-Key", CredentialFile: "/secrets/upstream/tracker-key"},
			// The ask target points at koinon-api's proxy, which holds no credential of the server.
			{Name: askServer, Org: "acme", URL: host(ask)},
		}}
		state.AddKey(key.Key)
		// The reconciler's grants (internal/reconciler/mcp.go): auto-tier tools on the server's
		// own target, ask-tier tools on the -ask target.
		state.GrantTools(agent,
			gateway.ToolGrant{Target: server, Tools: []string{"read_item"}},
			gateway.ToolGrant{Target: askServer, Tools: []string{"write_item"}},
		)
		_, err = r.driver.Apply(ctx, state)
		Expect(err).NotTo(HaveOccurred())
	}, NodeTimeout(3*time.Minute))

	AfterEach(func(ctx SpecContext) {
		if CurrentSpecReport().Failed() && r != nil {
			AddReportEntry("agentgateway logs", r.Logs(ctx))
		}
	})

	It("offers the auto-tier tool on the server's target and the ask-tier tool on the ask target, never on the server", func(ctx SpecContext) {
		res, err := session(ctx).ListTools(ctx, nil)
		Expect(err).NotTo(HaveOccurred())
		var names []string
		for _, t := range res.Tools {
			names = append(names, t.Name)
		}
		Expect(names).To(ConsistOf(server+"_read_item", askServer+"_write_item"))
	})

	It("sends a call to the ask tool to the ask target only, with the agent's identity and without the server's credential or the key", func(ctx SpecContext) {
		before := askLog.count()
		res, err := session(ctx).CallTool(ctx, &mcp.CallToolParams{Name: askServer + "_write_item"})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.IsError).To(BeFalse())
		Expect(askLog.count()).To(BeNumerically(">", before))
		Expect(seenCall(askLog, "write_item")).To(BeTrue())
		Expect(seenCall(realLog, "write_item")).To(BeFalse(), "nothing reaches the remote server itself")
		Expect(askLog.header(gateway.AgentHeader)).To(ContainElement(agent.String()), "the proxy learns who asks from the gateway")
		Expect(askLog.header("X-Api-Key")).To(BeEmpty(), "the ask target holds no credential")
		// The leak guard sees the call on the ask target, so it leaves the risk to mcpask's card.
		Expect(guard.Requests()).To(ContainElement(SatisfyAll(
			HaveField("Targets", ConsistOf(askServer)), HaveField("Agent", agent.String()), HaveField("Tool", "write_item"))))
		for _, h := range []string{"Authorization", "Cookie", "User-Agent"} {
			Expect(strings.Join(askLog.header(h), ",")).NotTo(ContainSubstring(gateway.KeyPrefix), h)
		}
	})

	It("refuses the ask tool on the server's own target", func(ctx SpecContext) {
		res, err := session(ctx).CallTool(ctx, &mcp.CallToolParams{Name: server + "_write_item"})
		if err == nil {
			Expect(res.IsError).To(BeTrue())
		}
		Expect(seenCall(realLog, "write_item")).To(BeFalse(), "an ask-tier tool is not callable without the approval")
	})

	It("injects the credential only for the auto tool on the server's own target", func(ctx SpecContext) {
		res, err := session(ctx).CallTool(ctx, &mcp.CallToolParams{Name: server + "_read_item"})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Content[0].(*mcp.TextContent).Text).To(Equal(server + " saw koinon_test_not-a-real-token-tracker"))
		Expect(strings.Join(realLog.header("Authorization"), ",")).NotTo(ContainSubstring(gateway.KeyPrefix))
	})
})

// requestLog records what a fake MCP server received.
type requestLog struct {
	mu    sync.Mutex
	hdrs  []http.Header
	calls []string
}

func (l *requestLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.hdrs)
}

func (l *requestLog) header(name string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, h := range l.hdrs {
		out = append(out, h.Values(name)...)
	}
	return out
}

func (l *requestLog) called(tool string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, c := range l.calls {
		if c == tool {
			return true
		}
	}
	return false
}

// newToolServer is an MCP server with read_item and write_item that answers each call with the
// credential header it received.
func newToolServer(name string, log *requestLog, credHeader string) *httptest.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: name, Version: "0.0.1"}, nil)
	type args struct{}
	for _, tool := range []string{"read_item", "write_item"} {
		mcp.AddTool(srv, &mcp.Tool{Name: tool, Description: tool},
			func(_ context.Context, req *mcp.CallToolRequest, _ args) (*mcp.CallToolResult, any, error) {
				log.mu.Lock()
				log.calls = append(log.calls, tool)
				log.mu.Unlock()
				got := strings.Join(req.Extra.Header.Values(credHeader), ",")
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: name + " saw " + got}}}, nil, nil
			})
	}
	// agentgateway reaches the server through the testcontainers tunnel, which arrives on
	// localhost with Host: host.testcontainers.internal.
	inner := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{DisableLocalhostProtection: true})
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.mu.Lock()
		log.hdrs = append(log.hdrs, r.Header.Clone())
		log.mu.Unlock()
		inner.ServeHTTP(w, r)
	}))
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func itoaPort(s *httptest.Server) string { return strconv.Itoa(portOf(s)) }
