// SPDX-License-Identifier: Apache-2.0

// Command koinon-fakegitea serves the Gitea fake from internal/testing/fakegitea as a binary,
// for the kind e2e test (plan item CH5). It is a test fixture, never a product component.
//
// Besides the Gitea REST endpoints (/api/v1) and git smart HTTP (/{owner}/{repo}.git), it
// serves two test-only endpoints:
//
//	GET /_fake/info            {"owner", "version", "login", "repositories"}  (never the token)
//	GET /_fake/repos/{repo}    the repository's state: branches, pulls, comments, write calls
//	GET /_fake/git             every git smart HTTP request: method, path, service, status and
//	                           whether it carried an Authorization header (never the header)
//
// With -tls-cert-file and -tls-key-file it serves HTTPS, as a real Gitea behind TLS: the gateway's
// egress listener sends a credential only over HTTPS (ADR 0120, #1086 part C2).
//
// The fake keeps its repositories as bare git repositories in the temporary directory and runs
// the git binary, so the image it runs in needs core git (not git-http-backend) and a writable
// TMPDIR. .ko.yaml points ko at a pinned Chainguard git base for this binary; mount an emptyDir
// at TMPDIR in the pod.
package main

import (
	"context"
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

	"github.com/stateracloud/koinon/internal/gateway/route"
	"github.com/stateracloud/koinon/internal/project"
	"github.com/stateracloud/koinon/internal/testing/fakegitea"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	owner := flag.String("owner", "koinon-demo", "organisation or user that owns the repositories")
	repos := flag.String("repos", "hello", "comma-separated repositories to create")
	token := flag.String("token", fakegitea.DefaultToken, "the access token the fake accepts (a fake; never a real credential)")
	readToken := flag.String("read-token", "", "a second token, read scope only (writes and pushes answer 403); empty for none")
	login := flag.String("login", fakegitea.DefaultLogin, "the login the token authenticates as")
	version := flag.String("gitea-version", "28.2.0", "version GET /api/v1/version reports")
	bearer := flag.Bool("allow-bearer", false, "also accept Authorization: Bearer <token>")
	certFile := flag.String("tls-cert-file", "", "serve HTTPS with this certificate (PEM); needs -tls-key-file")
	keyFile := flag.String("tls-key-file", "", "the key of -tls-cert-file (PEM)")
	flag.Parse()
	if (*certFile == "") != (*keyFile == "") {
		slog.Error("koinon-fakegitea: -tls-cert-file and -tls-key-file go together")
		os.Exit(2)
	}

	f := fakegitea.NewUnstarted()
	defer f.Close()
	f.Owner, f.Version, f.AllowBearer = *owner, *version, *bearer
	f.Tokens = map[string]string{*token: *login}
	if *readToken != "" {
		f.AddScopedToken(*readToken, *login, "read")
	}
	var names []string
	for r := range strings.SplitSeq(*repos, ",") {
		if r = strings.TrimSpace(r); r != "" {
			f.AddRepo(r)
			names = append(names, r)
		}
	}
	// Serve git or exit: a base image that cannot run git's services fails the rollout loudly.
	if err := f.CheckServe(); err != nil {
		slog.Error("koinon-fakegitea cannot serve git over HTTP", "err", err)
		f.Close()
		os.Exit(1) //nolint:gocritic // Close was called on the line above
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /_fake/info", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"owner": f.Owner, "version": f.Version, "login": *login, "repositories": names})
	})
	mux.HandleFunc("GET /_fake/repos/{repo}", func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recover() != nil {
				http.NotFound(w, r)
			}
		}()
		s := f.Snapshot(r.PathValue("repo"))
		writeJSON(w, map[string]any{
			"name": s.Name, "defaultBranch": s.DefaultBranch, "branches": s.Branches,
			"pulls": s.Pulls, "comments": s.Comments, "calls": s.Calls,
		})
	})
	git := &gitLog{}
	mux.Handle("GET /_fake/git", git)
	mux.Handle("/", git.record(f.Handler))

	srv := &http.Server{Addr: *addr, Handler: logRequests(mux), ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	slog.Info("starting koinon-fakegitea", "version", project.Version, "addr", *addr,
		"owner", f.Owner, "repositories", names, "tls", *certFile != "")
	serve := srv.ListenAndServe
	if *certFile != "" {
		serve = func() error { return srv.ListenAndServeTLS(*certFile, *keyFile) }
	}
	if err := serve(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("koinon-fakegitea failed", "err", err)
		f.Close()
		os.Exit(1) //nolint:gocritic // Close was called on the line above
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// logRequests logs each request's method, path and status, for the e2e test's diagnostics. It
// never logs headers, which carry the token.
func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(rec, r)
		slog.Info("request", "method", r.Method, "path", r.URL.Path, "status", rec.status)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// gitRequest is one git smart HTTP request the fake served. Auth says whether the request carried
// an Authorization header, and ActorToken whether it carried koinon-gateway's actor token header
// (the gateway removes it, so a request through the gateway never has it). No header value is kept.
type gitRequest struct {
	Method     string `json:"method"`
	Path       string `json:"path"`
	Service    string `json:"service,omitempty"`
	Status     int    `json:"status"`
	Auth       bool   `json:"auth"`
	ActorToken bool   `json:"actorToken"`
}

// gitLog keeps the git smart HTTP requests in arrival order (at most maxGitRequests), so the e2e
// test can see that a clone reached the fake with a credential it accepted.
type gitLog struct {
	mu   sync.Mutex
	reqs []gitRequest
}

const maxGitRequests = 1000

// record serves h and keeps each request to a path under /{owner}/{repo}.git/.
func (g *gitLog) record(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(rec, r)
		if !strings.Contains(r.URL.Path, ".git/") {
			return
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		if len(g.reqs) == maxGitRequests {
			g.reqs = g.reqs[1:]
		}
		g.reqs = append(g.reqs, gitRequest{
			Method: r.Method, Path: r.URL.Path, Service: r.URL.Query().Get("service"),
			Status: rec.status, Auth: r.Header.Get("Authorization") != "",
			ActorToken: r.Header.Get(route.TokenHeader) != "",
		})
	})
}

// ServeHTTP answers GET /_fake/git with the requests kept.
func (g *gitLog) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	g.mu.Lock()
	out := append([]gitRequest{}, g.reqs...)
	g.mu.Unlock()
	writeJSON(w, out)
}
