// SPDX-License-Identifier: Apache-2.0

package secretstore

// Envelope v2: a value sealed to a recipient's public key (ADR 0097 decision 12, #1151).
//
// The payload is sealed exactly as in v1: AES-256-GCM under a fresh random 32-byte DEK. The DEK is
// then sealed to the recipient with HPKE (RFC 9180, base mode, [crypto/hpke]). The sealer needs the
// public key only. Only the holder of the private key (today the gateway) opens the value. Nothing
// here calls a KMS: the private key is wrapped by [Wrappers.WrapRecipientKey] and loaded once.
//
// This file has no caller yet. Slice 2 adds the key lifecycle, slice 3 the writers and the readers.

import (
	"context"
	"crypto/fips140"
	"crypto/hpke"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sync"
)

// FormatV2 is the envelope format sealed to a recipient key. A v1 reader refuses it
// ([ErrUnsupportedFormat]), so an old binary fails closed on a new value.
const FormatV2 = 2

// Suites a v2 envelope may name (field "recipient": KEM, KDF and AEAD IDs in hex, RFC 9180 registry).
// The code has a fixed table of them and refuses any other.
const (
	// SuiteX25519 is DHKEM(X25519, HKDF-SHA256), HKDF-SHA256, AES-256-GCM. It is the default.
	SuiteX25519 = "hpke-0020-0001-0002"
	// SuiteP256 is DHKEM(P-256, HKDF-SHA256), HKDF-SHA256, AES-256-GCM, for a FIPS 140-3 install.
	SuiteP256 = "hpke-0010-0001-0002"
)

// DefaultRecipientSuite is the suite for a new key: [SuiteX25519], or [SuiteP256] when the process runs
// in FIPS 140-only mode, where Go refuses X25519 (checked on go1.27.4, 2026-11-19: "use of X25519 is not
// allowed in FIPS 140-only mode"). A key keeps its suite for life; the envelope names it.
func DefaultRecipientSuite() string {
	if fips140.Enforced() {
		return SuiteP256
	}
	return SuiteX25519
}

// suiteInfo is an accepted suite.
type suiteInfo struct {
	kemID, kdfID, aeadID uint16
}

var suites = map[string]suiteInfo{
	SuiteX25519: {kemID: 0x0020, kdfID: 0x0001, aeadID: 0x0002},
	SuiteP256:   {kemID: 0x0010, kdfID: 0x0001, aeadID: 0x0002},
}

func (s suiteInfo) kem() (hpke.KEM, error) { return hpke.NewKEM(s.kemID) }

// kdfAEAD returns the KDF and the AEAD of the suite.
func (s suiteInfo) kdfAEAD() (hpke.KDF, hpke.AEAD, error) {
	kdf, err := hpke.NewKDF(s.kdfID)
	if err != nil {
		return nil, nil, err
	}
	aead, err := hpke.NewAEAD(s.aeadID)
	return kdf, aead, err
}

func lookupSuite(name string) (suiteInfo, error) {
	s, ok := suites[name]
	if !ok {
		return suiteInfo{}, fmt.Errorf("%w: recipient suite %q", ErrUnsupportedFormat, name)
	}
	return s, nil
}

var (
	// ErrUnknownRecipientKey means the envelope names a recipient key this keyring does not hold.
	ErrUnknownRecipientKey = errors.New("secretstore: unknown recipient key")
	// ErrRecipientKeyID means a recipient key ID is not the hash of its public key.
	ErrRecipientKeyID = errors.New("secretstore: recipient key ID does not match the public key")
)

var recipientKeyIDPattern = regexp.MustCompile(`^gk-[0-9a-f]{16}$`)

// ValidRecipientKeyID reports whether id has the form of a recipient key ID: "gk-" and 16 hex
// characters. It also matches [ValidKEKID], so one column pattern fits both.
func ValidRecipientKeyID(id string) bool { return recipientKeyIDPattern.MatchString(id) }

// RecipientKeyID returns the ID of a public key: "gk-" and the first 16 hex characters of its SHA-256.
func RecipientKeyID(publicKey []byte) string {
	sum := sha256.Sum256(publicKey)
	return "gk-" + hex.EncodeToString(sum[:8])
}

// EnvelopeV2 is a value sealed to a recipient key. The field names are the stored JSON.
type EnvelopeV2 struct {
	Format         int    `json:"fmt"`
	Alg            string `json:"alg"`
	Recipient      string `json:"recipient"`
	RecipientKeyID string `json:"recipient_key_id"`
	Enc            []byte `json:"enc"`
	SealedDEK      []byte `json:"sealed_dek"`
	Nonce          []byte `json:"nonce"`
	Ciphertext     []byte `json:"ciphertext"`
}

// EnvelopeFormat returns the "fmt" of a stored envelope without reading any other field, so a caller
// can choose [Envelope] or [EnvelopeV2].
func EnvelopeFormat(raw []byte) (int, error) {
	var h struct {
		Format int `json:"fmt"`
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		return 0, fmt.Errorf("%w: not a JSON envelope", ErrUnsupportedFormat)
	}
	return h.Format, nil
}

// aadV2 is the associated data of a v2 envelope: the purpose, the recipient key ID and the binding,
// length-prefixed. The payload uses purpose "payload"; the HPKE info and aad use "dek". The tag differs
// from v1's, so the data of the two formats never coincide.
func (b Binding) aadV2(purpose, keyID string) []byte {
	return lengthPrefixed("koinon/secretstore/v2", purpose, keyID, b.OrgID, b.Table, b.RowID, b.Column)
}

// RecipientPublicKey is the public key a value is sealed to. It is not secret.
type RecipientPublicKey struct {
	suite string
	raw   []byte
	pub   hpke.PublicKey
	id    string
}

// NewRecipientPublicKey parses a public key of the suite.
func NewRecipientPublicKey(suite string, raw []byte) (RecipientPublicKey, error) {
	si, err := lookupSuite(suite)
	if err != nil {
		return RecipientPublicKey{}, err
	}
	kem, err := si.kem()
	if err != nil {
		return RecipientPublicKey{}, err
	}
	pub, err := kem.NewPublicKey(raw)
	if err != nil {
		return RecipientPublicKey{}, fmt.Errorf("secretstore: invalid %s public key: %w", suite, err)
	}
	return RecipientPublicKey{suite: suite, raw: append([]byte(nil), raw...), pub: pub, id: RecipientKeyID(raw)}, nil
}

// Suite returns the suite name.
func (p RecipientPublicKey) Suite() string { return p.suite }

// ID returns the key ID.
func (p RecipientPublicKey) ID() string { return p.id }

// Bytes returns the encoded public key.
func (p RecipientPublicKey) Bytes() []byte { return append([]byte(nil), p.raw...) }

// CheckID refuses a key whose stored ID is not the hash of its public key. A loader calls it with the
// ID from the database row.
func (p RecipientPublicKey) CheckID(id string) error {
	if id != p.id {
		return fmt.Errorf("%w: %q is not %q", ErrRecipientKeyID, id, p.id)
	}
	return nil
}

// SealV2 seals plaintext to the recipient's public key. It needs no secret.
func SealV2(to RecipientPublicKey, b Binding, plaintext []byte) (EnvelopeV2, error) {
	if to.pub == nil {
		return EnvelopeV2{}, errors.New("secretstore: no recipient public key")
	}
	dek := make([]byte, KeySize)
	if _, err := rand.Read(dek); err != nil {
		return EnvelopeV2{}, err
	}
	defer clear(dek)
	payload, err := newGCM(dek)
	if err != nil {
		return EnvelopeV2{}, err
	}
	nonce, err := randomNonce(payload)
	if err != nil {
		return EnvelopeV2{}, err
	}
	env := EnvelopeV2{Format: FormatV2, Alg: AlgAES256GCM, Recipient: to.suite, RecipientKeyID: to.id, Nonce: nonce}
	env.Ciphertext = payload.Seal(nil, nonce, plaintext, b.aadV2("payload", to.id))

	dekData := b.aadV2("dek", to.id)
	si, err := lookupSuite(to.suite)
	if err != nil {
		return EnvelopeV2{}, err
	}
	kdf, aead, err := si.kdfAEAD()
	if err != nil {
		return EnvelopeV2{}, err
	}
	enc, sender, err := hpke.NewSender(to.pub, kdf, aead, dekData)
	if err != nil {
		return EnvelopeV2{}, fmt.Errorf("secretstore: seal to %s: %w", to.id, err)
	}
	if env.SealedDEK, err = sender.Seal(dekData, dek); err != nil {
		return EnvelopeV2{}, fmt.Errorf("secretstore: seal to %s: %w", to.id, err)
	}
	env.Enc = enc
	return env, nil
}

// RecipientKey is a key pair. The private half opens values sealed to the public half.
type RecipientKey struct {
	suite string
	priv  hpke.PrivateKey
	pub   RecipientPublicKey
}

// GenerateRecipientKey makes a new key pair for the suite.
func GenerateRecipientKey(suite string) (*RecipientKey, error) {
	si, err := lookupSuite(suite)
	if err != nil {
		return nil, err
	}
	kem, err := si.kem()
	if err != nil {
		return nil, err
	}
	priv, err := kem.GenerateKey()
	if err != nil {
		return nil, err
	}
	return newRecipientKey(suite, priv)
}

// NewRecipientKey loads a private key of the suite (the bytes that [RecipientKey.PrivateBytes] gave).
func NewRecipientKey(suite string, privateKey []byte) (*RecipientKey, error) {
	si, err := lookupSuite(suite)
	if err != nil {
		return nil, err
	}
	kem, err := si.kem()
	if err != nil {
		return nil, err
	}
	priv, err := kem.NewPrivateKey(privateKey)
	if err != nil {
		return nil, fmt.Errorf("secretstore: invalid %s private key", suite)
	}
	return newRecipientKey(suite, priv)
}

func newRecipientKey(suite string, priv hpke.PrivateKey) (*RecipientKey, error) {
	pub, err := NewRecipientPublicKey(suite, priv.PublicKey().Bytes())
	if err != nil {
		return nil, err
	}
	return &RecipientKey{suite: suite, priv: priv, pub: pub}, nil
}

// Public returns the public half.
func (k *RecipientKey) Public() RecipientPublicKey { return k.pub }

// ID returns the key ID.
func (k *RecipientKey) ID() string { return k.pub.id }

// Suite returns the suite name.
func (k *RecipientKey) Suite() string { return k.suite }

// PrivateBytes returns the encoded private key. The caller wraps it at once and clears it.
func (k *RecipientKey) PrivateBytes() ([]byte, error) { return k.priv.Bytes() }

// Open decrypts env. Every failure is closed and gives no oracle: an unknown suite, algorithm or format
// is [ErrUnsupportedFormat]; another key is [ErrUnknownRecipientKey]; wrong bytes, another binding or
// another key's pair are [ErrOpen].
func (k *RecipientKey) Open(b Binding, env EnvelopeV2) ([]byte, error) {
	if env.Format != FormatV2 || env.Alg != AlgAES256GCM {
		return nil, fmt.Errorf("%w %d / %q", ErrUnsupportedFormat, env.Format, env.Alg)
	}
	si, err := lookupSuite(env.Recipient)
	if err != nil {
		return nil, err
	}
	if env.RecipientKeyID != k.pub.id || env.Recipient != k.suite {
		return nil, fmt.Errorf("%w %q", ErrUnknownRecipientKey, env.RecipientKeyID)
	}
	dekData := b.aadV2("dek", k.pub.id)
	kdf, aead, err := si.kdfAEAD()
	if err != nil {
		return nil, ErrOpen
	}
	r, err := hpke.NewRecipient(env.Enc, k.priv, kdf, aead, dekData)
	if err != nil {
		return nil, ErrOpen
	}
	dek, err := r.Open(dekData, env.SealedDEK)
	if err != nil {
		return nil, ErrOpen
	}
	defer clear(dek)
	payload, err := newGCM(dek)
	if err != nil || len(env.Nonce) != payload.NonceSize() {
		return nil, ErrOpen
	}
	plain, err := payload.Open(nil, env.Nonce, env.Ciphertext, b.aadV2("payload", k.pub.id))
	if err != nil {
		return nil, ErrOpen
	}
	return plain, nil
}

// RecipientKeyring holds the private keys of a recipient (the gateway holds every active and retiring
// key) and picks one by the envelope's key ID. It is safe for concurrent use.
type RecipientKeyring struct {
	mu   sync.RWMutex
	keys map[string]*RecipientKey
}

// NewRecipientKeyring returns a keyring of keys.
func NewRecipientKeyring(keys ...*RecipientKey) *RecipientKeyring {
	r := &RecipientKeyring{}
	r.Replace(keys...)
	return r
}

// Replace swaps the key set.
func (r *RecipientKeyring) Replace(keys ...*RecipientKey) {
	m := make(map[string]*RecipientKey, len(keys))
	for _, k := range keys {
		m[k.ID()] = k
	}
	r.mu.Lock()
	r.keys = m
	r.mu.Unlock()
}

// Has reports whether the keyring holds the key.
func (r *RecipientKeyring) Has(id string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.keys[id]
	return ok
}

// Open decrypts env with the key it names. An unknown key ID is [ErrUnknownRecipientKey]; no other key
// is tried.
func (r *RecipientKeyring) Open(b Binding, env EnvelopeV2) ([]byte, error) {
	r.mu.RLock()
	k, ok := r.keys[env.RecipientKeyID]
	r.mu.RUnlock()
	if !ok {
		if env.Format != FormatV2 || env.Alg != AlgAES256GCM {
			return nil, fmt.Errorf("%w %d / %q", ErrUnsupportedFormat, env.Format, env.Alg)
		}
		if _, err := lookupSuite(env.Recipient); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w %q", ErrUnknownRecipientKey, env.RecipientKeyID)
	}
	return k.Open(b, env)
}

// recipientKeyAAD binds a wrapped recipient (gateway) private key to its key ID and the install. Its
// purpose tag differs from [kekAAD]'s and [kekInstallAAD]'s, so a wrapped recipient key never unwraps
// as a KEK, and a wrapped KEK never unwraps as a recipient key. On the local backend the wrap is the
// identity and nothing is authenticated: the separation there is the Secret's name (ADR 0097 12.3).
func recipientKeyAAD(installID, id string) []byte {
	return lengthPrefixed("koinon/secretstore/v2", "recipient-key", id, installID)
}

// WrapRecipientKey wraps the private key of k under the primary wrapper and returns its stored form,
// bound to the key ID and the install. Only [NewKeyAdminWrappers] may wrap with a KMS
// ([ErrKeyAdminWraps]); the wrapping key is the gateway's own, never the KEK's.
func (ws Wrappers) WrapRecipientKey(ctx context.Context, k *RecipientKey) (StoredKEK, error) {
	priv, err := k.PrivateBytes()
	if err != nil {
		return StoredKEK{}, err
	}
	defer clear(priv)
	w := ws.Primary()
	var aad []byte
	install := ""
	if w.Backend() != BackendLocal {
		if !ws.keyAdmin {
			return StoredKEK{}, fmt.Errorf("wrap recipient key %q with %s: %w", k.ID(), w.Backend(), ErrKeyAdminWraps)
		}
		if install, err = ws.installID(ctx); err != nil {
			return StoredKEK{}, fmt.Errorf("secretstore: wrap recipient key %q with %s: %w", k.ID(), w.Backend(), err)
		}
		aad = recipientKeyAAD(install, k.ID())
	}
	ctx, cancel := ws.call(ctx)
	defer cancel()
	wrapped, err := w.Wrap(ctx, priv, aad)
	if err != nil {
		if ctx.Err() != nil && !errors.Is(err, ErrUnreachable) {
			err = fmt.Errorf("%w: %w", ErrUnreachable, err)
		}
		return StoredKEK{}, fmt.Errorf("secretstore: wrap recipient key %q with %s: %w", k.ID(), w.Backend(), err)
	}
	return StoredKEK{Backend: w.Backend(), KeyID: w.KeyID(), Wrapped: wrapped, InstallID: install}, nil
}

// UnwrapRecipientKey unwraps a recipient key of the suite and checks that its public key hashes to id.
// A key wrapped for another install, or under another ID, is [ErrOpen]; a key whose pair is not id is
// [ErrRecipientKeyID]. Unlike a KEK, a recipient key is always bound to its install on a KMS: an
// unbound one is refused.
func (ws Wrappers) UnwrapRecipientKey(ctx context.Context, suite, id string, s StoredKEK) (*RecipientKey, error) {
	w, err := ws.For(s)
	if err != nil {
		return nil, err
	}
	var aad []byte
	if s.Backend != BackendLocal {
		if !s.Bound() {
			return nil, fmt.Errorf("secretstore: recipient key %q with %s is not bound to an install: %w", id, s.String(), ErrOpen)
		}
		install, err := ws.installID(ctx)
		if err != nil {
			return nil, fmt.Errorf("secretstore: unwrap recipient key %q with %s: %w", id, s.String(), err)
		}
		aad = recipientKeyAAD(install, id)
	}
	ctx, cancel := ws.call(ctx)
	defer cancel()
	priv, err := w.Unwrap(ctx, s.Wrapped, aad)
	if err != nil {
		if ctx.Err() != nil && !errors.Is(err, ErrUnreachable) {
			err = fmt.Errorf("%w: %w", ErrUnreachable, err)
		}
		return nil, fmt.Errorf("secretstore: unwrap recipient key %q with %s: %w", id, s.String(), err)
	}
	defer clear(priv)
	k, err := NewRecipientKey(suite, priv)
	if err != nil {
		return nil, fmt.Errorf("secretstore: unwrap recipient key %q with %s: %w", id, s.String(), ErrOpen)
	}
	if err := k.pub.CheckID(id); err != nil {
		return nil, err
	}
	return k, nil
}
