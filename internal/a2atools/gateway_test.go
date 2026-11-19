// SPDX-License-Identifier: Apache-2.0

package a2atools_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/a2atools"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/mcpgateway"
)

type auditLog struct {
	mu   sync.Mutex
	rows []mcpgateway.AuditRow
}

func (a *auditLog) Record(_ context.Context, r mcpgateway.AuditRow) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rows = append(a.rows, r)
	return nil
}

func (a *auditLog) last() mcpgateway.AuditRow {
	a.mu.Lock()
	defer a.mu.Unlock()
	Expect(a.rows).NotTo(BeEmpty())
	return a.rows[len(a.rows)-1]
}

// spyGuard records what it saw, can refuse a reply that holds a marker, and can refuse a whole call (the
// ask tier).
type spyGuard struct {
	mu        sync.Mutex
	seen      []mcpgateway.Message
	calls     []mcpgateway.Call
	refuseRaw string
	refuseAsk bool
}

func (g *spyGuard) Inspect(_ context.Context, m mcpgateway.Message) mcpgateway.Verdict {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.seen = append(g.seen, m)
	if g.refuseRaw != "" && m.Dir == mcpgateway.FromUpstream && strings.Contains(string(m.Raw), g.refuseRaw) {
		return mcpgateway.Verdict{Refuse: true, Reason: "refused: the guard blocked the reply"}
	}
	return mcpgateway.Verdict{}
}

func (g *spyGuard) CheckCall(_ context.Context, c mcpgateway.Call) mcpgateway.Verdict {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, c)
	if g.refuseAsk {
		return mcpgateway.Verdict{Refuse: true, Reason: "refused: a person said no"}
	}
	return mcpgateway.Verdict{}
}

type bearer struct{ key string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.key)
	return http.DefaultTransport.RoundTrip(r)
}

func connect(url, key string) *mcp.ClientSession {
	GinkgoHelper()
	c := mcp.NewClient(&mcp.Implementation{Name: "koinon-test", Version: "0"}, nil)
	s, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: url + "/mcp", HTTPClient: &http.Client{Transport: bearer{key: key}}, DisableStandaloneSSE: true, MaxRetries: -1,
	}, nil)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() { _ = s.Close() })
	return s
}

func names(s *mcp.ClientSession) []string {
	GinkgoHelper()
	res, err := s.ListTools(context.Background(), nil)
	Expect(err).NotTo(HaveOccurred())
	out := []string{}
	for _, t := range res.Tools {
		out = append(out, t.Name)
	}
	return out
}

var _ = Describe("the built-in target behind the MCP gateway", func() {
	var (
		f     *fakeAgent
		guard *spyGuard
		audit *auditLog
		srv   *httptest.Server
		gw    *mcpgateway.Gateway
		scout gateway.IssuedKey
		evil  gateway.IssuedKey
		cfg   gateway.MCPConfig
	)

	send := func(s *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
		GinkgoHelper()
		r, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
		Expect(err).NotTo(HaveOccurred())
		return r
	}
	txt := func(r *mcp.CallToolResult) string {
		GinkgoHelper()
		Expect(r.Content).NotTo(BeEmpty())
		return r.Content[0].(*mcp.TextContent).Text
	}

	BeforeEach(func() {
		f = &fakeAgent{tasks: map[a2a.TaskID]*a2a.Task{}}
		f.send = func(*a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
			return f.put(withReply(task("t1", "c1", a2a.TaskStateCompleted, ""), "reply canary-untrusted")), nil
		}
		_, ra := serve(f)
		ra.Agents = []string{"scout"}
		keyFile := filepath.Join(GinkgoT().TempDir(), "handle-key")
		Expect(os.WriteFile(keyFile, []byte("koinon_test_not-a-real-handle-key"), 0o600)).To(Succeed())
		var err error
		scout, err = gateway.NewKey(gateway.AgentRef{Org: "acme", Name: "scout"})
		Expect(err).NotTo(HaveOccurred())
		evil, err = gateway.NewKey(gateway.AgentRef{Org: "evil", Name: "scout"})
		Expect(err).NotTo(HaveOccurred())
		cfg = gateway.MCPConfig{
			Keys: []gateway.Key{scout.Key, evil.Key},
			MCPState: gateway.MCPState{
				MCP: []gateway.MCPTarget{{Name: "acme-agents", Org: "acme", Builtin: gateway.BuiltinA2A, RemoteAgents: []gateway.MCPRemoteAgent{ra}, HandleKeyFile: keyFile}},
				Tools: map[gateway.AgentRef][]gateway.ToolGrant{
					scout.Agent: {{Target: "acme-agents", Tools: []string{"send_task", "get_task"}}},
					// A grant of another org's agent that names acme's target.
					evil.Agent: {{Target: "acme-agents", All: true}},
				},
			},
		}
		guard, audit = &spyGuard{}, &auditLog{}
		gw = mcpgateway.New(mcpgateway.Options{
			Guard: guard, Audit: audit, CallTimeout: 10 * time.Second, ListTimeout: 5 * time.Second,
			Builtins: map[string]mcpgateway.Builtin{gateway.BuiltinA2A: a2atools.New(a2atools.Options{Policy: policy(), RequestTimeout: 5 * time.Second, Wait: time.Second, PollInterval: 20 * time.Millisecond})},
		})
		_, err = gw.ApplyMCP(context.Background(), cfg)
		Expect(err).NotTo(HaveOccurred())
		mux := http.NewServeMux()
		mux.Handle("/mcp", gw.Handler())
		srv = httptest.NewServer(mux)
		DeferCleanup(srv.Close)
	})

	It("lists the granted tools of the target, with no card text", func() {
		s := connect(srv.URL, scout.Material)
		Expect(names(s)).To(ConsistOf("acme-agents_send_task", "acme-agents_get_task"))
		res, _ := s.ListTools(context.Background(), nil)
		for _, t := range res.Tools {
			Expect(t.Description).NotTo(ContainSubstring(cardName))
		}
	})

	It("runs a call, shows the reply to the guard as an upstream result, and audits it", func() {
		s := connect(srv.URL, scout.Material)
		r := send(s, "acme-agents_send_task", map[string]any{"agent": "kagent", "message": "hi"})
		Expect(r.IsError).To(BeFalse(), txt(r))
		Expect(txt(r)).To(ContainSubstring("canary-untrusted"))
		guard.mu.Lock()
		defer guard.mu.Unlock()
		var m *mcpgateway.Message
		for i := range guard.seen {
			if guard.seen[i].Dir == mcpgateway.FromUpstream && guard.seen[i].Kind == mcpgateway.KindResult {
				m = &guard.seen[i]
			}
		}
		Expect(m).NotTo(BeNil())
		Expect(m.Tool).To(Equal("send_task"))
		Expect(m.Org).To(Equal("acme"))
		Expect(string(m.Raw)).To(ContainSubstring("canary-untrusted"))
		Expect(audit.last().Outcome).To(Equal(mcpgateway.OutcomeOK))
		Expect(audit.last().Tool).To(Equal("send_task"))
	})

	It("hides a reply that the guard refuses", func() {
		guard.refuseRaw = "canary-untrusted"
		s := connect(srv.URL, scout.Material)
		r := send(s, "acme-agents_send_task", map[string]any{"agent": "kagent", "message": "hi"})
		Expect(r.IsError).To(BeTrue())
		Expect(txt(r)).NotTo(ContainSubstring("canary-untrusted"))
		Expect(audit.last().Outcome).To(Equal(mcpgateway.OutcomeRefusedGuard))
	})

	It("sends nothing to the remote agent when the call check refuses (the ask tier)", func() {
		guard.refuseAsk = true
		s := connect(srv.URL, scout.Material)
		r := send(s, "acme-agents_send_task", map[string]any{"agent": "kagent", "message": "hi"})
		Expect(r.IsError).To(BeTrue())
		Expect(txt(r)).To(ContainSubstring("a person said no"))
		Expect(f.sentCount()).To(BeZero())
		Expect(guard.calls).To(HaveLen(1))
		Expect(guard.calls[0].Tool).To(Equal("send_task"))
		Expect(guard.calls[0].Org).To(Equal("acme"))
	})

	It("refuses a tool that is not granted", func() {
		s := connect(srv.URL, scout.Material)
		r := send(s, "acme-agents_cancel_task", map[string]any{"agent": "kagent", "task_id": "x.y"})
		Expect(r.IsError).To(BeTrue())
		Expect(audit.last().Outcome).To(Equal(mcpgateway.OutcomeRefusedGrant))
	})

	It("audits a changed card as card_changed", func() {
		f.mu.Lock()
		f.card = func(c *a2a.AgentCard) { c.Description = "changed" }
		f.mu.Unlock()
		s := connect(srv.URL, scout.Material)
		r := send(s, "acme-agents_send_task", map[string]any{"agent": "kagent", "message": "hi"})
		Expect(r.IsError).To(BeTrue())
		Expect(txt(r)).To(ContainSubstring("agent card"))
		Expect(audit.last().Outcome).To(Equal(mcpgateway.OutcomeCardChanged))
		Expect(f.sentCount()).To(BeZero())
	})

	It("keeps the target of one org from the agents of another", func() {
		s := connect(srv.URL, evil.Material)
		Expect(names(s)).To(BeEmpty())
		r := send(s, "acme-agents_send_task", map[string]any{"agent": "kagent", "message": "hi"})
		Expect(r.IsError).To(BeTrue())
		Expect(f.sentCount()).To(BeZero())
	})

	It("drops a built-in target when the gateway has no code for it", func() {
		bare := mcpgateway.New(mcpgateway.Options{Guard: guard})
		_, err := bare.ApplyMCP(context.Background(), cfg)
		Expect(err).NotTo(HaveOccurred())
		mux := http.NewServeMux()
		mux.Handle("/mcp", bare.Handler())
		s2 := httptest.NewServer(mux)
		DeferCleanup(s2.Close)
		Expect(names(connect(s2.URL, scout.Material))).To(BeEmpty())
	})
})
