// SPDX-License-Identifier: Apache-2.0

package fakegitea

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/cgi" //nolint:gosec // test fixture; the Httpoxy fix is in every supported Go
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

var (
	backendOnce sync.Once
	backendPath string
)

// httpBackend locates git-http-backend, which serves the smart HTTP protocol.
func httpBackend() string {
	backendOnce.Do(func() {
		out, err := exec.CommandContext(context.Background(), "git", "--exec-path").Output()
		if err == nil {
			backendPath = filepath.Join(strings.TrimSpace(string(out)), "git-http-backend")
		}
	})
	return backendPath
}

// smartHTTP serves git clone, fetch and push over /{owner}/{repo}.git/... through
// git http-backend. Gitea answers 401 with a Basic challenge; git then retries with the
// credentials from its URL or helper. The token is the password.
func (f *Fake) smartHTTP(w http.ResponseWriter, r *http.Request) {
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 3)
	if len(parts) < 3 || !strings.HasSuffix(parts[1], ".git") {
		http.NotFound(w, r)
		return
	}
	login, ok := f.authenticate(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Basic realm="Gitea"`)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	name := strings.TrimSuffix(parts[1], ".git")
	f.mu.Lock()
	_, err := f.lookup(parts[0], name)
	f.mu.Unlock()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	be := httpBackend()
	if be == "" {
		http.Error(w, "git is not installed", http.StatusInternalServerError)
		return
	}
	// The owner directory on disk is f.Owner and the repository directory carries the name as
	// created, so match case-insensitively through the registry.
	f.mu.Lock()
	rp := f.repos[strings.ToLower(name)]
	f.mu.Unlock()
	r2 := r.Clone(r.Context())
	r2.URL.Path = "/" + f.Owner + "/" + rp.name + ".git/" + parts[2]
	// git sends a pack above http.postBuffer chunked; net/http/cgi refuses chunked bodies.
	if r2.ContentLength < 0 {
		body, err := io.ReadAll(r2.Body)
		if err != nil {
			http.Error(w, "reading request body: "+err.Error(), http.StatusBadRequest)
			return
		}
		r2.Body = io.NopCloser(bytes.NewReader(body))
		r2.ContentLength = int64(len(body))
		r2.TransferEncoding = nil
	}
	h := &cgi.Handler{
		Path: be,
		Env: append(slicesClone(gitBaseEnv),
			"GIT_PROJECT_ROOT="+f.root, "GIT_HTTP_EXPORT_ALL=1", "REMOTE_USER="+login,
			"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.receivepack", "GIT_CONFIG_VALUE_0=true"),
	}
	h.ServeHTTP(w, r2)
}
