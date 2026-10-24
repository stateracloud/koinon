// SPDX-License-Identifier: Apache-2.0

package adminapi

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// limiter slows down clients that keep presenting bad tokens: after freeFails failures within
// window, a client IP is refused for base, then twice as long for each further failure, up to max.
// A good token resets it. It keeps state in memory per replica; the IP is the connection's peer
// (behind an ingress that is the ingress, so one noisy client slows its neighbours: set a
// NetworkPolicy or ingress rule for finer control).
type limiter struct {
	mu      sync.Mutex
	clients map[string]*attempts
}

type attempts struct {
	fails   int
	first   time.Time
	blocked time.Time
}

const (
	freeFails    = 5
	failWindow   = time.Minute
	baseBackoff  = time.Second
	maxBackoff   = 15 * time.Minute
	maxTracked   = 10000
	trackedForgt = time.Hour
)

func newLimiter() *limiter { return &limiter{clients: map[string]*attempts{}} }

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// blocked returns how long the client must wait, or zero.
func (l *limiter) blocked(ip string, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if a, ok := l.clients[ip]; ok && now.Before(a.blocked) {
		return a.blocked.Sub(now)
	}
	return 0
}

func (l *limiter) fail(ip string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.clients) >= maxTracked {
		for k, a := range l.clients {
			if now.Sub(a.first) > trackedForgt && now.After(a.blocked) {
				delete(l.clients, k)
			}
		}
	}
	a, ok := l.clients[ip]
	if !ok || now.Sub(a.first) > failWindow && now.After(a.blocked) {
		a = &attempts{first: now}
		l.clients[ip] = a
	}
	a.fails++
	if over := a.fails - freeFails; over >= 0 {
		d := baseBackoff << min(over, 20)
		a.blocked = now.Add(min(d, maxBackoff))
	}
}

func (l *limiter) succeed(ip string) {
	l.mu.Lock()
	delete(l.clients, ip)
	l.mu.Unlock()
}
