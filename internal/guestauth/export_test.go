// SPDX-License-Identifier: Apache-2.0

package guestauth

import "time"

// SetSignerClock sets the clock of a Signer.
func SetSignerClock(s *Signer, now func() time.Time) { s.now = now }

// SetVerifierClock sets the clock of a Verifier.
func SetVerifierClock(v *Verifier, now func() time.Time) { v.now = now }

// ReplayEntries returns the number of jti entries a Verifier keeps.
func ReplayEntries(v *Verifier) int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.seen)
}
