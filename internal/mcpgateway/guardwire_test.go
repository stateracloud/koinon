// SPDX-License-Identifier: Apache-2.0

package mcpgateway_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/mcpgateway"
)

// loadGuard is a guard whose rules load late, panics on demand and checks calls like the ask tier.
type loadGuard struct {
	recordingGuard
	loadErr atomic.Pointer[error]
	panics  atomic.Bool
	calls   chan mcpgateway.Call // every call the checker saw
	decide  func(mcpgateway.Call) mcpgateway.Verdict
}

func (g *loadGuard) Loaded() error {
	if e := g.loadErr.Load(); e != nil {
		return *e
	}
	return nil
}

func (g *loadGuard) Inspect(ctx context.Context, m mcpgateway.Message) mcpgateway.Verdict {
	if g.panics.Load() {
		panic("a rule that cannot run")
	}
	return g.recordingGuard.Inspect(ctx, m)
}

func (g *loadGuard) CheckCall(_ context.Context, c mcpgateway.Call) mcpgateway.Verdict {
	if g.calls != nil {
		g.calls <- c
	}
	if g.decide != nil {
		return g.decide(c)
	}
	return mcpgateway.Verdict{}
}

var _ = Describe("the guard in the MCP path", func() {
	var (
		agent = gateway.AgentRef{Org: "acme", Name: "wiki-reader"}
		log   *upstreamLog
		guard *loadGuard
		gw    *mcpgateway.Gateway
		front *httptest.Server
		key   gateway.IssuedKey
		reg   *prometheus.Registry
		m     *mcpgateway.Metrics
	)

	BeforeEach(func(ctx SpecContext) {
		log = &upstreamLog{}
		up := fakeUpstream("acme-wiki", log)
		DeferCleanup(func() { up.CloseClientConnections(); up.Close() })
		guard = &loadGuard{recordingGuard: recordingGuard{refuse: map[string]string{}}, calls: make(chan mcpgateway.Call, 8)}
		reg = prometheus.NewRegistry()
		m = mcpgateway.NewMetrics(reg)
		gw = mcpgateway.New(mcpgateway.Options{Guard: guard, Metrics: m})
		DeferCleanup(gw.Drain)
		mux := http.NewServeMux()
		mux.Handle("/mcp", gw.Handler())
		front = httptest.NewServer(mux)
		DeferCleanup(func() { front.CloseClientConnections(); front.Close() })
		key = mustKey(agent)
		_, err := gw.ApplyMCP(ctx, gateway.MCPConfig{Keys: []gateway.Key{key.Key}, MCPState: gateway.MCPState{
			MCP: []gateway.MCPTarget{{Name: "acme-wiki", Org: "acme", URL: up.URL + "/mcp"}},
			Tools: map[gateway.AgentRef][]gateway.ToolGrant{
				agent: {{Target: "acme-wiki", Tools: []string{"read_item", "write_item"}}},
			},
		}})
		Expect(err).NotTo(HaveOccurred())
	})

	It("shows the guard the target, its org and the credential kind on every message", func(ctx SpecContext) {
		s := connect(ctx, front.URL, key.Material)
		res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "acme-wiki_read_item"})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.IsError).To(BeFalse())
		guard.mu.Lock()
		defer guard.mu.Unlock()
		Expect(guard.seen).NotTo(BeEmpty())
		for _, msg := range guard.seen {
			Expect(msg.Target).To(Equal("acme-wiki"))
			Expect(msg.Org).To(Equal("acme"))
			Expect(msg.Agent).To(Equal(agent))
			Expect(msg.Credential).To(Equal(mcpgateway.CredentialNone))
		}
	})

	It("names the tool on a tools/call and on the result that answers it, and on no other message", func(ctx SpecContext) {
		s := connect(ctx, front.URL, key.Material)
		_, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "acme-wiki_read_item", Arguments: map[string]any{"text": "x"}})
		Expect(err).NotTo(HaveOccurred())
		guard.mu.Lock()
		defer guard.mu.Unlock()
		var request, result int
		for _, msg := range guard.seen {
			if msg.Method == "tools/call" && (msg.Kind == mcpgateway.KindRequest || msg.Kind == mcpgateway.KindResult) {
				Expect(msg.Tool).To(Equal("read_item"))
				if msg.Kind == mcpgateway.KindRequest {
					request++
				} else {
					result++
				}
				continue
			}
			Expect(msg.Tool).To(BeEmpty(), "%s %s", msg.Kind, msg.Method)
		}
		Expect(request).To(Equal(1))
		Expect(result).To(Equal(1))
	})

	It("runs the call check after the grants, with the arguments and the credential kind", func(ctx SpecContext) {
		s := connect(ctx, front.URL, key.Material)
		_, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "acme-wiki_read_item", Arguments: map[string]any{"text": "hello"}})
		Expect(err).NotTo(HaveOccurred())
		var c mcpgateway.Call
		Expect(guard.calls).To(Receive(&c))
		Expect(c.Agent).To(Equal(agent))
		Expect(c.Target).To(Equal("acme-wiki"))
		Expect(c.Org).To(Equal("acme"))
		Expect(c.Tool).To(Equal("read_item"))
		Expect(c.Credential).To(Equal(mcpgateway.CredentialNone))
		Expect(string(c.Arguments)).To(ContainSubstring("hello"))

		// A tool with no grant never reaches the call check.
		res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "acme-wiki_fail"})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.IsError).To(BeTrue())
		Expect(guard.calls).NotTo(Receive())
	})

	It("holds a call for the ask tier and sends it only when the check allows it", func(ctx SpecContext) {
		release := make(chan mcpgateway.Verdict)
		guard.decide = func(mcpgateway.Call) mcpgateway.Verdict { return <-release }
		s := connect(ctx, front.URL, key.Material)
		type out struct {
			res *mcp.CallToolResult
			err error
		}
		done := make(chan out, 1)
		go func() {
			res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "acme-wiki_write_item"})
			done <- out{res, err}
		}()
		Eventually(guard.calls).Should(Receive())
		Consistently(done, "200ms").ShouldNot(Receive())
		Expect(log.header("Mcp-Session-Id")).To(BeEmpty(), "the target was contacted while the call waited")
		release <- mcpgateway.Verdict{Refuse: true, Reason: "refused: a person said no"}
		var o out
		Eventually(done).Should(Receive(&o))
		Expect(o.err).NotTo(HaveOccurred())
		Expect(o.res.IsError).To(BeTrue())
		Expect(text(o.res)).To(Equal("refused: a person said no"))
		Expect(log.header("Mcp-Session-Id")).To(BeEmpty())
	})

	It("closes the route while the guard rules are not loaded, and opens it when they are", func(ctx SpecContext) {
		failed := errors.New("rules did not load")
		guard.loadErr.Store(&failed)
		s := connect(ctx, front.URL, key.Material)
		res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "acme-wiki_read_item"})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.IsError).To(BeTrue())
		Expect(text(res)).To(ContainSubstring("the guard rules are not loaded"))
		Expect(strings.Join(toolNames(ctx, s), ",")).To(BeEmpty(), "a closed route lists no tool")
		Expect(log.header("Mcp-Session-Id")).To(BeEmpty(), "the target was contacted")
		Expect(testutil.ToFloat64(m.GuardFailures.WithLabelValues("not_loaded"))).To(BeNumerically(">", 0))

		guard.loadErr.Store(nil)
		res, err = s.CallTool(ctx, &mcp.CallToolParams{Name: "acme-wiki_read_item"})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.IsError).To(BeFalse())
	})

	It("closes the route when the guard panics, and the gateway stays up", func(ctx SpecContext) {
		guard.panics.Store(true)
		s := connect(ctx, front.URL, key.Material)
		res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "acme-wiki_read_item"})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.IsError).To(BeTrue())
		Expect(text(res)).To(ContainSubstring("the guard failed to decide"))
		Expect(testutil.ToFloat64(m.GuardFailures.WithLabelValues("panic"))).To(BeNumerically(">", 0))
		guard.panics.Store(false)
		res, err = s.CallTool(ctx, &mcp.CallToolParams{Name: "acme-wiki_read_item"})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.IsError).To(BeFalse())
	})

	It("drops an org target that has no real guard and counts it with a fixed label", func(ctx SpecContext) {
		m2 := mcpgateway.NewMetrics(nil)
		g2 := mcpgateway.New(mcpgateway.Options{Metrics: m2})
		k := mustKey(agent)
		withTarget := gateway.MCPConfig{Keys: []gateway.Key{k.Key}, MCPState: gateway.MCPState{
			MCP: []gateway.MCPTarget{
				{Name: "acme-wiki", Org: "acme", URL: "https://wiki.example/mcp"},
				{Name: "platform", URL: "https://platform.example/mcp"},
			},
		}}
		rev, err := g2.ApplyMCP(ctx, withTarget)
		Expect(err).NotTo(HaveOccurred())
		Expect(testutil.ToFloat64(m2.Dropped.WithLabelValues("org_needs_guard"))).To(BeEquivalentTo(1))
		only := withTarget
		only.MCP = withTarget.MCP[1:]
		rev2, err := g2.ApplyMCP(ctx, only)
		Expect(err).NotTo(HaveOccurred())
		Expect(rev).To(Equal(rev2), "the revision names what serves")
	})
})
