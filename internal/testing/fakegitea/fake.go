// SPDX-License-Identifier: Apache-2.0

// Package fakegitea is a fake of the Gitea 28.2 REST endpoints (/api/v1) Koinon will use, plus
// git smart HTTP, for tests and the fake-server binary cmd/koinon-fakegitea. Tests never call a
// real Gitea.
//
// Repositories are bare git repositories in a temporary directory, so the REST view and git
// clone, fetch and push always agree. The REST side and the git transport shell out to the git
// binary (git http-backend serves the transport), so the process running the fake needs git.
// Pull requests, issues and comments live in memory.
//
// Shapes follow Gitea's swagger (v28.2.0, templates/swagger/v1-swagger.generated.json), keeping
// the fields a client reads. Differences from a real Gitea that tests may meet:
//
//   - Every repository is private and every token sees every repository of the owner.
//   - GET .../pulls ignores everything except state, page and limit.
//   - Opening a pull request whose head equals its base answers 422; Gitea words and codes
//     this case differently across versions.
//   - There is no merge, review, label, milestone or webhook support.
package fakegitea

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultToken is the token a new Fake accepts, mapped to the login DefaultLogin.
const (
	DefaultToken = "fake-gitea-token" //nolint:gosec // a fake, obviously
	DefaultLogin = "koinon-bot"
)

// Fake is a fake Gitea server for one owner (an organisation or user) with a set of
// repositories.
type Fake struct {
	// Owner is the login of the account that owns every repository.
	Owner string
	// Version is what GET /api/v1/version reports.
	Version string
	// Tokens maps each accepted token to the login it authenticates as. Set before serving
	// requests, or use AddToken.
	Tokens map[string]string
	// AllowBearer also accepts "Authorization: Bearer <token>", as a real Gitea does. It is off
	// by default so tests can prove a client sends one of the two documented forms.
	AllowBearer bool
	// Now defaults to time.Now.
	Now func() time.Time

	// Server is the test server New starts; nil for NewUnstarted.
	Server *httptest.Server
	// Handler serves the fake's API and git transport.
	Handler http.Handler

	root string

	mu     sync.Mutex
	repos  map[string]*repo // keyed by lower-case name
	nextID int64
}

// New starts a fake with owner "acme" and the default token. Close it with Close.
func New() *Fake {
	f := NewUnstarted()
	f.Server = httptest.NewServer(f.Handler)
	return f
}

// NewUnstarted returns a fake whose Handler the caller serves. Set the exported fields before
// serving requests. Close removes its repositories.
func NewUnstarted() *Fake {
	root, err := os.MkdirTemp("", "fakegitea-")
	if err != nil {
		panic(err)
	}
	f := &Fake{
		Owner: "acme", Version: "28.2.0", Now: time.Now, root: root,
		Tokens: map[string]string{DefaultToken: DefaultLogin},
		repos:  map[string]*repo{},
	}
	f.Handler = f.routes()
	return f
}

// Close stops the test server, if any, and deletes the repositories.
func (f *Fake) Close() {
	if f.Server != nil {
		f.Server.Close()
	}
	_ = os.RemoveAll(f.root)
}

// URL is the base URL of the test server started by New.
func (f *Fake) URL() string { return f.Server.URL }

// AddToken accepts token as the given login.
func (f *Fake) AddToken(token, login string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Tokens[token] = login
}

// errMissingToken and friends are the messages Gitea answers with.
const swaggerURL = "https://gitea.example/api/swagger"

type apiError struct {
	status int
	msg    string
}

func (e *apiError) Error() string { return e.msg }

func errf(status int, format string, a ...any) *apiError {
	return &apiError{status: status, msg: fmt.Sprintf(format, a...)}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	if ae := new(apiError); errors.As(err, &ae) {
		writeJSON(w, ae.status, map[string]string{"message": ae.msg, "url": swaggerURL})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error(), "url": swaggerURL})
}

// authenticate returns the login of the request's token. Gitea takes a token as the password
// of Basic auth (git and curl -u) or as "Authorization: token <t>"; any user name works with a
// token password, and a token alone in the user name is taken too.
func (f *Fake) authenticate(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	var token string
	switch scheme, rest, _ := strings.Cut(h, " "); {
	case strings.EqualFold(scheme, "token"):
		token = strings.TrimSpace(rest)
	case strings.EqualFold(scheme, "bearer") && f.AllowBearer:
		token = strings.TrimSpace(rest)
	case strings.EqualFold(scheme, "basic"):
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(rest))
		if err != nil {
			return "", false
		}
		user, pass, _ := strings.Cut(string(raw), ":")
		token = pass
		if token == "" {
			token = user
		}
	}
	if token == "" {
		return "", false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for t, login := range f.Tokens {
		if subtle.ConstantTimeCompare([]byte(t), []byte(token)) == 1 {
			return login, true
		}
	}
	return "", false
}

func (f *Fake) now() time.Time { return f.Now().UTC() }

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// baseURL is the externally visible root of this request, used in the url fields.
func baseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// page reads Gitea's page and limit parameters (default limit 30, maximum 50) and slices.
func paginate[T any](w http.ResponseWriter, r *http.Request, all []T) []T {
	q := r.URL.Query()
	pageN, _ := strconv.Atoi(q.Get("page"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	if pageN < 1 {
		pageN = 1
	}
	if limit < 1 {
		limit = 30
	}
	limit = min(limit, 50)
	w.Header().Set("X-Total-Count", strconv.Itoa(len(all)))
	start := min((pageN-1)*limit, len(all))
	return slices.Clone(all[start:min(start+limit, len(all))])
}
