// SPDX-License-Identifier: Apache-2.0

// Command koinon-fakegithub serves the GitHub App fake from internal/connectors/github/githubtest
// as a binary, for the kind e2e test (plan item E1). It is a test fixture, never a product
// component: its App key is generated at start and handed out on /_fake/app.
//
// Besides the GitHub REST endpoints Koinon calls, it serves two test-only endpoints:
//
//	GET /_fake/app            {"appId", "installationId", "account", "privateKey"}
//	GET /_fake/repos/{repo}   the repository's state: refs, commits, pulls, write calls, and
//	                          the files of every branch
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

	"github.com/stateracloud/koinon/internal/connectors/github/githubtest"
	"github.com/stateracloud/koinon/internal/project"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	account := flag.String("account", "koinon-demo", "installation account (organisation login)")
	repos := flag.String("repos", "hello", "comma-separated repositories of the installation")
	appID := flag.Int64("app-id", 4242, "GitHub App ID")
	installationID := flag.Int64("installation-id", 777, "installation ID")
	flag.Parse()

	f := githubtest.NewUnstarted()
	f.Account, f.AppID, f.InstallationID = *account, *appID, *installationID
	f.Repositories = nil
	for r := range strings.SplitSeq(*repos, ",") {
		if r = strings.TrimSpace(r); r != "" {
			f.Repositories = append(f.Repositories, r)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /_fake/app", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"appId": f.AppID, "installationId": f.InstallationID, "account": f.Account, "privateKey": string(f.PEM),
		})
	})
	mux.HandleFunc("GET /_fake/repos/{repo}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("repo")
		snap := f.Snapshot(name)
		files := map[string]map[string]string{}
		for branch := range snap.Refs {
			files[branch] = map[string]string{}
			for p, b := range f.Files(name, branch) {
				files[branch][p] = string(b)
			}
		}
		writeJSON(w, map[string]any{
			"name": snap.Name, "defaultBranch": snap.DefaultBranch, "refs": snap.Refs, "commits": snap.Commits,
			"pulls": snap.Pulls, "comments": snap.Comments, "calls": snap.Calls, "files": files, "mints": f.Mints(),
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
	slog.Info("starting koinon-fakegithub", "version", project.Version, "addr", *addr,
		"account", f.Account, "repositories", f.Repositories)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("koinon-fakegithub failed", "err", err)
		os.Exit(1)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// logRequests logs each request's method, path and status, for the e2e test's diagnostics.
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
