// SPDX-License-Identifier: Apache-2.0

package runoutcome

import "time"

// SetDiagnosticTryTimeout sets the deadline of one diagnostic write try and returns a func that
// restores it.
func SetDiagnosticTryTimeout(d time.Duration) (restore func()) {
	old := diagnosticTryTimeout
	diagnosticTryTimeout = d
	return func() { diagnosticTryTimeout = old }
}
