// SPDX-License-Identifier: Apache-2.0

package reconciler

// Test hooks for the external suite.
var (
	LockBlocking = lockBlocking
	RetryDelay   = retryDelay
	NextFailures = nextFailures
)

const (
	RetryBase = retryBase
	RetryCap  = retryCap
)
