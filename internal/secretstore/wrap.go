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
	"time"
)

// BackendLocal is the [Local] adapter's backend name.
const BackendLocal = "local"

// ErrUnreachable means the key-wrapping key could not be used: the KMS did not answer, refused the
// sign-in, or this identity may not use the key. Adapters wrap it, so callers can tell an outage
// from a wrong key ([ErrOpen]).
var ErrUnreachable = errors.New("secretstore: the key-wrapping key is unreachable")

// Wrapper is one key-wrapping key: an adapter over the place that holds it. Implementations are
// safe for concurrent use and never log key material.
//
// Every call must end by its context's deadline, and an adapter must also apply its own per-call
// timeout to each request (the watch loops pass a context that lives as long as the process;
// [Wrappers] bounds each call by [CallTimeout] as well). A call that gave up wraps [ErrUnreachable].
// Adapters sign in with the pod's identity, never with a credential in values, env or a sandbox.
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
	// Nothing calls it on a timer yet: the unreachable-key metric that will is 415b.
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
//
// Only the key-admin Job wraps a new KEK with a KMS (procoduck, 2026-11-02): the services (koinon-api,
// the reconciler) are decrypt-only; agentd holds no keyring. [NewWrappers] builds a service's wrappers, whose Wrap
// refuses a KMS primary with [ErrKeyAdminWraps] before it calls the KMS; [NewKeyAdminWrappers]
// builds the Job's. With the [Local] primary both wrap, as before key wrapping. Processes build
// them from their configuration in one place (package secretstore/wrapping), not here.
type Wrappers struct {
	primary  Wrapper
	more     []Wrapper
	keyAdmin bool
	timeout  time.Duration
}

// ErrKeyAdminWraps means a service tried to wrap a new KEK with a KMS: only the key-admin Job holds
// encrypt rights.
var ErrKeyAdminWraps = errors.New("secretstore: new KEKs are wrapped by the key-admin Job; this process may only unwrap")

// CallTimeout bounds each adapter call that Wrappers makes, whatever the caller's context: the
// watch loops pass a context that lives as long as the process.
const CallTimeout = 10 * time.Second

// NewWrappers returns a service's wrappers: primary is the wrapping key of the key set (new KEKs
// are wrapped with it only when it is [Local]). Each backend and key ID pair appears once.
func NewWrappers(primary Wrapper, more ...Wrapper) (Wrappers, error) {
	return newWrappers(false, primary, more)
}

// NewKeyAdminWrappers returns the key-admin Job's wrappers: they wrap new KEKs with primary.
func NewKeyAdminWrappers(primary Wrapper, more ...Wrapper) (Wrappers, error) {
	return newWrappers(true, primary, more)
}

// WithCallTimeout returns ws with another per-call bound than [CallTimeout] (tests).
func (ws Wrappers) WithCallTimeout(d time.Duration) Wrappers {
	ws.timeout = d
	return ws
}

func (ws Wrappers) call(ctx context.Context) (context.Context, context.CancelFunc) {
	d := ws.timeout
	if d <= 0 {
		d = CallTimeout
	}
	return context.WithTimeout(ctx, d)
}

func newWrappers(keyAdmin bool, primary Wrapper, more []Wrapper) (Wrappers, error) {
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
	return Wrappers{primary: primary, more: more, keyAdmin: keyAdmin}, nil
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

// Wrap wraps a new KEK under the primary wrapper. A service's wrappers with a KMS primary refuse
// with [ErrKeyAdminWraps] and never call the KMS.
func (ws Wrappers) Wrap(ctx context.Context, id string, kek []byte) (StoredKEK, error) {
	if len(kek) != KeySize {
		return StoredKEK{}, fmt.Errorf("secretstore: a key is %d bytes, not %d", len(kek), KeySize)
	}
	w := ws.Primary()
	if w.Backend() != BackendLocal && !ws.keyAdmin {
		return StoredKEK{}, fmt.Errorf("wrap key %q with %s: %w", id, w.Backend(), ErrKeyAdminWraps)
	}
	ctx, cancel := ws.call(ctx)
	defer cancel()
	wrapped, err := w.Wrap(ctx, kek, kekAAD(id))
	if err != nil {
		if ctx.Err() != nil && !errors.Is(err, ErrUnreachable) {
			err = fmt.Errorf("%w: %w", ErrUnreachable, err)
		}
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
	ctx, cancel := ws.call(ctx)
	defer cancel()
	kek, err := w.Unwrap(ctx, s.Wrapped, kekAAD(id))
	if err != nil {
		if ctx.Err() != nil && !errors.Is(err, ErrUnreachable) {
			err = fmt.Errorf("%w: %w", ErrUnreachable, err)
		}
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
	ctx, cancel := ws.call(ctx)
	defer cancel()
	if err := w.Probe(ctx); err != nil {
		if !errors.Is(err, ErrUnreachable) {
			err = fmt.Errorf("%w: %w", ErrUnreachable, err)
		}
		return err
	}
	return nil
}

// Fingerprint identifies a KEK without revealing it: the first 16 bytes of HMAC-SHA256 keyed by
// the KEK over a fixed label, in hex. It is the same however the KEK is wrapped. Today "keys
// status" and "keys export" print it, so a backup can be matched with the live key set; recording
// it in the database is 415b.
func Fingerprint(kek []byte) string {
	m := hmac.New(sha256.New, kek)
	_, _ = m.Write([]byte("koinon/secretstore/kek-fingerprint/v1"))
	return hex.EncodeToString(m.Sum(nil)[:16])
}
