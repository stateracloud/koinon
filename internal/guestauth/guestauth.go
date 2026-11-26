// SPDX-License-Identifier: Apache-2.0

// Package guestauth is the credential of the guest API (issue #1309). The Substrate driver signs a
// short-lived token for each call to a sandbox's guest server, and the guest front
// (internal/guestfront) verifies it before the upstream handler runs, so agent code in the sandbox
// cannot call the guest API itself.
//
// The token is a compact JWS (EdDSA, Ed25519) with these claims:
//
//   - aud: AudiencePrefix + "<atespace>/<actor>", the one sandbox the call is for.
//   - iat, exp: the token lives TokenTTL.
//   - jti: random; the front accepts each jti once.
//   - koinon_prev: the actor this sandbox was restored from, when it was restored.
//
// Only the control plane holds the private key. The guest gets the public keys (VerifyKeysEnv), so
// the guest, its snapshots and the payload hold no secret (ADR 0120 section 1).
//
// The front cannot learn its own actor name: the router removes the target header, and an actor has
// no per-actor input. So the front pins the aud of the first verified call and then accepts only
// that aud. A restore makes a new actor with the source's memory, pin included. The driver then signs
// koinon_prev = the source actor, and the front moves its pin only when koinon_prev names the pin it
// has.
package guestauth

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// Header is the request header that carries the token. The atenet router forwards every header but
// its own target header, so it reaches the guest.
const Header = "x-koinon-guest-auth"

// AudiencePrefix starts every aud, so a token for the guest API is never valid for another
// audience that the same key could sign.
const AudiencePrefix = "koinon-guest:"

// VerifyKeysEnv is the environment variable of the actor template that holds the public keys, as
// EncodeKeys writes them. The front reads it and removes it from its environment before it starts a
// process.
const VerifyKeysEnv = "KOINON_GUEST_VERIFY_KEYS"

// BindMethod is the full gRPC method of the guest front's own Bind call. It does nothing but pass
// the token check, so a valid call sets or moves the pin. The driver calls it before a memory
// snapshot, so the snapshot holds the pin of the sandbox it was taken from, and a restore of it
// needs only one koinon_prev.
const BindMethod = "/koinon.guestfront.v1.Front/Bind"

// TokenTTL is how long a token is valid.
const TokenTTL = 60 * time.Second

// clockSkew is how far the clocks of the driver and the guest may differ.
const clockSkew = 30 * time.Second

// maxLifetime bounds exp - iat of an accepted token. It stops a token with a far exp, even when a
// signer has a bug.
const maxLifetime = TokenTTL + clockSkew

// maxTokenLen bounds the header before it is parsed.
const maxTokenLen = 4096

// maxActorRef bounds an actor reference in a claim.
const maxActorRef = 512

// Audience returns the aud of a call to the actor "<atespace>/<actor>".
func Audience(actorRef string) string { return AudiencePrefix + actorRef }

// claims is the payload of a token.
type claims struct {
	jwt.Claims
	Prev string `json:"koinon_prev,omitempty"`
}

// KeyID returns the kid of a public key: the first 16 hex characters of its SHA-256.
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// ParseSeed reads an Ed25519 private key from its 32-byte seed as 64 hex characters, the form of an
// ensure-secrets "ed25519-key" Secret. Spaces and newlines around it are ignored.
func ParseSeed(s string) (ed25519.PrivateKey, error) {
	b, err := hex.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != ed25519.SeedSize {
		return nil, errors.New("guestauth: the signing key is not 64 hex characters")
	}
	return ed25519.NewKeyFromSeed(b), nil
}

// EncodeKeys writes public keys for VerifyKeysEnv: unpadded base64url, comma-separated, in order.
func EncodeKeys(keys ...ed25519.PublicKey) string {
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = base64.RawURLEncoding.EncodeToString(k)
	}
	return strings.Join(parts, ",")
}

// DecodeKeys reads the value of VerifyKeysEnv. It fails on an empty value or a bad key: a front
// without a key must not start.
func DecodeKeys(s string) ([]ed25519.PublicKey, error) {
	if strings.TrimSpace(s) == "" {
		return nil, errors.New("guestauth: no verify key")
	}
	var keys []ed25519.PublicKey
	for _, p := range strings.Split(s, ",") {
		b, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(p))
		if err != nil || len(b) != ed25519.PublicKeySize {
			return nil, errors.New("guestauth: a verify key is not a base64url Ed25519 public key")
		}
		keys = append(keys, ed25519.PublicKey(b))
	}
	return keys, nil
}

// Signer makes tokens. It is safe for concurrent use.
type Signer struct {
	signer jose.Signer
	pub    ed25519.PublicKey
	now    func() time.Time
}

// NewSigner returns a Signer for key.
func NewSigner(key ed25519.PrivateKey) (*Signer, error) {
	pub, ok := key.Public().(ed25519.PublicKey)
	if !ok || len(key) != ed25519.PrivateKeySize {
		return nil, errors.New("guestauth: not an Ed25519 private key")
	}
	s, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", KeyID(pub)))
	if err != nil {
		return nil, fmt.Errorf("guestauth: %w", err)
	}
	return &Signer{signer: s, pub: pub, now: time.Now}, nil
}

// PublicKey returns the public key that verifies the Signer's tokens.
func (s *Signer) PublicKey() ed25519.PublicKey { return s.pub }

// Sign returns a token for one call to actorRef ("<atespace>/<actor>"). prev is the actor reference
// the sandbox was restored from, or empty.
func (s *Signer) Sign(actorRef, prev string) (string, error) {
	if actorRef == "" || len(actorRef) > maxActorRef || len(prev) > maxActorRef {
		return "", errors.New("guestauth: bad actor reference")
	}
	var jti [16]byte
	if _, err := rand.Read(jti[:]); err != nil {
		return "", fmt.Errorf("guestauth: %w", err)
	}
	now := s.now()
	c := claims{
		Claims: jwt.Claims{
			Audience: jwt.Audience{Audience(actorRef)},
			IssuedAt: jwt.NewNumericDate(now),
			Expiry:   jwt.NewNumericDate(now.Add(TokenTTL)),
			ID:       hex.EncodeToString(jti[:]),
		},
		Prev: prev,
	}
	tok, err := jwt.Signed(s.signer).Claims(c).Serialize()
	if err != nil {
		return "", fmt.Errorf("guestauth: %w", err)
	}
	return tok, nil
}

// ErrDenied is the error of every refused token. The reason is in the wrapped text; the front logs
// it and never sends it to the caller.
var ErrDenied = errors.New("guestauth: denied")

// Verifier checks tokens and holds the pin. It is safe for concurrent use.
type Verifier struct {
	keys map[string]ed25519.PublicKey
	now  func() time.Time

	mu   sync.Mutex
	pin  string               // the aud accepted; empty until the first verified call
	seen map[string]time.Time // jti -> exp of accepted tokens, until they expire
}

// NewVerifier returns a Verifier for keys. It fails without a key.
func NewVerifier(keys []ed25519.PublicKey) (*Verifier, error) {
	if len(keys) == 0 {
		return nil, errors.New("guestauth: no verify key")
	}
	m := make(map[string]ed25519.PublicKey, len(keys))
	for _, k := range keys {
		if len(k) != ed25519.PublicKeySize {
			return nil, errors.New("guestauth: a verify key is not an Ed25519 public key")
		}
		m[KeyID(k)] = k
	}
	return &Verifier{keys: m, now: time.Now, seen: map[string]time.Time{}}, nil
}

// Pin returns the aud the Verifier accepts, or empty before the first verified call.
func (v *Verifier) Pin() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.pin
}

// Clone returns a copy of the Verifier with its pin and replay entries, as a memory snapshot of the
// guest holds them. Test fakes of a restore use it.
func (v *Verifier) Clone() *Verifier {
	v.mu.Lock()
	defer v.mu.Unlock()
	seen := make(map[string]time.Time, len(v.seen))
	for id, until := range v.seen {
		seen[id] = until
	}
	return &Verifier{keys: v.keys, now: v.now, pin: v.pin, seen: seen}
}

func deny(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrDenied}, args...)...)
}

// Verify checks token and, when it is valid, applies the pin rule. Every error wraps ErrDenied.
func (v *Verifier) Verify(token string) error {
	if token == "" {
		return deny("no token")
	}
	if len(token) > maxTokenLen {
		return deny("token too long")
	}
	tok, err := jwt.ParseSigned(token, []jose.SignatureAlgorithm{jose.EdDSA})
	if err != nil {
		return deny("parse: %v", err)
	}
	if len(tok.Headers) != 1 {
		return deny("want one signature")
	}
	key, ok := v.keys[tok.Headers[0].KeyID]
	if !ok {
		return deny("unknown key id")
	}
	var c claims
	if err := tok.Claims(key, &c); err != nil {
		return deny("signature: %v", err)
	}
	now := v.now()
	if c.Expiry == nil || c.IssuedAt == nil || c.ID == "" || len(c.ID) > 64 {
		return deny("missing exp, iat or jti")
	}
	exp, iat := c.Expiry.Time(), c.IssuedAt.Time()
	switch {
	case !now.Before(exp.Add(clockSkew)):
		return deny("expired")
	case iat.After(now.Add(clockSkew)):
		return deny("issued in the future")
	case exp.Sub(iat) > maxLifetime || exp.Before(iat):
		return deny("bad lifetime")
	}
	if len(c.Audience) != 1 || !strings.HasPrefix(c.Audience[0], AudiencePrefix) ||
		len(c.Audience[0]) == len(AudiencePrefix) || len(c.Audience[0]) > len(AudiencePrefix)+maxActorRef {
		return deny("bad audience")
	}
	aud := c.Audience[0]

	v.mu.Lock()
	defer v.mu.Unlock()
	v.prune(now)
	if _, dup := v.seen[c.ID]; dup {
		return deny("replayed jti")
	}
	switch {
	case v.pin == "" || v.pin == aud:
	case c.Prev != "" && Audience(c.Prev) == v.pin:
		// A restore: the driver names the actor this sandbox came from.
	default:
		return deny("audience is not this sandbox")
	}
	v.pin = aud
	v.seen[c.ID] = exp.Add(clockSkew)
	return nil
}

// prune drops replay entries whose token can no longer pass the expiry check. v.mu is held.
func (v *Verifier) prune(now time.Time) {
	for id, until := range v.seen {
		if !now.Before(until) {
			delete(v.seen, id)
		}
	}
}
