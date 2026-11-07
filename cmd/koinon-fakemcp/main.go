// SPDX-License-Identifier: Apache-2.0

// Command koinon-fakemcp is a fake remote MCP server for tests and e2e (plan item MC1). It serves three
// tools (read_item, write_item and the fetch-style fetch_url, which names a destination) over Streamable HTTP at /mcp, refuses requests without the expected credential header, and
// records what it receives at /_fake/requests, so a test can check that the gateway injected the
// credential and what identity it forwarded.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stateracloud/koinon/internal/project"
)

// record is one request the server received.
type record struct {
	Time   time.Time `json:"time"`
	Method string    `json:"method"`
	Path   string    `json:"path"`
	// MCPMethod is agentgateway's Mcp-Method header, when it sent one.
	MCPMethod string `json:"mcpMethod,omitempty"`
	// Authorized says the credential header matched.
	Authorized bool `json:"authorized"`
	// Agent is the X-Koinon-Agent header agentgateway sets.
	Agent string `json:"agent,omitempty"`
	// ClientAuthorization says an Authorization header arrived that is not the credential (the
	// gateway must not forward the agent's own key).
	ClientAuthorization bool `json:"clientAuthorization,omitempty"`
	// Tool is the tool a tools/call asked for and Args its raw arguments; set by the tool handlers.
	Tool string `json:"tool,omitempty"`
	Args string `json:"args,omitempty"`
}

type log struct {
	mu   sync.Mutex
	list []record
}

func (l *log) add(r record) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.list = append(l.list, r)
	return len(l.list) - 1
}

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	header := flag.String("header", "X-Api-Key", "the credential header")
	value := flag.String("value", "not-a-real-token", "the credential header's expected value")
	flag.Parse()
	if err := run(*addr, *header, *value); err != nil {
		slog.Error("koinon-fakemcp failed", "err", err)
		os.Exit(1)
	}
}

func run(addr, header, value string) error {
	recs := &log{}
	srv := mcp.NewServer(&mcp.Implementation{Name: project.Name + "-fakemcp", Version: project.Version}, nil)
	type args struct {
		Title string `json:"title,omitempty"`
	}
	for _, tool := range []string{"read_item", "write_item"} {
		mcp.AddTool(srv, &mcp.Tool{
			Name: tool, Description: "Fake tool " + tool,
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: tool == "read_item"},
		}, func(_ context.Context, req *mcp.CallToolRequest, in args) (*mcp.CallToolResult, any, error) {
			recs.add(record{Time: time.Now(), Method: "TOOL", Tool: tool, Args: string(req.Params.Arguments), Authorized: true, Agent: req.Extra.Header.Get("X-Koinon-Agent")})
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: tool + " ok: " + in.Title}}}, nil, nil
		})
	}
	// fetch_url is the fetch-style tool: an argument that names a destination. Like the others it only
	// records the call, so a request the leak guard let through shows in /_fake/requests (#348).
	type fetchArgs struct {
		URL string `json:"url"`
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fetch_url", Description: "Fake tool fetch_url: fetches a URL",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false},
	}, func(_ context.Context, req *mcp.CallToolRequest, in fetchArgs) (*mcp.CallToolResult, any, error) {
		recs.add(record{Time: time.Now(), Method: "TOOL", Tool: "fetch_url", Args: string(req.Params.Arguments), Authorized: true, Agent: req.Extra.Header.Get("X-Koinon-Agent")})
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "fetch_url ok: " + in.URL}}}, nil, nil
	})
	inner := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{DisableLocalhostProtection: true})
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		ok := subtle.ConstantTimeCompare([]byte(r.Header.Get(header)), []byte(value)) == 1
		recs.add(record{
			Time: time.Now(), Method: r.Method, Path: r.URL.Path, MCPMethod: r.Header.Get("Mcp-Method"), Authorized: ok,
			Agent: r.Header.Get("X-Koinon-Agent"), ClientAuthorization: r.Header.Get("Authorization") != "",
		})
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="fakemcp"`)
			http.Error(w, "credential required", http.StatusUnauthorized)
			return
		}
		inner.ServeHTTP(w, r)
	})
	// A server that wants per-user OAuth, as the MCP authorization specification describes it: a 401
	// whose WWW-Authenticate names the protected resource metadata.
	mux.HandleFunc("/oauth-mcp", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="http://fakemcp/.well-known/oauth-protected-resource"`)
		http.Error(w, "sign in", http.StatusUnauthorized)
	})
	mux.HandleFunc("GET /_fake/requests", func(w http.ResponseWriter, _ *http.Request) {
		recs.mu.Lock()
		defer recs.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(recs.list)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	hs := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hs.Shutdown(sctx)
	}()
	slog.Info("serving the fake MCP server", "addr", addr)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
