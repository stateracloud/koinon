// SPDX-License-Identifier: Apache-2.0

// Package githubtest is a fake of the GitHub App REST endpoints Koinon calls,
// for tests: it verifies App JWTs against the App's public key and narrows
// installation tokens the way GitHub documents. Tests never call GitHub.
package githubtest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Fake is a fake GitHub API for one App and one installation.
type Fake struct {
	AppID          int64
	InstallationID int64
	Account        string
	// Repositories the installation can access (names, without the owner).
	Repositories []string
	// Private marks repositories (by name) as private; the rest are public.
	Private map[string]bool
	// Internal marks repositories (by name) as GitHub Enterprise "internal": private=true and
	// visibility "internal", readable by the whole enterprise.
	Internal map[string]bool
	// OmitVisibility leaves the "visibility" field out of repository objects, as an older GitHub
	// Enterprise Server does: only "private" is sent, true for internal repositories too.
	OmitVisibility bool
	// Permissions the App was granted.
	Permissions map[string]string
	// Now defaults to time.Now; tokens expire an hour after Now.
	Now func() time.Time
	// LieAboutPermissions makes token responses claim every App permission,
	// whatever was requested, to test that clients check responses.
	LieAboutPermissions bool
	// TruncateTrees makes recursive tree listings answer only the root's entries, marked
	// truncated, as GitHub does for a very large repository.
	TruncateTrees bool

	// Prefix is the path the REST API is served under (NewGHES); empty for the API root.
	Prefix string

	// Server is the test server New starts; nil for NewUnstarted.
	Server *httptest.Server
	// Handler serves the fake's API.
	Handler http.Handler
	// PEM is the App's private key, as GitHub's downloaded .pem file.
	PEM []byte

	key *rsa.PrivateKey

	mu        sync.Mutex
	mints     int
	listCalls int
	requests  []Request
	tokens    map[string]tokenInfo
	repos     map[string]*Repo
	polls     map[string]*pollFeed
}

// tokenInfo is what a minted token may do.
type tokenInfo struct {
	repos []string // names, when the token was minted for selected repositories
	all   bool     // minted for the whole installation: covers Repositories as they are now
	perms map[string]string
}

// covers is the repositories a token reaches now. As on GitHub, a token follows the installation:
// one minted for the whole installation covers what it holds now, and a repository that leaves the
// installation leaves every token, also one minted for selected repositories (which never gains a
// repository it was not minted for). The caller holds f.mu.
func (f *Fake) covers(info tokenInfo) []string {
	if info.all {
		return f.Repositories
	}
	out := make([]string, 0, len(info.repos))
	for _, r := range info.repos {
		if slices.Contains(f.Repositories, r) {
			out = append(out, r)
		}
	}
	return out
}

// Request is a token request the fake received.
type Request struct {
	Repositories []string
	Permissions  map[string]string
}

// New starts a fake with a fresh 2048-bit key. Close it with f.Server.Close.
func New() *Fake {
	f := NewUnstarted()
	f.Server = httptest.NewServer(f.Handler)
	return f
}

// GHESPrefix is where GitHub Enterprise Server serves its REST API on the web host.
const GHESPrefix = "/api/v3"

// NewGHES starts a fake that serves like a GitHub Enterprise Server: over TLS with a certificate
// no system root signs (trust CAPEM), and the REST API under GHESPrefix. Point a connection's
// baseUrl at WebURL and its caBundle at CAPEM. Close it with f.Server.Close.
func NewGHES() *Fake {
	f := NewUnstarted()
	f.Prefix = GHESPrefix
	mux := http.NewServeMux()
	mux.Handle(GHESPrefix+"/", http.StripPrefix(GHESPrefix, f.Handler))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
	})
	f.Server = httptest.NewTLSServer(mux)
	return f
}

// CAPEM is the PEM certificate that signs a NewGHES fake's TLS certificate; empty for a plain one.
func (f *Fake) CAPEM() string {
	if f.Server == nil || f.Server.TLS == nil {
		return ""
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.Server.Certificate().Raw}))
}

// WebURL is the base URL of the host: where a connection's baseUrl points.
func (f *Fake) WebURL() string { return f.Server.URL }

// NewUnstarted returns a fake with a fresh 2048-bit key whose Handler the caller serves, as
// cmd/koinon-fakegithub does. Set the exported fields before serving requests.
func NewUnstarted() *Fake {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	f := &Fake{
		AppID: 4242, InstallationID: 777, Account: "acme",
		Repositories: []string{"widgets", "gadgets"},
		Permissions:  map[string]string{"metadata": "read", "contents": "write", "pull_requests": "write", "issues": "write"},
		Now:          time.Now,
		key:          key,
		tokens:       map[string]tokenInfo{},
		repos:        map[string]*Repo{},
		PEM:          pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /app", f.app)
	mux.HandleFunc("GET /app/installations/{id}", f.installation)
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", f.accessToken)
	f.routeRepos(mux)
	f.routePolls(mux)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
	})
	f.Handler = mux
	return f
}

// canonical returns the installation's spelling of a repository name, which GitHub matches
// case-insensitively.
func (f *Fake) canonical(name string) (string, bool) {
	i := slices.IndexFunc(f.Repositories, func(r string) bool { return strings.EqualFold(r, name) })
	if i < 0 {
		return "", false
	}
	return f.Repositories[i], true
}

// URL is the REST API's base URL.
func (f *Fake) URL() string { return f.Server.URL + f.Prefix }

// Mints returns how many tokens the fake has minted.
func (f *Fake) Mints() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mints
}

// ResetTokens forgets the tokens minted and requested so far.
func (f *Fake) ResetTokens() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mints, f.requests = 0, nil
}

// Requests returns the token requests received.
func (f *Fake) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// authorise checks the App JWT as GitHub does: RS256 over the App's key,
// iss = app ID, iat in the past, exp at most ten minutes after now.
func (f *Fake) authorise(w http.ResponseWriter, r *http.Request) bool {
	fail := func(msg string) bool {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": msg})
		return false
	}
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return fail("A JSON web token could not be decoded")
	}
	if r.Header.Get("X-GitHub-Api-Version") == "" || r.Header.Get("Accept") != "application/vnd.github+json" {
		return fail("missing API version or Accept header")
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return fail("A JSON web token could not be decoded")
	}
	var header struct{ Alg string }
	if hb, err := base64.RawURLEncoding.DecodeString(parts[0]); err != nil || json.Unmarshal(hb, &header) != nil || header.Alg != "RS256" {
		return fail("'alg' must be RS256")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return fail("A JSON web token could not be decoded")
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(&f.key.PublicKey, crypto.SHA256, digest[:], sig) != nil {
		return fail("A JSON web token could not be decoded")
	}
	var claims struct {
		Iat, Exp int64
		Iss      string
	}
	cb, _ := base64.RawURLEncoding.DecodeString(parts[1])
	if json.Unmarshal(cb, &claims) != nil {
		return fail("A JSON web token could not be decoded")
	}
	now := f.Now().Unix()
	switch {
	case claims.Iss != strconv.FormatInt(f.AppID, 10):
		return fail("Integration not found")
	case claims.Iat > now:
		return fail("'Issued at' claim ('iat') must be an Integer representing a time in the past.")
	case claims.Exp <= now || claims.Exp > now+600:
		return fail("'Expiration time' claim ('exp') is too far in the future")
	}
	return true
}

func (f *Fake) app(w http.ResponseWriter, r *http.Request) {
	if !f.authorise(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": f.AppID, "slug": "koinon-test", "name": "Koinon test", "permissions": f.Permissions})
}

func (f *Fake) installation(w http.ResponseWriter, r *http.Request) {
	if !f.authorise(w, r) {
		return
	}
	if r.PathValue("id") != strconv.FormatInt(f.InstallationID, 10) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": f.InstallationID, "app_id": f.AppID,
		"account":              map[string]string{"login": f.Account, "type": "Organization"},
		"repository_selection": "selected",
		"permissions":          f.Permissions,
	})
}

func (f *Fake) accessToken(w http.ResponseWriter, r *http.Request) {
	if !f.authorise(w, r) {
		return
	}
	if r.PathValue("id") != strconv.FormatInt(f.InstallationID, 10) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	var req struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Invalid request"})
		return
	}
	perms := f.Permissions
	if req.Permissions != nil {
		for p, level := range req.Permissions {
			have, ok := f.Permissions[p]
			if !ok || (level == "write" && have != "write") {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
					"message": fmt.Sprintf("The permissions requested are not granted to this installation (%s).", p),
				})
				return
			}
		}
		perms = req.Permissions
	}
	if f.LieAboutPermissions {
		perms = f.Permissions
	}
	repos := f.Repositories
	selection := "all"
	if req.Repositories != nil {
		// Names are case-insensitive, as on GitHub; the response spells them canonically.
		repos = nil
		for _, name := range req.Repositories {
			canonical, ok := f.canonical(name)
			if !ok {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
					"message": "There is at least one repository that does not exist or is not accessible to the parent installation.",
				})
				return
			}
			repos = append(repos, canonical)
		}
		selection = "selected"
	}
	full := make([]map[string]string, 0, len(repos))
	for _, name := range repos {
		full = append(full, map[string]string{"name": name, "full_name": f.Account + "/" + name})
	}
	f.mu.Lock()
	f.mints++
	n := f.mints
	f.requests = append(f.requests, Request{Repositories: req.Repositories, Permissions: req.Permissions})
	token := fmt.Sprintf("ghs_faketoken%04d", n)
	f.tokens[token] = tokenInfo{repos: slices.Clone(repos), all: req.Repositories == nil, perms: perms}
	f.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{
		"token":                token,
		"expires_at":           f.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"permissions":          perms,
		"repository_selection": selection,
		"repositories":         full,
	})
}
