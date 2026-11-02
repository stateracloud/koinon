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
	// Lookup reports, for each fingerprint, whether scope holds it, and whether the store has
	// evicted fingerprints of scope (so a miss may be text it once held).
	Lookup(ctx context.Context, scope Scope, fps []Fingerprint) (Hits, error)
	// Drop forgets scope, for example when its home generation ends.
	Drop(ctx context.Context, scope Scope) error
}

// Hits is the answer of a Lookup: Found[i] reports whether the scope holds fingerprint i.
// Evicted reports that the store has forgotten some of the scope's fingerprints (or the whole
// scope) to stay within its size limits, so a miss is not proof the text is not private.
type Hits struct {
	Found   []bool
	Evicted bool
}

// ErrStoreFull is the error of an Add that would take a MemoryStore past MaxTotalFingerprints.
// The caller fails closed on it: text it could not record is not known to be private.
var ErrStoreFull = errors.New("leakguard: the taint store is full")

// MemoryStoreOptions size a MemoryStore. The zero value of a field means its default.
type MemoryStoreOptions struct {
	// MaxFingerprints per scope (default 1<<20, about 1 MiB of private canonical text at the
	// default window). Beyond it the oldest fingerprints are evicted.
	MaxFingerprints int
	// MaxScopes held at once (default 4096). Beyond it the least recently used scope is evicted.
	MaxScopes int
	// MaxTotalFingerprints across all scopes (default 1<<23, roughly 1 GiB of memory). An Add
	// past it fails with ErrStoreFull instead of evicting another scope's taint.
	MaxTotalFingerprints int
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
	maxTotal  int
	total     int
	clock     uint64
	scopes    map[Scope]*scopeSet
	evictedFP uint64
	evictedSc uint64
	// gone remembers scopes evicted whole, so a later Lookup reports them as evicted; past
	// maxGone entries it is cleared and goneLost makes every unknown scope count as evicted.
	gone     map[Scope]struct{}
	goneLost bool
}

// maxGone bounds the memory of evicted scopes, as a multiple of MaxScopes.
const maxGone = 4

type scopeSet struct {
	seq     map[Fingerprint]uint64 // fingerprint -> sequence number of its latest add
	ring    []ringEntry            // adds in order; entries older than seq are stale
	head    int
	next    uint64
	lastUse uint64
	evicted bool // some of this scope's fingerprints were evicted
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
	if o.MaxTotalFingerprints <= 0 {
		o.MaxTotalFingerprints = 1 << 23
	}
	return &MemoryStore{
		maxFP: o.MaxFingerprints, maxScopes: o.MaxScopes, maxTotal: o.MaxTotalFingerprints,
		scopes: map[Scope]*scopeSet{}, gone: map[Scope]struct{}{},
	}
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
	// the scope's size after this add, at most maxFP; fail closed if the total would pass maxTotal
	cur := 0
	if s, ok := m.scopes[scope]; ok {
		cur = len(s.seq)
		grow := 0
		for _, fp := range fps {
			if _, held := s.seq[fp]; !held {
				grow++
			}
		}
		if m.total+min(cur+grow, m.maxFP)-cur > m.maxTotal {
			return ErrStoreFull
		}
	} else if m.total+min(len(fps), m.maxFP) > m.maxTotal {
		return ErrStoreFull
	}
	s := m.scope(scope, true)
	before := len(s.seq)
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
			s.evicted = true
		}
	}
	m.total += len(s.seq) - before
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
func (m *MemoryStore) Lookup(ctx context.Context, scope Scope, fps []Fingerprint) (Hits, error) {
	if scope.Generation == "" {
		return Hits{}, errNoScope
	}
	if err := ctx.Err(); err != nil {
		return Hits{}, err
	}
	out := Hits{Found: make([]bool, len(fps))}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.scope(scope, false)
	if s == nil {
		_, gone := m.gone[scope]
		out.Evicted = gone || m.goneLost
		return out, nil
	}
	out.Evicted = s.evicted
	for i, fp := range fps {
		_, out.Found[i] = s.seq[fp]
	}
	return out, nil
}

// Drop implements Store.
func (m *MemoryStore) Drop(_ context.Context, scope Scope) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.scopes[scope]; ok {
		m.total -= len(s.seq)
		delete(m.scopes, scope)
	}
	delete(m.gone, scope)
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
		n := len(m.scopes[oldest].seq)
		m.evictedFP += uint64(n)
		m.total -= n
		delete(m.scopes, oldest)
		m.evictedSc++
		if len(m.gone) >= maxGone*m.maxScopes {
			clear(m.gone)
			m.goneLost = true
		}
		m.gone[oldest] = struct{}{}
	}
	_, wasGone := m.gone[sc]
	delete(m.gone, sc)
	s := &scopeSet{seq: map[Fingerprint]uint64{}, lastUse: m.clock, evicted: wasGone || m.goneLost}
	m.scopes[sc] = s
	return s
}
