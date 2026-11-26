// SPDX-License-Identifier: Apache-2.0

package fakegitea

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
)

// The smart HTTP protocol (gitprotocol-http) over git's own stateless-rpc mode. The fake runs
// git upload-pack and git receive-pack directly, not git-http-backend: both are built into core git,
// while distributions ship git-http-backend in a split package (git-daemon on Alpine and Wolfi), and
// a base image without it answered every clone with a bare 500 (#1086 part C2). The fake always speaks
// protocol version 0, which every git client supports.
var smartServices = map[string]bool{"git-upload-pack": true, "git-receive-pack": true}

// pktLine frames s as one git pkt-line.
func pktLine(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }

// service runs git's upload-pack or receive-pack on the bare repository at dir.
func service(ctx context.Context, name, dir, login string, advertise bool, stdin io.Reader, stdout io.Writer) error {
	args := []string{strings.TrimPrefix(name, "git-"), "--stateless-rpc"}
	if advertise {
		args = append(args, "--advertise-refs")
	}
	cmd := exec.CommandContext(ctx, "git", append(args, dir)...) //nolint:gosec // fixed binary; name is one of smartServices
	// A push writes the login into reflogs where they are on, as git-http-backend does with REMOTE_USER.
	cmd.Env = append(slicesClone(gitBaseEnv), "GIT_COMMITTER_NAME="+login, "GIT_COMMITTER_EMAIL="+login+"@fakegitea.invalid")
	cmd.Stdin, cmd.Stdout = stdin, stdout
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return nil
}

// CheckServe runs the ref advertisement of both services on every repository, the first step of
// every clone and push. koinon-fakegitea calls it at startup and exits when it fails, so a base image
// that cannot serve git fails the rollout instead of answering clones with 500.
func (f *Fake) CheckServe() error {
	f.mu.Lock()
	dirs := make([]string, 0, len(f.repos))
	for _, rp := range f.repos {
		dirs = append(dirs, rp.dir)
	}
	f.mu.Unlock()
	for _, dir := range dirs {
		for name := range smartServices {
			if err := service(context.Background(), name, dir, "check", true, nil, io.Discard); err != nil {
				return err
			}
		}
	}
	return nil
}

// smartHTTP serves git clone, fetch and push over /{owner}/{repo}.git/... Gitea answers 401 with a
// Basic challenge; git then retries with the credentials from its URL or helper. The token is the
// password.
func (f *Fake) smartHTTP(w http.ResponseWriter, r *http.Request) {
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 3)
	if len(parts) < 3 || !strings.HasSuffix(parts[1], ".git") {
		http.NotFound(w, r)
		return
	}
	token, login, ok := f.identify(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Basic realm="Gitea"`)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	name := strings.TrimSuffix(parts[1], ".git")
	f.mu.Lock()
	_, err := f.lookup(parts[0], name)
	perm := f.permission(login, name)
	scope := f.scopes[token]
	// The repository directory carries the name as created, so match case-insensitively.
	rp := f.repos[strings.ToLower(name)]
	f.mu.Unlock()
	if err != nil || perm == "" || rp == nil {
		http.NotFound(w, r)
		return
	}
	var svc string
	advertise := false
	switch {
	case parts[2] == "info/refs" && r.Method == http.MethodGet:
		svc, advertise = r.URL.Query().Get("service"), true
	case r.Method == http.MethodPost:
		svc = parts[2]
	}
	if !smartServices[svc] {
		// Only the smart protocol: git-http-backend would also serve the dumb one, no Koinon client uses it.
		http.Error(w, "the fake serves only the smart HTTP protocol", http.StatusForbidden)
		return
	}
	// A push is the receive-pack service, announced by info/refs and then posted.
	if svc == "git-receive-pack" && (scope == "read" || !canWrite(perm)) {
		http.Error(w, "token does not have write access to this repository", http.StatusForbidden)
		return
	}
	w.Header().Set("Cache-Control", "no-cache, max-age=0, must-revalidate")
	if advertise {
		var out bytes.Buffer
		if err := service(r.Context(), svc, rp.dir, login, true, nil, &out); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/x-"+svc+"-advertisement")
		_, _ = io.WriteString(w, pktLine("# service="+svc+"\n")+"0000")
		_, _ = w.Write(out.Bytes())
		return
	}
	if r.Header.Get("Content-Type") != "application/x-"+svc+"-request" {
		http.Error(w, "unexpected Content-Type "+r.Header.Get("Content-Type"), http.StatusUnsupportedMediaType)
		return
	}
	body := io.Reader(r.Body)
	if r.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, "reading the gzip request body: "+err.Error(), http.StatusBadRequest)
			return
		}
		defer func() { _ = zr.Close() }()
		body = zr
	}
	// Buffer the answer, so a failure is a 500 and not a cut stream.
	var out bytes.Buffer
	if err := service(r.Context(), svc, rp.dir, login, false, body, &out); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-"+svc+"-result")
	_, _ = w.Write(out.Bytes())
}
