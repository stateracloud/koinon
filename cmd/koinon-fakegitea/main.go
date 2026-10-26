// SPDX-License-Identifier: Apache-2.0

// Command koinon-fakegitea serves the Gitea fake from internal/testing/fakegitea as a binary,
// for the kind e2e test (plan item CH5). It is a test fixture, never a product component.
//
// Besides the Gitea REST endpoints (/api/v1) and git smart HTTP (/{owner}/{repo}.git), it
// serves two test-only endpoints:
//
//	GET /_fake/info            {"owner", "version", "login", "repositories"}  (never the token)
//	GET /_fake/repos/{repo}    the repository's state: branches, pulls, comments, write calls
//
// The fake keeps its repositories as bare git repositories in the temporary directory and runs
// the git binary, so the image it runs in needs git and a writable TMPDIR.
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
	"syscall"
	"time"

	"github.com/stateracloud/koinon/internal/project"
	"github.com/stateracloud/koinon/internal/testing/fakegitea"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	owner := flag.String("owner", "koinon-demo", "organisation or user that owns the repositories")
	repos := flag.String("repos", "hello", "comma-separated repositories to create")
	token := flag.String("token", fakegitea.DefaultToken, "the access token the fake accepts (a fake; never a real credential)")
	login := flag.String("login", fakegitea.DefaultLogin, "the login the token authenticates as")
	version := flag.String("gitea-version", "28.2.0", "version GET /api/v1/version reports")
	bearer := flag.Bool("allow-bearer", false, "also accept Authorization: Bearer <token>")
	flag.Parse()

	f := fakegitea.NewUnstarted()
	defer f.Close()
	f.Owner, f.Version, f.AllowBearer = *owner, *version, *bearer
	f.Tokens = map[string]string{*token: *login}
	var names []string
	for r := range strings.SplitSeq(*repos, ",") {
		if r = strings.TrimSpace(r); r != "" {
			f.AddRepo(r)
			names = append(names, r)
		}
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
	mux.Handle("/", f.Handler)

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
		"owner", f.Owner, "repositories", names)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
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
