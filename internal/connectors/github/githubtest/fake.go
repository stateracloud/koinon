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
	// Permissions the App was granted.
	Permissions map[string]string
	// Now defaults to time.Now; tokens expire an hour after Now.
	Now func() time.Time
	// LieAboutPermissions makes token responses claim every App permission,
	// whatever was requested, to test that clients check responses.
	LieAboutPermissions bool

	Server *httptest.Server
	// PEM is the App's private key, as GitHub's downloaded .pem file.
	PEM []byte

	key *rsa.PrivateKey

	mu       sync.Mutex
	mints    int
	requests []Request
}

// Request is a token request the fake received.
type Request struct {
	Repositories []string
	Permissions  map[string]string
}

// New starts a fake with a fresh 2048-bit key. Close it with f.Server.Close.
func New() *Fake {
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
		PEM:          pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /app", f.app)
	mux.HandleFunc("GET /app/installations/{id}", f.installation)
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", f.accessToken)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
	})
	f.Server = httptest.NewServer(mux)
	return f
}

// URL is the fake's base URL.
func (f *Fake) URL() string { return f.Server.URL }

// Mints returns how many tokens the fake has minted.
func (f *Fake) Mints() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mints
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
		for _, name := range req.Repositories {
			if !slices.Contains(f.Repositories, name) {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
					"message": "There is at least one repository that does not exist or is not accessible to the parent installation.",
				})
				return
			}
		}
		repos, selection = req.Repositories, "selected"
	}
	full := make([]map[string]string, 0, len(repos))
	for _, name := range repos {
		full = append(full, map[string]string{"name": name, "full_name": f.Account + "/" + name})
	}
	f.mu.Lock()
	f.mints++
	n := f.mints
	f.requests = append(f.requests, Request{Repositories: req.Repositories, Permissions: req.Permissions})
	f.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{
		"token":                fmt.Sprintf("ghs_faketoken%04d", n),
		"expires_at":           f.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"permissions":          perms,
		"repository_selection": selection,
		"repositories":         full,
	})
}
