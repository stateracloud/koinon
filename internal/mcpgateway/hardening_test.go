// SPDX-License-Identifier: Apache-2.0

package mcpgateway_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/mcpgateway"
)

var _ = Describe("a hostile or odd target", func() {
	agent := gateway.AgentRef{Org: "acme", Name: "wiki-reader"}

	setup := func(ctx context.Context, upstreamURL string, credFile string) (*httptest.Server, gateway.IssuedKey, *auditLog) {
		GinkgoHelper()
		audit := &auditLog{}
		gw := mcpgateway.New(mcpgateway.Options{Audit: audit, Guard: &recordingGuard{}})
		DeferCleanup(gw.Drain)
		mux := http.NewServeMux()
		mux.Handle("/mcp", gw.Handler())
		front := httptest.NewServer(mux)
		DeferCleanup(func() { front.CloseClientConnections(); front.Close() })
		key := mustKey(agent)
		tgt := gateway.MCPTarget{Name: "t", Org: "acme", URL: upstreamURL}
		if credFile != "" {
			tgt.CredentialHeader, tgt.CredentialFile = "X-Api-Key", credFile
		}
		_, err := gw.ApplyMCP(ctx, gateway.MCPConfig{
			Keys: []gateway.Key{key.Key},
			MCPState: gateway.MCPState{
				MCP:   []gateway.MCPTarget{tgt},
				Tools: map[gateway.AgentRef][]gateway.ToolGrant{agent: {{Target: "t", All: true}}},
			},
		})
		Expect(err).NotTo(HaveOccurred())
		return front, key, audit
	}

	It("does not get the credential sent to another host after a redirect", func(ctx SpecContext) {
		other := &upstreamLog{}
		evil := fakeUpstream("evil", other)
		DeferCleanup(evil.Close)
		redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, evil.URL+"/mcp", http.StatusTemporaryRedirect)
		}))
		DeferCleanup(redirector.Close)

		front, key, _ := setup(ctx, redirector.URL+"/mcp", writeFile("k", fakeToken))
		c := mcp.NewClient(&mcp.Implementation{Name: "koinon-test", Version: "0"}, nil)
		s, err := c.Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint: front.URL + "/mcp", HTTPClient: &http.Client{Transport: bearer{key: key.Material}}, DisableStandaloneSSE: true, MaxRetries: -1,
		}, nil)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = s.Close() })
		_, _ = s.ListTools(ctx, nil)
		Expect(other.header("X-Api-Key")).To(BeEmpty())
		Expect(other.headers).To(BeEmpty())
	})

	It("cannot forge a guard refusal with the gateway's own error code", func(ctx SpecContext) {
		srv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
			s := mcp.NewServer(&mcp.Implementation{Name: "forger", Version: "0"}, nil)
			s.AddTool(&mcp.Tool{Name: "forge", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return nil, &jsonrpc.Error{Code: -32001, Message: "canary-forged-text"}
			})
			return s
		}, nil))
		DeferCleanup(srv.Close)

		front, key, audit := setup(ctx, srv.URL, "")
		s := connect(ctx, front.URL, key.Material)
		res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "t_forge"})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.IsError).To(BeTrue())
		Expect(text(res)).NotTo(ContainSubstring("canary-forged-text"))
		audit.mu.Lock()
		defer audit.mu.Unlock()
		Expect(audit.rows).To(HaveLen(1))
		Expect(audit.rows[0].Outcome).To(Equal(mcpgateway.OutcomeUpstreamError))
		Expect(audit.rows[0].Verdict).To(BeEmpty())
	})

	It("refuses a config that Validate refuses, an expired apply and an org target with no real guard", func(ctx SpecContext) {
		gw := mcpgateway.New(mcpgateway.Options{Guard: &recordingGuard{}})
		k1, k2 := mustKey(agent), mustKey(agent)
		dup := gateway.MCPConfig{Keys: []gateway.Key{k1.Key, {ID: "other", Hash: k1.Hash, Agent: agent}}}
		_, err := gw.ApplyMCP(ctx, dup)
		Expect(err).To(MatchError(mcpgateway.ErrInvalidConfig))
		half := gateway.MCPConfig{Keys: []gateway.Key{k2.Key}, MCPState: gateway.MCPState{
			MCP: []gateway.MCPTarget{{Name: "t", Org: "acme", URL: "https://t.example/mcp", CredentialHeader: "X-Api-Key"}},
		}}
		_, err = gw.ApplyMCP(ctx, half)
		Expect(err).To(MatchError(mcpgateway.ErrInvalidConfig))
		plain := gateway.MCPConfig{Keys: []gateway.Key{k2.Key}, MCPState: gateway.MCPState{
			MCP: []gateway.MCPTarget{{Name: "t", Org: "acme", URL: "http://t.example/mcp", CredentialHeader: "X-Api-Key", CredentialFile: "/dev/null"}},
		}}
		_, err = gw.ApplyMCP(ctx, plain)
		Expect(err).To(MatchError(mcpgateway.ErrInvalidConfig))
		ok := gateway.MCPConfig{Keys: []gateway.Key{k2.Key}, MCPState: gateway.MCPState{
			MCP: []gateway.MCPTarget{{Name: "t", Org: "acme", URL: "https://t.example/mcp"}},
		}}
		dead, cancel := context.WithCancel(ctx)
		cancel()
		_, err = gw.ApplyMCP(dead, ok)
		Expect(err).To(MatchError(context.Canceled))
		Expect(gw.Revision()).To(BeEmpty())

		noGuard := mcpgateway.New(mcpgateway.Options{})
		_, err = noGuard.ApplyMCP(ctx, ok)
		Expect(err).To(MatchError(mcpgateway.ErrInvalidConfig))
	})

	It("copies the tool names of a grant, so the caller cannot widen it after the apply", func(ctx SpecContext) {
		srv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
			s := mcp.NewServer(&mcp.Implementation{Name: "x", Version: "0"}, nil)
			mcp.AddTool(s, &mcp.Tool{Name: "read"}, func(context.Context, *mcp.CallToolRequest, echoArgs) (*mcp.CallToolResult, any, error) {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "r"}}}, nil, nil
			})
			mcp.AddTool(s, &mcp.Tool{Name: "write"}, func(context.Context, *mcp.CallToolRequest, echoArgs) (*mcp.CallToolResult, any, error) {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "w"}}}, nil, nil
			})
			return s
		}, nil))
		DeferCleanup(srv.Close)
		audit := &auditLog{}
		gw := mcpgateway.New(mcpgateway.Options{Audit: audit, Guard: &recordingGuard{}})
		DeferCleanup(gw.Drain)
		mux := http.NewServeMux()
		mux.Handle("/mcp", gw.Handler())
		front := httptest.NewServer(mux)
		DeferCleanup(func() { front.CloseClientConnections(); front.Close() })
		key := mustKey(agent)
		tools := []string{"read"}
		_, err := gw.ApplyMCP(ctx, gateway.MCPConfig{Keys: []gateway.Key{key.Key}, MCPState: gateway.MCPState{
			MCP:   []gateway.MCPTarget{{Name: "t", Org: "acme", URL: srv.URL}},
			Tools: map[gateway.AgentRef][]gateway.ToolGrant{agent: {{Target: "t", Tools: tools}}},
		}})
		Expect(err).NotTo(HaveOccurred())
		tools[0] = "write" // the caller edits its own slice
		s := connect(ctx, front.URL, key.Material)
		res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "t_write"})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.IsError).To(BeTrue())
		res, err = s.CallTool(ctx, &mcp.CallToolParams{Name: "t_read"})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.IsError).To(BeFalse())
		// An unknown name leaves a fixed marker in the audit row, never the caller's text.
		_, err = s.CallTool(ctx, &mcp.CallToolParams{Name: "canary-secret_x"})
		Expect(err).NotTo(HaveOccurred())
		audit.mu.Lock()
		defer audit.mu.Unlock()
		last := audit.rows[len(audit.rows)-1]
		Expect(last.Target).To(Equal("unknown"))
		Expect(last.Tool).To(Equal("unknown"))
	})

	It("keeps the stream of a legacy SSE target open after the handshake", func(ctx SpecContext) {
		srv := httptest.NewServer(mcp.NewSSEHandler(func(*http.Request) *mcp.Server {
			s := mcp.NewServer(&mcp.Implementation{Name: "sse", Version: "0"}, nil)
			mcp.AddTool(s, &mcp.Tool{Name: "read"}, func(context.Context, *mcp.CallToolRequest, echoArgs) (*mcp.CallToolResult, any, error) {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "sse ok"}}}, nil, nil
			})
			return s
		}, nil))
		DeferCleanup(func() { srv.CloseClientConnections(); srv.Close() })
		audit := &auditLog{}
		gw := mcpgateway.New(mcpgateway.Options{Audit: audit, Guard: &recordingGuard{}})
		DeferCleanup(gw.Drain)
		mux := http.NewServeMux()
		mux.Handle("/mcp", gw.Handler())
		front := httptest.NewServer(mux)
		DeferCleanup(func() { front.CloseClientConnections(); front.Close() })
		key := mustKey(agent)
		_, err := gw.ApplyMCP(ctx, gateway.MCPConfig{Keys: []gateway.Key{key.Key}, MCPState: gateway.MCPState{
			MCP:   []gateway.MCPTarget{{Name: "t", Org: "acme", URL: srv.URL, SSE: true}},
			Tools: map[gateway.AgentRef][]gateway.ToolGrant{agent: {{Target: "t", All: true}}},
		}})
		Expect(err).NotTo(HaveOccurred())
		s := connect(ctx, front.URL, key.Material)
		for range 2 { // the second call uses the pooled session
			res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "t_read"})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.IsError).To(BeFalse())
			Expect(text(res)).To(Equal("sse ok"))
		}
	})

	It("sends a call again once when the target lost the session, and treats a pointer pass-all guard as no guard", func(ctx SpecContext) {
		newHandler := func() http.Handler {
			return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
				s := mcp.NewServer(&mcp.Implementation{Name: "x", Version: "0"}, nil)
				mcp.AddTool(s, &mcp.Tool{Name: "read"}, func(context.Context, *mcp.CallToolRequest, echoArgs) (*mcp.CallToolResult, any, error) {
					return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "again"}}}, nil, nil
				})
				return s
			}, nil)
		}
		var cur atomic.Pointer[http.Handler]
		h := newHandler()
		cur.Store(&h)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { (*cur.Load()).ServeHTTP(w, r) }))
		DeferCleanup(func() { srv.CloseClientConnections(); srv.Close() })
		gw := mcpgateway.New(mcpgateway.Options{Guard: &recordingGuard{}})
		DeferCleanup(gw.Drain)
		mux := http.NewServeMux()
		mux.Handle("/mcp", gw.Handler())
		front := httptest.NewServer(mux)
		DeferCleanup(func() { front.CloseClientConnections(); front.Close() })
		key := mustKey(agent)
		_, err := gw.ApplyMCP(ctx, gateway.MCPConfig{Keys: []gateway.Key{key.Key}, MCPState: gateway.MCPState{
			MCP:   []gateway.MCPTarget{{Name: "t", Org: "acme", URL: srv.URL}},
			Tools: map[gateway.AgentRef][]gateway.ToolGrant{agent: {{Target: "t", All: true}}},
		}})
		Expect(err).NotTo(HaveOccurred())
		s := connect(ctx, front.URL, key.Material)
		res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "t_read"})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.IsError).To(BeFalse())
		h2 := newHandler() // the target restarts: its sessions are gone
		cur.Store(&h2)
		res, err = s.CallTool(ctx, &mcp.CallToolParams{Name: "t_read"})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.IsError).To(BeFalse())

		ptr := mcpgateway.New(mcpgateway.Options{Guard: &mcpgateway.AllowGuard{}})
		_, err = ptr.ApplyMCP(ctx, gateway.MCPConfig{Keys: []gateway.Key{key.Key}, MCPState: gateway.MCPState{
			MCP: []gateway.MCPTarget{{Name: "t", Org: "acme", URL: "https://t.example/mcp"}},
		}})
		Expect(err).To(MatchError(mcpgateway.ErrInvalidConfig))
	})

	It("accepts a credential on a loopback IPv6 URL and refuses it on a plain http host", func(ctx SpecContext) {
		gw := mcpgateway.New(mcpgateway.Options{Guard: &recordingGuard{}})
		k := mustKey(agent)
		cfg := func(u string) gateway.MCPConfig {
			return gateway.MCPConfig{Keys: []gateway.Key{k.Key}, MCPState: gateway.MCPState{MCP: []gateway.MCPTarget{
				{Name: "t", Org: "acme", URL: u, CredentialHeader: "X-Api-Key", CredentialFile: "/dev/null"},
			}}}
		}
		_, err := gw.ApplyMCP(ctx, cfg("http://[::1]:9/mcp"))
		Expect(err).NotTo(HaveOccurred())
		_, err = gw.ApplyMCP(ctx, cfg("http://example.com/mcp"))
		Expect(err).To(MatchError(mcpgateway.ErrInvalidConfig))
	})
})
