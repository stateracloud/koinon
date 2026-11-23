// SPDX-License-Identifier: Apache-2.0

package mcpgateway

import (
	"log/slog"
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
