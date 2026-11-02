// SPDX-License-Identifier: Apache-2.0

package leakguard

import (
	"context"
	"errors"
	"sync"
)

// Scope is the unit of taint: one home generation of one agent (ADR 0084 decision 4). A new
// home generation is a new Scope and starts empty.
type Scope struct {
	Org        string
	Agent      string
	Generation string
}

// Store holds the fingerprints of private data, per scope. Implementations must be safe for
// concurrent use. The in-memory MemoryStore is the reference; where the store lives in
// production (koinon-api memory or Postgres, restarts, replicas) is decided in #349.
type Store interface {
	// Add records fingerprints for scope.
	Add(ctx context.Context, scope Scope, fps []Fingerprint) error
	// Lookup reports, for each fingerprint, whether scope holds it.
	Lookup(ctx context.Context, scope Scope, fps []Fingerprint) ([]bool, error)
	// Drop forgets scope, for example when its home generation ends.
	Drop(ctx context.Context, scope Scope) error
}

// MemoryStoreOptions size a MemoryStore. The zero value of a field means its default.
type MemoryStoreOptions struct {
	// MaxFingerprints per scope (default 1<<20, about 1 MiB of private canonical text at the
	// default window). Beyond it the oldest fingerprints are evicted.
	MaxFingerprints int
	// MaxScopes held at once (default 4096). Beyond it the least recently used scope is evicted.
	MaxScopes int
}

// StoreStats counts what a MemoryStore holds and what it evicted. An eviction forgets taint, so
// a caller should count and alert on it.
type StoreStats struct {
	Scopes              int
	Fingerprints        int
	EvictedFingerprints uint64
	EvictedScopes       uint64
}

// MemoryStore is an in-memory Store with eviction by size: per scope, the oldest added
// fingerprints go first (adding a fingerprint again makes it new); across scopes, the least
// recently used scope goes first.
type MemoryStore struct {
	mu        sync.Mutex
	maxFP     int
	maxScopes int
	clock     uint64
	scopes    map[Scope]*scopeSet
	evictedFP uint64
	evictedSc uint64
}

type scopeSet struct {
	seq     map[Fingerprint]uint64 // fingerprint -> sequence number of its latest add
	ring    []ringEntry            // adds in order; entries older than seq are stale
	head    int
	next    uint64
	lastUse uint64
}

type ringEntry struct {
	fp  Fingerprint
	seq uint64
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore(o MemoryStoreOptions) *MemoryStore {
	if o.MaxFingerprints <= 0 {
		o.MaxFingerprints = 1 << 20
	}
	if o.MaxScopes <= 0 {
		o.MaxScopes = 4096
	}
	return &MemoryStore{maxFP: o.MaxFingerprints, maxScopes: o.MaxScopes, scopes: map[Scope]*scopeSet{}}
}

var errNoScope = errors.New("leakguard: the scope has no generation")

// Add implements Store.
func (m *MemoryStore) Add(ctx context.Context, scope Scope, fps []Fingerprint) error {
	if scope.Generation == "" {
		return errNoScope
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.scope(scope, true)
	for _, fp := range fps {
		s.next++
		s.seq[fp] = s.next
		s.ring = append(s.ring, ringEntry{fp, s.next})
	}
	for len(s.seq) > m.maxFP {
		e := s.ring[s.head]
		s.head++
		if s.seq[e.fp] == e.seq {
			delete(s.seq, e.fp)
			m.evictedFP++
		}
	}
	// drop stale entries and compact when the ring has grown to twice what it holds
	if len(s.ring)-s.head > 2*len(s.seq)+1024 || s.head > len(s.ring)/2 {
		live := make([]ringEntry, 0, len(s.seq))
		for _, e := range s.ring[s.head:] {
			if s.seq[e.fp] == e.seq {
				live = append(live, e)
			}
		}
		s.ring, s.head = live, 0
	}
	return nil
}

// Lookup implements Store.
func (m *MemoryStore) Lookup(ctx context.Context, scope Scope, fps []Fingerprint) ([]bool, error) {
	if scope.Generation == "" {
		return nil, errNoScope
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]bool, len(fps))
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.scope(scope, false)
	if s == nil {
		return out, nil
	}
	for i, fp := range fps {
		_, out[i] = s.seq[fp]
	}
	return out, nil
}

// Drop implements Store.
func (m *MemoryStore) Drop(_ context.Context, scope Scope) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.scopes, scope)
	return nil
}

// Stats returns the store's counters.
func (m *MemoryStore) Stats() StoreStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := StoreStats{Scopes: len(m.scopes), EvictedFingerprints: m.evictedFP, EvictedScopes: m.evictedSc}
	for _, s := range m.scopes {
		st.Fingerprints += len(s.seq)
	}
	return st
}

// scope returns the set of sc, creating it (and evicting the least recently used scope) when
// create is set. The caller holds m.mu.
func (m *MemoryStore) scope(sc Scope, create bool) *scopeSet {
	m.clock++
	if s, ok := m.scopes[sc]; ok {
		s.lastUse = m.clock
		return s
	}
	if !create {
		return nil
	}
	if len(m.scopes) >= m.maxScopes {
		var oldest Scope
		var oldestUse uint64
		first := true
		for k, s := range m.scopes {
			if first || s.lastUse < oldestUse {
				oldest, oldestUse, first = k, s.lastUse, false
			}
		}
		m.evictedFP += uint64(len(m.scopes[oldest].seq))
		delete(m.scopes, oldest)
		m.evictedSc++
	}
	s := &scopeSet{seq: map[Fingerprint]uint64{}, lastUse: m.clock}
	m.scopes[sc] = s
	return s
}
