// SPDX-License-Identifier: Apache-2.0

package harnesses_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/testing/fakemodel"
)

// hangMarker in a prompt makes the gateway hold the model request until the client gives up,
// so a turn can be cancelled while it waits for the model.
const hangMarker = "KOINON-HANG"

// gateway stands in for agentgateway: the fake model server under /v1 and a minimal MCP
// server with one tool and one prompt under /mcp, on one listener the containers reach through
// host.docker.internal.
type gateway struct {
	srv   *httptest.Server
	model *fakemodel.Server

	mu       sync.Mutex
	auth     []string // Authorization headers seen, model and MCP, empty ones left out
	anon     []string // "METHOD /path" of requests without an Authorization header
	mcpCalls []string // MCP methods called
	hung     chan struct{}
}

func newGateway(scripts ...fakemodel.Script) *gateway {
	model, err := fakemodel.New(scripts...)
	Expect(err).NotTo(HaveOccurred())
	g := &gateway{model: model, hung: make(chan struct{}, 16)}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/", g.serveModel)
	mux.HandleFunc("/mcp", g.serveMCP)

	// Containers connect from outside the loopback interface.
	var lc net.ListenConfig
	l, err := lc.Listen(context.Background(), "tcp", "0.0.0.0:0")
	Expect(err).NotTo(HaveOccurred())
	g.srv = &httptest.Server{Listener: l, Config: &http.Server{Handler: mux}}
	g.srv.Start()
	DeferCleanup(func() {
		g.srv.CloseClientConnections()
		g.srv.Close()
	})
	return g
}

// url is the gateway's address as seen from a container.
func (g *gateway) url(path string) string {
	port := g.srv.Listener.Addr().(*net.TCPAddr).Port
	return fmt.Sprintf("http://host.docker.internal:%d%s", port, path)
}

func (g *gateway) record(r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if a := r.Header.Get("Authorization"); a != "" {
		g.auth = append(g.auth, a)
	} else {
		g.anon = append(g.anon, r.Method+" "+r.URL.Path)
	}
}

func (g *gateway) serveModel(w http.ResponseWriter, r *http.Request) {
	g.record(r)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.Contains(lastUserText(body), hangMarker) {
		select {
		case g.hung <- struct{}{}:
		default:
		}
		<-r.Context().Done()
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	g.model.ServeHTTP(w, r)
}

// lastUserText returns the text of the last user message of an OpenAI chat request, so that a
// marker earlier in the history does not count.
func lastUserText(body []byte) string {
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &req) != nil {
		return ""
	}
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			return string(req.Messages[i].Content)
		}
	}
	return ""
}

// modelBodies returns the bodies of the chat requests received so far.
func (g *gateway) modelBodies() []string {
	var out []string
	for _, r := range g.model.Requests() {
		out = append(out, string(r.Body))
	}
	return out
}

func (g *gateway) authHeaders() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.auth...)
}

// anonymous returns the requests that carried no Authorization header.
func (g *gateway) anonymous() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.anon...)
}

func (g *gateway) mcpMethods() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.mcpCalls...)
}

// mcpTool is the one tool the stand-in MCP server offers.
const mcpTool = "koinon_contract_ping"

// readyPrompt is the one prompt it offers, as Koinon's MCP server does behind agentgateway
// (gateway.ReadyPrompt): a harness that turns MCP prompts into commands announces it once the
// server is connected, which koinon-agentd waits for (ADR 0052).
// agentgateway names it after its target (prefixMode always): github_koinon-ready.
const readyPrompt = "github_koinon-ready"

// serveMCP is the smallest Streamable HTTP MCP server a harness accepts: JSON responses to
// POSTed JSON-RPC requests, 202 for notifications, no server-initiated stream.
// https://modelcontextprotocol.io/specification/2025-06-18/basic/transports
func (g *gateway) serveMCP(w http.ResponseWriter, r *http.Request) {
	g.record(r)
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	g.mu.Lock()
	g.mcpCalls = append(g.mcpCalls, req.Method)
	g.mu.Unlock()
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
	switch req.Method {
	case "initialize":
		version := req.Params.ProtocolVersion
		if version == "" {
			version = "2025-06-18"
		}
		resp["result"] = map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}, "prompts": map[string]any{}},
			"serverInfo":      map[string]any{"name": "koinon-contract", "version": "0"},
		}
	case "tools/list":
		resp["result"] = map[string]any{"tools": []any{map[string]any{
			"name":        mcpTool,
			"description": "Koinon harness contract test tool. Returns pong.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		}}}
	case "prompts/list":
		resp["result"] = map[string]any{"prompts": []any{map[string]any{
			"name": readyPrompt, "description": "Koinon's tools are connected.",
		}}}
	case "prompts/get":
		resp["result"] = map[string]any{"messages": []any{map[string]any{
			"role": "user", "content": map[string]any{"type": "text", "text": "Koinon's tools are connected."},
		}}}
	case "tools/call":
		resp["result"] = map[string]any{"content": []any{map[string]any{"type": "text", "text": "pong"}}}
	case "ping":
		resp["result"] = map[string]any{}
	default:
		resp["error"] = map[string]any{"code": -32601, "message": "method not found"}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func newLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(GinkgoWriter, nil))
}
