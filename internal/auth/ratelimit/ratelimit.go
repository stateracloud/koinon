// SPDX-License-Identifier: Apache-2.0

// Package ratelimit slows down clients that keep failing to authenticate. It is the one limiter of
// Koinon's credential entry points: the admin API's bearer tokens (internal/adminapi) and the
// password login (internal/auth).
//
// A key is a client address and the credential it names (Key). After FreeFails failures within
// FailWindow a key is refused for BaseBackoff, then twice as long for each further failure, up to
// MaxBackoff. A key that was refused starts afresh only after a quiet FailWindow that follows the end
// of its block, so a client that waits out each block keeps doubling it. A success resets the key.
//
// The state lives in memory, per replica. At most maxTracked keys that are not blocked are kept (the
// least recently used is dropped first), and at most maxBlocked blocked ones. A blocked key is not
// dropped to make room for a fresh key, so flooding the limiter with fresh keys does not reset a
// backoff. One address (an IPv6 one counts per /48 here) may hold at most maxBlockedPerAddr blocked
// keys; when it would hold more, the address's own shared slot (Key(addr, "")) and wide slot
// (Key(addr, WidePart)) are blocked for the wait of the key that overflowed, and that key is not
// tracked. Only that address is refused. When the blocked set is still full, the block that ends
// soonest is dropped to make room, and nobody else is refused. The address is the connection's peer,
// an IPv6 one masked to its /64; X-Forwarded-For is not trusted.
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
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"net/netip"
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

	// maxTracked is the bound on the keys kept that are not blocked; past it the least recently used
	// key is dropped. maxBlocked bounds the blocked keys; past it the block that ends soonest is dropped.
	maxTracked = 10000
	maxBlocked = 100000

	// maxBlockedPerAddr bounds the blocked keys one address (an IPv6 one: its /48) may hold.
	maxBlockedPerAddr = 8

	// WidePart is the part of an address's wide slot, which the password login checks over all
	// usernames; the limiter blocks it with the shared slot when an address overflows its cap.
	WidePart = "all-users"
)

// Limiter counts failures per key. The zero value is not usable; call New.
//
// Keys that are blocked live in a heap ordered by the end of their block, apart from the keys that are
// not blocked (a recency list). Only the recency list evicts, so a flood of fresh keys cannot reset the
// backoff of a blocked one. A block that has ended moves its key back to the recency list.
type Limiter struct {
	mu      sync.Mutex
	keys    map[string]*attempts
	lru     *list.List     // unblocked keys; front: most recently used
	blocked blockHeap      // blocked keys, earliest block end first
	perAddr map[string]int // non-exempt blocked keys per group
}

type attempts struct {
	key     string
	fails   int
	first   time.Time
	blocked time.Time
	// refused counts the requests refused while the key was over the limit, and failures that were
	// not recorded one by one, since the caller last took the count (TakeRefused).
	refused int
	elem    *list.Element // in lru, or nil
	hidx    int           // index in the blocked heap, or -1
	group   string        // the address, or an IPv6 address's /48, for the per-address cap
	exempt  bool          // an address-level slot: blocked past the cap, not counted in it
}

type blockHeap []*attempts

func (h blockHeap) Len() int           { return len(h) }
func (h blockHeap) Less(i, j int) bool { return h[i].blocked.Before(h[j].blocked) }
func (h blockHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].hidx, h[j].hidx = i, j
}

func (h *blockHeap) Push(x any) {
	a := x.(*attempts)
	a.hidx = len(*h)
	*h = append(*h, a)
}

func (h *blockHeap) Pop() any {
	old := *h
	a := old[len(old)-1]
	old[len(old)-1] = nil
	*h = old[:len(old)-1]
	a.hidx = -1
	return a
}

// New returns an empty Limiter.
func New() *Limiter {
	return &Limiter{keys: map[string]*attempts{}, lru: list.New(), perAddr: map[string]int{}}
}

// Len is the number of keys tracked.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.keys)
}

// expire moves the keys whose block has ended from the blocked heap to the recency list.
func (l *Limiter) expire(now time.Time) {
	for len(l.blocked) > 0 && !now.Before(l.blocked[0].blocked) {
		a := l.dropBlocked(l.blocked[0])
		a.elem = l.lru.PushFront(a)
	}
	for l.lru.Len() > maxTracked {
		l.forget(l.lru.Back().Value.(*attempts))
	}
}

// groupOf is the address part of a key (everything before the first "|"), an IPv6 address widened
// to its /48.
func groupOf(key string) string {
	addr, _, _ := strings.Cut(key, "|")
	if ip, err := netip.ParseAddr(addr); err == nil && ip.Is6() {
		if p, err := ip.Prefix(48); err == nil {
			return p.String()
		}
	}
	return addr
}

// dropBlocked takes a from the blocked heap, keeps it in the key map, and returns it.
func (l *Limiter) dropBlocked(a *attempts) *attempts {
	heap.Remove(&l.blocked, a.hidx)
	if !a.exempt {
		if l.perAddr[a.group]--; l.perAddr[a.group] <= 0 {
			delete(l.perAddr, a.group)
		}
	}
	return a
}

// forget removes a from every structure.
func (l *Limiter) forget(a *attempts) {
	if a.elem != nil {
		l.lru.Remove(a.elem)
		a.elem = nil
	} else if a.hidx >= 0 {
		l.dropBlocked(a)
	}
	delete(l.keys, a.key)
}

// addBlocked moves a to the blocked heap, first dropping the block that ends soonest when the heap is
// full.
func (l *Limiter) addBlocked(a *attempts) {
	if len(l.blocked) >= maxBlocked {
		l.forget(l.blocked[0])
	}
	if a.elem != nil {
		l.lru.Remove(a.elem)
		a.elem = nil
	}
	heap.Push(&l.blocked, a)
	if !a.exempt {
		l.perAddr[a.group]++
	}
}

// blockAddress blocks the address's shared and wide slots until at least until.
func (l *Limiter) blockAddress(key string, until, now time.Time) {
	addr, _, _ := strings.Cut(key, "|")
	for _, k := range []string{Key(addr, ""), Key(addr, WidePart)} {
		a := l.keys[k]
		if a == nil {
			a = &attempts{key: k, first: now, group: groupOf(k), exempt: true, hidx: -1}
			l.keys[k] = a
		}
		if a.blocked.Before(until) {
			a.blocked = until
		}
		if a.hidx >= 0 {
			heap.Fix(&l.blocked, a.hidx)
			continue
		}
		a.exempt = true
		l.addBlocked(a)
	}
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

// Blocked returns how long key must wait, or zero.
func (l *Limiter) Blocked(key string, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.expire(now)
	if a, ok := l.keys[key]; ok && now.Before(a.blocked) {
		return a.blocked.Sub(now)
	}
	return 0
}

// Fail records a failure for key; the key is blocked after FreeFails failures.
func (l *Limiter) Fail(key string, now time.Time) { l.FailAfter(key, FreeFails, now) }

// FailAfter records a failure for key, which is blocked after freeFails failures within FailWindow.
func (l *Limiter) FailAfter(key string, freeFails int, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.expire(now)
	a := l.keys[key]
	if a == nil {
		if l.lru.Len() >= maxTracked {
			l.forget(l.lru.Back().Value.(*attempts))
		}
		a = &attempts{key: key, first: now, hidx: -1, group: groupOf(key)}
		a.elem = l.lru.PushFront(a)
		l.keys[key] = a
	} else if a.elem != nil {
		l.lru.MoveToFront(a.elem)
	}
	if now.After(a.blocked) && now.Sub(a.quietSince()) > FailWindow {
		// The refusals not yet taken stay counted when the failures start afresh.
		a.fails, a.first, a.blocked = 0, now, time.Time{}
	}
	a.fails++
	if over := a.fails - freeFails; over >= 0 {
		d := BaseBackoff << min(over, 20)
		a.blocked = now.Add(min(d, MaxBackoff))
	}
	if !now.Before(a.blocked) {
		return
	}
	switch {
	case a.hidx >= 0:
		heap.Fix(&l.blocked, a.hidx)
	case !a.exempt && l.perAddr[a.group] >= maxBlockedPerAddr:
		// The address holds as many blocked keys as it may: block the address itself, not this key.
		until := a.blocked
		l.forget(a)
		l.blockAddress(key, until, now)
	default:
		l.addBlocked(a)
	}
}

// quietSince is when the key's quiet window starts: its first failure, or the end of its block.
func (a *attempts) quietSince() time.Time {
	if a.blocked.After(a.first) {
		return a.blocked
	}
	return a.first
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

// Succeed forgets key.
func (l *Limiter) Succeed(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if a, ok := l.keys[key]; ok {
		l.forget(a)
	}
}
