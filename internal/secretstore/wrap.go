// SPDX-License-Identifier: Apache-2.0

package secretstore

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
)

// BackendLocal is the [Local] adapter's backend name.
const BackendLocal = "local"

// ErrUnreachable means the key-wrapping key could not be used: the KMS did not answer, refused the
// sign-in, or this identity may not use the key. Adapters wrap it, so callers can tell an outage
// from a wrong key ([ErrOpen]).
var ErrUnreachable = errors.New("secretstore: the key-wrapping key is unreachable")

// Wrapper is one key-wrapping key: an adapter over the place that holds it. Implementations are
// safe for concurrent use and never log key material.
type Wrapper interface {
	// Backend names the adapter: a short fixed word ("local", "openbao", "awskms"). It is the value
	// of the metric label "backend" (ADR 0023: a fixed set) and is stored with every wrapped KEK.
	Backend() string
	// KeyID names the wrapping key within the backend (a transit key name, a KMS key ARN), or "" for
	// [Local]. It is not secret; it is stored with every wrapped KEK so the matching adapter unwraps it.
	KeyID() string
	// Wrap encrypts kek under the wrapping key, authenticating aad. The result is opaque and stored.
	Wrap(ctx context.Context, kek, aad []byte) ([]byte, error)
	// Unwrap reverses Wrap. A wrong key, aad or ciphertext is [ErrOpen]; an outage wraps
	// [ErrUnreachable].
	Unwrap(ctx context.Context, wrapped, aad []byte) ([]byte, error)
	// Probe reports, without changing anything, whether the wrapping key is reachable and usable by
	// this process (nil), or why not (wrapping [ErrUnreachable]). It needs no right beyond decrypt.
	Probe(ctx context.Context) error
}

// Local keeps the KEK unwrapped: the KEK Secret holds the key bytes (base64), as it always has.
type Local struct{}

// Backend implements Wrapper.
func (Local) Backend() string { return BackendLocal }

// KeyID implements Wrapper.
func (Local) KeyID() string { return "" }

// Wrap implements Wrapper. The stored form is the key itself; aad cannot be bound.
func (Local) Wrap(_ context.Context, kek, _ []byte) ([]byte, error) { return bytes.Clone(kek), nil }

// Unwrap implements Wrapper.
func (Local) Unwrap(_ context.Context, wrapped, _ []byte) ([]byte, error) {
	if len(wrapped) != KeySize {
		return nil, ErrOpen
	}
	return bytes.Clone(wrapped), nil
}

// Probe implements Wrapper: the key is in the Secret, so there is nothing to reach.
func (Local) Probe(context.Context) error { return nil }

var backendPattern = regexp.MustCompile(`^[a-z][a-z0-9]{0,15}$`)

// StoredKEK is a KEK as the KEK Secret holds it: wrapped by the named key. Its String and LogValue
// name the wrapping key only, never the bytes ([Local]'s are the key itself).
type StoredKEK struct {
	Backend string
	KeyID   string
	Wrapped []byte
}

// String implements fmt.Stringer without the key material.
func (s StoredKEK) String() string {
	if s.KeyID == "" {
		return s.Backend
	}
	return s.Backend + ":" + s.KeyID
}

// GoString keeps %#v from printing the key material.
func (s StoredKEK) GoString() string { return "secretstore.StoredKEK{" + s.String() + "}" }

// LogValue implements slog.LogValuer without the key material.
func (s StoredKEK) LogValue() slog.Value { return slog.StringValue(s.String()) }

// storedJSON is the Secret value of a KEK wrapped by a KMS. A local KEK keeps the plain base64 of
// its bytes, so Secrets and backups made before key wrapping read unchanged, and an older binary of
// the same minor line still reads a key set that holds only local KEKs.
type storedJSON struct {
	Format  int    `json:"fmt"`
	Backend string `json:"backend"`
	KeyID   string `json:"key"`
	Wrapped []byte `json:"wrapped"`
}

const storedFormatV1 = 1

// Encode returns the Secret value of s.
func (s StoredKEK) Encode() ([]byte, error) {
	if s.Backend == BackendLocal {
		if len(s.Wrapped) != KeySize {
			return nil, fmt.Errorf("a local key is %d bytes, not %d", len(s.Wrapped), KeySize)
		}
		return EncodeKey(s.Wrapped), nil
	}
	if !backendPattern.MatchString(s.Backend) || len(s.Wrapped) == 0 {
		return nil, fmt.Errorf("secretstore: cannot store a KEK wrapped by %q", s.String())
	}
	return json.Marshal(storedJSON{Format: storedFormatV1, Backend: s.Backend, KeyID: s.KeyID, Wrapped: s.Wrapped})
}

// decodeStored reads a Secret value: JSON for a wrapped KEK, otherwise a local key (base64 or
// hex). Errors never quote the value.
func decodeStored(v []byte) (StoredKEK, error) {
	s := strings.TrimSpace(string(v))
	if strings.HasPrefix(s, "{") {
		var j storedJSON
		if err := json.Unmarshal([]byte(s), &j); err != nil {
			return StoredKEK{}, errors.New("a wrapped key that is not valid JSON")
		}
		if j.Format != storedFormatV1 || j.Backend == BackendLocal || !backendPattern.MatchString(j.Backend) || len(j.Wrapped) == 0 {
			return StoredKEK{}, fmt.Errorf("a wrapped key of unsupported format %d, backend %q", j.Format, j.Backend)
		}
		return StoredKEK{Backend: j.Backend, KeyID: j.KeyID, Wrapped: j.Wrapped}, nil
	}
	raw, ok := decodeKey(s)
	if !ok {
		return StoredKEK{}, fmt.Errorf("want the base64 or 64 hex characters of %d random bytes, or a wrapped key", KeySize)
	}
	return StoredKEK{Backend: BackendLocal, Wrapped: raw}, nil
}

// kekAAD binds a wrapped KEK to its ID, so a wrapped KEK copied under another ID does not unwrap.
func kekAAD(id string) []byte { return Binding{}.aad("kek", id) }

// Wrappers are the key-wrapping keys a process can use: one wraps new KEKs, the others (and
// [Local], always) only unwrap, so a key set can hold KEKs of an earlier backend while its values
// are rewrapped. The zero value is [Local] alone: today's behaviour.
type Wrappers struct {
	primary Wrapper
	more    []Wrapper
}

// NewWrappers returns the wrappers with primary wrapping new KEKs. Each backend and key ID pair
// appears once.
func NewWrappers(primary Wrapper, more ...Wrapper) (Wrappers, error) {
	if primary == nil {
		return Wrappers{}, errors.New("secretstore: no key-wrapping key to wrap new keys with")
	}
	seen := map[string]bool{}
	for _, w := range append([]Wrapper{primary}, more...) {
		if !backendPattern.MatchString(w.Backend()) {
			return Wrappers{}, fmt.Errorf("secretstore: invalid backend name %q", w.Backend())
		}
		k := w.Backend() + "\x00" + w.KeyID()
		if seen[k] {
			return Wrappers{}, fmt.Errorf("secretstore: the key-wrapping key %s:%s is configured twice", w.Backend(), w.KeyID())
		}
		seen[k] = true
	}
	return Wrappers{primary: primary, more: more}, nil
}

// Primary is the wrapper new KEKs are wrapped with.
func (ws Wrappers) Primary() Wrapper {
	if ws.primary == nil {
		return Local{}
	}
	return ws.primary
}

// For returns the wrapper that unwraps s.
func (ws Wrappers) For(s StoredKEK) (Wrapper, error) {
	for _, w := range append([]Wrapper{ws.Primary()}, ws.more...) {
		if w.Backend() == s.Backend && w.KeyID() == s.KeyID {
			return w, nil
		}
	}
	if s.Backend == BackendLocal {
		return Local{}, nil
	}
	return nil, fmt.Errorf("secretstore: no adapter is configured for the key-wrapping key %s", s.String())
}

// Wrap wraps a new KEK under the primary wrapper.
func (ws Wrappers) Wrap(ctx context.Context, id string, kek []byte) (StoredKEK, error) {
	if len(kek) != KeySize {
		return StoredKEK{}, fmt.Errorf("secretstore: a key is %d bytes, not %d", len(kek), KeySize)
	}
	w := ws.Primary()
	wrapped, err := w.Wrap(ctx, kek, kekAAD(id))
	if err != nil {
		return StoredKEK{}, fmt.Errorf("secretstore: wrap key %q with %s: %w", id, w.Backend(), err)
	}
	return StoredKEK{Backend: w.Backend(), KeyID: w.KeyID(), Wrapped: wrapped}, nil
}

// Unwrap returns the KEK id that s holds.
func (ws Wrappers) Unwrap(ctx context.Context, id string, s StoredKEK) ([]byte, error) {
	w, err := ws.For(s)
	if err != nil {
		return nil, err
	}
	kek, err := w.Unwrap(ctx, s.Wrapped, kekAAD(id))
	if err != nil {
		return nil, fmt.Errorf("secretstore: unwrap key %q with %s: %w", id, s.String(), err)
	}
	if len(kek) != KeySize {
		clear(kek)
		return nil, fmt.Errorf("secretstore: unwrap key %q with %s: %w", id, s.String(), ErrOpen)
	}
	return kek, nil
}

// UnwrapAll unwraps every KEK of a key set.
func (ws Wrappers) UnwrapAll(ctx context.Context, stored map[string]StoredKEK) (map[string][]byte, error) {
	keys := make(map[string][]byte, len(stored))
	var errs []error
	for id, s := range stored {
		kek, err := ws.Unwrap(ctx, id, s)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		keys[id] = kek
	}
	if err := errors.Join(errs...); err != nil {
		for _, k := range keys {
			clear(k)
		}
		return nil, err
	}
	return keys, nil
}

// Probe probes the wrapping key of s.
func (ws Wrappers) Probe(ctx context.Context, s StoredKEK) error {
	w, err := ws.For(s)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	return w.Probe(ctx)
}

// Fingerprint identifies a KEK without revealing it: the first 16 bytes of HMAC-SHA256 keyed by
// the KEK over a fixed label, in hex. It is the same however the KEK is wrapped, so a backup (the
// key export) can be matched with the fingerprints the database records.
func Fingerprint(kek []byte) string {
	m := hmac.New(sha256.New, kek)
	_, _ = m.Write([]byte("koinon/secretstore/kek-fingerprint/v1"))
	return hex.EncodeToString(m.Sum(nil)[:16])
}
