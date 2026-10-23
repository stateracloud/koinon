// SPDX-License-Identifier: Apache-2.0

// Package oidctest is a minimal in-process OpenID Connect provider for tests:
// discovery, JWKS, an authorize endpoint that logs in a configurable user
// without a UI, and a token endpoint that enforces client authentication and
// PKCE (S256). It is not an IdP: never use it outside tests.
package oidctest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
)

const keyID = "test-key"

// User is the identity the issuer logs in at the next authorization.
type User struct {
	Subject string
	Email   string
	Name    string
	Groups  []string
}

// Issuer is a test OIDC provider served by httptest.
type Issuer struct {
	Server       *httptest.Server
	ClientID     string
	ClientSecret string
	// GroupsClaim names the claim carrying User.Groups; default "groups".
	GroupsClaim string
	// Now stamps ID tokens; defaults to time.Now.
	Now func() time.Time

	key *rsa.PrivateKey

	mu          sync.Mutex
	user        User
	codes       map[string]grant
	wrongNonce  bool
	Exchanges   int
	LastRequest url.Values
}

type grant struct {
	clientID, redirectURI, challenge, nonce string
	user                                    User
}

// New starts an issuer for one client.
func New(clientID, clientSecret string) *Issuer {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	i := &Issuer{
		ClientID: clientID, ClientSecret: clientSecret, GroupsClaim: "groups", Now: time.Now,
		key: key, codes: map[string]grant{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", i.discovery)
	mux.HandleFunc("GET /jwks", i.jwks)
	mux.HandleFunc("GET /authorize", i.authorize)
	mux.HandleFunc("POST /token", i.token)
	i.Server = httptest.NewServer(mux)
	return i
}

// URL is the issuer URL.
func (i *Issuer) URL() string { return i.Server.URL }

// Close stops the server.
func (i *Issuer) Close() { i.Server.Close() }

// SetUser sets who logs in next.
func (i *Issuer) SetUser(u User) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.user = u
}

// SetWrongNonce makes the next ID tokens carry a nonce the client did not
// send.
func (i *Issuer) SetWrongNonce(v bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.wrongNonce = v
}

func (i *Issuer) discovery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                i.URL(),
		"authorization_endpoint":                i.URL() + "/authorize",
		"token_endpoint":                        i.URL() + "/token",
		"jwks_uri":                              i.URL() + "/jwks",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"code_challenge_methods_supported":      []string{"S256"},
	})
}

func (i *Issuer) jwks(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key: &i.key.PublicKey, KeyID: keyID, Algorithm: string(jose.RS256), Use: "sig",
	}}})
}

func (i *Issuer) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	i.mu.Lock()
	i.LastRequest = q
	user := i.user
	i.mu.Unlock()
	if q.Get("response_type") != "code" || q.Get("client_id") != i.ClientID {
		http.Error(w, "bad authorization request", http.StatusBadRequest)
		return
	}
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		http.Error(w, "PKCE S256 required", http.StatusBadRequest)
		return
	}
	redirect, err := url.Parse(q.Get("redirect_uri"))
	if err != nil || redirect.Scheme == "" {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	code := randString()
	i.mu.Lock()
	i.codes[code] = grant{
		clientID: q.Get("client_id"), redirectURI: q.Get("redirect_uri"),
		challenge: q.Get("code_challenge"), nonce: q.Get("nonce"), user: user,
	}
	i.mu.Unlock()
	rq := redirect.Query()
	rq.Set("code", code)
	rq.Set("state", q.Get("state"))
	redirect.RawQuery = rq.Encode()
	http.Redirect(w, r, redirect.String(), http.StatusFound) //nolint:gosec // test IdP; redirect_uri is the client's
}

func (i *Issuer) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		tokenError(w, "invalid_request")
		return
	}
	id, secret, ok := r.BasicAuth()
	if !ok {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id != i.ClientID || subtle.ConstantTimeCompare([]byte(secret), []byte(i.ClientSecret)) != 1 {
		w.Header().Set("WWW-Authenticate", "Basic")
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_client"})
		return
	}
	if r.PostForm.Get("grant_type") != "authorization_code" {
		tokenError(w, "unsupported_grant_type")
		return
	}
	i.mu.Lock()
	code := r.PostForm.Get("code")
	g, found := i.codes[code]
	delete(i.codes, code) // codes are single use
	i.Exchanges++
	wrongNonce := i.wrongNonce
	i.mu.Unlock()
	if !found || g.redirectURI != r.PostForm.Get("redirect_uri") || g.clientID != id {
		tokenError(w, "invalid_grant")
		return
	}
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
		tokenError(w, "invalid_grant")
		return
	}
	nonce := g.nonce
	if wrongNonce {
		nonce = "not-the-nonce"
	}
	now := i.Now()
	claims := map[string]any{
		"iss": i.URL(), "sub": g.user.Subject, "aud": i.ClientID,
		"iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(), "nonce": nonce,
		"email": g.user.Email, "name": g.user.Name,
	}
	if g.user.Groups != nil {
		claims[i.GroupsClaim] = g.user.Groups
	}
	idToken, err := i.sign(claims)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": randString(), "token_type": "Bearer", "expires_in": 300, "id_token": idToken,
	})
}

func (i *Issuer) sign(claims map[string]any) (string, error) {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: i.key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", keyID))
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	jws, err := signer.Sign(payload)
	if err != nil {
		return "", err
	}
	return jws.CompactSerialize()
}

func tokenError(w http.ResponseWriter, code string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": code})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func randString() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
