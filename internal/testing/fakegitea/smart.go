// SPDX-License-Identifier: Apache-2.0

package fakegitea

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os/exec"
	"strings"
)

// The smart HTTP protocol (gitprotocol-http) over git's own stateless-rpc mode. The fake runs
// git upload-pack and git receive-pack directly, not git-http-backend: both are built into core git,
// while distributions ship git-http-backend in a split package (git-daemon on Alpine and Wolfi), and
// a base image without it answered every clone with a bare 500 (#1086 part C2). The fake always speaks
// protocol version 0, which every git client supports.
var smartServices = []string{"git-upload-pack", "git-receive-pack"}

// gitCommand is the git subcommand of a smart HTTP service, a constant: the service name comes from the
// request, and only these two run.
func gitCommand(svc string) (string, bool) {
	switch svc {
	case "git-upload-pack":
		return "upload-pack", true
	case "git-receive-pack":
		return "receive-pack", true
	}
	return "", false
}

// pktLine frames s as one git pkt-line.
func pktLine(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }

// service runs git's upload-pack or receive-pack on the bare repository at dir.
func service(ctx context.Context, name, dir, login string, advertise bool, stdin io.Reader, stdout io.Writer) error {
	sub, ok := gitCommand(name)
	if !ok {
		return fmt.Errorf("%q is not a git smart HTTP service", name)
	}
	args := []string{sub, "--stateless-rpc"}
	if advertise {
		args = append(args, "--advertise-refs")
	}
	// dir is the registry's path of a known repository, never the request's; -- ends the options.
	cmd := exec.CommandContext(ctx, "git", append(args, "--", dir)...) //nolint:gosec // fixed binary; the subcommand is a constant of gitCommand
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
		for _, name := range smartServices {
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
	rp, err := f.lookup(parts[0], name)
	perm := f.permission(login, name)
	scope := f.scopes[token]
	f.mu.Unlock()
	if err != nil || perm == "" {
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
	if _, ok := gitCommand(svc); !ok {
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
			serviceFailed(w, svc, err)
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
		serviceFailed(w, svc, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-"+svc+"-result")
	_, _ = w.Write(out.Bytes())
}

// serviceFailed logs git's error, which names the repository's path on disk, and answers with a short
// body.
func serviceFailed(w http.ResponseWriter, svc string, err error) {
	slog.Warn("koinon-fakegitea: git failed", "service", svc, "err", err)
	http.Error(w, svc+" failed; see the fake's log", http.StatusInternalServerError)
}
