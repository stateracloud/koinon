// SPDX-License-Identifier: Apache-2.0

package otlpauth

import "time"

// SetClock replaces the clock that decides when the token files are re-read, so tests reload
// them by moving the clock instead of sleeping.
func SetClock(t *Tokens, now func() time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.now = now
}
