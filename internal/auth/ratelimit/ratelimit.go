// SPDX-License-Identifier: Apache-2.0

// Package ratelimit slows down clients that keep failing to authenticate. It is the one limiter of
// Koinon's credential entry points: the admin API's bearer tokens (internal/adminapi) and the
// password login (internal/auth).
//
// A key is a client address and the credential it names (Key). Every Fail counts at three levels: the
// key, the client address (an IPv6 one masked to its /64) and the group (an IPv6 /48; for IPv4 the
// address is the group and has no level of its own). Each level has its own free budget within
// FailWindow (FreeFails for a key, AddressFreeFails, GroupFreeFails). Past it the level is refused for
// BaseBackoff, then twice as long for each further failure, up to MaxBackoff. A level that was refused
// starts afresh only after a quiet FailWindow that follows the end of its block, so a client that
// waits out each block keeps doubling it. Blocked(key) is the longest wait over the three levels, so a
// client that spreads its failures over many credentials, or many /64s of one /48, is refused once it
// passes the address or group budget. The /64 neighbours of an IPv6 client share the group budget. A
// success resets the key only. X-Forwarded-For is not trusted.
//
// The state lives in memory, in one table of at most maxEntries entries, and it is per replica: with N
// replicas of koinon-api a client has N times the budget (shared state is a follow-up). Only an entry
// past its expiry (its block and its quiet window have ended) is dropped to make room. When the table
// is full of live entries a new source is not tracked, and its failures are not counted, until
// entries expire; no request is refused because the table is full, and a flood of fresh keys cannot
// reset a backoff. The limiter checks, then the caller hashes or looks up, then records: requests
// that run at the same time can all pass the check before the first failure is recorded, so a burst
// can overshoot a budget by about the number of concurrent checks.
//
// Lockout risk behind an ingress: every client then shares the ingress's address, because the peer
// address is the ingress's and the forwarded address is not read. One client can therefore lock all
// password logins out, for everyone, by failing against the address's budget, and anyone who knows a
// username or a token prefix can lock that credential for the others by failing against it. The fix is
// a trusted-proxy setting that reads the forwarded address only from a configured proxy; it is tracked
// in issue 859. Until it ships, a NetworkPolicy or an ingress rule gives finer control.
//
// The keys hold a hash of the credential name, not the name. The audit event of a failed
// password login stores the attempted username as typed (cut to a bounded length), so a user who types
// a password into the username field leaves it in the audit log.
package ratelimit

import (
	"container/heap"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The limits. They are the same for every entry point.
const (
	FreeFails   = 5
	FailWindow  = time.Minute
	BaseBackoff = time.Second
	MaxBackoff  = 15 * time.Minute

	// AddressFreeFails and GroupFreeFails are the failures per FailWindow one client address (/64 or
	// IPv4) and one group (/48) may make over all credentials. They are larger than FreeFails because
	// clients behind one ingress share an address.
	AddressFreeFails = 30
	GroupFreeFails   = 120

	// maxEntries is the bound on the entries of the table.
	maxEntries = 110000
)

// Limiter counts failures per key, address and group. The zero value is not usable; call New.
//
// All entries sit in one heap ordered by the time their state ends (expiry), so the root is the
// entry that costs least to drop.
type Limiter struct {
	mu   sync.Mutex
	keys map[string]*entry
	h    entryHeap
}

type entry struct {
	key     string
	fails   int
	first   time.Time
	blocked time.Time
	// refused counts the requests refused while the key was over the limit, and failures that were
	// not recorded one by one, since the caller last took the count (TakeRefused).
	refused int
	idx     int // index in the heap
}

// quietSince is when the entry's quiet window starts: its first failure, or the end of its block.
func (a *entry) quietSince() time.Time {
	if a.blocked.After(a.first) {
		return a.blocked
	}
	return a.first
}

// expiry is when the entry's state ends: a quiet FailWindow after quietSince.
func (a *entry) expiry() time.Time { return a.quietSince().Add(FailWindow) }

type entryHeap []*entry

func (h entryHeap) Len() int           { return len(h) }
func (h entryHeap) Less(i, j int) bool { return h[i].expiry().Before(h[j].expiry()) }
func (h entryHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].idx, h[j].idx = i, j
}

func (h *entryHeap) Push(x any) {
	a := x.(*entry)
	a.idx = len(*h)
	*h = append(*h, a)
}

func (h *entryHeap) Pop() any {
	old := *h
	a := old[len(old)-1]
	old[len(old)-1] = nil
	*h = old[:len(old)-1]
	return a
}

// New returns an empty Limiter.
func New() *Limiter { return &Limiter{keys: map[string]*entry{}} }

// Len is the number of entries tracked (keys, addresses and groups).
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.keys)
}

// addrOf is the address part of a key: everything before the first "|".
func addrOf(key string) string {
	addr, _, _ := strings.Cut(key, "|")
	return addr
}

// groupOf is the group of an address: an IPv6 address widened to its /48, anything else itself.
func groupOf(addr string) string {
	if ip, err := netip.ParseAddr(addr); err == nil && ip.Is6() {
		if p, err := ip.Prefix(48); err == nil {
			return p.String()
		}
	}
	return addr
}

// The keys of a key's address and group levels. No Key is ever of this form.
func addrSlot(addr string) string   { return "\x00a|" + addr }
func groupSlot(group string) string { return "\x00g|" + group }

// levels returns the keys of the address and group levels of key; the group level is empty when the
// group is the address itself.
func levels(key string) (addr, group string) {
	a := addrOf(key)
	addr = addrSlot(a)
	if g := groupOf(a); g != a {
		group = groupSlot(g)
	}
	return addr, group
}

// Key is the slot of a client address and the credential it names (a token prefix, a username);
// part "" is the address's shared slot. The part is client-chosen, so the key holds a truncated
// SHA-256 of it, which bounds the key's size.
func Key(addr, part string) string {
	if part == "" {
		return addr + "|"
	}
	sum := sha256.Sum256([]byte(part))
	return addr + "|" + hex.EncodeToString(sum[:8])
}

// ClientAddr is the request's peer address without the port. An IPv6 address is masked to its /64,
// the smallest block a client commonly holds, so rotating the host part does not escape a key; an
// IPv4 address stays whole. The /64 mask also groups in-cluster IPv6 clients (dual-stack pods on one
// node often share a /64) into one address for the admin API. X-Forwarded-For is not read.
func ClientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip, perr := netip.ParseAddr(host); perr == nil {
		ip = ip.Unmap()
		if ip.Is6() {
			if p, perr := ip.Prefix(64); perr == nil {
				return p.Masked().Addr().String()
			}
		}
		return ip.String()
	}
	return host
}

// RetryAfter is the Retry-After value, in whole seconds rounded up, for a wait of d.
func RetryAfter(d time.Duration) string {
	return strconv.Itoa(int((d + time.Second - 1) / time.Second))
}

// Blocked returns how long key must wait, or zero: the longest wait over the key, its address and its
// group.
func (l *Limiter) Blocked(key string, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	addr, group := levels(key)
	return max(l.wait(key, now), l.wait(addr, now), l.wait(group, now))
}

// BlockedKey is Blocked for the key's own level only, for a caller that must not refuse a client
// because of its neighbours (a valid admin token).
func (l *Limiter) BlockedKey(key string, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.wait(key, now)
}

func (l *Limiter) wait(k string, now time.Time) time.Duration {
	if a, ok := l.keys[k]; ok && now.Before(a.blocked) {
		return a.blocked.Sub(now)
	}
	return 0
}

// Fail records a failure of key, and of its address and group; the key is blocked after FreeFails
// failures, the address after AddressFreeFails and the group after GroupFreeFails.
func (l *Limiter) Fail(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	addr, group := levels(key)
	l.makeRoom(now, key, addr, group)
	// The group and the address come first: when the table has room for some of the entries only, the
	// key's own is the one left untracked.
	if group != "" {
		l.record(group, GroupFreeFails, MaxBackoff, now)
	}
	l.record(addr, AddressFreeFails, MaxBackoff, now)
	l.record(key, FreeFails, MaxBackoff, now)
}

// FailAfter records a failure of key alone, which is blocked after freeFails failures within
// FailWindow.
func (l *Limiter) FailAfter(key string, freeFails int, now time.Time) {
	l.FailCapped(key, freeFails, MaxBackoff, now)
}

// FailCapped is FailAfter with a longest block of maxBackoff.
func (l *Limiter) FailCapped(key string, freeFails int, maxBackoff time.Duration, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.makeRoom(now, key)
	l.record(key, freeFails, maxBackoff, now)
}

// makeRoom drops entries past their expiry until the keys of a call that are not yet in the table
// fit, or none is left. The call's own entries are not dropped.
func (l *Limiter) makeRoom(now time.Time, keys ...string) {
	need := 0
	for _, k := range keys {
		if k != "" && l.keys[k] == nil {
			need++
		}
	}
	var kept []*entry
	for len(l.keys)+need > maxEntries && len(l.h) > 0 && !now.Before(l.h[0].expiry()) {
		old := heap.Pop(&l.h).(*entry)
		if slices.Contains(keys, old.key) {
			kept = append(kept, old)
			continue
		}
		delete(l.keys, old.key)
	}
	for _, e := range kept {
		heap.Push(&l.h, e)
	}
}

// record counts one failure on the entry k, creating it if the table has room, and blocks it once it
// is past freeFails, for at most maxBackoff.
func (l *Limiter) record(k string, freeFails int, maxBackoff time.Duration, now time.Time) {
	a := l.keys[k]
	if a == nil {
		if len(l.keys) >= maxEntries {
			return
		}
		a = &entry{key: k, first: now}
		l.keys[k] = a
		heap.Push(&l.h, a)
	}
	if now.After(a.blocked) && now.Sub(a.quietSince()) > FailWindow {
		// The refusals not yet taken stay counted when the failures start afresh.
		a.fails, a.first, a.blocked = 0, now, time.Time{}
	}
	a.fails++
	if over := a.fails - freeFails; over >= 0 {
		d := BaseBackoff << min(over, 20)
		a.blocked = now.Add(min(d, maxBackoff))
	}
	heap.Fix(&l.h, a.idx)
}

// Refuse counts one request refused for key without an event of its own. It does nothing for a key
// the limiter does not track.
func (l *Limiter) Refuse(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if a, ok := l.keys[key]; ok {
		a.refused++
	}
}

// TakeRefused returns the requests counted with Refuse since the last call, and resets the count.
func (l *Limiter) TakeRefused(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	a, ok := l.keys[key]
	if !ok {
		return 0
	}
	n := a.refused
	a.refused = 0
	return n
}

// Succeed forgets key (not its address or group).
func (l *Limiter) Succeed(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if a, ok := l.keys[key]; ok {
		heap.Remove(&l.h, a.idx)
		delete(l.keys, key)
	}
}
