// SPDX-License-Identifier: Apache-2.0

package mcpgateway_test

import (
	"bytes"
	"context"
	"encoding/pem"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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

	It("fails a call whose upstream reply is over the size limit, and serves the next call", func(ctx SpecContext) {
		var big atomic.Bool
		inner := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
			s := mcp.NewServer(&mcp.Implementation{Name: "big", Version: "0"}, nil)
			mcp.AddTool(s, &mcp.Tool{Name: "dump"}, func(context.Context, *mcp.CallToolRequest, echoArgs) (*mcp.CallToolResult, any, error) {
				n := 100
				if big.Load() {
					n = 3 << 20 // over the limit of this spec
				}
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: strings.Repeat("x", n)}}}, nil, nil
			})
			return s
		}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
		srv := httptest.NewServer(inner)
		DeferCleanup(srv.Close)

		audit := &auditLog{}
		gw := mcpgateway.New(mcpgateway.Options{Audit: audit, Guard: &recordingGuard{}, MaxResponseBytes: 1 << 20})
		DeferCleanup(gw.Drain)
		mux := http.NewServeMux()
		mux.Handle("/mcp", gw.Handler())
		front := httptest.NewServer(mux)
		DeferCleanup(func() { front.CloseClientConnections(); front.Close() })
		key := mustKey(agent)
		_, err := gw.ApplyMCP(ctx, gateway.MCPConfig{
			Keys: []gateway.Key{key.Key},
			MCPState: gateway.MCPState{
				MCP:   []gateway.MCPTarget{{Name: "t", Org: "acme", URL: srv.URL}},
				Tools: map[gateway.AgentRef][]gateway.ToolGrant{agent: {{Target: "t", All: true}}},
			},
		})
		Expect(err).NotTo(HaveOccurred())
		s := connect(ctx, front.URL, key.Material)

		res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "t_dump"})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.IsError).To(BeFalse(), "a small reply passes")

		big.Store(true)
		res, err = s.CallTool(ctx, &mcp.CallToolParams{Name: "t_dump"})
		if err == nil {
			Expect(res.IsError).To(BeTrue())
			Expect(text(res)).To(Not(ContainSubstring("xxxxxxxxxx")), "the oversized reply never reaches the agent")
		}

		big.Store(false)
		s2 := connect(ctx, front.URL, key.Material)
		res, err = s2.CallTool(ctx, &mcp.CallToolParams{Name: "t_dump"})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.IsError).To(BeFalse(), "the gateway still serves")
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
		// A credential on a plain http URL closes that route only. The rest of the config applies,
		// so a key revocation never waits on an unrelated target.
		gw2 := mcpgateway.New(mcpgateway.Options{Guard: &recordingGuard{}})
		rev, err := gw2.ApplyMCP(ctx, plain)
		Expect(err).NotTo(HaveOccurred())
		Expect(gw2.Revision()).To(Equal(rev))
		revoked := gateway.MCPConfig{MCPState: plain.MCPState}
		rev2, err := gw2.ApplyMCP(ctx, revoked)
		Expect(err).NotTo(HaveOccurred())
		Expect(rev2).NotTo(Equal(rev))
		ok := gateway.MCPConfig{Keys: []gateway.Key{k2.Key}, MCPState: gateway.MCPState{
			MCP: []gateway.MCPTarget{{Name: "t", Org: "acme", URL: "https://t.example/mcp"}},
		}}
		dead, cancel := context.WithCancel(ctx)
		cancel()
		_, err = gw.ApplyMCP(dead, ok)
		Expect(err).To(MatchError(context.Canceled))
		Expect(gw.Revision()).To(BeEmpty())

		// An org target with no real guard closes its route only (ADR 0120: a rule that fails to load
		// closes the route). The config applies, and its revision is the one of the config without it.
		noGuard := mcpgateway.New(mcpgateway.Options{})
		rev3, err := noGuard.ApplyMCP(ctx, ok)
		Expect(err).NotTo(HaveOccurred())
		withoutTarget := gateway.MCPConfig{Keys: []gateway.Key{k2.Key}}
		rev4, err := noGuard.ApplyMCP(ctx, withoutTarget)
		Expect(err).NotTo(HaveOccurred())
		Expect(rev3).To(Equal(rev4))
		rev5, err := gw.ApplyMCP(ctx, ok)
		Expect(err).NotTo(HaveOccurred())
		Expect(rev5).NotTo(Equal(rev3))
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
		var connections atomic.Int32 // one per SSE session the target accepted
		srv := httptest.NewServer(mcp.NewSSEHandler(func(*http.Request) *mcp.Server {
			connections.Add(1)
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
		// Both calls used one upstream session: the second did not connect again.
		Expect(connections.Load()).To(BeEquivalentTo(1))
		Expect(gw.OpenSessions()).To(Equal(1))
	})

	It("never sends a call again when the target lost the session, and treats a pointer pass-all guard as no guard", func(ctx SpecContext) {
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
		Expect(res.IsError).To(BeTrue()) // not sent again: it may have run
		res, err = s.CallTool(ctx, &mcp.CallToolParams{Name: "t_read"})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.IsError).To(BeFalse()) // the session was replaced

		ptr := mcpgateway.New(mcpgateway.Options{Guard: &mcpgateway.AllowGuard{}})
		rev, err := ptr.ApplyMCP(ctx, gateway.MCPConfig{Keys: []gateway.Key{key.Key}, MCPState: gateway.MCPState{
			MCP: []gateway.MCPTarget{{Name: "t", Org: "acme", URL: "https://t.example/mcp"}},
		}})
		// The org target is dropped: the pointer form is no real guard either.
		Expect(err).NotTo(HaveOccurred())
		bare, err := ptr.ApplyMCP(ctx, gateway.MCPConfig{Keys: []gateway.Key{key.Key}})
		Expect(err).NotTo(HaveOccurred())
		Expect(rev).To(Equal(bare))
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
		Expect(err).NotTo(HaveOccurred()) // the route closes; the config applies
		_, err = gw.ApplyMCP(ctx, cfg("https:mcp"))
		Expect(err).NotTo(HaveOccurred())
		Expect(gateway.CredentialSafeURL("https:mcp")).To(BeFalse()) // no host: the route closes
		Expect(gateway.CredentialSafeURL("https://t.example/mcp")).To(BeTrue())
	})

	It("skips certificate verification for the target that asks for it and for no other (#278)", func(ctx SpecContext) {
		tlsUp := httptest.NewTLSServer(fakeUpstream("scratch", &upstreamLog{}).Config.Handler)
		DeferCleanup(tlsUp.Close)
		gw := mcpgateway.New(mcpgateway.Options{Guard: &recordingGuard{}})
		DeferCleanup(gw.Drain)
		front := httptest.NewServer(gw.Handler())
		DeferCleanup(func() { front.CloseClientConnections(); front.Close() })
		key := mustKey(agent)
		grant := func(n string) gateway.ToolGrant { return gateway.ToolGrant{Target: n, All: true} }
		_, err := gw.ApplyMCP(ctx, gateway.MCPConfig{Keys: []gateway.Key{key.Key}, MCPState: gateway.MCPState{
			MCP: []gateway.MCPTarget{
				{Name: "scratch", Org: "acme", URL: tlsUp.URL + "/mcp", InsecureSkipVerify: true},
				{Name: "plain", Org: "acme", URL: tlsUp.URL + "/mcp"},
			},
			Tools: map[gateway.AgentRef][]gateway.ToolGrant{agent: {grant("scratch"), grant("plain")}},
		}})
		Expect(err).NotTo(HaveOccurred())
		_, err = gw.ApplyMCP(ctx, gateway.MCPConfig{Keys: []gateway.Key{key.Key}, MCPState: gateway.MCPState{MCP: []gateway.MCPTarget{
			{Name: "both", Org: "acme", URL: tlsUp.URL + "/mcp", InsecureSkipVerify: true, CAFile: writeFile("both.crt", "x")},
		}}})
		Expect(err).To(MatchError(ContainSubstring("exclude each other")), "a CA file and skipped verification are refused together")
		_, err = gw.ApplyMCP(ctx, gateway.MCPConfig{Keys: []gateway.Key{key.Key}, MCPState: gateway.MCPState{
			MCP:   []gateway.MCPTarget{{Name: "scratch", Org: "acme", URL: tlsUp.URL + "/mcp", InsecureSkipVerify: true}, {Name: "plain", Org: "acme", URL: tlsUp.URL + "/mcp"}},
			Tools: map[gateway.AgentRef][]gateway.ToolGrant{agent: {grant("scratch"), grant("plain")}},
		}})
		Expect(err).NotTo(HaveOccurred())
		s := connect(ctx, front.URL, key.Material)
		res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "scratch_read_item", Arguments: map[string]any{"text": "x"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.IsError).To(BeFalse())
		for _, name := range []string{"plain"} {
			res, err = s.CallTool(ctx, &mcp.CallToolParams{Name: name + "_read_item", Arguments: map[string]any{"text": "x"}})
			Expect(err != nil || res.IsError).To(BeTrue(), "%s must not connect", name)
		}
	})

	It("verifies a target with its own CA bundle only, and every other target with the system roots (#278)", func(ctx SpecContext) {
		tlsUp := httptest.NewTLSServer(fakeUpstream("private", &upstreamLog{}).Config.Handler)
		DeferCleanup(tlsUp.Close)
		caPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tlsUp.Certificate().Raw}))
		gw := mcpgateway.New(mcpgateway.Options{Guard: &recordingGuard{}})
		DeferCleanup(gw.Drain)
		front := httptest.NewServer(gw.Handler())
		DeferCleanup(func() { front.CloseClientConnections(); front.Close() })
		key := mustKey(agent)
		grant := func(n string) gateway.ToolGrant { return gateway.ToolGrant{Target: n, All: true} }
		_, err := gw.ApplyMCP(ctx, gateway.MCPConfig{Keys: []gateway.Key{key.Key}, MCPState: gateway.MCPState{
			MCP: []gateway.MCPTarget{
				{Name: "private", Org: "acme", URL: tlsUp.URL + "/mcp", CAFile: writeFile("ca.crt", caPEM)},
				{Name: "plain", Org: "acme", URL: tlsUp.URL + "/mcp"},
				{Name: "nofile", Org: "acme", URL: tlsUp.URL + "/mcp", CAFile: filepath.Join(GinkgoT().TempDir(), "missing.crt")},
				{Name: "junk", Org: "acme", URL: tlsUp.URL + "/mcp", CAFile: writeFile("junk.crt", "not a certificate")},
			},
			Tools: map[gateway.AgentRef][]gateway.ToolGrant{agent: {grant("private"), grant("plain"), grant("nofile"), grant("junk")}},
		}})
		Expect(err).NotTo(HaveOccurred())
		s := connect(ctx, front.URL, key.Material)
		res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "private_read_item", Arguments: map[string]any{"text": "x"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.IsError).To(BeFalse())
		for _, name := range []string{"plain", "nofile", "junk"} {
			res, err = s.CallTool(ctx, &mcp.CallToolParams{Name: name + "_read_item", Arguments: map[string]any{"text": "x"}})
			Expect(err != nil || res.IsError).To(BeTrue(), "%s must not connect", name)
		}
	})

	It("logs a dropped target with a fixed reason and no URL", func(ctx SpecContext) {
		var buf bytes.Buffer
		gw := mcpgateway.New(mcpgateway.Options{Guard: &recordingGuard{}, Logger: slog.New(slog.NewTextHandler(&buf, nil))})
		DeferCleanup(gw.Drain)
		_, err := gw.ApplyMCP(ctx, gateway.MCPConfig{MCPState: gateway.MCPState{MCP: []gateway.MCPTarget{
			{Name: "bad", Org: "acme", URL: "HTTP://example.com/mcp", CredentialHeader: "X-Api-Key", CredentialFile: "/dev/null"},
		}}})
		Expect(err).NotTo(HaveOccurred())
		Expect(buf.String()).To(ContainSubstring("mcp target dropped"))
		Expect(buf.String()).To(ContainSubstring("credential_needs_https"))
		Expect(buf.String()).NotTo(ContainSubstring("example.com"))
		Expect(buf.String()).NotTo(ContainSubstring("acme"))
	})

	It("closes only the unsafe target and still applies a key revocation", func(ctx SpecContext) {
		gw := mcpgateway.New(mcpgateway.Options{Guard: &recordingGuard{}})
		DeferCleanup(gw.Drain)
		mux := http.NewServeMux()
		mux.Handle("/mcp", gw.Handler())
		front := httptest.NewServer(mux)
		DeferCleanup(func() { front.CloseClientConnections(); front.Close() })
		k1, k2 := mustKey(agent), mustKey(agent)
		cfg := func(keys ...gateway.Key) gateway.MCPConfig {
			return gateway.MCPConfig{Keys: keys, MCPState: gateway.MCPState{
				MCP:   []gateway.MCPTarget{{Name: "bad", Org: "acme", URL: "http://example.com/mcp", CredentialHeader: "X-Api-Key", CredentialFile: "/dev/null"}},
				Tools: map[gateway.AgentRef][]gateway.ToolGrant{agent: {{Target: "bad", All: true}}},
			}}
		}
		_, err := gw.ApplyMCP(ctx, cfg(k1.Key, k2.Key))
		Expect(err).NotTo(HaveOccurred())
		s := connect(ctx, front.URL, k1.Material)
		res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "bad_read"})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.IsError).To(BeTrue()) // the route is closed
		_, err = gw.ApplyMCP(ctx, cfg(k2.Key))
		Expect(err).NotTo(HaveOccurred())
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, front.URL+"/mcp", strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+k1.Material)
		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		_ = resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized)) // the revoked key is refused
	})
})
