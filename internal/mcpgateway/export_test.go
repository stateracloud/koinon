// SPDX-License-Identifier: Apache-2.0

package mcpgateway

import "time"

// SetScrubClock sets the clock of s, for specs of Retain.
func SetScrubClock(s *CredentialScrub, now func() time.Time) { s.now = now }
