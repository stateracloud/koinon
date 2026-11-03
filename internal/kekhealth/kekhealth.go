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
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/secretstore"
)

// Metrics are the KEK metrics. The label set is fixed (ADR 0023): backend is the adapter name
// ("local", "openbao", "awskms"), never a key ID.
type Metrics struct {
	// ActiveReachable is 1 while the wrapping key of the active KEK answers a probe, else 0. Only the
	// active KEK's backend has a series.
	ActiveReachable *prometheus.GaugeVec
}

// NewMetrics creates the metrics and registers them with reg (nil: not registered).
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{ActiveReachable: prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "koinon_kek_active_reachable",
		Help: "1 while the key-wrapping key of the active KEK is reachable by this process, else 0; local is always 1. By backend.",
	}, []string{"backend"})}
	if reg != nil {
		reg.MustRegister(m.ActiveReachable)
	}
	return m
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
	active, ok := l.Stored[l.Active]
	if !ok {
		return
	}
	reachable := m.probe(ctx, active, l.Err, log)
	if m.Metrics != nil {
		m.Metrics.ActiveReachable.Reset() // the backend can change with the active KEK
		v := 0.0
		if reachable {
			v = 1
		}
		m.Metrics.ActiveReachable.WithLabelValues(active.Backend).Set(v)
	}
	if active.Backend == secretstore.BackendLocal && m.Wrappers.Primary().Backend() != secretstore.BackendLocal {
		// Only an install that opted in to a KMS is told; a local-only install never is.
		log.Warn("the active key-encryption key is a local one but a KMS is configured: run keys add and keys activate with the key-admin Job", "kek", l.Active)
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
		log.Warn("a key-encryption key differs from the one the database recorded for its ID: a restored or wrong KEK Secret opens nothing",
			"kek", mm.KEKID, "recorded", mm.Recorded, "loaded", mm.Loaded)
	}
}

// probe reports whether the wrapping key of active answers. A load that already failed as
// unreachable counts as unreachable without a second call.
func (m *Monitor) probe(ctx context.Context, active secretstore.StoredKEK, loadErr error, log *slog.Logger) bool {
	if errors.Is(loadErr, secretstore.ErrUnreachable) {
		log.Warn("the key-wrapping key of the active key-encryption key is unreachable", "wrapped_by", active.String(), "err", loadErr)
		return false
	}
	if err := m.Wrappers.Probe(ctx, active); err != nil {
		log.Warn("the key-wrapping key of the active key-encryption key is unreachable", "wrapped_by", active.String(), "err", err)
		return false
	}
	return true
}
