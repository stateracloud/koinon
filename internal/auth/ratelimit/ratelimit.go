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
// The state lives in memory, per replica. The address is the connection's peer: behind an ingress
// that is the ingress, so one noisy client slows its neighbours down (set a NetworkPolicy or an
// ingress rule for finer control).
package ratelimit

import (
	"net"
	"net/http"
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

	// maxTracked bounds the keys kept; past it, keys idle for forgetAfter are dropped.
	maxTracked  = 10000
	forgetAfter = time.Hour
)

// Limiter counts failures per key. The zero value is not usable; call New.
type Limiter struct {
	mu   sync.Mutex
	keys map[string]*attempts
}

type attempts struct {
	fails   int
	first   time.Time
	blocked time.Time
	// refused counts the requests refused while the key was over the limit, and failures that were
	// not recorded one by one, since the caller last took the count (TakeRefused).
	refused int
}

// New returns an empty Limiter.
func New() *Limiter { return &Limiter{keys: map[string]*attempts{}} }

// Key is the slot of a client address and the credential it names (a token prefix, a username);
// part "" is the address's shared slot.
func Key(addr, part string) string { return addr + "|" + part }

// ClientAddr is the request's peer address without the port.
func ClientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
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
	if a, ok := l.keys[key]; ok && now.Before(a.blocked) {
		return a.blocked.Sub(now)
	}
	return 0
}

// Fail records a failure for key.
func (l *Limiter) Fail(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.keys) >= maxTracked {
		for k, a := range l.keys {
			if now.Sub(a.first) > forgetAfter && now.After(a.blocked) {
				delete(l.keys, k)
			}
		}
	}
	a, ok := l.keys[key]
	if !ok || now.After(a.blocked) && now.Sub(a.quietSince()) > FailWindow {
		// The refusals not yet taken stay counted when the failures start afresh.
		fresh := &attempts{first: now}
		if ok {
			fresh.refused = a.refused
		}
		a = fresh
		l.keys[key] = a
	}
	a.fails++
	if over := a.fails - FreeFails; over >= 0 {
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
	delete(l.keys, key)
	l.mu.Unlock()
}
