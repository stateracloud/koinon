// SPDX-License-Identifier: Apache-2.0

// Package github authenticates Koinon to GitHub as a GitHub App (plan item C1,
// ADR 0036, 0049) and mints installation tokens narrowed to the repositories
// and permissions of one use.
//
// It talks plain HTTP to three REST endpoints rather than depending on
// go-github: the surface is small, and every response is checked here anyway.
//
//   - GET  /app                                         (App JWT; checks app ID and key)
//   - GET  /app/installations/{installation_id}         (App JWT; checks the installation)
//   - POST /app/installations/{installation_id}/access_tokens (App JWT; mints a token)
//
// Upstream references, checked 2026-10-22:
//
//   - App JWT: RS256, iat 60 seconds in the past for clock drift, exp at most 10 minutes
//     ahead, iss the App's client ID or app ID.
//     https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-a-json-web-token-jwt-for-a-github-app
//   - Installation tokens: the body's "repositories" (names) and "permissions" narrow the
//     token; it "cannot be granted access to repositories that the installation was not
//     granted access to" nor "permissions that the app was not granted"; up to 500
//     repositories; the token expires one hour after creation; 201 on success, and the
//     response carries token, expires_at, permissions, repository_selection and repositories.
//     https://docs.github.com/en/rest/apps/apps#create-an-installation-access-token-for-an-app
//     https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-an-installation-access-token-for-a-github-app
//
// Secrets: the private key is held as a [PrivateKey], whose String and LogValue are
// redacted, and tokens as a [Token], likewise. Error messages never contain either.
package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/stateracloud/koinon/internal/codehost"
)

// DefaultBaseURL is GitHub's REST API.
const DefaultBaseURL = "https://api.github.com"

// APIVersion is the REST API version Koinon sends (X-GitHub-Api-Version).
const APIVersion = "2026-03-10"

// DefaultRefreshMargin is how long before expiry a cached token is replaced.
// Installation tokens live one hour; a caller that receives a token can use it
// for at least this long.
const DefaultRefreshMargin = 10 * time.Minute

// ErrRepositoryMismatch is returned when a minted token does not cover exactly
// the requested repositories, for example because one belongs to another
// account than the installation's.
var ErrRepositoryMismatch = errors.New("github: repositories do not match the request")

// MaxRepositories is GitHub's limit on repositories per installation token.
//
// The connector data (spec.capabilities.maxRepositories) is the source; this is the bound used
// when a Client is built without it.
const MaxRepositories = codehost.DefaultMaxRepositories

const redacted = "[redacted]"

// PrivateKey is a GitHub App private key. Its value never leaves the package:
// it formats and logs as "[redacted]".
type PrivateKey struct {
	k           *rsa.PrivateKey
	fingerprint string
}

// ErrInvalidPrivateKey is returned for anything that is not an RSA private key
// in PEM form. It never quotes the input.
var ErrInvalidPrivateKey = errors.New("github: the private key is not an RSA private key in PEM form (the .pem file GitHub generates)")

// ParsePrivateKey parses the .pem file GitHub generates (PKCS old issue 1), or a PKCS old issue 8
// RSA key.
func ParsePrivateKey(data []byte) (*PrivateKey, error) {
	block, rest := pem.Decode(bytes.TrimSpace(data))
	if block == nil || len(bytes.TrimSpace(rest)) != 0 {
		return nil, ErrInvalidPrivateKey
	}
	var key *rsa.PrivateKey
	switch block.Type {
	case "RSA PRIVATE KEY":
		k, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, ErrInvalidPrivateKey
		}
		key = k
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, ErrInvalidPrivateKey
		}
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, ErrInvalidPrivateKey
		}
		key = rk
	default:
		return nil, ErrInvalidPrivateKey
	}
	if key.N.BitLen() < 2048 {
		return nil, fmt.Errorf("github: the private key has %d bits; GitHub App keys have at least 2048", key.N.BitLen())
	}
	pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, ErrInvalidPrivateKey
	}
	sum := sha256.Sum256(pub)
	return &PrivateKey{k: key, fingerprint: "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])}, nil
}

// Fingerprint identifies the key by its public half (safe to show and log).
func (p *PrivateKey) Fingerprint() string { return p.fingerprint }

// String implements fmt.Stringer without revealing the key.
func (p *PrivateKey) String() string { return redacted }

// GoString implements fmt.GoStringer without revealing the key.
func (p *PrivateKey) GoString() string { return redacted }

// LogValue implements slog.LogValuer without revealing the key.
func (p *PrivateKey) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalJSON keeps the key out of JSON.
func (p *PrivateKey) MarshalJSON() ([]byte, error) { return json.Marshal(redacted) }

// App identifies one GitHub App installation and holds the App's key.
type App struct {
	AppID          int64
	InstallationID int64
	Key            *PrivateKey
}

func (a App) validate() error {
	if a.AppID <= 0 || a.InstallationID <= 0 || a.Key == nil {
		return errors.New("github: app ID, installation ID and private key are required")
	}
	return nil
}

// Client calls GitHub's REST API as an App and caches installation tokens.
// The zero value is not usable; use NewClient.
type Client struct {
	baseURL string
	http    *http.Client
	dial    func(ctx context.Context, network, addr string) (net.Conn, error)
	now     func() time.Time
	margin  time.Duration

	// mu guards cache. Clients derived with For share both, so a token is minted once per
	// connection's host, App and narrowing however many derived clients ask.
	mu      *sync.Mutex
	cache   map[string]*cacheEntry
	derived *sync.Map // base URL and CA bundle hash to *Client
}

// Options configures a Client.
type Options struct {
	// BaseURL defaults to DefaultBaseURL; GitHub Enterprise Server uses
	// https://<host>/api/v3.
	BaseURL string
	// HTTPClient defaults to a client with a 30-second timeout that dials through DialContext and
	// returns a 3xx response instead of following it.
	HTTPClient *http.Client
	// DialContext is the egress policy's dial check (catalog.URLPolicy.DialContext): a Connection's
	// base URL is admin-supplied, so the default client connects only to addresses it allows. It is
	// unused when HTTPClient is set. Nil dials unchecked, for tests and api.github.com-only use.
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	// RefreshMargin defaults to DefaultRefreshMargin.
	RefreshMargin time.Duration
	// Now defaults to time.Now.
	Now func() time.Time
}

// For returns a client for a connection's own host: apiBase is its REST API's base URL and
// caBundle PEM certificates trusted besides the system roots for it. Both empty returns c. The
// derived client shares c's token cache (keys include the base URL) and its settings, and is
// itself cached, so connections reuse their transports.
func (c *Client) For(apiBase, caBundle string) (*Client, error) {
	apiBase = strings.TrimRight(apiBase, "/")
	if (apiBase == "" || apiBase == c.baseURL) && strings.TrimSpace(caBundle) == "" {
		return c, nil
	}
	if apiBase == "" {
		apiBase = c.baseURL
	}
	sum := sha256.Sum256([]byte(caBundle))
	key := apiBase + "|" + hex.EncodeToString(sum[:])
	if d, ok := c.derived.Load(key); ok {
		return d.(*Client), nil
	}
	hc := *c.http
	hc.CheckRedirect = noRedirect
	if strings.TrimSpace(caBundle) != "" {
		pool, err := codehost.CertPool(caBundle)
		if err != nil {
			return nil, fmt.Errorf("github: %w", err)
		}
		var tr *http.Transport
		switch t := hc.Transport.(type) {
		case nil:
			tr = http.DefaultTransport.(*http.Transport).Clone()
		case *http.Transport:
			tr = t.Clone()
		default:
			return nil, errors.New("github: the HTTP client's transport cannot take a CA bundle")
		}
		if tr.TLSClientConfig == nil {
			tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		}
		tr.TLSClientConfig.RootCAs = pool
		hc.Transport = tr
	}
	d := &Client{baseURL: apiBase, http: &hc, dial: c.dial, now: c.now, margin: c.margin, mu: c.mu, cache: c.cache, derived: c.derived}
	got, _ := c.derived.LoadOrStore(key, d)
	return got.(*Client), nil
}

// newHTTPClient returns the default client: it connects only through dial (when set), ignores the
// environment's proxy (a proxy would take the traffic around the dial check) and returns a 3xx
// response instead of following it, so a Connection's admin-supplied URL cannot send Koinon on to
// another host. It mirrors the trace exporter's client (internal/tracing/export).
func newHTTPClient(dial func(ctx context.Context, network, addr string) (net.Conn, error), timeout time.Duration) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if dial != nil {
		tr.DialContext = dial
		tr.Proxy = nil
	}
	return &http.Client{Timeout: timeout, Transport: tr, CheckRedirect: noRedirect}
}

// noRedirect makes the client return the 3xx response itself.
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// DialContext returns the dial check the client was built with, nil when it has none, so the
// providers registry can give the other code hosts the same outbound rule.
func (c *Client) DialContext() func(ctx context.Context, network, addr string) (net.Conn, error) {
	return c.dial
}

// NewClient returns a Client.
func NewClient(o Options) *Client {
	if o.BaseURL == "" {
		o.BaseURL = DefaultBaseURL
	}
	if o.HTTPClient == nil {
		o.HTTPClient = newHTTPClient(o.DialContext, 30*time.Second)
	}
	if o.RefreshMargin <= 0 {
		o.RefreshMargin = DefaultRefreshMargin
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Client{
		baseURL: strings.TrimRight(o.BaseURL, "/"), http: o.HTTPClient, dial: o.DialContext, now: o.Now,
		margin: o.RefreshMargin, mu: new(sync.Mutex), cache: map[string]*cacheEntry{}, derived: new(sync.Map),
	}
}

// AppJWT returns a JWT that authenticates as the App for nine minutes.
func (c *Client) AppJWT(app App) (string, error) {
	if err := app.validate(); err != nil {
		return "", err
	}
	now := c.now()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{
		// 60 seconds back for clock drift; nine minutes ahead stays under GitHub's ten
		// even with a slightly fast local clock.
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": strconv.FormatInt(app.AppID, 10),
	})
	if err != nil {
		return "", err
	}
	signing := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, app.Key.k, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("github: sign app JWT: %w", err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// AppInfo is the part of GET /app Koinon reads.
type AppInfo struct {
	ID          int64             `json:"id"`
	Slug        string            `json:"slug"`
	Name        string            `json:"name"`
	Permissions map[string]string `json:"permissions"`
}

// GetApp returns the authenticated App: the check that app ID and key match.
func (c *Client) GetApp(ctx context.Context, app App) (AppInfo, error) {
	var out AppInfo
	err := c.asApp(ctx, app, http.MethodGet, "/app", nil, http.StatusOK, &out)
	return out, err
}

// Installation is the part of GET /app/installations/{id} Koinon reads.
type Installation struct {
	ID      int64 `json:"id"`
	AppID   int64 `json:"app_id"`
	Account struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"account"`
	RepositorySelection string            `json:"repository_selection"`
	Permissions         map[string]string `json:"permissions"`
	SuspendedAt         *time.Time        `json:"suspended_at"`
}

// GetInstallation returns the App's installation.
func (c *Client) GetInstallation(ctx context.Context, app App) (Installation, error) {
	var out Installation
	err := c.asApp(ctx, app, http.MethodGet, fmt.Sprintf("/app/installations/%d", app.InstallationID), nil, http.StatusOK, &out)
	return out, err
}

// TokenRequest is what one use of an installation token needs.
type TokenRequest struct {
	// Repositories are full names (owner/name). They must all belong to the
	// installation's account. Empty means every repository the installation
	// can access (GitHub's default), which only suits the connection test's
	// metadata-read token; every agent use names its repositories.
	Repositories []string
	// Permissions by GitHub App permission name, "read" or "write". Required:
	// Koinon never mints a token with every permission the App has.
	Permissions map[string]string
}

// Token is a minted installation token. Its value formats and logs as
// "[redacted]"; call Value to use it.
type Token struct {
	value        string
	ExpiresAt    time.Time
	Permissions  map[string]string
	Repositories []string
}

// Value returns the token for an Authorization header. Never log it.
func (t Token) Value() string { return t.value }

// String implements fmt.Stringer without revealing the token.
func (t Token) String() string {
	return fmt.Sprintf("github installation token (expires %s, %d repositories)", t.ExpiresAt.Format(time.RFC3339), len(t.Repositories))
}

// GoString implements fmt.GoStringer without revealing the token.
func (t Token) GoString() string { return t.String() }

// LogValue implements slog.LogValuer without revealing the token.
func (t Token) LogValue() slog.Value {
	return slog.GroupValue(slog.Time("expires_at", t.ExpiresAt), slog.Any("repositories", t.Repositories))
}

type cacheEntry struct {
	mu  sync.Mutex // held while minting, so concurrent callers share one mint
	tok Token
}

// cacheKey covers everything that changes what a token may do or who minted
// it, including the key's fingerprint, so a rotated key never serves tokens of
// the old one.
func cacheKey(base string, app App, repos []string, perms map[string]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s|%d|%d|%s|", base, app.AppID, app.InstallationID, app.Key.Fingerprint())
	b.WriteString(strings.Join(repos, ","))
	b.WriteByte('|')
	for _, k := range slices.Sorted(maps.Keys(perms)) {
		b.WriteString(k + "=" + perms[k] + ";")
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// InstallationToken returns a token narrowed to req, from the cache while it
// has more than the refresh margin left, minted otherwise. The response is
// checked against the request: a token with a permission or repository that
// was not asked for is never returned.
func (c *Client) InstallationToken(ctx context.Context, app App, req TokenRequest) (Token, error) {
	if err := app.validate(); err != nil {
		return Token{}, err
	}
	repos, names, perms, err := normalise(req)
	if err != nil {
		return Token{}, err
	}
	key := cacheKey(c.baseURL, app, repos, perms)

	c.mu.Lock()
	e, ok := c.cache[key]
	if !ok {
		e = &cacheEntry{}
		c.cache[key] = e
	}
	c.mu.Unlock()

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.tok.value != "" && c.now().Add(c.margin).Before(e.tok.ExpiresAt) {
		return e.tok, nil
	}
	tok, err := c.mint(ctx, app, repos, names, perms)
	if err != nil {
		return Token{}, err
	}
	e.tok = tok
	c.prune()
	return tok, nil
}

// prune drops expired entries; called with an entry lock held but not c.mu.
func (c *Client) prune() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for k, e := range c.cache {
		if e.mu.TryLock() {
			if e.tok.value != "" && !now.Before(e.tok.ExpiresAt) {
				delete(c.cache, k)
			}
			e.mu.Unlock()
		}
	}
}

func normalise(req TokenRequest) (full, names []string, perms map[string]string, err error) {
	if len(req.Permissions) == 0 {
		return nil, nil, nil, errors.New("github: a token request must name its permissions")
	}
	perms = map[string]string{}
	for p, level := range req.Permissions {
		if level != "read" && level != "write" {
			return nil, nil, nil, fmt.Errorf("github: permission %s: level must be read or write, not %q", p, level)
		}
		perms[p] = level
	}
	if limit := maxRepositories(); len(req.Repositories) > limit {
		return nil, nil, nil, fmt.Errorf("github: at most %d repositories per token", limit)
	}
	seen := map[string]bool{}
	for _, r := range req.Repositories {
		owner, name, ok := strings.Cut(r, "/")
		if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
			return nil, nil, nil, fmt.Errorf("github: repository %q must be owner/name", r)
		}
		lower := strings.ToLower(r)
		if seen[lower] {
			continue
		}
		seen[lower] = true
		full = append(full, lower)
		names = append(names, name)
	}
	slices.Sort(full)
	slices.Sort(names)
	return full, names, perms, nil
}

func (c *Client) mint(ctx context.Context, app App, full, names []string, perms map[string]string) (Token, error) {
	body := map[string]any{"permissions": perms}
	if len(names) > 0 {
		body["repositories"] = names
	}
	var out struct {
		Token               string            `json:"token"`
		ExpiresAt           time.Time         `json:"expires_at"`
		Permissions         map[string]string `json:"permissions"`
		RepositorySelection string            `json:"repository_selection"`
		Repositories        []struct {
			FullName string `json:"full_name"`
		} `json:"repositories"`
	}
	path := fmt.Sprintf("/app/installations/%d/access_tokens", app.InstallationID)
	if err := c.asApp(ctx, app, http.MethodPost, path, body, http.StatusCreated, &out); err != nil {
		return Token{}, err
	}
	if out.Token == "" || out.ExpiresAt.IsZero() {
		return Token{}, errors.New("github: token response without token or expires_at")
	}
	// Defence in depth: GitHub narrows the token, and Koinon checks it did.
	for p, level := range out.Permissions {
		want, ok := perms[p]
		if !ok || (level == "write" && want == "read") {
			return Token{}, fmt.Errorf("github: minted token has permission %s=%s, which was not requested", p, level)
		}
	}
	got := make([]string, 0, len(out.Repositories))
	for _, r := range out.Repositories {
		got = append(got, strings.ToLower(r.FullName))
	}
	slices.Sort(got)
	if len(full) > 0 {
		// Names in the request carry no owner: comparing full names also catches a
		// target whose owner is not the installation's account.
		if out.RepositorySelection != "selected" || !slices.Equal(got, full) {
			return Token{}, fmt.Errorf("%w: minted token covers repositories %v, requested %v", ErrRepositoryMismatch, got, full)
		}
	}
	return Token{value: out.Token, ExpiresAt: out.ExpiresAt, Permissions: out.Permissions, Repositories: got}, nil
}

// APIError is a non-success response from GitHub.
type APIError struct {
	Method, Path string
	Status       int
	// Message is GitHub's "message" field; it never contains credentials.
	Message string
}

func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	return fmt.Sprintf("github: %s %s: %d %s", e.Method, e.Path, e.Status, msg)
}

func (c *Client) asApp(ctx context.Context, app App, method, path string, body any, want int, out any) error {
	jwt, err := c.AppJWT(app)
	if err != nil {
		return err
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", APIVersion)
	req.Header.Set("Authorization", "Bearer "+jwt)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// url.Error carries only the URL, which holds no credentials.
		return fmt.Errorf("github: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("github: %s %s: read response: %w", method, path, err)
	}
	if resp.StatusCode != want {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &e)
		return &APIError{Method: method, Path: path, Status: resp.StatusCode, Message: truncate(e.Message, 300)}
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("github: %s %s: decode response: %w", method, path, err)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// HTTPStatus implements codehost.StatusError.
func (e *APIError) HTTPStatus() int { return e.Status }

// Detail implements codehost.StatusError.
func (e *APIError) Detail() string { return e.Message }
