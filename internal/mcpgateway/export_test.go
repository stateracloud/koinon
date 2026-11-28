// SPDX-License-Identifier: Apache-2.0

package mcpgateway

import (
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"
)

// SetScrubClock sets the clock of s, for specs of Retain.
func SetScrubClock(s *CredentialScrub, now func() time.Time) { s.now = now }

// ScrubRebuilds is how often s built the matcher of all orgs.
func ScrubRebuilds(s *CredentialScrub) int64 { return s.allBuilds.Load() }

// ScrubTouch and ScrubEvict are the fast-path open and the locked removal of one value time.
func ScrubTouch(t *atomic.Int64, now int64) bool { return touch(t, now) }

func ScrubEvict(t *atomic.Int64, at int64) bool { return evict(t, at) }

// UseScrubMetrics sets the counters and the logger of s, as New does.
func UseScrubMetrics(s *CredentialScrub, m *Metrics, log *slog.Logger) { s.use(m, log) }

// AddFor records values for org by value, as an opener of a by-value kind does. Specs of the value set use it.
func (s *CredentialScrub) AddFor(org string, values ...string) { s.addFor(org, values...) }

// Add records values for no org by value.
func (s *CredentialScrub) Add(values ...string) { s.addFor("", values...) }

// LimitTransport wraps base with the bound on an upstream reply, as connect does.
func LimitTransport(base http.RoundTripper, max int64, onLimit func()) http.RoundTripper {
	return &limitTransport{base: base, max: max, onLimit: onLimit}
}
