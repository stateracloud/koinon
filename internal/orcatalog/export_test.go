// SPDX-License-Identifier: Apache-2.0

package orcatalog

// SetBeforeCommit sets the hook that runs at the end of a store's transaction and returns a func that
// clears it.
func SetBeforeCommit(f func()) (restore func()) {
	beforeCommit = f
	return func() { beforeCommit = nil }
}
