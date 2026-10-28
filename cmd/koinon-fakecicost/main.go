// SPDX-License-Identifier: Apache-2.0

// Command koinon-fakecicost is a fake CI-cost service for the RT1 home-lab story test. It serves
// synthetic, deterministic CI cost data two ways: a REST endpoint, GET /v1/costs, and an MCP tool,
// ci_costs, over Streamable HTTP at /mcp. Both need a bearer token, so a test can show that
// agentgateway injects the Authorization header. Requests are recorded at /_fake/requests. One
// repository's cost doubles yesterday because of a new matrix job (see anomalyRepo).
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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stateracloud/koinon/internal/project"
)

// tokenEnv names the environment variable that carries the bearer token, when -token is not given.
const tokenEnv = "KOINON_FAKECICOST_TOKEN" //nolint:gosec // the name of an environment variable, not a credential

// record is one request the server received.
type record struct {
	Time       time.Time `json:"time"`
	Method     string    `json:"method"`
	Path       string    `json:"path"`
	Query      string    `json:"query,omitempty"`
	MCPMethod  string    `json:"mcpMethod,omitempty"`
	Authorized bool      `json:"authorized"`
	Agent      string    `json:"agent,omitempty"`
	// Tool is the tool a tools/call asked for and Args its raw arguments.
	Tool string `json:"tool,omitempty"`
	Args string `json:"args,omitempty"`
}

type requestLog struct {
	mu   sync.Mutex
	list []record
}

func (l *requestLog) add(r record) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.list = append(l.list, r)
}

func (l *requestLog) snapshot() []record {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]record{}, l.list...)
}

// server holds the handlers' state; now is replaceable for tests.
type server struct {
	token string
	now   func() time.Time
	recs  *requestLog
}

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	token := flag.String("token", os.Getenv(tokenEnv), "the bearer token to require (default: $"+tokenEnv+")")
	flag.Parse()
	if *token == "" {
		slog.Error("koinon-fakecicost needs a bearer token: set -token or " + tokenEnv)
		os.Exit(1)
	}
	if err := run(*addr, &server{token: *token, now: time.Now, recs: &requestLog{}}); err != nil {
		slog.Error("koinon-fakecicost failed", "err", err)
		os.Exit(1)
	}
}

func run(addr string, s *server) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	hs := &http.Server{Addr: addr, Handler: s.handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hs.Shutdown(sctx)
	}()
	slog.Info("serving the fake CI-cost service", "addr", addr)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// authorized reports whether the request carries the expected bearer token.
func (s *server) authorized(r *http.Request) bool {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) == 1
}

// guard records the request and refuses it with 401 unless it carries the bearer token.
func (s *server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok := s.authorized(r)
		s.recs.add(record{
			Time: time.Now(), Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
			MCPMethod: r.Header.Get("Mcp-Method"), Authorized: ok, Agent: r.Header.Get("X-Koinon-Agent"),
		})
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="fakecicost"`)
			http.Error(w, "bearer token required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /v1/costs", s.guard(http.HandlerFunc(s.restCosts)))

	srv := mcp.NewServer(&mcp.Implementation{Name: project.Name + "-fakecicost", Version: project.Version}, nil)
	mcp.AddTool(srv, &mcp.Tool{
		Name: "ci_costs",
		Description: "CI spend in USD per day, from first day to last day inclusive (UTC), optionally grouped by " +
			"repo, workflow or runner. Data covers the last 60 days.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(_ context.Context, req *mcp.CallToolRequest, in costsQuery) (*mcp.CallToolResult, costsResponse, error) {
		s.recs.add(record{Time: time.Now(), Method: "TOOL", Tool: "ci_costs", Args: string(req.Params.Arguments), Authorized: true, Agent: req.Extra.Header.Get("X-Koinon-Agent")})
		out, err := costs(in, s.now())
		if err != nil {
			return nil, costsResponse{}, err
		}
		b, _ := json.Marshal(out)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, out, nil
	})
	inner := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{DisableLocalhostProtection: true})
	mux.Handle("/mcp", s.guard(inner))

	mux.HandleFunc("GET /_fake/requests", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.recs.snapshot())
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	return mux
}

func (s *server) restCosts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	out, err := costs(costsQuery{From: q.Get("from"), To: q.Get("to"), GroupBy: q.Get("group_by")}, s.now())
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(out)
}
