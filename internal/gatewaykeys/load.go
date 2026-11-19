// SPDX-License-Identifier: Apache-2.0

package gatewaykeys

import (
	"bytes"
	"context"
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

// Failure reasons of [Metrics.Failures]. Slice 3 counts the open failures; the reasons are fixed here.
const (
	ReasonUnknownKey = "unknown_key"
	ReasonOpen       = "open"
	ReasonKMS        = "kms_unreachable"
)

// NewMetrics registers the metrics on reg and sets every series to zero.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Loaded: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "koinon_gateway_recipient_key_loaded",
			Help: "Gateway recipient keys this process holds, by the state of the key's row."}, []string{"state"}),
		Custody: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "koinon_gateway_recipient_key_custody",
			Help: "1 for the custody of the key's wrapping key: kms (only the gateway identity unwraps) or local (the Secret holds the key unwrapped)."}, []string{"custody"}),
		Failures: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "koinon_gateway_credential_open_failures_total",
			Help: "Stored MCP credentials the gateway could not open, by reason."}, []string{"reason"}),
	}
	reg.MustRegister(m.Loaded, m.Custody, m.Failures)
	for _, s := range []State{Pending, Active, Retiring} {
		m.Loaded.WithLabelValues(string(s)).Set(0)
	}
	for _, c := range []string{"kms", "local"} {
		m.Custody.WithLabelValues(c).Set(0)
	}
	for _, r := range []string{ReasonUnknownKey, ReasonOpen, ReasonKMS} {
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

	mu    sync.Mutex
	ready bool
	err   error
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
func (l *Loader) Load(ctx context.Context) error {
	err := l.load(ctx)
	l.mu.Lock()
	l.ready, l.err = err == nil, err
	l.mu.Unlock()
	return err
}

func (l *Loader) load(ctx context.Context) error {
	rows, err := List(ctx, l.DB)
	if err != nil {
		return err
	}
	entries, err := readDir(l.Dir)
	if err != nil {
		return err
	}
	var keys []*secretstore.RecipientKey
	counts := map[State]float64{}
	var loaded []Row
	custody := ""
	var errs []error
	for _, r := range rows {
		if r.State == Retired {
			continue
		}
		pub, err := r.PublicKey()
		if err != nil {
			errs = append(errs, err)
			continue
		}
		s, ok := entries[r.KeyID]
		if !ok {
			errs = append(errs, fmt.Errorf("gateway key %s (%s) has no wrapped private key in %s", r.KeyID, r.State, l.Dir))
			continue
		}
		k, err := l.Wrap.UnwrapRecipientKey(ctx, r.Recipient, r.KeyID, s)
		if err != nil {
			errs = append(errs, fmt.Errorf("gateway key %s: %w", r.KeyID, err))
			continue
		}
		if k.ID() != r.KeyID || !bytes.Equal(k.Public().Bytes(), pub.Bytes()) {
			errs = append(errs, fmt.Errorf("gateway key %s: its private key does not match the public row", r.KeyID))
			continue
		}
		keys = append(keys, k)
		loaded = append(loaded, r)
		counts[r.State]++
		if s.Backend == secretstore.BackendLocal {
			custody = "local"
		} else {
			custody = "kms"
		}
	}
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
		for _, c := range []string{"kms", "local"} {
			v := 0.0
			if c == custody {
				v = 1
			}
			l.Metrics.Custody.WithLabelValues(c).Set(v)
		}
	}
	// Report the keys held, also when another key failed: a loaded key stays loaded.
	for _, r := range loaded {
		if err := ReportLoaded(ctx, l.DB, r.KeyID, l.CopyID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
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
func readDir(dir string) (map[string]secretstore.StoredKEK, error) {
	des, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) || dir == "" {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	data := map[string][]byte{}
	for _, de := range des {
		if de.IsDir() || !secretstore.ValidRecipientKeyID(de.Name()) {
			continue // the kubelet's ..data links and dot files
		}
		b, err := os.ReadFile(filepath.Join(dir, de.Name()))
		if err != nil {
			return nil, err
		}
		data[de.Name()] = b
	}
	stored, _, err := secretstore.ParseStoredKeys(data)
	return stored, err
}

// PublicKeys is koinon-api's view of the gateway's public keys: the newest `active` key to seal to. It
// holds no private key.
type PublicKeys struct {
	mu     sync.RWMutex
	active *secretstore.RecipientPublicKey
	id     string
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
