// SPDX-License-Identifier: Apache-2.0

package actortoken

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// The Substrate verifier checks the actor JWT that Substrate's ate-api-server mints
// (MintActorJwt) and atenet-egress puts in the Koinon-Actor-Token header, outside the guest (ADR
// 0120, amendment for issue #1386). The gateway holds no private key: it reads the issuer's
// public keys from the issuer's OIDC discovery document and JWK set, over TLS checked against a
// configured CA bundle only.
//
// Every failure refuses. A fetch error never lets a token through: without fresh keys the
// verifier returns ErrUnavailable.

// Limits of the Substrate verifier.
const (
	// MaxTokenSize is the largest token the verifier parses.
	MaxTokenSize = 16 << 10
	// MaxDocumentSize is the largest discovery document or JWK set the verifier reads.
	MaxDocumentSize = 1 << 20
	// SubstrateLeeway is the clock skew the Substrate verifier accepts on exp, nbf and iat.
	SubstrateLeeway = 30 * time.Second
	// KeyRefetchInterval is the shortest time between two fetches of the issuer's keys. An
	// unknown kid causes a fetch at most this often, so a stream of forged kids cannot make the
	// gateway flood the issuer.
	KeyRefetchInterval = time.Minute
	// KeyMaxAge is how long fetched keys stay in use. After it the verifier fetches again, so a
	// key the issuer removed stops working. Keys older than this are not used, even when the
	// fetch fails.
	KeyMaxAge = 10 * time.Minute

	fetchTimeout = 10 * time.Second
	dialTimeout  = 5 * time.Second

	substrateActorClaim  = "actor.ate.dev"
	substrateWorkerClaim = "worker-for-actor.ate.dev"
	substrateActorPrefix = "actor/"
	wellKnownPath        = "/.well-known/openid-configuration"
)

// substrateAlgs are the only signature algorithms the Substrate verifier accepts.
var substrateAlgs = []jose.SignatureAlgorithm{jose.ES256, jose.RS256}

// reasonError is a token failure with a fixed reason. Every reasonError matches ErrInvalid with
// errors.Is, so a caller that only needs "invalid" can test for that.
type reasonError struct{ reason string }

func (e *reasonError) Error() string { return "actortoken: invalid actor token: " + e.reason }

// Is makes every reasonError match ErrInvalid.
func (e *reasonError) Is(target error) bool { return target == ErrInvalid }

// Reasons the Substrate verifier refuses a token. Each matches ErrInvalid with errors.Is. The
// reason text is a fixed, low-cardinality string (Reason), safe for a log field or a metric label.
var (
	// ErrPlaceholder means the request carried Placeholder: atenet-egress did not replace it.
	ErrPlaceholder error = &reasonError{"placeholder"}
	// ErrTokenTooLarge means the token is longer than MaxTokenSize.
	ErrTokenTooLarge error = &reasonError{"too_large"}
	// ErrMalformed means the token is not a compact JWS with one signature and an allowed alg.
	ErrMalformed error = &reasonError{"malformed"}
	// ErrUnknownKey means the token has no kid, a kid the issuer does not publish, or an alg that
	// is not the alg of that key.
	ErrUnknownKey error = &reasonError{"unknown_key"}
	// ErrSignature means the signature does not verify.
	ErrSignature error = &reasonError{"signature"}
	// ErrIssuer means the iss claim is not the configured issuer.
	ErrIssuer error = &reasonError{"issuer"}
	// ErrAudience means the aud claim does not contain Audience.
	ErrAudience error = &reasonError{"audience"}
	// ErrExpired means exp is in the past (beyond the leeway).
	ErrExpired error = &reasonError{"expired"}
	// ErrNotYetValid means nbf or iat is in the future (beyond the leeway).
	ErrNotYetValid error = &reasonError{"not_yet_valid"}
	// ErrLifetime means exp, iat, nbf or jti is missing, or exp - iat is outside MinTTL to MaxTTL.
	ErrLifetime error = &reasonError{"lifetime"}
	// ErrSubject means the token does not name one Substrate actor: sub and the actor.ate.dev
	// claim disagree or are incomplete, or the token is a worker-for-actor token.
	ErrSubject error = &reasonError{"subject"}
)

// ErrUnavailable means the verifier has no fresh keys: the issuer's discovery document or JWK set
// could not be fetched or failed a check. It does not match ErrInvalid: the token was not judged.
// The request is refused all the same.
var ErrUnavailable = errors.New("actortoken: Substrate issuer keys are unavailable")

// Reason returns a fixed, low-cardinality reason for an error from SubstrateVerifier.Verify:
// the reason of a token failure, "unavailable" for ErrUnavailable, "" for nil, and "invalid" for
// any other error.
func Reason(err error) string {
	var re *reasonError
	switch {
	case err == nil:
		return ""
	case errors.As(err, &re):
		return re.reason
	case errors.Is(err, ErrUnavailable):
		return "unavailable"
	}
	return "invalid"
}

// SubstrateConfig configures a SubstrateVerifier.
type SubstrateConfig struct {
	// Issuer is the one issuer the verifier trusts, for example "https://idp.ate-system.svc".
	// It must be an https URL. The verifier reads <Issuer>/.well-known/openid-configuration, and
	// the document's issuer must equal Issuer exactly. The token never chooses the issuer.
	Issuer string
	// CAFile is a PEM bundle of the CAs that may sign the issuer's TLS certificate. Set CAFile or
	// RootCAs, not both. There is no option to skip TLS verification, and the system roots are
	// not used.
	CAFile string
	// RootCAs is the same as CAFile, as a pool.
	RootCAs *x509.CertPool
	// Now is the clock. Nil means time.Now. It is for tests.
	Now func() time.Time
}

// SubstrateClaims are the checked contents of a Substrate actor token.
type SubstrateClaims struct {
	Atespace  string
	ActorName string
	ActorUID  string
	Expiry    time.Time
}

// SubstrateVerifier checks Substrate actor JWTs. It is safe for concurrent use.
type SubstrateVerifier struct {
	ctx       context.Context
	issuer    string
	discovery string
	client    *http.Client
	now       func() time.Time

	mu          sync.Mutex
	keys        map[string]jose.JSONWebKey
	fetchedAt   time.Time     // when the fetch that gave keys started; zero if never
	lastAttempt time.Time     // when the last fetch started; zero if never
	lastErr     error         // the error of the last fetch, nil after a success
	inflight    chan struct{} // closed when the fetch in flight ends; nil if none
}

// NewSubstrateVerifier returns a verifier for cfg. It does not contact the issuer: the first
// Verify fetches the keys. ctx bounds every fetch; when it ends, Verify returns ErrUnavailable
// once the cached keys are stale.
func NewSubstrateVerifier(ctx context.Context, cfg SubstrateConfig) (*SubstrateVerifier, error) {
	if err := checkIssuerURL(cfg.Issuer); err != nil {
		return nil, err
	}
	pool, err := rootPool(cfg)
	if err != nil {
		return nil, err
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &SubstrateVerifier{
		ctx:       ctx,
		issuer:    cfg.Issuer,
		discovery: strings.TrimSuffix(cfg.Issuer, "/") + wellKnownPath,
		client:    newIssuerClient(pool),
		now:       now,
	}, nil
}

func checkIssuerURL(issuer string) error {
	if issuer == "" {
		return errors.New("actortoken: a Substrate verifier needs an issuer")
	}
	u, err := url.Parse(issuer)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" ||
		u.Fragment != "" || strings.HasSuffix(issuer, "#") || strings.HasSuffix(issuer, "?") {
		return fmt.Errorf("actortoken: the Substrate issuer %q must be an https URL with no user, query or fragment", issuer)
	}
	return nil
}

func rootPool(cfg SubstrateConfig) (*x509.CertPool, error) {
	switch {
	case cfg.CAFile != "" && cfg.RootCAs != nil:
		return nil, errors.New("actortoken: set the Substrate CA file or the CA pool, not both")
	case cfg.RootCAs != nil:
		if cfg.RootCAs.Equal(x509.NewCertPool()) {
			return nil, errors.New("actortoken: the Substrate CA pool is empty")
		}
		return cfg.RootCAs, nil
	case cfg.CAFile != "":
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("actortoken: read the Substrate CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("actortoken: the Substrate CA file %q has no PEM certificate", cfg.CAFile)
		}
		return pool, nil
	}
	return nil, errors.New("actortoken: a Substrate verifier needs a CA file or a CA pool for the issuer's TLS")
}

var errRedirect = errors.New("actortoken: the Substrate issuer sent a redirect; redirects are refused")

// newIssuerClient returns an HTTP client that trusts only pool, connects directly (no proxy),
// refuses redirects and has timeouts.
func newIssuerClient(pool *x509.CertPool) *http.Client {
	tr := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: dialTimeout}).DialContext,
		TLSClientConfig:       &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   dialTimeout,
		ResponseHeaderTimeout: fetchTimeout,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          2,
		IdleConnTimeout:       90 * time.Second,
	}
	return &http.Client{
		Transport:     tr,
		Timeout:       fetchTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return errRedirect },
	}
}

// substrateWire is the JSON payload of a Substrate actor JWT (Substrate internal/actoridjwt
// WireClaims). aud is always an array there; jwt.Audience also reads a string.
type substrateWire struct {
	Issuer    string           `json:"iss"`
	Subject   string           `json:"sub"`
	Audience  jwt.Audience     `json:"aud"`
	Expiry    *jwt.NumericDate `json:"exp"`
	NotBefore *jwt.NumericDate `json:"nbf"`
	IssuedAt  *jwt.NumericDate `json:"iat"`
	ID        string           `json:"jti"`
	Actor     *substrateActor  `json:"actor.ate.dev"`
	// Worker is decoded only to see that it is absent.
	Worker json.RawMessage `json:"worker-for-actor.ate.dev"`
}

type substrateActor struct {
	Atespace  string `json:"atespace"`
	ActorName string `json:"actorName"`
	ActorUID  string `json:"actorUID"`
	Node      string `json:"node"`
}

// Verify checks token and returns its claims. A token failure matches ErrInvalid (and one of the
// reason errors); a key fetch failure is ErrUnavailable. Every error refuses the request.
func (v *SubstrateVerifier) Verify(token string) (SubstrateClaims, error) {
	if len(token) > MaxTokenSize {
		return SubstrateClaims{}, ErrTokenTooLarge
	}
	if token == Placeholder {
		return SubstrateClaims{}, ErrPlaceholder
	}
	tok, err := jwt.ParseSigned(token, substrateAlgs)
	if err != nil || len(tok.Headers) != 1 {
		return SubstrateClaims{}, ErrMalformed
	}
	h := tok.Headers[0]
	if h.KeyID == "" {
		return SubstrateClaims{}, ErrUnknownKey
	}
	key, err := v.key(h.KeyID)
	if err != nil {
		return SubstrateClaims{}, err
	}
	if key.Algorithm != h.Algorithm {
		return SubstrateClaims{}, ErrUnknownKey
	}
	var w substrateWire
	if err := tok.Claims(key.Key, &w); err != nil {
		return SubstrateClaims{}, ErrSignature
	}
	return v.check(&w)
}

func (v *SubstrateVerifier) check(w *substrateWire) (SubstrateClaims, error) {
	if w.Issuer != v.issuer {
		return SubstrateClaims{}, ErrIssuer
	}
	if !w.Audience.Contains(Audience) {
		return SubstrateClaims{}, ErrAudience
	}
	if w.Expiry == nil || w.IssuedAt == nil || w.NotBefore == nil || w.ID == "" {
		return SubstrateClaims{}, ErrLifetime
	}
	exp, iat, nbf := w.Expiry.Time(), w.IssuedAt.Time(), w.NotBefore.Time()
	// exp - iat, not exp - nbf: ate-api-server sets nbf to iat - 5 min.
	if life := exp.Sub(iat); life < MinTTL || life > MaxTTL {
		return SubstrateClaims{}, ErrLifetime
	}
	now := v.now()
	if now.After(exp.Add(SubstrateLeeway)) {
		return SubstrateClaims{}, ErrExpired
	}
	if now.Add(SubstrateLeeway).Before(nbf) || now.Add(SubstrateLeeway).Before(iat) {
		return SubstrateClaims{}, ErrNotYetValid
	}
	if !isNull(w.Worker) || w.Actor == nil {
		return SubstrateClaims{}, ErrSubject
	}
	a := w.Actor
	if a.Atespace == "" || a.ActorName == "" || a.ActorUID == "" || a.Node != "" ||
		strings.Contains(a.Atespace, "/") || strings.Contains(a.ActorName, "/") ||
		w.Subject != substrateActorPrefix+a.Atespace+"/"+a.ActorName {
		return SubstrateClaims{}, ErrSubject
	}
	return SubstrateClaims{Atespace: a.Atespace, ActorName: a.ActorName, ActorUID: a.ActorUID, Expiry: exp}, nil
}

func isNull(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s == "" || s == "null"
}

// key returns the published key kid. It fetches the issuer's keys when there are none, when they
// are older than KeyMaxAge, or when kid is unknown, but at most once per KeyRefetchInterval. The
// fetch runs without the lock, so requests with a known kid do not wait for it; a request that
// needs the fetch in flight waits for it to end.
func (v *SubstrateVerifier) key(kid string) (jose.JSONWebKey, error) {
	v.mu.Lock()
	now := v.now()
	if k, ok := v.keys[kid]; ok && v.freshLocked(now) {
		v.mu.Unlock()
		return k, nil
	}
	switch wait := v.inflight; {
	case wait != nil:
		v.mu.Unlock()
		<-wait
	case v.lastAttempt.IsZero() || now.Sub(v.lastAttempt) >= KeyRefetchInterval:
		v.lastAttempt = now
		done := make(chan struct{})
		v.inflight = done
		v.mu.Unlock()
		keys, err := v.fetchKeys()
		v.mu.Lock()
		if err == nil {
			v.keys, v.fetchedAt = keys, now
		}
		v.lastErr = err
		v.inflight = nil
		close(done)
		v.mu.Unlock()
	default:
		v.mu.Unlock()
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.freshLocked(v.now()) {
		if v.lastErr != nil {
			return jose.JSONWebKey{}, fmt.Errorf("%w: %w", ErrUnavailable, v.lastErr)
		}
		return jose.JSONWebKey{}, ErrUnavailable
	}
	if k, ok := v.keys[kid]; ok {
		return k, nil
	}
	return jose.JSONWebKey{}, ErrUnknownKey
}

// freshLocked reports whether the cached keys are younger than KeyMaxAge. v.mu must be held.
func (v *SubstrateVerifier) freshLocked(now time.Time) bool {
	return v.keys != nil && now.Sub(v.fetchedAt) < KeyMaxAge
}

type discoveryDoc struct {
	Issuer  string `json:"issuer"`
	JWKSURI string `json:"jwks_uri"`
}

// fetchKeys reads the discovery document and the JWK set and returns the usable signing keys by
// kid. Any failure is an error; the caller then keeps no fresh keys.
func (v *SubstrateVerifier) fetchKeys() (map[string]jose.JSONWebKey, error) {
	var d discoveryDoc
	if err := v.fetchJSON(v.discovery, &d); err != nil {
		return nil, err
	}
	if d.Issuer != v.issuer {
		return nil, fmt.Errorf("actortoken: discovery issuer %q is not the configured issuer", d.Issuer)
	}
	u, err := url.Parse(d.JWKSURI)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return nil, errors.New("actortoken: discovery jwks_uri is not an https URL")
	}
	var set jose.JSONWebKeySet
	if err := v.fetchJSON(u.String(), &set); err != nil {
		return nil, err
	}
	keys := make(map[string]jose.JSONWebKey, len(set.Keys))
	for _, k := range set.Keys {
		if !usableKey(k) {
			continue
		}
		if _, dup := keys[k.KeyID]; dup {
			return nil, fmt.Errorf("actortoken: the JWK set has kid %q twice", k.KeyID)
		}
		keys[k.KeyID] = k
	}
	if len(keys) == 0 {
		return nil, errors.New("actortoken: the JWK set has no usable ES256 or RS256 key")
	}
	return keys, nil
}

// usableKey reports whether k is a public signing key with a kid and an alg the verifier accepts,
// and whether the key type fits the alg: ES256 needs a P-256 key, RS256 an RSA key of at least
// 2048 bits.
func usableKey(k jose.JSONWebKey) bool {
	if k.KeyID == "" || (k.Use != "" && k.Use != "sig") || !k.Valid() || !k.IsPublic() {
		return false
	}
	switch k.Algorithm {
	case string(jose.ES256):
		pk, ok := k.Key.(*ecdsa.PublicKey)
		return ok && pk.Curve == elliptic.P256()
	case string(jose.RS256):
		pk, ok := k.Key.(*rsa.PublicKey)
		return ok && pk.N.BitLen() >= 2048
	}
	return false
}

func (v *SubstrateVerifier) fetchJSON(target string, out any) error {
	ctx, cancel := context.WithTimeout(v.ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return fmt.Errorf("actortoken: issuer request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := v.client.Do(req)
	if err != nil {
		return fmt.Errorf("actortoken: issuer fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("actortoken: issuer fetch: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxDocumentSize+1))
	if err != nil {
		return fmt.Errorf("actortoken: issuer read: %w", err)
	}
	if len(body) > MaxDocumentSize {
		return errors.New("actortoken: issuer document is larger than the limit")
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("actortoken: issuer document: %w", err)
	}
	return nil
}
