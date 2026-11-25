// SPDX-License-Identifier: Apache-2.0

package gatewaykeys

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/secretstore"
)

// ReportInterval is how often the gateway refreshes its load report and re-reads its key directory.
const ReportInterval = 30 * time.Second

// Metrics are the gateway's key metrics. Their label sets are fixed (ADR 0023): the state of a key, the
// custody of the wrapping key, and a failure reason from a closed list.
type Metrics struct {
	Loaded   *prometheus.GaugeVec   // koinon_gateway_recipient_key_loaded{state}
	Custody  *prometheus.GaugeVec   // koinon_gateway_recipient_key_custody{custody}
	Failures *prometheus.CounterVec // koinon_gateway_credential_open_failures_total{reason}
}

// Failure reasons of [Metrics.Failures]. The gateway counts a refused sealed credential by these reasons: no key of that ID (unknown_key), wrong bytes, binding or key (open), an envelope of a format or suite it does not know (format).
const (
	ReasonUnknownKey = "unknown_key"
	ReasonOpen       = "open"
	ReasonFormat     = "format"
	ReasonKMS        = "kms_unreachable"
)

// NewMetrics registers the metrics on reg and sets every series to zero.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Loaded: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "koinon_gateway_recipient_key_loaded",
			Help: "Gateway recipient keys this process holds, by the state of the key's row.",
		}, []string{"state"}),
		Custody: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "koinon_gateway_recipient_key_custody",
			Help: "1 for the custody of the key's wrapping key: kms (only the gateway identity unwraps) or local (the Secret holds the key unwrapped).",
		}, []string{"custody"}),
		Failures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "koinon_gateway_credential_open_failures_total",
			Help: "Stored MCP credentials the gateway could not open, by reason.",
		}, []string{"reason"}),
	}
	reg.MustRegister(m.Loaded, m.Custody, m.Failures)
	for _, s := range []State{Pending, Active, Retiring} {
		m.Loaded.WithLabelValues(string(s)).Set(0)
	}
	for _, c := range []string{"kms", "local"} {
		m.Custody.WithLabelValues(c).Set(0)
	}
	for _, r := range []string{ReasonUnknownKey, ReasonOpen, ReasonFormat, ReasonKMS} {
		m.Failures.WithLabelValues(r).Add(0)
	}
	return m
}

// Loader is the gateway's holder of its recipient keys. It reads the wrapped keys from the mounted Secret
// directory, unwraps each with the gateway identity, checks each against its table row (the derived ID
// and the public bytes), keeps the keyring, and reports each loaded key to the database.
type Loader struct {
	Dir     string
	DB      store.DBTX
	Wrap    secretstore.Wrappers
	CopyID  string
	Metrics *Metrics
	Keyring *secretstore.RecipientKeyring
	Logf    func(format string, args ...any)
	// NoReport makes Load skip the load report. The re-seal Job loads the keys to open credentials: it is
	// not a gateway process, and its login may not write the reports (ADR 0097 12.4).
	NoReport bool

	mu    sync.Mutex
	ready bool
	err   error
	// good is true once one pass held every key it needed. A later pass that fails only on the database
	// (a list or a report error) keeps the loader ready: the keyring of the last good pass still serves.
	good bool

	// cache keeps each unwrapped key by ID and the digest of its stored bytes, so a pass calls the KMS only
	// for a new or changed entry and a loaded key stays held through a KMS outage.
	cache map[string]cachedKey
}

type cachedKey struct {
	sum [sha256.Size]byte
	key *secretstore.RecipientKey
}

// NewLoader returns a loader with an empty keyring.
func NewLoader(dir string, db store.DBTX, ws secretstore.Wrappers, copyID string, m *Metrics) *Loader {
	return &Loader{Dir: dir, DB: db, Wrap: ws, CopyID: copyID, Metrics: m, Keyring: secretstore.NewRecipientKeyring()}
}

// Ready reports whether the last Load held every key the table says it needs, and the error when not.
// A gateway with no key rows yet is ready: there is nothing to hold (before the key-admin Job ran).
func (l *Loader) Ready() (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ready, l.err
}

// Load runs one pass. It needs every `pending`, `active` and `retiring` row's key; a missing one makes
// the loader not ready. A key with no row, or whose row differs from the key, is refused.
//
// A database error (the list of rows or a load report) is transient: a Postgres restart or a CNPG
// switchover must not remove every MCP endpoint. After one good pass, such an error leaves the loader
// ready and the keyring of that pass in place; Load still returns the error, so the caller logs it.
// Before the first good pass the loader stays not ready.
func (l *Loader) Load(ctx context.Context) error {
	err := l.load(ctx)
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case err == nil:
		l.good = true
		l.ready, l.err = true, nil
	case l.good && isTransient(err):
		l.ready, l.err = true, nil
	default:
		// good means "the last pass that was not a database error held every key": a key that is missing
		// must not turn ready again on a later database error.
		l.good = false
		l.ready, l.err = false, err
	}
	return err
}

// dbError marks an error of the database, as opposed to a key that the loader cannot hold.
type dbError struct{ error }

func (e dbError) Unwrap() error { return e.error }

// isTransient reports whether every error in err is a database error. The error of a missing migration
// is not: the table is not there, and the loader is not ready until it is.
func isTransient(err error) bool {
	if err == nil {
		return false
	}
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		for _, e := range j.Unwrap() {
			if !isTransient(e) {
				return false
			}
		}
		return true
	}
	var d dbError
	return errors.As(err, &d) && !errors.Is(err, ErrNotMigrated)
}

func (l *Loader) load(ctx context.Context) error {
	rows, err := List(ctx, l.DB)
	if err != nil {
		return dbError{err}
	}
	entries, sums, err := readDir(l.Dir)
	if err != nil {
		return err
	}
	if l.cache == nil {
		l.cache = map[string]cachedKey{}
	}
	var keys []*secretstore.RecipientKey
	counts := map[State]float64{}
	var loaded []Row
	local := false
	kms := false
	var errs []error
	next := map[string]cachedKey{}
	for _, r := range rows {
		if r.State == Retired {
			continue
		}
		pub, err := r.PublicKey()
		if err != nil {
			if r.State != Pending {
				errs = append(errs, err)
			}
			continue
		}
		s, ok := entries[r.KeyID]
		if !ok {
			if r.State == Pending {
				// The key-admin writes the row before the Secret entry (or the Secret is not mounted yet): the
				// gateway does not need a pending key to serve, so it waits and does not report it loaded.
				l.logf("gateway key %s is pending and has no wrapped private key yet", r.KeyID)
				continue
			}
			errs = append(errs, fmt.Errorf("gateway key %s (%s) has no wrapped private key in %s", r.KeyID, r.State, l.Dir))
			continue
		}
		var k *secretstore.RecipientKey
		if c, ok := l.cache[r.KeyID]; ok && c.sum == sums[r.KeyID] {
			k = c.key
		} else {
			k, err = l.Wrap.UnwrapRecipientKey(ctx, r.Recipient, r.KeyID, s)
			if err != nil {
				l.failure(ReasonKMS)
				if r.State == Pending {
					l.logf("gateway key %s (pending): %v", r.KeyID, err)
				} else {
					errs = append(errs, fmt.Errorf("gateway key %s: %w", r.KeyID, err))
				}
				continue
			}
			if k.ID() != r.KeyID || !bytes.Equal(k.Public().Bytes(), pub.Bytes()) {
				l.failure(ReasonOpen)
				if r.State != Pending {
					errs = append(errs, fmt.Errorf("gateway key %s: its private key does not match the public row", r.KeyID))
				}
				continue
			}
		}
		next[r.KeyID] = cachedKey{sum: sums[r.KeyID], key: k}
		keys = append(keys, k)
		loaded = append(loaded, r)
		counts[r.State]++
		if s.Backend == secretstore.BackendLocal {
			local = true
		} else {
			kms = true
		}
	}
	l.cache = next
	for id := range entries {
		known := false
		for _, r := range rows {
			known = known || r.KeyID == id
		}
		if !known {
			errs = append(errs, fmt.Errorf("the key directory holds %s, which no table row names", id))
		}
	}
	if len(errs) == 0 {
		l.Keyring.Replace(keys...)
	}
	if l.Metrics != nil {
		for _, s := range []State{Pending, Active, Retiring} {
			l.Metrics.Loaded.WithLabelValues(string(s)).Set(counts[s])
		}
		// `local` wins: one key that koinon-api can read is the weakest custody of the install.
		for _, c := range []string{"kms", "local"} {
			v := 0.0
			if (c == "local" && local) || (c == "kms" && kms && !local) {
				v = 1
			}
			l.Metrics.Custody.WithLabelValues(c).Set(v)
		}
	}
	// Report the keys held, also when another key failed: a loaded key stays loaded.
	for _, r := range loaded {
		if l.NoReport {
			break
		}
		if err := ReportLoaded(ctx, l.DB, r.KeyID, l.CopyID); err != nil {
			errs = append(errs, dbError{err})
		}
	}
	return errors.Join(errs...)
}

func (l *Loader) logf(format string, args ...any) {
	if l.Logf != nil {
		l.Logf(format, args...)
	}
}

// CountFailure counts a stored credential the gateway refused, by one of the fixed Reason constants. The
// gateway's credential source calls it.
func (l *Loader) CountFailure(reason string) { l.failure(reason) }

func (l *Loader) failure(reason string) {
	if l.Metrics != nil {
		l.Metrics.Failures.WithLabelValues(reason).Inc()
	}
}

// Run loads at once and then every [ReportInterval] until ctx ends.
func (l *Loader) Run(ctx context.Context) {
	t := time.NewTicker(ReportInterval)
	defer t.Stop()
	for {
		if err := l.Load(ctx); err != nil && l.Logf != nil {
			l.Logf("gateway recipient keys: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// readDir reads a mounted Secret directory; a missing directory is empty (the Secret is optional).
func readDir(dir string) (map[string]secretstore.StoredKEK, map[string][sha256.Size]byte, error) {
	des, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) || dir == "" {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	data := map[string][]byte{}
	sums := map[string][sha256.Size]byte{}
	for _, de := range des {
		if de.IsDir() || !secretstore.ValidRecipientKeyID(de.Name()) {
			continue // the kubelet's ..data links and dot files
		}
		b, err := os.ReadFile(filepath.Join(dir, de.Name())) //nolint:gosec // dir is the operator-set key mount; the name passed ValidRecipientKeyID
		if err != nil {
			return nil, nil, err
		}
		data[de.Name()] = b
		sums[de.Name()] = sha256.Sum256(b)
	}
	stored, _, err := secretstore.ParseStoredKeys(data)
	return stored, sums, err
}

// PublicKeys is koinon-api's view of the gateway's public keys: the newest `active` key to seal to. It
// holds no private key.
type PublicKeys struct {
	mu     sync.RWMutex
	active *secretstore.RecipientPublicKey
	err    error
}

// Current returns the active key to seal to. It fails when none is active or the last read failed and
// there is no earlier key: the caller then refuses to seal (no caller seals v2 before slice 3).
func (p *PublicKeys) Current() (secretstore.RecipientPublicKey, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.active == nil {
		if p.err != nil {
			return secretstore.RecipientPublicKey{}, p.err
		}
		return secretstore.RecipientPublicKey{}, ErrNoActiveKey
	}
	return *p.active, nil
}

// ErrNoActiveKey means no gateway key is active yet.
var ErrNoActiveKey = errors.New("gatewaykeys: no gateway recipient key is active")

// Refresh reads the active row. A row that fails its check is refused and the earlier key stays.
func (p *PublicKeys) Refresh(ctx context.Context, db store.DBTX) error {
	var r Row
	var st string
	err := db.QueryRow(ctx, `SELECT key_id, recipient, public_key, state FROM gateway_recipient_keys WHERE state = 'active'`).
		Scan(&r.KeyID, &r.Recipient, &r.Public, &st)
	p.mu.Lock()
	defer p.mu.Unlock()
	if errors.Is(err, pgx.ErrNoRows) {
		p.active, p.err = nil, nil
		return nil
	}
	if err != nil {
		if undefinedTable(err) {
			err = ErrNotMigrated
		}
		p.err = err
		return err
	}
	k, err := r.PublicKey()
	if err != nil {
		p.err = err
		return err
	}
	p.active, p.err = &k, nil
	return nil
}

// Run refreshes every [APIReloadInterval] until ctx ends.
func (p *PublicKeys) Run(ctx context.Context, db store.DBTX, logf func(string, ...any)) {
	t := time.NewTicker(APIReloadInterval)
	defer t.Stop()
	for {
		if err := p.Refresh(ctx, db); err != nil && logf != nil {
			logf("gateway public key: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
