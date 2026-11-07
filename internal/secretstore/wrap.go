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
	"slices"
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
	// The watch loops of koinon-api and the reconciler call it on every reload of the key set
	// (internal/kekhealth, koinon_kek_active_reachable).
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
	// InstallID is the install the KEK was wrapped for (stored format 2, old issue 615): its wrap bound this
	// install ID into the associated data. Empty for a KEK wrapped before that (format 1, bound to
	// its KEK ID only) and for a [Local] KEK, which cannot bind anything. It is not trusted: an
	// unwrap binds the install ID of the process ([Wrappers.WithInstallID]), never this one, which
	// only selects the format and explains a mismatch.
	InstallID string
}

// Bound reports whether the KEK is bound to an install (stored format 2).
func (s StoredKEK) Bound() bool { return s.InstallID != "" }

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
	// Install is the install ID the KEK was wrapped for; only in format 2.
	Install string `json:"install,omitempty"`
}

// The stored formats of a KMS-wrapped KEK. Format 1 (old issue 416) binds the KEK to its ID ([kekAAD]);
// format 2 (old issue 615) binds it to its ID and the install ([kekInstallAAD]), so a KEK copied from
// another install, or restored next to another install's database, does not unwrap. Every new wrap
// writes format 2; format 1 is read for as long as a Secret or a backup may hold it. A binary from
// before old issue 615 refuses format 2 when it parses the key set ("unsupported format 2"): it fails closed
// and never reads a bound KEK as an unbound one.
const (
	storedFormatV1 = 1
	storedFormatV2 = 2
)

// installIDPattern is an install ID: a UUID in its canonical, lowercase text form, as Postgres
// prints one (table install_identity).
var installIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ValidInstallID reports whether id is an install ID: a lowercase canonical UUID.
func ValidInstallID(id string) bool { return installIDPattern.MatchString(id) }

// Encode returns the Secret value of s.
func (s StoredKEK) Encode() ([]byte, error) {
	if s.Backend == BackendLocal {
		if s.InstallID != "" {
			return nil, errors.New("secretstore: a local key cannot be bound to an install")
		}
		if len(s.Wrapped) != KeySize {
			return nil, fmt.Errorf("a local key is %d bytes, not %d", len(s.Wrapped), KeySize)
		}
		return EncodeKey(s.Wrapped), nil
	}
	if !backendPattern.MatchString(s.Backend) || len(s.Wrapped) == 0 {
		return nil, fmt.Errorf("secretstore: cannot store a KEK wrapped by %q", s.String())
	}
	j := storedJSON{Format: storedFormatV1, Backend: s.Backend, KeyID: s.KeyID, Wrapped: s.Wrapped}
	if s.InstallID != "" {
		if !ValidInstallID(s.InstallID) {
			return nil, fmt.Errorf("secretstore: cannot store a KEK wrapped by %q for an invalid install ID", s.String())
		}
		j.Format, j.Install = storedFormatV2, s.InstallID
	}
	return json.Marshal(j)
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
		if j.Backend == BackendLocal || !backendPattern.MatchString(j.Backend) || len(j.Wrapped) == 0 {
			return StoredKEK{}, fmt.Errorf("a wrapped key of unsupported format %d, backend %q", j.Format, j.Backend)
		}
		switch {
		case j.Format == storedFormatV1 && j.Install == "":
		case j.Format == storedFormatV2 && ValidInstallID(j.Install):
		default:
			return StoredKEK{}, fmt.Errorf("a wrapped key of unsupported format %d, backend %q", j.Format, j.Backend)
		}
		return StoredKEK{Backend: j.Backend, KeyID: j.KeyID, Wrapped: j.Wrapped, InstallID: j.Install}, nil
	}
	raw, ok := decodeKey(s)
	if !ok {
		return StoredKEK{}, fmt.Errorf("want the base64 or 64 hex characters of %d random bytes, or a wrapped key", KeySize)
	}
	return StoredKEK{Backend: BackendLocal, Wrapped: raw}, nil
}

// kekAAD binds a wrapped KEK to its ID, so a wrapped KEK copied under another ID does not unwrap.
// It is the associated data of stored format 1 and must never change: KEKs wrapped with it are in
// Secrets and backups.
func kekAAD(id string) []byte { return Binding{}.aad("kek", id) }

// kekInstallAAD binds a wrapped KEK to its ID and to the install (stored format 2), so a KEK copied
// from another install, or under another ID, does not unwrap. Its purpose tag differs from
// [kekAAD]'s, so the associated data of the two formats never coincide: a format 2 KEK relabelled as
// format 1 (or the reverse) does not unwrap either.
func kekInstallAAD(installID, id string) []byte {
	return lengthPrefixed("koinon/secretstore/v1", "kek-install", id, installID)
}

// InstallIDSource returns the ID of the install whose data the KEKs protect. Koinon reads it from
// the database (table install_identity, internal/installid), where a migration creates it once; it
// never changes afterwards. A source may be called on every load of the key set.
type InstallIDSource func(context.Context) (string, error)

// StaticInstallID is a source that always returns id (tests, and callers that read it already).
func StaticInstallID(id string) InstallIDSource {
	return func(context.Context) (string, error) { return id, nil }
}

// ErrNoInstallID means a KEK bound to an install (format 2) was to be wrapped or unwrapped, and the
// process does not know the install ID: it has no source, or the source failed.
var ErrNoInstallID = errors.New("secretstore: the install ID is not known")

// BoundOnly is the install's "bound only" marker (old issue 615): once it is set, a KEK a KMS wrapped before
// the install binding (stored format 1) is refused, so an old KEK Secret backup, which holds the
// same KEKs unbound, no longer opens next to this install's database or any other. It is set once,
// by the key-admin Job when it creates the first KEK of an install or when --rewrap-unbound leaves
// no unbound KEK, and never cleared (table kek_bound_only, internal/installid).
type BoundOnly struct {
	Set bool
	At  time.Time
	// How says what set it: [BoundOnlyFirstInstall] or [BoundOnlyRewrap].
	How string
}

// The ways the marker gets set.
const (
	BoundOnlyFirstInstall = "first-install"
	BoundOnlyRewrap       = "rewrap-unbound"
)

// String says whether the marker is set, and when and how.
func (b BoundOnly) String() string {
	if !b.Set {
		return "not set"
	}
	how := b.How
	switch b.How {
	case BoundOnlyFirstInstall:
		how = "the first KEK of this install was created"
	case BoundOnlyRewrap:
		how = "the key-admin Job (--rewrap-unbound) bound the last unbound KEK"
	}
	return fmt.Sprintf("set at %s, when %s", b.At.UTC().Format(time.RFC3339), how)
}

// BoundOnlySource reads the marker. Koinon reads it from the database on every load of the key set
// that holds an unbound KMS KEK, until it is set; a set marker may be kept, since it is never
// cleared.
type BoundOnlySource func(context.Context) (BoundOnly, error)

// StaticBoundOnly is a source that always returns b (tests).
func StaticBoundOnly(b BoundOnly) BoundOnlySource {
	return func(context.Context) (BoundOnly, error) { return b, nil }
}

// UnreadableBoundOnly is a source that always fails with err: for a process that has no database
// to read the marker from. KMS KEKs stored unbound (format 1) then never open (fail closed); local
// KEKs and bound KEKs are not affected.
func UnreadableBoundOnly(err error) BoundOnlySource {
	return func(context.Context) (BoundOnly, error) { return BoundOnly{}, err }
}

// UnboundKEKs returns the IDs, sorted, of the KEKs a KMS wrapped without the install binding
// (format 1). Local KEKs are not wrapped, so never unbound.
func UnboundKEKs(stored map[string]StoredKEK) []string {
	var ids []string
	for id, s := range stored {
		if s.Backend != BackendLocal && !s.Bound() {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

// ErrBoundOnly means a KEK stored unbound (format 1) was refused because the install accepts bound
// KEKs only. It is also [ErrOpen]: the KEK does not open here, whatever the KMS says.
var ErrBoundOnly = errors.New("secretstore: this install accepts only KEKs bound to the install ID")

// Wrappers are the key-wrapping keys a process can use: one wraps new KEKs, the others (and
// [Local], always) only unwrap, so a key set can hold KEKs of an earlier backend while its values
// are rewrapped. The zero value is [Local] alone: today's behaviour.
//
// Only the key-admin Job wraps a new KEK with a KMS (procoduck, 2026-11-02): the services (koinon-api,
// the reconciler) are decrypt-only; agentd holds no keyring. [NewWrappers] builds a service's wrappers, whose Wrap
// refuses a KMS primary with [ErrKeyAdminWraps] before it calls the KMS; [NewKeyAdminWrappers]
// builds the Job's. With the [Local] primary both wrap, as before key wrapping. Processes build
// them from their configuration in one place (package secretstore/wrapping), not here.
//
// A KMS-wrapped KEK is bound to the install ([Wrappers.WithInstallID], old issue 615): every new wrap with a
// KMS needs the install ID, and so does every unwrap of a KEK stored in format 2.
type Wrappers struct {
	primary   Wrapper
	more      []Wrapper
	keyAdmin  bool
	timeout   time.Duration
	install   InstallIDSource
	boundOnly BoundOnlySource
}

// acceptUnbound reports whether ws may open the KMS KEK id stored unbound (format 1).
func (ws Wrappers) acceptUnbound(ctx context.Context, id string) error {
	if ws.boundOnly == nil {
		// Only the zero value has no source, and it opens local KEKs only; refuse rather than guess.
		return fmt.Errorf("secretstore: KEK %q is stored unbound (format 1) and these wrappers read no bound-only marker", id)
	}
	// The source reads the database: bound it like an adapter call, so a hung database fails this
	// load (fail closed) instead of blocking it and every load after it.
	cctx, cancel := ws.call(ctx)
	defer cancel()
	b, err := ws.boundOnly(cctx)
	if err != nil {
		if cctx.Err() != nil && ctx.Err() == nil {
			slog.Warn("reading the bound-only marker timed out; the KEK stored unbound is refused (fail closed)",
				"kek", id, "timeout", ws.callTimeout())
		}
		return fmt.Errorf("secretstore: KEK %q is stored unbound (format 1) and whether this install still accepts that is not known: %w", id, err)
	}
	if b.Set {
		return fmt.Errorf("%w: %w: KEK %q is stored unbound (format 1, wrapped before old issue 615), and the bound-only marker of this install was %s. "+
			"The KEK Secret is an old backup or belongs to another install: restore one taken after the KEKs were bound. "+
			"If the database holds no encrypted values yet (a new install, where GitOps or a backup restore put an old Secret in place), "+
			"delete the KEK Secret and rerun the key-admin Job instead: no values are lost (docs/ops/secret-keys.md)",
			ErrBoundOnly, ErrOpen, id, b)
	}
	return nil
}

// WithInstallID returns ws reading the install ID from src: new KMS wraps bind it, and the KEKs
// bound to an install unwrap only with it. Without a source, ws reads local and format 1 KEKs only
// and wraps with [Local] only.
func (ws Wrappers) WithInstallID(src InstallIDSource) Wrappers {
	ws.install = src
	return ws
}

// installID returns the install ID from the source, checked.
func (ws Wrappers) installID(ctx context.Context) (string, error) {
	if ws.install == nil {
		return "", fmt.Errorf("%w: this process reads no install ID (the database's install_identity)", ErrNoInstallID)
	}
	id, err := ws.install(ctx)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrNoInstallID, err)
	}
	if !ValidInstallID(id) {
		return "", fmt.Errorf("%w: the install ID %q is not a lowercase UUID", ErrNoInstallID, id)
	}
	return id, nil
}

// ErrKeyAdminWraps means a service tried to wrap a new KEK with a KMS: only the key-admin Job holds
// encrypt rights.
var ErrKeyAdminWraps = errors.New("secretstore: new KEKs are wrapped by the key-admin Job; this process may only unwrap")

// CallTimeout bounds each adapter call that Wrappers makes, whatever the caller's context: the
// watch loops pass a context that lives as long as the process.
const CallTimeout = 10 * time.Second

// NewWrappers returns a service's wrappers: primary is the wrapping key of the key set (new KEKs
// are wrapped with it only when it is [Local]). Each backend and key ID pair appears once.
//
// boundOnly is required (old issue 615): it reads the install's "bound only" marker before a KMS KEK stored
// unbound (format 1) is unwrapped. With the marker set, such a KEK is [ErrBoundOnly]; when the
// marker cannot be read, it is refused too (fail closed). There is no "no marker" mode: a process
// without a database passes a source that returns an error, and tests pass [StaticBoundOnly].
func NewWrappers(boundOnly BoundOnlySource, primary Wrapper, more ...Wrapper) (Wrappers, error) {
	return newWrappers(false, boundOnly, primary, more)
}

// NewKeyAdminWrappers returns the key-admin Job's wrappers: they wrap new KEKs with primary.
// boundOnly is required, as for [NewWrappers].
func NewKeyAdminWrappers(boundOnly BoundOnlySource, primary Wrapper, more ...Wrapper) (Wrappers, error) {
	return newWrappers(true, boundOnly, primary, more)
}

// ErrNoBoundOnlySource means wrappers were built without a source for the bound-only marker.
var ErrNoBoundOnlySource = errors.New("secretstore: no source for the bound-only marker (old issue 615)")

// WithCallTimeout returns ws with another per-call bound than [CallTimeout] (tests).
func (ws Wrappers) WithCallTimeout(d time.Duration) Wrappers {
	ws.timeout = d
	return ws
}

func (ws Wrappers) callTimeout() time.Duration {
	if ws.timeout <= 0 {
		return CallTimeout
	}
	return ws.timeout
}

func (ws Wrappers) call(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, ws.callTimeout())
}

func newWrappers(keyAdmin bool, boundOnly BoundOnlySource, primary Wrapper, more []Wrapper) (Wrappers, error) {
	if boundOnly == nil {
		return Wrappers{}, ErrNoBoundOnlySource
	}
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
	return Wrappers{primary: primary, more: more, keyAdmin: keyAdmin, boundOnly: boundOnly}, nil
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
	// The stored form of a local KEK is the key itself: there is nothing to bind.
	aad, install := kekAAD(id), ""
	if w.Backend() != BackendLocal {
		if !ws.keyAdmin {
			return StoredKEK{}, fmt.Errorf("wrap key %q with %s: %w", id, w.Backend(), ErrKeyAdminWraps)
		}
		// Every new KMS wrap is bound to the install (format 2); without the install ID nothing is
		// wrapped, so no new KEK is ever left unbound.
		var err error
		if install, err = ws.installID(ctx); err != nil {
			return StoredKEK{}, fmt.Errorf("secretstore: wrap key %q with %s: %w", id, w.Backend(), err)
		}
		aad = kekInstallAAD(install, id)
	}
	ctx, cancel := ws.call(ctx)
	defer cancel()
	wrapped, err := w.Wrap(ctx, kek, aad)
	if err != nil {
		if ctx.Err() != nil && !errors.Is(err, ErrUnreachable) {
			err = fmt.Errorf("%w: %w", ErrUnreachable, err)
		}
		return StoredKEK{}, fmt.Errorf("secretstore: wrap key %q with %s: %w", id, w.Backend(), err)
	}
	return StoredKEK{Backend: w.Backend(), KeyID: w.KeyID(), Wrapped: wrapped, InstallID: install}, nil
}

// aadFor is the associated data s was wrapped with: the KEK ID alone for format 1 (and a local
// KEK, which ignores it), the KEK ID and this process's install ID for format 2. It returns that
// install ID too ("" for format 1).
func (ws Wrappers) aadFor(ctx context.Context, id string, s StoredKEK) ([]byte, string, error) {
	if s.Backend == BackendLocal {
		return kekAAD(id), "", nil
	}
	if !s.Bound() {
		if err := ws.acceptUnbound(ctx, id); err != nil {
			return nil, "", err
		}
		return kekAAD(id), "", nil
	}
	install, err := ws.installID(ctx)
	if err != nil {
		return nil, "", err
	}
	return kekInstallAAD(install, id), install, nil
}

// Unwrap returns the KEK id that s holds. A KEK bound to another install than this process's is
// [ErrOpen], like any wrong key.
func (ws Wrappers) Unwrap(ctx context.Context, id string, s StoredKEK) ([]byte, error) {
	w, err := ws.For(s)
	if err != nil {
		return nil, err
	}
	aad, install, err := ws.aadFor(ctx, id, s)
	if err != nil {
		return nil, fmt.Errorf("secretstore: unwrap key %q with %s: %w", id, s.String(), err)
	}
	ctx, cancel := ws.call(ctx)
	defer cancel()
	kek, err := w.Unwrap(ctx, s.Wrapped, aad)
	if err != nil {
		if ctx.Err() != nil && !errors.Is(err, ErrUnreachable) {
			err = fmt.Errorf("%w: %w", ErrUnreachable, err)
		}
		if errors.Is(err, ErrOpen) && install != s.InstallID {
			// The recorded install ID is not trusted, but it says what probably happened.
			err = fmt.Errorf("%w: it was wrapped for install %s and this is install %s: the KEK Secret belongs to another install, or the database was restored into the wrong one", err, s.InstallID, install)
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
// status" and "keys export" print it, so a backup can be matched with the live key set, and
// koinon-api records it in the database (kek_fingerprints) so a wrong KEK Secret is noticed.
func Fingerprint(kek []byte) string {
	m := hmac.New(sha256.New, kek)
	_, _ = m.Write([]byte("koinon/secretstore/kek-fingerprint/v1"))
	return hex.EncodeToString(m.Sum(nil)[:16])
}
