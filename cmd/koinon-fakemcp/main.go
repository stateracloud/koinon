// SPDX-License-Identifier: Apache-2.0

// Command koinon-fakemcp is a fake remote MCP server for tests and e2e (plan item MC1). It serves three
// tools (read_item, write_item and the fetch-style fetch_url, which names a destination) over Streamable HTTP at /mcp, refuses requests without the expected credential header, and
// records what it receives at /_fake/requests, so a test can check that the gateway injected the
// credential and what identity it forwarded. With -tls-cert-file and -tls-key-file it serves HTTPS
// (ADR 0120: a credential goes only over HTTPS). It never makes a certificate itself: a test run makes
// one CA and certificate (internal/faketls, cmd/koinon-faketls) and gives them as a Secret, so a restart
// of the pod keeps the same CA and the server record that trusts it stays valid.
package main

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stateracloud/koinon/internal/faketls"
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
	certFile := flag.String("tls-cert-file", "", "serve HTTPS with this PEM certificate (needs -tls-key-file)")
	keyFile := flag.String("tls-key-file", "", "the PEM private key of -tls-cert-file")
	httpAddr := flag.String("http-addr", "", "with TLS: also serve plain HTTP on this address (for a server that needs no credential, such as the OAuth case)")
	flag.Parse()
	if (*certFile == "") != (*keyFile == "") {
		slog.Error("-tls-cert-file and -tls-key-file go together")
		os.Exit(2)
	}
	if *httpAddr != "" && *certFile == "" {
		slog.Error("-http-addr needs TLS (without it, -addr already serves plain HTTP)")
		os.Exit(2)
	}
	if err := run(*addr, *httpAddr, *header, *value, *certFile, *keyFile); err != nil {
		slog.Error("koinon-fakemcp failed", "err", err)
		os.Exit(1)
	}
}

func run(addr, httpAddr, header, value, certFile, keyFile string) error {
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
	// It records the request, so a test sees that the add-time OAuth check came without a credential.
	mux.HandleFunc("/oauth-mcp", func(w http.ResponseWriter, r *http.Request) {
		recs.add(record{
			Time: time.Now(), Method: r.Method, Path: r.URL.Path, Authorized: r.Header.Get(header) == value,
			Agent: r.Header.Get("X-Koinon-Agent"), ClientAuthorization: r.Header.Get("Authorization") != "",
		})
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
	useTLS := certFile != ""
	if useTLS {
		// ADR 0120: Koinon refuses a credential over cleartext, so a test that sends a credential to this
		// server needs HTTPS. The certificate comes from the run's Secret (the key is never in the tree).
		cert, err := faketls.Load(certFile, keyFile)
		if err != nil {
			return fmt.Errorf("load the TLS certificate: %w", err)
		}
		hs.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hs.Shutdown(sctx)
	}()
	if httpAddr != "" {
		plain := &http.Server{Addr: httpAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			<-ctx.Done()
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = plain.Shutdown(sctx)
		}()
		go func() {
			if err := plain.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("plain HTTP listener failed", "err", err)
				stop()
			}
		}()
	}
	slog.Info("serving the fake MCP server", "addr", addr, "tls", useTLS, "httpAddr", httpAddr)
	serve := hs.ListenAndServe
	if useTLS {
		serve = func() error { return hs.ListenAndServeTLS("", "") }
	}
	if err := serve(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
