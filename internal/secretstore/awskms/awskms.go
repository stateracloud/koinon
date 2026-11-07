// SPDX-License-Identifier: Apache-2.0

// Package awskms is the secretstore.Wrapper over an AWS KMS symmetric key (old issue 417, ADR 0085 decision
// 12). The KEKs are encrypted by KMS (Encrypt, Decrypt); the key never leaves KMS.
//
// It speaks the KMS JSON 1.1 API and the STS query API directly over net/http, with its own
// Signature Version 4 signer (sigv4.go, checked against AWS's published test vectors). The adapter
// needs three calls (KMS Encrypt and Decrypt, STS AssumeRoleWithWebIdentity) and one credential
// endpoint; aws-sdk-go-v2's kms, sts and config modules would bring about fifteen modules and their
// release churn for that, each a dependency to age and review (ADR 0079). The cost is owning the
// signer, which is small, specified and tested against the vectors.
//
// Identity: pod identity only, IRSA or EKS Pod Identity (credentials.go). The adapter never reads
// static access keys, so no AWS credential is in values, env or a flag. Rights, by IAM policy (and
// the key policy) on the key:
//   - the key-admin Job: kms:Encrypt and kms:Decrypt;
//   - koinon-api and the reconciler: kms:Decrypt only. Wrap then answers AccessDeniedException,
//     which is secretstore.ErrUnreachable; secretstore.Wrappers refuses a service's Wrap before it
//     gets here (secretstore.ErrKeyAdminWraps).
//
// Every request names the configured key, Decrypt included, so KMS refuses a ciphertext of another
// key (IncorrectKeyException) instead of decrypting it with whatever key it names. The plaintext is
// the remotekms frame, which binds the associated data; the fixed encryption context only marks
// the ciphertexts as Koinon's KEKs (CloudTrail, kms:EncryptionContext conditions).
package awskms

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
const Backend = "awskms"

// Config is the KMS key and how to reach it. It holds no credential.
type Config struct {
	// Region is the key's AWS region, e.g. eu-central-1.
	Region string
	// KeyID is the KMS key: a key ARN (recommended), a key ID, an alias name (alias/koinon-kek) or an
	// alias ARN. It is stored with every wrapped KEK and must stay spelled the same. An alias that
	// is pointed at another key no longer opens the KEKs wrapped before (KMS: IncorrectKeyException).
	KeyID string
	// Endpoint is the KMS endpoint, https://host[:port]; default https://kms.<region>.amazonaws.com.
	// Set it for a VPC endpoint with its own DNS name, or FIPS. Plain http only on a loopback address.
	Endpoint string
	// STSEndpoint is the STS endpoint for IRSA; default https://sts.<region>.amazonaws.com.
	STSEndpoint string
	// CACertFile is a PEM bundle that verifies the endpoints' certificates; empty is the system roots.
	CACertFile string
	// Timeout bounds each call (a credential refresh included); default remotekms.DefaultTimeout.
	Timeout time.Duration
	// Identity is the pod's identity; wrapping fills it from the environment (IdentityFromEnv).
	Identity Identity
}

var (
	regionPattern = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]{1,2}$`)
	keyIDPattern  = regexp.MustCompile(`^([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}|mrk-[0-9a-f]{32})$`)
	aliasPattern  = regexp.MustCompile(`^alias/[A-Za-z0-9/_-]{1,250}$`)
	arnPattern    = regexp.MustCompile(`^arn:(aws|aws-cn|aws-us-gov|aws-iso|aws-iso-[a-z])+:kms:([a-z0-9-]+):([0-9]{12}):(key/.+|alias/.+)$`)
	rolePattern   = regexp.MustCompile(`^arn:aws[a-z-]*:iam::[0-9]{12}:role/[\w+=,.@/-]{1,512}$`)
)

// encryptionContext is sent with every Encrypt and Decrypt. It is fixed, so every KEK ever wrapped
// still decrypts; the frame, not this, binds a KEK to its ID.
var encryptionContext = map[string]string{"koinon": "kek-wrap/v1"}

// Wrapper is the AWS KMS adapter. It is safe for concurrent use.
type Wrapper struct {
	cfg      Config
	endpoint *url.URL
	sts      *url.URL
	// http reaches KMS and STS; agent reaches the EKS Pod Identity Agent.
	http, agent *http.Client

	// fetch returns fresh credentials (IRSA or EKS Pod Identity).
	fetch func(ctx context.Context) (credentials, error)
	now   func() time.Time

	mu      sync.Mutex
	creds   credentials
	refresh time.Time // zero: no known expiry
	// expires is when creds expire (zero: no known expiry).
	expires time.Time
	// pending is the refresh in flight, shared by every caller that needs credentials meanwhile.
	pending *refreshCall

	// log is where the adapter logs (nil: slog.Default()).
	log *slog.Logger
}

// refreshCall is one credential refresh; done closes once creds and err are set.
type refreshCall struct {
	done  chan struct{}
	creds credentials
	err   error
}

var _ secretstore.Wrapper = (*Wrapper)(nil)

// New returns the adapter for cfg. It does not call AWS: the first call obtains credentials.
func New(cfg Config) (*Wrapper, error) {
	w, err := newWrapper(cfg, false)
	if err != nil {
		return nil, err
	}
	id := cfg.Identity
	switch {
	case id.RoleARN != "" || id.WebIdentityTokenFile != "":
		if id.RoleARN == "" || id.WebIdentityTokenFile == "" {
			return nil, fmt.Errorf("awskms: IRSA needs both %s and %s", EnvRoleARN, EnvWebIdentityTokenFile)
		}
		if !rolePattern.MatchString(id.RoleARN) {
			return nil, fmt.Errorf("awskms: %s %q is not an IAM role ARN", EnvRoleARN, id.RoleARN)
		}
		w.fetch = w.webIdentity
	case id.CredentialsURI != "":
		if err := validCredentialsURI(id.CredentialsURI, false); err != nil {
			return nil, fmt.Errorf("awskms: %w", err)
		}
		w.fetch = w.podIdentity
	default:
		return nil, fmt.Errorf("awskms: %w", errNoIdentity)
	}
	return w, nil
}

func newWrapper(cfg Config, anyHTTP bool) (*Wrapper, error) {
	if !regionPattern.MatchString(cfg.Region) {
		return nil, fmt.Errorf("awskms: invalid region %q", cfg.Region)
	}
	if err := validKeyID(cfg.KeyID, cfg.Region); err != nil {
		return nil, err
	}
	suffix := "amazonaws.com"
	if strings.HasPrefix(cfg.Region, "cn-") {
		suffix = "amazonaws.com.cn"
	}
	endpoint, err := parseEndpoint("endpoint", cfg.Endpoint, "https://kms."+cfg.Region+"."+suffix, anyHTTP)
	if err != nil {
		return nil, err
	}
	sts, err := parseEndpoint("STS endpoint", cfg.STSEndpoint, "https://sts."+cfg.Region+"."+suffix, anyHTTP)
	if err != nil {
		return nil, err
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.CACertFile != "" {
		pem, err := os.ReadFile(cfg.CACertFile)
		if err != nil {
			return nil, fmt.Errorf("awskms: read the CA bundle: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("awskms: no certificate in the CA bundle %s", cfg.CACertFile)
		}
		tlsCfg.RootCAs = pool
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tlsCfg
	// KMS and STS are public AWS endpoints, and a cluster whose egress goes through an HTTP proxy
	// reaches them only through it (the AWS SDKs honour HTTPS_PROXY too). Unlike OpenBao, which sits
	// inside the operator's network, a proxy here is a legitimate path, so the standard proxy
	// variables apply; the chart sets none. Through a proxy the TLS session still ends at AWS
	// (CONNECT), and the secret access key is never sent: a request carries only its signature.
	// A VPC endpoint is reached directly when NO_PROXY names it; plain http is loopback only, which
	// is never proxied.
	tr.Proxy = http.ProxyFromEnvironment
	noRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	agentTr := http.DefaultTransport.(*http.Transport).Clone()
	agentTr.TLSClientConfig = tlsCfg.Clone()
	agentTr.Proxy = nil // the Pod Identity Agent is on the node's link-local address: never proxied
	return &Wrapper{
		cfg: cfg, endpoint: endpoint, sts: sts,
		// Never follow a redirect: it would re-send a signed request (and on sign-in the web
		// identity token) to wherever it points, plain http included. A redirect means a wrong
		// endpoint.
		http:  &http.Client{Transport: tr, CheckRedirect: noRedirect},
		agent: &http.Client{Transport: agentTr, CheckRedirect: noRedirect},
		now:   time.Now,
	}, nil
}

func parseEndpoint(what, raw, def string, anyHTTP bool) (*url.URL, error) {
	if raw == "" {
		raw = def
	}
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.Path != "" || u.RawQuery != "" || u.User != nil {
		return nil, fmt.Errorf("awskms: the %s %q is not an http(s) URL without a path", what, raw)
	}
	if u.Scheme == "http" && !anyHTTP && !loopback(u.Hostname()) {
		return nil, fmt.Errorf("awskms: the %s %q is plain http; use https (http only on a loopback address)", what, raw)
	}
	u.Path = "/"
	return u, nil
}

// validKeyID checks the forms KMS accepts for a key, and that an ARN is in region.
func validKeyID(id, region string) error {
	switch {
	case keyIDPattern.MatchString(id):
		return nil
	case aliasPattern.MatchString(id):
		if strings.HasPrefix(id, "alias/aws/") {
			return fmt.Errorf("awskms: %q is an AWS managed key, whose key policy cannot limit who decrypts; use a customer managed key", id)
		}
		return nil
	}
	m := arnPattern.FindStringSubmatch(id)
	if m == nil {
		return fmt.Errorf("awskms: invalid key %q: want a key ARN, key ID, alias/<name> or alias ARN", id)
	}
	if m[2] != region {
		return fmt.Errorf("awskms: the key %q is in region %s, not %s", id, m[2], region)
	}
	res := m[4]
	if r, ok := strings.CutPrefix(res, "key/"); ok && keyIDPattern.MatchString(r) {
		return nil
	}
	if strings.HasPrefix(res, "alias/") && aliasPattern.MatchString(res) && !strings.HasPrefix(res, "alias/aws/") {
		return nil
	}
	return fmt.Errorf("awskms: invalid key %q: want a key ARN, key ID, alias/<name> or alias ARN", id)
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Backend implements secretstore.Wrapper.
func (w *Wrapper) Backend() string { return Backend }

// KeyID implements secretstore.Wrapper: the key as configured.
func (w *Wrapper) KeyID() string { return w.cfg.KeyID }

// Wrap implements secretstore.Wrapper.
func (w *Wrapper) Wrap(ctx context.Context, kek, aad []byte) ([]byte, error) {
	ctx, cancel := remotekms.Bound(ctx, w.cfg.Timeout)
	defer cancel()
	frame := remotekms.Frame(kek, aad)
	defer clear(frame)
	what := "awskms encrypt with " + w.cfg.KeyID
	var out struct {
		CiphertextBlob []byte `json:"CiphertextBlob"`
	}
	in := map[string]any{"KeyId": w.cfg.KeyID, "Plaintext": frame, "EncryptionContext": encryptionContext}
	if err := w.call(ctx, "Encrypt", in, &out); err != nil {
		return nil, remotekms.Unreachable(what, err)
	}
	if len(out.CiphertextBlob) == 0 {
		return nil, remotekms.Unreachable(what, errors.New("an answer without a ciphertext"))
	}
	return out.CiphertextBlob, nil
}

// openRefusals are KMS's answers (HTTP 400, its own JSON error type) to a Decrypt of a ciphertext
// that the configured key does not open: malformed or altered, other encryption context
// (InvalidCiphertextException), or another key's (IncorrectKeyException, since every Decrypt names
// the key). Only these are secretstore.ErrOpen, which callers may treat as final. A missing,
// disabled or pending-deletion key, a refused identity or right, throttling and anything that is
// not KMS's JSON (a proxy's page) are settings or outages: unreachable.
var openRefusals = []string{"InvalidCiphertextException", "IncorrectKeyException"}

// Unwrap implements secretstore.Wrapper.
func (w *Wrapper) Unwrap(ctx context.Context, wrapped, aad []byte) ([]byte, error) {
	ctx, cancel := remotekms.Bound(ctx, w.cfg.Timeout)
	defer cancel()
	var out struct {
		Plaintext []byte `json:"Plaintext"`
	}
	err := w.decrypt(ctx, wrapped, &out)
	var ae *apiError
	switch {
	case err == nil:
		return remotekms.Unframe(out.Plaintext, aad)
	case errors.As(err, &ae) && ae.status == http.StatusBadRequest && slices.Contains(openRefusals, ae.typ):
		return nil, secretstore.ErrOpen
	default:
		return nil, remotekms.Unreachable("awskms decrypt with "+w.cfg.KeyID, err)
	}
}

// probeCiphertext is the probe's CiphertextBlob: no KMS ciphertext. With a dry run that ignores
// the ciphertext (below) KMS never reads it; it only has to be there.
var probeCiphertext = []byte("koinon-probe")

// Probe implements secretstore.Wrapper: a dry-run Decrypt with the key and encryption context of a
// real one, DryRun with the modifier IGNORE_CIPHERTEXT, which asks KMS to check the authorization
// (the identity, kms:Decrypt on the key, a kms:EncryptionContext condition) and not the ciphertext.
// Healthy is exactly KMS's DryRunOperationException, its answer to a dry run that would have been
// allowed; AccessDeniedException, any other error, and a proxy's page are not. A plain Decrypt of a
// junk ciphertext would not do: KMS may refuse the ciphertext before it checks the right, so a role
// without kms:Decrypt would probe healthy.
//
// An endpoint that does not know the dry run (an emulator such as LocalStack, a KMS-compatible
// service) ignores the parameters and answers as to a plain Decrypt of the junk
// (InvalidCiphertextException), or rejects them. Neither proves the decrypt right, so the probe
// calls that endpoint unreachable and says why (errDryRunUnverified); there is no weaker fallback.
func (w *Wrapper) Probe(ctx context.Context) error {
	ctx, cancel := remotekms.Bound(ctx, w.cfg.Timeout)
	defer cancel()
	what := "awskms probe of " + w.cfg.KeyID
	in := map[string]any{
		"KeyId": w.cfg.KeyID, "CiphertextBlob": probeCiphertext, "EncryptionContext": encryptionContext,
		"DryRun": true, "DryRunModifiers": []string{"IGNORE_CIPHERTEXT"},
	}
	err := w.call(ctx, "Decrypt", in, nil)
	var ae *apiError
	switch {
	case errors.As(err, &ae) && ae.status == http.StatusBadRequest && ae.typ == "DryRunOperationException":
		return nil
	case err == nil:
		return remotekms.Unreachable(what, errors.New("KMS decrypted a dry run"))
	case errors.As(err, &ae) && dryRunUnsupported(ae):
		return remotekms.Unreachable(what, fmt.Errorf("%w: %w", errDryRunUnverified, ae))
	default:
		return remotekms.Unreachable(what, err)
	}
}

// errDryRunUnverified is the probe's reason for an endpoint that does not do the dry run.
var errDryRunUnverified = errors.New("awskms: the endpoint ignored or rejected DryRun; the decrypt right is unverified")

// dryRunUnsupported is an endpoint's answer to a dry run it does not do: it ignored DryRun and
// refused the junk ciphertext (AWS KMS never reads it with IGNORE_CIPHERTEXT), or it rejected the
// parameters with a 400 validation or serialization error that names them. An authorization
// refusal never is.
func dryRunUnsupported(ae *apiError) bool {
	if ae.status != http.StatusBadRequest {
		return false
	}
	switch ae.typ {
	case "InvalidCiphertextException":
		return true
	case "ValidationException", "SerializationException":
		return strings.Contains(ae.msg, "DryRun")
	default:
		return false
	}
}

func (w *Wrapper) logger() *slog.Logger {
	if w.log != nil {
		return w.log
	}
	return slog.Default()
}

func (w *Wrapper) decrypt(ctx context.Context, blob []byte, out any) error {
	in := map[string]any{"KeyId": w.cfg.KeyID, "CiphertextBlob": blob, "EncryptionContext": encryptionContext}
	return w.call(ctx, "Decrypt", in, out)
}

// apiError is KMS's own error answer: an HTTP status and the JSON error type it names.
type apiError struct {
	status   int
	typ, msg string
}

func (e *apiError) Error() string {
	if e.typ == "" {
		return fmt.Sprintf("HTTP %d", e.status)
	}
	if e.msg == "" {
		return fmt.Sprintf("HTTP %d: %s", e.status, e.typ)
	}
	return fmt.Sprintf("HTTP %d: %s: %s", e.status, e.typ, e.msg)
}

// staleCredentials are KMS's answers to credentials that expired or were revoked: refresh once.
var staleCredentials = []string{"ExpiredTokenException", "UnrecognizedClientException", "InvalidSignatureException"}

// signatureErrors may quote the canonical request, which holds the session token: never shown.
var signatureErrors = []string{"InvalidSignatureException", "IncompleteSignatureException", "IncompleteSignature"}

// errRedirect is a 3xx answer, which the adapter never follows.
var errRedirect = errors.New("redirect refused: set the endpoint KMS answers on")

// maxResponse bounds what is read of an answer: a KEK's ciphertext is well under a kilobyte.
const maxResponse = 1 << 20

// call calls the KMS operation op with in, refreshing the credentials first if needed, and once
// more if KMS refused them as stale. It returns nil (out holds the answer), an *apiError (KMS
// answered with its own error), or another error (no answer, a redirect, an answer that is not
// KMS's).
func (w *Wrapper) call(ctx context.Context, op string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	defer clear(body)
	for attempt := 0; ; attempt++ {
		creds, err := w.credentials(ctx)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.endpoint.String(), bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/x-amz-json-1.1")
		req.Header.Set("X-Amz-Target", "TrentService."+op)
		sign(req, body, creds, w.cfg.Region, "kms", w.now())
		status, raw, err := w.do(w.http, req)
		if err != nil {
			return err
		}
		switch {
		case status == http.StatusOK:
			if out == nil {
				return nil
			}
			if err := json.Unmarshal(raw, out); err != nil {
				clear(raw)
				return errors.New("an answer that is not KMS's")
			}
			clear(raw)
			return nil
		case status >= 300 && status < 400:
			return fmt.Errorf("HTTP %d: %w", status, errRedirect)
		}
		ae := parseError(status, raw, creds)
		if attempt == 0 && slices.Contains(staleCredentials, ae.typ) {
			w.forget(creds)
			continue
		}
		return ae
	}
}

// parseError reads KMS's JSON error ({"__type": ..., "message": ...}). A body that is not one (a
// proxy's page) leaves the type empty, so it never matches a KMS error. The message is dropped
// where it could quote a credential.
func parseError(status int, raw []byte, c credentials) *apiError {
	var e struct {
		Type     string `json:"__type"`
		Message  string `json:"message"`
		Message2 string `json:"Message"`
	}
	if json.Unmarshal(raw, &e) != nil || e.Type == "" {
		return &apiError{status: status}
	}
	typ := e.Type
	if i := strings.LastIndex(typ, "#"); i >= 0 {
		typ = typ[i+1:]
	}
	typ, _, _ = strings.Cut(typ, ":")
	msg := e.Message
	if msg == "" {
		msg = e.Message2
	}
	if slices.Contains(signatureErrors, typ) ||
		(c.AccessKeyID != "" && strings.Contains(msg, c.AccessKeyID)) ||
		(c.SessionToken != "" && strings.Contains(msg, c.SessionToken)) {
		msg = ""
	}
	return &apiError{status: status, typ: typ, msg: short(msg)}
}

// expirySkew is how long before their expiry credentials are no longer used: a request signed now
// must still be valid when KMS checks it.
const expirySkew = time.Minute

// refreshRetry is how long after a failed refresh the next one starts while the current credentials
// are still usable: during an STS or Pod Identity Agent outage the calls go on with them, and at
// most one refresh per refreshRetry tries the endpoint.
const refreshRetry = 10 * time.Second

// credentials returns credentials that have not expired, refreshing them when there are none.
// After 4/5 of their lifetime it starts a refresh in the background and keeps using the current
// ones until they expire (less expirySkew): a failed refresh is logged, not a failed call, and the
// next one waits refreshRetry. Callers that need credentials while a refresh is in flight share it,
// and each stops waiting when its own ctx ends. The refresh is not bound to the first caller's ctx
// (its end would fail every other waiter), only to the configured timeout.
func (w *Wrapper) credentials(ctx context.Context) (credentials, error) {
	w.mu.Lock()
	now := w.now()
	if c := w.creds; c.valid() && w.usable(now) {
		if !w.refresh.IsZero() && !now.Before(w.refresh) && w.pending == nil {
			w.startRefresh(ctx)
		}
		w.mu.Unlock()
		return c, nil
	}
	call := w.pending
	if call == nil {
		call = w.startRefresh(ctx)
	}
	w.mu.Unlock()
	select {
	case <-call.done:
		return call.creds, call.err
	case <-ctx.Done():
		return credentials{}, fmt.Errorf("waiting for credentials: %w", ctx.Err())
	}
}

// usable reports whether the current credentials may still sign a request at now; w.mu is held.
func (w *Wrapper) usable(now time.Time) bool {
	return w.expires.IsZero() || now.Before(w.expires.Add(-expirySkew))
}

// startRefresh starts a refresh; w.mu is held.
func (w *Wrapper) startRefresh(ctx context.Context) *refreshCall {
	call := &refreshCall{done: make(chan struct{})}
	w.pending = call
	go w.runRefresh(context.WithoutCancel(ctx), call)
	return call
}

func (w *Wrapper) runRefresh(ctx context.Context, call *refreshCall) {
	ctx, cancel := remotekms.Bound(ctx, w.cfg.Timeout)
	defer cancel()
	c, err := w.fetch(ctx)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pending = nil
	call.creds, call.err = c, err
	switch {
	case err == nil:
		w.creds, w.refresh, w.expires = c, time.Time{}, c.Expires
		if !c.Expires.IsZero() {
			// Refresh when 4/5 of the lifetime is gone (STS: 1 h; the Pod Identity Agent: up to 6 h).
			now := w.now()
			w.refresh = now.Add(c.Expires.Sub(now) * 4 / 5)
		}
	case w.creds.valid() && w.usable(w.now()):
		// The current credentials stay in use until they expire; the next refresh waits refreshRetry.
		w.refresh = w.now().Add(refreshRetry)
		w.logger().Warn("awskms: credential refresh failed; the current credentials stay in use until they expire",
			"expires", w.expires, "retry_in", refreshRetry, "error", err)
	}
	close(call.done)
}

// forget drops c if it is still the current one, so the next call refreshes.
func (w *Wrapper) forget(c credentials) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.creds.AccessKeyID == c.AccessKeyID && w.creds.SessionToken == c.SessionToken {
		w.creds, w.refresh, w.expires = credentials{}, time.Time{}, time.Time{}
	}
}

// scrubURL is a transport error without the request URL, with its inner cause, which is what tells
// a refused connection from a DNS failure.
func scrubURL(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err.Error()
	}
	return err.Error()
}
