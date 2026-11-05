// SPDX-License-Identifier: Apache-2.0

package gatewaytest

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stateracloud/koinon/internal/gateway"
)

// ProviderErrors maps the models the acme fake provider answers with an error status. The 429 with
// the "-retry" name also sends x-should-retry: true, which the gateway must leave alone.
var ProviderErrors = map[string]int{
	"provider-400":       http.StatusBadRequest,
	"provider-403":       http.StatusForbidden,
	"provider-429":       http.StatusTooManyRequests,
	"provider-429-retry": http.StatusTooManyRequests,
	"provider-500":       http.StatusInternalServerError,
}

// fakeUpstream is an OpenAI-compatible chat completions server that reports
// 10 input and 10 output tokens per request and counts requests in hits.
func fakeUpstream(hits *atomic.Int64) *httptest.Server {
	return newServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		// A provider that answers with an error status, to tell its answers from the gateway's own
		// refusals (the do-not-retry marker, koinon-private#342).
		if status, ok := ProviderErrors[req.Model]; ok {
			if req.Model == "provider-429-retry" {
				w.Header().Set(gateway.DoNotRetryHeader, "true")
			}
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "provider says " + strconv.Itoa(status)}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-test", "object": "chat.completion", "created": time.Now().Unix(), "model": req.Model,
			"choices": []any{map[string]any{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "ok"},
			}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 10, "total_tokens": 20},
		})
	}))
}

// taggedUpstream is fakeUpstream that names itself in every answer, counts its requests
// and keeps the last request body, so a spec can tell which org's provider answered and
// what the gateway forced into the request.
type taggedUpstream struct {
	*httptest.Server
	hits atomic.Int64
	last atomic.Value // string: the last request body
}

func newTaggedUpstream(tag string) *taggedUpstream {
	u := &taggedUpstream{}
	u.Server = newServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		raw, _ := io.ReadAll(r.Body)
		u.last.Store(string(raw))
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(raw, &req)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-test", "object": "chat.completion", "created": time.Now().Unix(), "model": req.Model,
			"choices": []any{map[string]any{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "answered by " + tag},
			}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 10, "total_tokens": 20},
		})
	}))
	return u
}

func (u *taggedUpstream) lastBody() string {
	b, _ := u.last.Load().(string)
	return b
}

// fakeTools is an MCP server with the two parking tools and a bypass tool
// that no agent is granted (spike step 6). Each tool answers with the agent
// header it received.
func fakeTools() *httptest.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "fake-tools", Version: "0.0.1"}, nil)
	type args struct {
		Title string `json:"title,omitempty"`
	}
	for _, name := range []string{"create_pull_request", "get_approval_status", "direct_create_pull_request"} {
		mcp.AddTool(srv, &mcp.Tool{Name: name, Description: name},
			func(_ context.Context, req *mcp.CallToolRequest, _ args) (*mcp.CallToolResult, any, error) {
				agent := strings.Join(req.Extra.Header.Values(gateway.AgentHeader), ",")
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: name + " reached by " + agent}}}, nil, nil
			})
	}
	// The readiness prompt every agent must see, and one no rule admits.
	for _, name := range []string{gateway.ReadyPrompt, "hidden_prompt"} {
		srv.AddPrompt(&mcp.Prompt{Name: name, Description: name},
			func(context.Context, *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
				return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: name}}}}, nil
			})
	}
	// agentgateway reaches it through the testcontainers SSH tunnel, which
	// arrives on localhost with Host: host.testcontainers.internal.
	return newServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{DisableLocalhostProtection: true}))
}

func port(s *httptest.Server) int {
	_, p, _ := net.SplitHostPort(s.Listener.Addr().String())
	n, _ := strconv.Atoi(p)
	return n
}

type bearer struct {
	key  string
	base http.RoundTripper
	// spoof, when set, is sent as the agent header, which agentgateway must
	// overwrite.
	spoof string
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.key)
	if b.spoof != "" {
		r.Header.Set(gateway.AgentHeader, b.spoof)
	}
	return b.base.RoundTrip(r)
}

// headerLog records the headers of the requests a fake remote MCP server received.
type headerLog struct {
	mu   sync.Mutex
	seen []http.Header
}

func (l *headerLog) add(h http.Header) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen = append(l.seen, h.Clone())
}

func (l *headerLog) values(name string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, h := range l.seen {
		out = append(out, h.Values(name)...)
	}
	return out
}

// fakeRemote is a remote MCP server with two tools that answers each call with the credential
// header it received, and records every request's headers.
func fakeRemote(name string, log *headerLog, credHeader string) *httptest.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: name, Version: "0.0.1"}, nil)
	type args struct{}
	for _, tool := range []string{"read_item", "write_item"} {
		mcp.AddTool(srv, &mcp.Tool{Name: tool, Description: tool, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: tool == "read_item"}},
			func(_ context.Context, req *mcp.CallToolRequest, _ args) (*mcp.CallToolResult, any, error) {
				got := strings.Join(req.Extra.Header.Values(credHeader), ",")
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: name + " saw " + got}}}, nil, nil
			})
	}
	inner := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{DisableLocalhostProtection: true})
	return newServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.add(r.Header)
		inner.ServeHTTP(w, r)
	}))
}

type headerSetter struct {
	key, name, value string
	base             http.RoundTripper
}

func (h headerSetter) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+h.key)
	r.Header.Set(h.name, h.value)
	return h.base.RoundTrip(r)
}

func itoa(n int) string { return strconv.Itoa(n) }

// newServer starts h on all interfaces, not only loopback: a gateway in a kind cluster reaches the
// test process at the address of the Docker network's gateway, where a loopback listener is not
// reachable. The servers are fakes with dummy credentials that live for one spec run.
func newServer(h http.Handler) *httptest.Server {
	s := httptest.NewUnstartedServer(h)
	l, err := net.Listen("tcp", "0.0.0.0:0") //nolint:gosec,noctx // see above: the kind cluster reaches the test host over the network
	if err != nil {
		panic(err)
	}
	_ = s.Listener.Close()
	s.Listener = l
	s.Start()
	return s
}
