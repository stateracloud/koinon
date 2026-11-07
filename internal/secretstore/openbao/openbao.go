// SPDX-License-Identifier: Apache-2.0

// Package openbao is the secretstore.Wrapper over an OpenBao (or Vault) transit key (old issue 416, ADR 0085
// decision 12). The KEKs are encrypted by the transit engine; the key never leaves OpenBao.
//
// It speaks the HTTP API directly (four endpoints: Kubernetes login, encrypt, decrypt and nothing
// else), so no SDK and its dependency tree enter the module (ADR 0079).
//
// Identity: the pod signs in with its own service account token (Kubernetes auth). No OpenBao token
// or other credential is configured anywhere. Rights, by OpenBao policy on the transit key:
//   - the key-admin Job: update on transit/encrypt/<key> and transit/decrypt/<key>, never create:
//     with create, encrypt makes a missing key on the spot (upsert), so a typo in the key name
//     would wrap the KEK under a new key instead of failing;
//   - koinon-api and the reconciler: update on transit/decrypt/<key> only. Wrap then answers
//     permission denied, which is secretstore.ErrUnreachable; secretstore.Wrappers refuses a
//     service's Wrap before it gets here (secretstore.ErrKeyAdminWraps).
package openbao

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/secretstore/remotekms"
)

// Backend is the adapter's backend name, stored with every KEK it wraps.
const Backend = "openbao"

// DefaultTokenFile is where Kubernetes mounts the pod's service account token.
const DefaultTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token" //nolint:gosec // a path, not a credential

// Config is where the transit key is and how the pod signs in. It holds no credential.
type Config struct {
	// Address is the OpenBao server, https://host:port. Plain http is accepted only on a loopback
	// address (tests, a sidecar agent).
	Address string
	// CACertFile is a PEM bundle that verifies the server's certificate; empty is the system roots.
	CACertFile string
	// Namespace is the OpenBao namespace (X-Vault-Namespace), if any.
	Namespace string
	// TransitMount is the transit engine's mount path; default "transit".
	TransitMount string
	// Key is the transit key's name.
	Key string
	// AuthMount is the Kubernetes auth method's mount path; default "kubernetes".
	AuthMount string
	// Role is the Kubernetes auth role the pod signs in as.
	Role string
	// TokenFile is the pod's service account token; default DefaultTokenFile. It is read on every
	// sign-in, since a projected token is rotated.
	TokenFile string
	// Timeout bounds each call (sign-in included); default remotekms.DefaultTimeout.
	Timeout time.Duration
}

var (
	keyPattern   = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
	mountPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)*$`)
)

// Wrapper is the transit adapter. It is safe for concurrent use.
type Wrapper struct {
	cfg  Config
	base *url.URL
	http *http.Client

	// login signs in and returns a client token and how long it may be used (0: until refused).
	login func(ctx context.Context) (string, time.Duration, error)

	mu      sync.Mutex
	token   string
	expires time.Time // zero: no known expiry
	// pending is the sign-in in flight, shared by every caller that needs a token meanwhile.
	pending *signInCall
}

// signInCall is one sign-in; done closes once token and err are set.
type signInCall struct {
	done  chan struct{}
	token string
	err   error
}

var _ secretstore.Wrapper = (*Wrapper)(nil)

// New returns the adapter for cfg. It does not call OpenBao: the first call signs in.
func New(cfg Config) (*Wrapper, error) {
	w, err := newWrapper(cfg, false)
	if err != nil {
		return nil, err
	}
	if !nonEmpty(cfg.Role) {
		return nil, errors.New("openbao: the Kubernetes auth role is not set")
	}
	w.login = w.kubernetesLogin
	return w, nil
}

func newWrapper(cfg Config, anyHTTP bool) (*Wrapper, error) {
	if cfg.TransitMount == "" {
		cfg.TransitMount = "transit"
	}
	if cfg.AuthMount == "" {
		cfg.AuthMount = "kubernetes"
	}
	if cfg.TokenFile == "" {
		cfg.TokenFile = DefaultTokenFile
	}
	if !keyPattern.MatchString(cfg.Key) || dots(cfg.Key) {
		return nil, fmt.Errorf("openbao: invalid transit key name %q", cfg.Key)
	}
	for _, m := range []string{cfg.TransitMount, cfg.AuthMount} {
		if !validPath(m) {
			return nil, fmt.Errorf("openbao: invalid mount path %q", m)
		}
	}
	// A namespace is a path too ("team/koinon"; OpenBao accepts a trailing slash).
	if ns := strings.TrimSuffix(cfg.Namespace, "/"); cfg.Namespace != "" && !validPath(ns) {
		return nil, fmt.Errorf("openbao: invalid namespace %q", cfg.Namespace)
	}
	base, err := url.Parse(strings.TrimRight(cfg.Address, "/"))
	if err != nil || base.Host == "" || (base.Scheme != "https" && base.Scheme != "http") {
		return nil, fmt.Errorf("openbao: the address %q is not an http(s) URL", cfg.Address)
	}
	if base.Scheme == "http" && !anyHTTP && !loopback(base.Hostname()) {
		return nil, fmt.Errorf("openbao: the address %q is plain http; use https (http only on a loopback address)", cfg.Address)
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.CACertFile != "" {
		pem, err := os.ReadFile(cfg.CACertFile)
		if err != nil {
			return nil, fmt.Errorf("openbao: read the CA bundle: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("openbao: no certificate in the CA bundle %s", cfg.CACertFile)
		}
		tlsCfg.RootCAs = pool
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tlsCfg
	tr.Proxy = nil // the KMS is reached directly, never through an egress proxy
	return &Wrapper{cfg: cfg, base: base, http: &http.Client{
		Transport: tr,
		// Never follow a redirect: it would re-send the token (and on sign-in the service account
		// token) to wherever it points, plain http included, which gets around the loopback-only
		// http rule. An HA standby or a load balancer that redirects means a wrong address.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

// errRedirect is a 3xx answer, which the adapter never follows.
var errRedirect = errors.New("redirect refused: set the active node's address")

func validPath(p string) bool {
	return mountPattern.MatchString(p) && !slices.ContainsFunc(strings.Split(p, "/"), dots)
}

// dots is a path segment that a server would resolve ("." or "..").
func dots(seg string) bool { return seg == "." || seg == ".." }

func nonEmpty(s string) bool { return strings.TrimSpace(s) != "" }

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Backend implements secretstore.Wrapper.
func (w *Wrapper) Backend() string { return Backend }

// KeyID implements secretstore.Wrapper: the transit mount and key, "transit/koinon-kek". The
// address is not part of it, so the same key behind another address still unwraps.
func (w *Wrapper) KeyID() string { return w.cfg.TransitMount + "/" + w.cfg.Key }

// Wrap implements secretstore.Wrapper.
func (w *Wrapper) Wrap(ctx context.Context, kek, aad []byte) ([]byte, error) {
	ctx, cancel := remotekms.Bound(ctx, w.cfg.Timeout)
	defer cancel()
	frame := remotekms.Frame(kek, aad)
	defer clear(frame)
	var out struct {
		Ciphertext string `json:"ciphertext"`
	}
	if err := w.transit(ctx, "encrypt", map[string]string{"plaintext": base64.StdEncoding.EncodeToString(frame)}, &out); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(out.Ciphertext, "vault:v") {
		return nil, fmt.Errorf("openbao: encrypt with %s answered no ciphertext", w.KeyID())
	}
	return []byte(out.Ciphertext), nil
}

// Unwrap implements secretstore.Wrapper.
func (w *Wrapper) Unwrap(ctx context.Context, wrapped, aad []byte) ([]byte, error) {
	ctx, cancel := remotekms.Bound(ctx, w.cfg.Timeout)
	defer cancel()
	var out struct {
		Plaintext string `json:"plaintext"`
	}
	if err := w.transit(ctx, "decrypt", map[string]string{"ciphertext": string(wrapped)}, &out); err != nil {
		return nil, err
	}
	plain, err := base64.StdEncoding.DecodeString(out.Plaintext)
	if err != nil {
		return nil, secretstore.ErrOpen
	}
	return remotekms.Unframe(plain, aad)
}

// probeCiphertext is well formed but too short to be one: decrypting it proves the sign-in, the
// right to decrypt and that the key exists, without a stored ciphertext or the right to encrypt.
const probeCiphertext = "vault:v1:AA=="

// Probe implements secretstore.Wrapper: it decrypts probeCiphertext, which OpenBao refuses as an
// invalid ciphertext only once the pod is signed in, may decrypt with the key, and the key exists.
// Healthy is exactly that refusal (OpenBao's own JSON error, see openRefusals); any other answer,
// a proxy's 400 page included, is not.
func (w *Wrapper) Probe(ctx context.Context) error {
	ctx, cancel := remotekms.Bound(ctx, w.cfg.Timeout)
	defer cancel()
	err := w.transit(ctx, "decrypt", map[string]string{"ciphertext": probeCiphertext}, nil)
	if err == nil || errors.Is(err, secretstore.ErrOpen) {
		return nil
	}
	return err
}

// transit calls transit/<op>/<key>, signing in first if needed, and once more if the token was
// refused (expired or revoked). out receives the response's data.
func (w *Wrapper) transit(ctx context.Context, op string, body map[string]string, out any) error {
	what := fmt.Sprintf("openbao %s with %s", op, w.KeyID())
	path := "/v1/" + w.cfg.TransitMount + "/" + op + "/" + w.cfg.Key
	for attempt := 0; ; attempt++ {
		token, err := w.signIn(ctx)
		if err != nil {
			return remotekms.Unreachable(what, err)
		}
		status, r, err := w.post(ctx, path, token, body)
		if err != nil {
			return remotekms.Unreachable(what, err)
		}
		data, msg, errs := r.Data, r.message(), r.Errors
		switch {
		case status == http.StatusOK:
			if out == nil {
				return nil
			}
			if err := json.Unmarshal(data, out); err != nil {
				return fmt.Errorf("%s: unreadable answer", what)
			}
			return nil
		case status == http.StatusForbidden && attempt == 0:
			w.forget(token)
			continue
		case status >= 300 && status < 400:
			return remotekms.Unreachable(what, fmt.Errorf("HTTP %d: %w", status, errRedirect))
		case status == http.StatusBadRequest && op == "decrypt" && openRefusal(errs):
			return secretstore.ErrOpen
		case status == http.StatusBadRequest && slices.Contains(errs, "encryption key not found"):
			return remotekms.Unreachable(what, errors.New("the transit key does not exist"))
		case status == http.StatusBadRequest, status == http.StatusForbidden, status == http.StatusNotFound,
			status == http.StatusTooManyRequests, status >= 500:
			return remotekms.Unreachable(what, fmt.Errorf("HTTP %d: %s", status, msg))
		default:
			return fmt.Errorf("%s: HTTP %d: %s", what, status, msg)
		}
	}
}

// openRefusals are OpenBao's answers (2.4.1, its "errors" array) to a decrypt of a ciphertext that
// is not one of this key's: malformed, or failing authentication (another key, or altered bytes).
// Only these are secretstore.ErrOpen, which callers may treat as final. A key version OpenBao does
// not have (too new: the key was restored from an older backup; too old: min_decryption_version
// was raised) is a setting an operator can undo, so it is unreachable, as is anything else, such
// as a proxy's own 400 page.
var openRefusals = []string{
	"invalid ciphertext length",
	"invalid ciphertext: no prefix",
	"invalid ciphertext: wrong number of fields",
	"invalid ciphertext: could not decode base64",
	"invalid ciphertext: version number could not be decoded",
	"cipher: message authentication failed",
}

func openRefusal(errs []string) bool {
	return len(errs) == 1 && slices.Contains(openRefusals, errs[0])
}

// signIn returns a token that has not expired, signing in when there is none. Callers that need a
// token while a sign-in is in flight share it, and each stops waiting when its own ctx ends. The
// sign-in is not bound to the first caller's ctx (its end would fail every other waiter), only to
// the configured timeout.
func (w *Wrapper) signIn(ctx context.Context) (string, error) {
	w.mu.Lock()
	if w.token != "" && (w.expires.IsZero() || time.Now().Before(w.expires)) {
		token := w.token
		w.mu.Unlock()
		return token, nil
	}
	call := w.pending
	if call == nil {
		call = &signInCall{done: make(chan struct{})}
		w.pending = call
		go w.runSignIn(context.WithoutCancel(ctx), call)
	}
	w.mu.Unlock()
	select {
	case <-call.done:
		return call.token, call.err
	case <-ctx.Done():
		return "", fmt.Errorf("waiting for the sign-in: %w", ctx.Err())
	}
}

func (w *Wrapper) runSignIn(ctx context.Context, call *signInCall) {
	ctx, cancel := remotekms.Bound(ctx, w.cfg.Timeout)
	defer cancel()
	token, ttl, err := w.login(ctx)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pending = nil
	call.token, call.err = token, err
	if err == nil {
		w.token, w.expires = token, time.Time{}
		if ttl > 0 {
			w.expires = time.Now().Add(ttl * 4 / 5) // sign in again before it expires
		}
	}
	close(call.done)
}

// forget drops token if it is still the current one, so the next call signs in again.
func (w *Wrapper) forget(token string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.token == token {
		w.token, w.expires = "", time.Time{}
	}
}

// kubernetesLogin signs in with the pod's service account token.
func (w *Wrapper) kubernetesLogin(ctx context.Context) (string, time.Duration, error) {
	jwt, err := os.ReadFile(w.cfg.TokenFile)
	if err != nil {
		return "", 0, fmt.Errorf("read the service account token: %w", err)
	}
	body := map[string]string{"role": w.cfg.Role, "jwt": strings.TrimSpace(string(jwt))}
	clear(jwt)
	status, r, err := w.post(ctx, "/v1/auth/"+w.cfg.AuthMount+"/login", "", body)
	if err != nil {
		return "", 0, err
	}
	msg, auth := r.message(), r.Auth
	if status >= 300 && status < 400 {
		return "", 0, fmt.Errorf("sign-in: HTTP %d: %w", status, errRedirect)
	}
	if status != http.StatusOK || auth == nil || auth.ClientToken == "" {
		return "", 0, fmt.Errorf("sign-in as role %q refused: HTTP %d: %s", w.cfg.Role, status, msg)
	}
	return auth.ClientToken, time.Duration(auth.LeaseDuration) * time.Second, nil
}

type response struct {
	Data   json.RawMessage `json:"data"`
	Auth   *authInfo       `json:"auth"`
	Errors []string        `json:"errors"`
}

type authInfo struct {
	ClientToken   string `json:"client_token"`
	LeaseDuration int64  `json:"lease_duration"`
}

// message is OpenBao's error text, shortened. It never holds a token or a payload.
func (r response) message() string {
	m := strings.Join(r.Errors, "; ")
	if len(m) > 200 {
		m = m[:200]
	}
	return m
}

// maxResponse bounds what is read of an answer: a KEK's ciphertext is well under a kilobyte.
const maxResponse = 1 << 20

func (w *Wrapper) post(ctx context.Context, path, token string, body map[string]string) (int, response, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return 0, response{}, err
	}
	defer clear(b)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.base.String()+path, bytes.NewReader(b))
	if err != nil {
		return 0, response{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}
	if w.cfg.Namespace != "" {
		req.Header.Set("X-Vault-Namespace", w.cfg.Namespace)
	}
	resp, err := w.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, response{}, fmt.Errorf("no answer: %w", ctx.Err())
		}
		return 0, response{}, errors.New("no answer: " + scrubURL(err))
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return 0, response{}, fmt.Errorf("reading the answer: %w", err)
	}
	var r response
	if len(raw) > 0 && json.Unmarshal(raw, &r) != nil && resp.StatusCode == http.StatusOK {
		return 0, response{}, errors.New("an answer that is not JSON")
	}
	return resp.StatusCode, r, nil
}

// scrubURL is a transport error without the request URL's query (there is none today) and with
// its inner cause, which is what tells a refused connection from a DNS failure.
func scrubURL(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err.Error()
	}
	return err.Error()
}
