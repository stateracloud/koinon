// SPDX-License-Identifier: Apache-2.0

package reconciler

// Test hooks for the external suite.
var (
	LockBlocking = lockBlocking
	RetryDelay   = retryDelay
)

const (
	RetryBase = retryBase
	RetryCap  = retryCap
)
