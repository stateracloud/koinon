// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
)

var _ = Describe("state predicates", func() {
	It("finishes a run only in a final state", func() {
		var finished []store.RunState
		for _, s := range allStates {
			if s.Finished() {
				finished = append(finished, s)
			}
		}
		Expect(finished).To(ConsistOf(store.RunStateSucceeded, store.RunStateFailed, store.RunStateCanceled))
	})

	It("keeps an approval open while pending or approved", func() {
		var open []store.ApprovalStatus
		for _, s := range []store.ApprovalStatus{
			store.ApprovalStatusPending, store.ApprovalStatusApproved, store.ApprovalStatusDenied, store.ApprovalStatusExecuted,
			store.ApprovalStatusFailed, store.ApprovalStatusExpired, store.ApprovalStatusCanceled,
		} {
			if s.Open() {
				open = append(open, s)
			}
		}
		Expect(open).To(ConsistOf(store.ApprovalStatusPending, store.ApprovalStatusApproved))
	})
})
