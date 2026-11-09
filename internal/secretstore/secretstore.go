// SPDX-License-Identifier: Apache-2.0

// Package secretstore encrypts the secret values Koinon keeps in Postgres:
// provider API keys and connection credentials (ADR 0056, plan item PV1).
//
// # Envelope encryption
//
// Every value gets a fresh random 256-bit data-encryption key (DEK) and is
// sealed with AES-256-GCM under a random 96-bit nonce. The DEK is wrapped
// (AES-256-GCM, its own random nonce) by the active key-encryption key (KEK).
// An [Envelope] stores the format version, the algorithm, the KEK's ID, the
// wrapped DEK, both nonces and the ciphertext; it holds no plaintext and is
// safe in a jsonb column.
//
// Because each value has its own DEK, DEKs never need scheduled rotation
// (docs/research/kek-rotation.md). KEK rotation re-wraps the stored DEKs
// ([Keyring.Rewrap]) without touching a payload. After a suspected compromise
// every value is sealed again under a new DEK ([Keyring.Seal] on the opened
// value) and the credentials themselves are rotated at their providers.
//
// # Key wrapping (ADR 0085 decision 12)
//
// The keys form a hierarchy: a value is sealed by its own DEK, a DEK is wrapped by a KEK
// ([Keyring]), and a KEK is stored wrapped by a key-wrapping key that a [Wrapper] holds. Only the
// last step differs between deployments:
//
//   - [Local] (kind, development, and every install before a KMS is configured): there is no
//     wrapping key. The KEK Secret holds the KEK itself, and its backup is the key export
//     ("koinon-api keys export").
//   - A KMS adapter (OpenBao/Vault transit old issue 416, AWS KMS old issue 417; GCP and Azure later): the wrapping
//     key never leaves the KMS. The KEK Secret holds the KMS's ciphertext of each KEK, and each
//     process signs in with its pod identity (Kubernetes auth; the AWS default chain with IRSA):
//     no credential in Helm values, env or a sandbox.
//
// The KMS is called when a key set is loaded or reloaded, never once per value: [Keyring.Seal]
// and [Keyring.Open] stay local and synchronous, and a short KMS outage does not stop reads. The
// services (koinon-api, the reconciler; agentd holds no keyring) only unwrap; wrapping a new KEK with a KMS is the
// key-admin Job's alone ([ErrKeyAdminWraps]; the Job comes with the KMS adapters). [Wrappers] picks
// the adapter of each stored KEK, so a key set can move from one backend to another by add,
// activate, rewrap and retire.
//
// What exists today: the interface, [Local], the stored form and [Fingerprint] (printed by
// "keys status" and "keys export"). Not yet: the fingerprints recorded in the database and the
// unreachable-key metric that calls [Wrapper.Probe] (415b), and the KMS adapters (old issue 416, old issue 417).
//
// # Additional authenticated data
//
// Both the payload and the wrapped DEK are authenticated with a [Binding]:
// the owning org, table, row ID and column. An envelope copied to another row,
// column or org (by a bug, or by someone with write access to the database)
// fails to open.
//
// # Wrap counts
//
// AES-GCM with random nonces is safe for at most 2^32 invocations per key
// (NIST SP 800-38D 8.3). A KEK is invoked once per wrap, and wraps happen on
// writes of a secret and on rewrap. Koinon holds tens to thousands of values,
// so a KEK with the default 365-day active period stays many orders of
// magnitude below the limit; the limit is why a KEK still has a finite period
// and why the KEK is never reused as a payload key. Koinon keeps no counter.
package secretstore

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"regexp"
	"sync"
)

// Envelope format.
const (
	// FormatV1 is the only envelope format.
	FormatV1 = 1
	// AlgAES256GCM names AES-256-GCM for the payload and the DEK wrap.
	AlgAES256GCM = "AES-256-GCM"
	// KeySize is the size of KEKs and DEKs in bytes.
	KeySize = 32
)

// Envelope is a sealed value. The field names are the stored JSON.
type Envelope struct {
	Format     int    `json:"fmt"`
	Alg        string `json:"alg"`
	KEKID      string `json:"kek_id"`
	WrappedDEK []byte `json:"wrapped_dek"`
	DEKNonce   []byte `json:"dek_nonce"`
	Nonce      []byte `json:"nonce"`
	Ciphertext []byte `json:"ciphertext"`
}

// Binding names where an envelope lives. It is authenticated, not stored.
type Binding struct {
	OrgID  string
	Table  string
	RowID  string
	Column string
}

// aad encodes the binding with a purpose tag, length-prefixed so fields cannot
// run into each other.
func (b Binding) aad(purpose, kekID string) []byte {
	return lengthPrefixed("koinon/secretstore/v1", purpose, kekID, b.OrgID, b.Table, b.RowID, b.Column)
}

// lengthPrefixed encodes fields, each with its length first, so fields cannot run into each other.
func lengthPrefixed(fields ...string) []byte {
	var out []byte
	for _, f := range fields {
		out = binary.BigEndian.AppendUint32(out, uint32(len(f))) //nolint:gosec // short strings
		out = append(out, f...)
	}
	return out
}

// Errors.
var (
	// ErrUnknownKEK means the envelope names a KEK this keyring does not hold.
	ErrUnknownKEK = errors.New("secretstore: unknown key-encryption key")
	// ErrOpen means the envelope did not authenticate: wrong key, a different
	// binding, or modified data.
	ErrOpen = errors.New("secretstore: cannot open envelope (wrong key, wrong binding or modified data)")
	// ErrNoActiveKEK means the keyring has no key to wrap with.
	ErrNoActiveKEK = errors.New("secretstore: no active key-encryption key")
)

var kekIDPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]{0,60}[a-z0-9])?$`)

// ValidKEKID reports whether id can name a KEK. "active" is reserved for the
// file that names the active KEK.
func ValidKEKID(id string) bool { return id != "active" && kekIDPattern.MatchString(id) }

// Keyring holds KEKs and the ID of the one that wraps new DEKs. It is safe for
// concurrent use; [Keyring.Replace] swaps the key set when the mounted Secret
// changes.
type Keyring struct {
	mu     sync.RWMutex
	keys   map[string]cipher.AEAD
	active string
}

// NewKeyring returns a keyring of keys (ID to 32 key bytes) with active as the
// wrapping KEK.
func NewKeyring(keys map[string][]byte, active string) (*Keyring, error) {
	k := &Keyring{}
	if err := k.Replace(keys, active); err != nil {
		return nil, err
	}
	return k, nil
}

// Replace swaps the key set. active may be empty for a keyring that only opens
// and re-wraps nothing; Seal then fails.
func (k *Keyring) Replace(keys map[string][]byte, active string) error {
	aeads := make(map[string]cipher.AEAD, len(keys))
	for id, key := range keys {
		if !ValidKEKID(id) {
			return fmt.Errorf("secretstore: invalid key-encryption key ID %q", id)
		}
		a, err := newGCM(key)
		if err != nil {
			return fmt.Errorf("secretstore: key %q: %w", id, err)
		}
		aeads[id] = a
	}
	if active != "" {
		if _, ok := aeads[active]; !ok {
			return fmt.Errorf("secretstore: the active key-encryption key %q is not among the keys", active)
		}
	}
	k.mu.Lock()
	k.keys, k.active = aeads, active
	k.mu.Unlock()
	return nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("a key is %d bytes, not %d", len(key), KeySize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Active returns the ID of the wrapping KEK.
func (k *Keyring) Active() string {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.active
}

// IDs returns the IDs of the held KEKs, unordered.
func (k *Keyring) IDs() []string {
	k.mu.RLock()
	defer k.mu.RUnlock()
	ids := make([]string, 0, len(k.keys))
	for id := range k.keys {
		ids = append(ids, id)
	}
	return ids
}

// Has reports whether the keyring holds the KEK.
func (k *Keyring) Has(id string) bool {
	k.mu.RLock()
	defer k.mu.RUnlock()
	_, ok := k.keys[id]
	return ok
}

func (k *Keyring) kek(id string) (cipher.AEAD, error) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	a, ok := k.keys[id]
	if !ok {
		return nil, fmt.Errorf("%w %q", ErrUnknownKEK, id)
	}
	return a, nil
}

// Seal encrypts plaintext under a fresh DEK wrapped by the active KEK.
func (k *Keyring) Seal(b Binding, plaintext []byte) (Envelope, error) {
	k.mu.RLock()
	activeID := k.active
	k.mu.RUnlock()
	if activeID == "" {
		return Envelope{}, ErrNoActiveKEK
	}
	dek := make([]byte, KeySize)
	if _, err := rand.Read(dek); err != nil {
		return Envelope{}, err
	}
	defer clear(dek)
	payload, err := newGCM(dek)
	if err != nil {
		return Envelope{}, err
	}
	nonce, err := randomNonce(payload)
	if err != nil {
		return Envelope{}, err
	}
	env := Envelope{Format: FormatV1, Alg: AlgAES256GCM, KEKID: activeID, Nonce: nonce}
	env.Ciphertext = payload.Seal(nil, nonce, plaintext, b.aad("payload", ""))
	if err := k.wrap(b, &env, dek); err != nil {
		return Envelope{}, err
	}
	return env, nil
}

// wrap wraps dek under the envelope's KEK ID and stores it in env.
func (k *Keyring) wrap(b Binding, env *Envelope, dek []byte) error {
	kek, err := k.kek(env.KEKID)
	if err != nil {
		return err
	}
	nonce, err := randomNonce(kek)
	if err != nil {
		return err
	}
	env.DEKNonce = nonce
	env.WrappedDEK = kek.Seal(nil, nonce, dek, b.aad("dek", env.KEKID))
	return nil
}

func randomNonce(a cipher.AEAD) ([]byte, error) {
	n := make([]byte, a.NonceSize())
	if _, err := rand.Read(n); err != nil {
		return nil, err
	}
	return n, nil
}

// ErrUnsupportedFormat means the envelope has a format or algorithm this build does not know. It is the
// one predicate for that case: [UnsupportedFormat] tests an envelope, and keyscli files the error under
// its "envelope" failure kind.
var ErrUnsupportedFormat = errors.New("secretstore: unsupported envelope format")

// UnsupportedFormat reports whether env has a format or algorithm this build does not know.
func UnsupportedFormat(env Envelope) bool {
	return env.Format != FormatV1 || env.Alg != AlgAES256GCM
}

func checkFormat(env Envelope) error {
	if UnsupportedFormat(env) {
		return fmt.Errorf("%w %d / %q", ErrUnsupportedFormat, env.Format, env.Alg)
	}
	return nil
}

// unwrap returns the DEK. The caller clears it.
func (k *Keyring) unwrap(b Binding, env Envelope) ([]byte, error) {
	if err := checkFormat(env); err != nil {
		return nil, err
	}
	kek, err := k.kek(env.KEKID)
	if err != nil {
		return nil, err
	}
	if len(env.DEKNonce) != kek.NonceSize() {
		return nil, ErrOpen
	}
	dek, err := kek.Open(nil, env.DEKNonce, env.WrappedDEK, b.aad("dek", env.KEKID))
	if err != nil {
		return nil, ErrOpen
	}
	return dek, nil
}

// Open decrypts env.
func (k *Keyring) Open(b Binding, env Envelope) ([]byte, error) {
	dek, err := k.unwrap(b, env)
	if err != nil {
		return nil, err
	}
	defer clear(dek)
	payload, err := newGCM(dek)
	if err != nil {
		return nil, err
	}
	if len(env.Nonce) != payload.NonceSize() {
		return nil, ErrOpen
	}
	plain, err := payload.Open(nil, env.Nonce, env.Ciphertext, b.aad("payload", ""))
	if err != nil {
		return nil, ErrOpen
	}
	return plain, nil
}

// Rewrap wraps env's DEK under the active KEK. The payload, its nonce and its
// ciphertext are untouched and never decrypted. The payload's AAD omits the
// KEK ID for this reason; the wrapped DEK's AAD names it.
func (k *Keyring) Rewrap(b Binding, env Envelope) (Envelope, error) {
	k.mu.RLock()
	activeID := k.active
	k.mu.RUnlock()
	if activeID == "" {
		return Envelope{}, ErrNoActiveKEK
	}
	dek, err := k.unwrap(b, env)
	if err != nil {
		return Envelope{}, err
	}
	defer clear(dek)
	out := env
	out.KEKID = activeID
	if err := k.wrap(b, &out, dek); err != nil {
		return Envelope{}, err
	}
	return out, nil
}
