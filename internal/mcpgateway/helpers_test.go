// SPDX-License-Identifier: Apache-2.0

package mcpgateway_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/mcpgateway"
)

// fakeToken is obviously fake, as the gitleaks rule needs.
const fakeToken = "koinon_test_not-a-real-token"

// upstreamLog records what a fake upstream server saw.
type upstreamLog struct {
	mu      sync.Mutex
	headers []http.Header
	roots   []string
	metas   []map[string]any // the _meta of each read_item call
}

func (l *upstreamLog) meta() []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.metas)
}

func (l *upstreamLog) header(name string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, h := range l.headers {
		if v := h.Get(name); v != "" {
			out = append(out, v)
		}
	}
	return out
}

type echoArgs struct {
	Text string `json:"text,omitempty"`
}

// fakeUpstream is an MCP server with the tools read_item, write_item, roots (lists the client's
// roots), sample (asks for sampling), elicit (asks for elicitation), fail (a tool error) and log
// (sends a progress notification).
func fakeUpstream(name string, l *upstreamLog) *httptest.Server {
	newServer := func(*http.Request) *mcp.Server {
		s := mcp.NewServer(&mcp.Implementation{Name: name, Version: "0"}, nil)
		mcp.AddTool(s, &mcp.Tool{Name: "read_item", Description: "reads " + name}, func(_ context.Context, r *mcp.CallToolRequest, a echoArgs) (*mcp.CallToolResult, any, error) {
			l.mu.Lock()
			l.metas = append(l.metas, map[string]any(r.Params.Meta))
			l.mu.Unlock()
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: name + " read " + a.Text}}}, nil, nil
		})
		mcp.AddTool(s, &mcp.Tool{Name: "write_item", Description: "writes " + name}, func(context.Context, *mcp.CallToolRequest, echoArgs) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: name + " wrote"}}}, nil, nil
		})
		mcp.AddTool(s, &mcp.Tool{Name: "fail"}, func(context.Context, *mcp.CallToolRequest, echoArgs) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "failed"}}}, nil, nil
		})
		mcp.AddTool(s, &mcp.Tool{Name: "roots"}, func(ctx context.Context, r *mcp.CallToolRequest, _ echoArgs) (*mcp.CallToolResult, any, error) {
			res, err := r.Session.ListRoots(ctx, nil) //nolint:staticcheck // the gateway must answer the deprecated roots/list (ADR 0097)
			if err != nil {
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "roots: " + err.Error()}}}, nil, nil
			}
			var uris []string
			for _, root := range res.Roots {
				uris = append(uris, root.URI)
			}
			l.mu.Lock()
			l.roots = uris
			l.mu.Unlock()
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: strings.Join(uris, ",")}}}, nil, nil
		})
		mcp.AddTool(s, &mcp.Tool{Name: "sample"}, func(ctx context.Context, r *mcp.CallToolRequest, _ echoArgs) (*mcp.CallToolResult, any, error) {
			// A server that still asks for sampling is what the gateway refuses.
			msg := &mcp.SamplingMessage{Role: "user", Content: &mcp.TextContent{Text: "canary-sampling"}}                         //nolint:staticcheck // see above
			_, err := r.Session.CreateMessage(ctx, &mcp.CreateMessageParams{MaxTokens: 1, Messages: []*mcp.SamplingMessage{msg}}) //nolint:staticcheck // see above
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "sampling: " + errText(err)}}}, nil, nil
		})
		mcp.AddTool(s, &mcp.Tool{Name: "log"}, func(ctx context.Context, r *mcp.CallToolRequest, _ echoArgs) (*mcp.CallToolResult, any, error) {
			_ = r.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{ProgressToken: "t", Progress: 1, Message: "canary-log"})
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "logged"}}}, nil, nil
		})
		return s
	}
	h := mcp.NewStreamableHTTPHandler(newServer, nil)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		l.headers = append(l.headers, r.Header.Clone())
		l.mu.Unlock()
		h.ServeHTTP(w, r)
	}))
}

func errText(err error) string {
	if err == nil {
		return "ok"
	}
	return "error"
}

// recordingGuard keeps what it saw, and refuses messages that carry a marker word.
type recordingGuard struct {
	mu         sync.Mutex
	seen       []mcpgateway.Message
	violations []mcpgateway.Violation
	refuse     map[string]string // a substring of Raw -> reason
}

func (g *recordingGuard) Inspect(_ context.Context, m mcpgateway.Message) mcpgateway.Verdict {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.seen = append(g.seen, m)
	for sub, reason := range g.refuse {
		if m.Dir == mcpgateway.FromUpstream && strings.Contains(string(m.Raw), sub) {
			return mcpgateway.Verdict{Refuse: true, Reason: reason}
		}
	}
	return mcpgateway.Verdict{}
}

func (g *recordingGuard) Violation(_ context.Context, _ mcpgateway.Message, v mcpgateway.Violation) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.violations = append(g.violations, v)
}

func (g *recordingGuard) sawKind(dir mcpgateway.Direction, kind mcpgateway.Kind, method string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, m := range g.seen {
		if m.Dir == dir && m.Kind == kind && (method == "" || m.Method == method) {
			return true
		}
	}
	return false
}

func (g *recordingGuard) sawText(s string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, m := range g.seen {
		if strings.Contains(string(m.Raw), s) {
			return true
		}
	}
	return false
}

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

// bearer sets the gateway key on each request.
type bearer struct{ key string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.key)
	return http.DefaultTransport.RoundTrip(r)
}

func connect(ctx context.Context, url, key string) *mcp.ClientSession {
	GinkgoHelper()
	c := mcp.NewClient(&mcp.Implementation{Name: "koinon-test", Version: "0"}, nil)
	s, err := c.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: url + "/mcp", HTTPClient: &http.Client{Transport: bearer{key: key}}, DisableStandaloneSSE: true, MaxRetries: -1,
	}, nil)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() { _ = s.Close() })
	return s
}

func toolNames(ctx context.Context, s *mcp.ClientSession) []string {
	GinkgoHelper()
	res, err := s.ListTools(ctx, nil)
	Expect(err).NotTo(HaveOccurred())
	names := []string{}
	for _, t := range res.Tools {
		names = append(names, t.Name)
	}
	return names
}

func text(res *mcp.CallToolResult) string {
	GinkgoHelper()
	Expect(res.Content).NotTo(BeEmpty())
	return res.Content[0].(*mcp.TextContent).Text
}

func writeFile(name, content string) string {
	GinkgoHelper()
	p := filepath.Join(GinkgoT().TempDir(), name)
	Expect(os.WriteFile(p, []byte(content), 0o600)).To(Succeed())
	return p
}

func mustKey(a gateway.AgentRef) gateway.IssuedKey {
	GinkgoHelper()
	k, err := gateway.NewKey(a)
	Expect(err).NotTo(HaveOccurred())
	return k
}
