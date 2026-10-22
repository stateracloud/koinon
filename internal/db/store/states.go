// SPDX-License-Identifier: Apache-2.0

package store

// Written by hand, next to the generated enums: the state predicates every package shares.

// Finished reports whether a run is in a final state: succeeded, failed or canceled.
func (s RunState) Finished() bool {
	return s == RunStateSucceeded || s == RunStateFailed || s == RunStateCanceled
}

// Open reports whether an approval request still keeps its run parked: it is pending, or
// approved and not executed yet.
func (s ApprovalStatus) Open() bool {
	return s == ApprovalStatusPending || s == ApprovalStatusApproved
}
