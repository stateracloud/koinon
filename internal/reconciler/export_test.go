// SPDX-License-Identifier: Apache-2.0

package reconciler

import "context"

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

// ProbeTickForTest runs one interval probe now.
func (r *Reconciler) ProbeTickForTest(ctx context.Context) { r.probeTick(ctx) }

// EndLeadForTest runs what the end of a lead does to the crash path state (lead needs a lock connection).
func (r *Reconciler) EndLeadForTest() { r.endLeadFleet() }
