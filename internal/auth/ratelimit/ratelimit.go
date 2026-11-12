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
// The state lives in memory, per replica, in at most maxTracked keys (the least recently used key is
// dropped first). The address is the connection's peer, an IPv6 one masked to its /64;
// X-Forwarded-For is not trusted.
//
// Lockout risk behind an ingress: every client then shares the ingress's address, so one noisy client
// slows its neighbours down, and anyone who knows a username or a token prefix can lock that
// credential for the others by failing against it (set a NetworkPolicy or an ingress rule for finer
// control). The keys hold a hash of the credential name, not the name. The audit event of a failed
// password login stores the attempted username as typed (cut to a bounded length), so a user who types
// a password into the username field leaves it in the audit log.
package ratelimit

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"time"
)

// The limits. They are the same for every entry point.
const (
	FreeFails   = 5
	FailWindow  = time.Minute
	BaseBackoff = time.Second
	MaxBackoff  = 15 * time.Minute

	// maxTracked is the hard bound on the keys kept; past it the least recently used key is dropped.
	maxTracked = 10000
)

// Limiter counts failures per key. The zero value is not usable; call New.
type Limiter struct {
	mu   sync.Mutex
	keys map[string]*list.Element // value: *attempts
	lru  *list.List               // front: most recently used
}

type attempts struct {
	key     string
	fails   int
	first   time.Time
	blocked time.Time
	// refused counts the requests refused while the key was over the limit, and failures that were
	// not recorded one by one, since the caller last took the count (TakeRefused).
	refused int
}

// New returns an empty Limiter.
func New() *Limiter { return &Limiter{keys: map[string]*list.Element{}, lru: list.New()} }

// Len is the number of keys tracked.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.keys)
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
// IPv4 address stays whole. X-Forwarded-For is not read.
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
	if e, ok := l.keys[key]; ok {
		if a := e.Value.(*attempts); now.Before(a.blocked) {
			return a.blocked.Sub(now)
		}
	}
	return 0
}

// Fail records a failure for key; the key is blocked after FreeFails failures.
func (l *Limiter) Fail(key string, now time.Time) { l.FailAfter(key, FreeFails, now) }

// FailAfter records a failure for key, which is blocked after freeFails failures within FailWindow.
func (l *Limiter) FailAfter(key string, freeFails int, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var a *attempts
	if e, ok := l.keys[key]; ok {
		l.lru.MoveToFront(e)
		a = e.Value.(*attempts)
	}
	if a == nil || now.After(a.blocked) && now.Sub(a.quietSince()) > FailWindow {
		// The refusals not yet taken stay counted when the failures start afresh.
		fresh := &attempts{key: key, first: now}
		if a != nil {
			fresh.refused = a.refused
			*a = *fresh
		} else {
			if len(l.keys) >= maxTracked {
				oldest := l.lru.Back()
				l.lru.Remove(oldest)
				delete(l.keys, oldest.Value.(*attempts).key)
			}
			a = fresh
			l.keys[key] = l.lru.PushFront(a)
		}
	}
	a.fails++
	if over := a.fails - freeFails; over >= 0 {
		d := BaseBackoff << min(over, 20)
		a.blocked = now.Add(min(d, MaxBackoff))
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
	if e, ok := l.keys[key]; ok {
		e.Value.(*attempts).refused++
	}
}

// TakeRefused returns the requests counted with Refuse since the last call, and resets the count.
func (l *Limiter) TakeRefused(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.keys[key]
	if !ok {
		return 0
	}
	a := e.Value.(*attempts)
	n := a.refused
	a.refused = 0
	return n
}

// Succeed forgets key.
func (l *Limiter) Succeed(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.keys[key]; ok {
		l.lru.Remove(e)
		delete(l.keys, key)
	}
}
