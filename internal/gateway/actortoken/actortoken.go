// SPDX-License-Identifier: Apache-2.0

// Package actortoken mints and checks the actor identity token (ADR 0120 section 1, ADR 0119 D1, ADR
// 0097 decision 11).
//
// The execution layer mints one token for each sandbox actor and injects it outside the guest. It is
// the only credential-like value the guest holds. Its audience is the Koinon gateway, and its
// lifetime is 300 to 3600 seconds. The gateway checks it on every forge and registry route (package
// route) before any credential is attached.
//
// The token is a compact JWS signed with Ed25519 (alg EdDSA). The minter holds the private key. The
// gateway holds public keys only, so a gateway breach does not let an attacker mint tokens. The
// verifier accepts no other algorithm, and no "none".
//
// A token names who acts (org, agent, run) and nothing else. It carries no credential and no route
// rule: what an actor may reach is the gateway's route table, read from Koinon state.
package actortoken

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// Audience is the audience of every Ed25519 actor token (the OpenShell driver): the Koinon gateway.
const Audience = "koinon-gateway"

// The audiences of the Substrate actor JWT, one for each koinon-gateway listener (issue #1489, ADR 0120
// amendment of 2026-11-27). atenet-egress mints the token of each gateway rule with the audience of that
// listener, and each listener accepts only its own: a token copied from one listener's request is
// refused on another.
const (
	AudienceModel  = "koinon-gateway-model"
	AudienceMCP    = "koinon-gateway-mcp"
	AudienceEgress = "koinon-gateway-egress"
)

// listenerAudiences are the Substrate listener audiences.
var listenerAudiences = [...]string{AudienceModel, AudienceMCP, AudienceEgress}

// Lifetimes (ADR 0120 section 1.2).
const (
	MinTTL = 300 * time.Second
	MaxTTL = 3600 * time.Second
	// DefaultTTL is what a minter uses when the caller names none.
	DefaultTTL = 900 * time.Second
	// DefaultLeeway is the clock skew a verifier accepts.
	DefaultLeeway = 5 * time.Second
)

// Errors. A verifier names the class of the failure, never the token.
var (
	// ErrInvalid means the token is malformed, signed by an unknown key, expired, not yet valid, for
	// another audience or issuer, or has a lifetime outside MinTTL to MaxTTL. Callers do not say which.
	ErrInvalid = errors.New("actortoken: invalid actor token")
	// ErrTTL means a requested lifetime is outside MinTTL to MaxTTL.
	ErrTTL = errors.New("actortoken: lifetime must be 300 to 3600 seconds")
)

// Actor is who a token speaks for.
type Actor struct {
	Org   string
	Agent string
	// Run is the run the sandbox belongs to. It may be empty for an actor outside a run.
	Run string
}

// Validate reports whether the actor names an org and an agent.
func (a Actor) Validate() error {
	if a.Org == "" || a.Agent == "" {
		return errors.New("actortoken: an actor needs an org and an agent")
	}
	return nil
}

// Claims are the checked contents of a token.
type Claims struct {
	Issuer    string
	Actor     Actor
	ID        string
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// wire is the JSON payload. Field names are short and fixed.
type wire struct {
	Issuer    string           `json:"iss"`
	Subject   string           `json:"sub"`
	Audience  jwt.Audience     `json:"aud"`
	Expiry    *jwt.NumericDate `json:"exp"`
	IssuedAt  *jwt.NumericDate `json:"iat"`
	NotBefore *jwt.NumericDate `json:"nbf"`
	ID        string           `json:"jti"`
	Org       string           `json:"org"`
	Agent     string           `json:"agent"`
	Run       string           `json:"run,omitempty"`
}

// Minter signs actor tokens. It holds the private key and runs outside the guest and outside the
// gateway.
type Minter struct {
	issuer string
	kid    string
	signer jose.Signer
	now    func() time.Time
}

// NewMinter returns a Minter for issuer, signing with key. kid names the key for the verifier.
func NewMinter(issuer, kid string, key ed25519.PrivateKey) (*Minter, error) {
	if issuer == "" || kid == "" {
		return nil, errors.New("actortoken: a minter needs an issuer and a key id")
	}
	if len(key) != ed25519.PrivateKeySize {
		return nil, errors.New("actortoken: the key is not an Ed25519 private key")
	}
	s, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.EdDSA, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", kid),
	)
	if err != nil {
		return nil, fmt.Errorf("actortoken: signer: %w", err)
	}
	return &Minter{issuer: issuer, kid: kid, signer: s, now: time.Now}, nil
}

// WithClock returns a copy of m that reads time from now. It is for tests.
func (m *Minter) WithClock(now func() time.Time) *Minter {
	c := *m
	c.now = now
	return &c
}

// Mint returns a token for actor that lives ttl. A zero ttl means DefaultTTL. A ttl outside MinTTL to
// MaxTTL is an error: there is no way to mint a longer token.
func (m *Minter) Mint(actor Actor, ttl time.Duration) (string, error) {
	if err := actor.Validate(); err != nil {
		return "", err
	}
	if ttl == 0 {
		ttl = DefaultTTL
	}
	if ttl < MinTTL || ttl > MaxTTL {
		return "", ErrTTL
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", fmt.Errorf("actortoken: jti: %w", err)
	}
	now := m.now().UTC().Truncate(time.Second)
	w := wire{
		Issuer:    m.issuer,
		Subject:   "org/" + actor.Org + "/agent/" + actor.Agent,
		Audience:  jwt.Audience{Audience},
		Expiry:    jwt.NewNumericDate(now.Add(ttl)),
		IssuedAt:  jwt.NewNumericDate(now),
		NotBefore: jwt.NewNumericDate(now),
		ID:        hex.EncodeToString(id[:]),
		Org:       actor.Org,
		Agent:     actor.Agent,
		Run:       actor.Run,
	}
	tok, err := jwt.Signed(m.signer).Claims(w).Serialize()
	if err != nil {
		return "", fmt.Errorf("actortoken: sign: %w", err)
	}
	return tok, nil
}

// Verifier checks actor tokens against public keys. It holds no private key.
// Substrate uses SubstrateVerifier instead; this one stays for OpenShell (exit: #1386 PR B, after #1094).
type Verifier struct {
	issuer string
	keys   map[string]ed25519.PublicKey
	leeway time.Duration
	now    func() time.Time
}

// NewVerifier returns a Verifier that accepts tokens from issuer signed by one of keys (by key id).
func NewVerifier(issuer string, keys map[string]ed25519.PublicKey) (*Verifier, error) {
	if issuer == "" || len(keys) == 0 {
		return nil, errors.New("actortoken: a verifier needs an issuer and at least one key")
	}
	cp := make(map[string]ed25519.PublicKey, len(keys))
	for kid, k := range keys {
		if kid == "" || len(k) != ed25519.PublicKeySize {
			return nil, errors.New("actortoken: a verifier key needs an id and an Ed25519 public key")
		}
		cp[kid] = k
	}
	return &Verifier{issuer: issuer, keys: cp, leeway: DefaultLeeway, now: time.Now}, nil
}

// WithClock returns a copy of v that reads time from now. It is for tests.
func (v *Verifier) WithClock(now func() time.Time) *Verifier {
	c := *v
	c.now = now
	return &c
}

// Identify checks token and returns the actor it speaks for, so a Verifier is a route.Identifier: the
// Ed25519 path of the OpenShell driver, where the token names the org and the agent itself.
// exit: #1386 PR B, after #1094 (the OpenShell driver and this path go away). A nil Verifier refuses.
func (v *Verifier) Identify(token string) (Actor, error) {
	if v == nil {
		return Actor{}, ErrInvalid
	}
	c, err := v.Verify(token)
	if err != nil {
		return Actor{}, err
	}
	return c.Actor, nil
}

// Verify checks token and returns its claims. Every failure is ErrInvalid.
func (v *Verifier) Verify(token string) (Claims, error) {
	tok, err := jwt.ParseSigned(strings.TrimSpace(token), []jose.SignatureAlgorithm{jose.EdDSA})
	if err != nil || len(tok.Headers) != 1 {
		return Claims{}, ErrInvalid
	}
	key, ok := v.keys[tok.Headers[0].KeyID]
	if !ok {
		return Claims{}, ErrInvalid
	}
	var w wire
	if err := tok.Claims(key, &w); err != nil {
		return Claims{}, ErrInvalid
	}
	if w.Issuer != v.issuer || len(w.Audience) != 1 || w.Audience[0] != Audience {
		return Claims{}, ErrInvalid
	}
	if w.Expiry == nil || w.IssuedAt == nil || w.ID == "" {
		return Claims{}, ErrInvalid
	}
	exp, iat := w.Expiry.Time(), w.IssuedAt.Time()
	life := exp.Sub(iat)
	if life < MinTTL || life > MaxTTL {
		return Claims{}, ErrInvalid
	}
	now := v.now()
	if now.After(exp.Add(v.leeway)) || now.Add(v.leeway).Before(iat) {
		return Claims{}, ErrInvalid
	}
	if w.NotBefore != nil && now.Add(v.leeway).Before(w.NotBefore.Time()) {
		return Claims{}, ErrInvalid
	}
	a := Actor{Org: w.Org, Agent: w.Agent, Run: w.Run}
	if a.Validate() != nil || w.Subject != "org/"+a.Org+"/agent/"+a.Agent {
		return Claims{}, ErrInvalid
	}
	return Claims{Issuer: w.Issuer, Actor: a, ID: w.ID, IssuedAt: iat, ExpiresAt: exp}, nil
}
