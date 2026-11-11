// SPDX-License-Identifier: Apache-2.0

package orcatalog

// SetBeforeCommit sets the hook that runs at the end of a store's transaction and returns a func that
// clears it.
func SetBeforeCommit(f func()) (restore func()) {
	beforeCommit = f
	return func() { beforeCommit = nil }
}

// SetAfterLimitRead sets the hook that runs in a store between its read of an accepted limit and its
// lookup of the proposals, and returns a func that clears it.
func SetAfterLimitRead(f func()) (restore func()) {
	afterLimitRead = f
	return func() { afterLimitRead = nil }
}
