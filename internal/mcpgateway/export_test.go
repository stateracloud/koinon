// SPDX-License-Identifier: Apache-2.0

package mcpgateway

import (
	"log/slog"
	"time"
)

// SetScrubClock sets the clock of s, for specs of Retain.
func SetScrubClock(s *CredentialScrub, now func() time.Time) { s.now = now }

// UseScrubMetrics sets the counters and the logger of s, as New does.
func UseScrubMetrics(s *CredentialScrub, m *Metrics, log *slog.Logger) { s.use(m, log) }
