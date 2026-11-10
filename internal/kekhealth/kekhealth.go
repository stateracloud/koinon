// SPDX-License-Identifier: Apache-2.0

// Package kekhealth watches the key-encryption keys (ADR 0085 decision 12, part 2): it probes the
// wrapping key of the active KEK and reports it as koinon_kek_active_reachable{backend}, and it
// records each KEK's fingerprint in the database so a restored or wrong KEK Secret can be told from
// the one the database was written with.
//
// A local-only install stays quiet: the Local wrapper is always reachable, so the gauge is 1 and
// nothing is logged. Nothing here holds key material.
package kekhealth

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/project"
	"github.com/stateracloud/koinon/internal/secretstore"
)

// Metrics are the KEK metrics. The label set is fixed (ADR 0023): backend is the adapter name
// ("local", "openbao", "awskms"), never a key ID.
type Metrics struct {
	// ActiveReachable is 1 while the wrapping key of the active KEK answers a probe, else 0. Only the
	// active KEK's backend has a series.
	ActiveReachable *prometheus.GaugeVec
	// Unbound is the number of KEKs in the loaded key set that a KMS wrapped before old issue 615, so they
	// are not bound to the install ID. Local KEKs are not counted (they are not wrapped). No labels.
	Unbound prometheus.Gauge
}

// NewMetrics creates the metrics and registers them with reg (nil: not registered).
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		ActiveReachable: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "koinon_kek_active_reachable",
			Help: "1 while the key-wrapping key of the active KEK is reachable by this process, else 0; local is always 1. By backend.",
		}, []string{"backend"}),
		Unbound: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "koinon_kek_unbound",
			Help: "KEKs in the loaded key set wrapped by a KMS but not bound to the install ID (wrapped before old issue 615); local KEKs are not counted.",
		}),
	}
	if reg != nil {
		reg.MustRegister(m.Collectors()...)
	}
	return m
}

// Collectors are all the KEK metrics, for a registry made after NewMetrics.
func (m *Metrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{m.ActiveReachable, m.Unbound}
}

// Mismatch is a KEK whose fingerprint differs from the one recorded for its ID.
type Mismatch struct {
	KEKID    string
	Recorded string
	Loaded   string
}

// Recorder keeps the fingerprints in the database.
type Recorder struct {
	Q store.Querier
}

// Record writes the fingerprint of each loaded KEK that has none yet, updates what wraps each one
// (and, for the active KEK, when it became active, if activatedAt is not zero), and returns the KEKs
// whose recorded fingerprint differs from the loaded one. A recorded fingerprint is never replaced.
func (r Recorder) Record(ctx context.Context, l secretstore.Loaded) ([]Mismatch, error) {
	var out []Mismatch
	var errs []error
	for id, fp := range l.Fingerprints {
		s := l.Stored[id]
		arg := store.RecordKEKFingerprintParams{KekID: id, Fingerprint: fp, Backend: s.Backend, KeyID: s.KeyID}
		if id == l.Active && !l.ActivatedAt.IsZero() {
			arg.ActivatedAt = pgtype.Timestamptz{Time: l.ActivatedAt, Valid: true}
		}
		row, err := r.Q.RecordKEKFingerprint(ctx, arg)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if row.Fingerprint != fp {
			out = append(out, Mismatch{KEKID: id, Recorded: row.Fingerprint, Loaded: fp})
		}
	}
	return out, errors.Join(errs...)
}

// Recorded returns the recorded fingerprint of each KEK ID.
func (r Recorder) Recorded(ctx context.Context) (map[string]store.KekFingerprint, error) {
	rows, err := r.Q.ListKEKFingerprints(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]store.KekFingerprint, len(rows))
	for _, row := range rows {
		out[row.KekID] = row
	}
	return out, nil
}

// Monitor is a [secretstore.Observer] source: it probes the active KEK's wrapping key on every
// load and, with a Recorder, records fingerprints.
type Monitor struct {
	Wrappers secretstore.Wrappers
	Metrics  *Metrics // nil: no gauge
	Recorder *Recorder
	Log      *slog.Logger

	mu     sync.Mutex
	warned map[string]time.Time // throttle key -> last warning
	last   string               // backend of the series now set
}

// warnEvery bounds a repeated warning (the reconciler retries a failed load every 5 seconds).
const warnEvery = 10 * time.Minute

// throttle reports whether the warning for key is due, and marks it. A zero every means once per
// process.
func (m *Monitor) throttle(key string, every time.Duration) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.warned == nil {
		m.warned = map[string]time.Time{}
	}
	if t, ok := m.warned[key]; ok && (every == 0 || time.Since(t) < every) {
		return false
	}
	m.warned[key] = time.Now()
	return true
}

func (m *Monitor) clearThrottle(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.warned, key)
}

// Observer returns the observer to pass to the keyring's load and watch.
func (m *Monitor) Observer() secretstore.Observer { return m.Observe }

// Observe handles one load of the key set. The first load happens at start and the next ones on the
// watch interval, so the gauge follows the wrapping key about once a minute. It never fails the load.
func (m *Monitor) Observe(ctx context.Context, l secretstore.Loaded) {
	log := m.Log
	if log == nil {
		log = slog.Default()
	}
	m.observeUnbound(l, log)
	active, ok := l.Stored[l.Active]
	if !ok {
		return
	}
	reachable := m.probe(ctx, active, log)
	if m.Metrics != nil {
		m.mu.Lock()
		if m.last != "" && m.last != active.Backend { // the backend changed with the active KEK
			m.Metrics.ActiveReachable.DeleteLabelValues(m.last)
		}
		m.last = active.Backend
		m.mu.Unlock()
		v := 0.0
		if reachable {
			v = 1
		}
		m.Metrics.ActiveReachable.WithLabelValues(active.Backend).Set(v)
	}
	if active.Backend == secretstore.BackendLocal && m.Wrappers.Primary().Backend() != secretstore.BackendLocal {
		// Only an install that opted in to a KMS is told; a local-only install never is.
		if m.throttle("local-active", warnEvery) {
			log.Warn("the active key-encryption key is a local one but a KMS is configured: run keys add and keys activate with the key-admin Job", "kek", l.Active)
		}
	}
	if m.Recorder == nil || l.Err != nil {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	mismatches, err := m.Recorder.Record(rctx, l)
	if err != nil {
		log.Warn("recording the key-encryption key fingerprints failed", "err", err)
	}
	for _, mm := range mismatches {
		if !m.throttle("mismatch/"+mm.KEKID+"/"+mm.Loaded, 0) {
			continue
		}
		log.Warn("a key-encryption key differs from the one the database recorded for its ID: a restored or wrong KEK Secret opens nothing",
			"kek", mm.KEKID, "recorded", mm.Recorded, "loaded", mm.Loaded)
	}
}

// UnboundKEKs counts the KEKs of the key set that a KMS wrapped without the install binding (old issue 615).
func UnboundKEKs(stored map[string]secretstore.StoredKEK) int {
	return len(secretstore.UnboundKEKs(stored))
}

// observeUnbound sets koinon_kek_unbound on every load and warns while it is above 0.
func (m *Monitor) observeUnbound(l secretstore.Loaded, log *slog.Logger) {
	n := UnboundKEKs(l.Stored)
	if m.Metrics != nil && m.Metrics.Unbound != nil {
		m.Metrics.Unbound.Set(float64(n))
	}
	if n == 0 {
		m.clearThrottle("unbound")
		return
	}
	if !m.throttle("unbound", warnEvery) {
		return
	}
	if errors.Is(l.Err, secretstore.ErrBoundOnly) {
		// Rewrapping cannot help: the install accepts bound KEKs only, so the unbound ones are from a
		// Secret restored from before the binding.
		log.Warn("key-encryption keys stored unbound were refused by the bound-only policy of this install (old issue 615): "+
			"restore a KEK Secret taken after the KEKs were bound, or, while the database holds no encrypted values, delete it and rerun the key-admin Job ("+project.SecretKeysDocURL+")",
			"unbound", n, "err", l.Err)
		return
	}
	log.Warn("key-encryption keys wrapped by a KMS are not bound to the install ID (wrapped before old issue 615): "+
		"bind them with the key-admin Job (secrets.kek.kms.keyAdmin.rewrapUnbound, --rewrap-unbound) or rotate them out",
		"unbound", n)
}

// probe reports whether the wrapping key of active answers. It always probes the active KEK's own
// wrapper (bounded by [secretstore.CallTimeout]): a failed load can be a retained KEK on another
// backend, which must not make the active one look unreachable.
func (m *Monitor) probe(ctx context.Context, active secretstore.StoredKEK, log *slog.Logger) bool {
	key := "unreachable/" + active.String()
	if err := m.Wrappers.Probe(ctx, active); err != nil {
		if m.throttle(key, warnEvery) {
			log.Warn("the key-wrapping key of the active key-encryption key is unreachable", "wrapped_by", active.String(), "err", err)
		}
		return false
	}
	m.clearThrottle(key)
	return true
}
