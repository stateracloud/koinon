// SPDX-License-Identifier: Apache-2.0

package a2aedge_test

import (
	"github.com/a2aproject/a2a-go/v2/a2a"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/a2aedge"
	"github.com/stateracloud/koinon/internal/db/store"
)

var _ = Describe("State mapping", func() {
	DescribeTable("maps a Koinon task state to an A2A state",
		func(in store.TaskState, want a2a.TaskState) {
			Expect(a2aedge.State(in)).To(Equal(want))
		},
		Entry("submitted", store.TaskStateSubmitted, a2a.TaskStateSubmitted),
		Entry("working", store.TaskStateWorking, a2a.TaskStateWorking),
		Entry("input_required", store.TaskStateInputRequired, a2a.TaskStateInputRequired),
		Entry("completed", store.TaskStateCompleted, a2a.TaskStateCompleted),
		Entry("canceled", store.TaskStateCanceled, a2a.TaskStateCanceled),
		Entry("failed", store.TaskStateFailed, a2a.TaskStateFailed),
		Entry("an unknown state", store.TaskState("nope"), a2a.TaskStateUnspecified),
	)

	It("marks end states, and input_required as a state the client acts on", func() {
		for _, s := range []store.TaskState{store.TaskStateCompleted, store.TaskStateCanceled, store.TaskStateFailed} {
			Expect(a2aedge.Ended(s)).To(BeTrue(), string(s))
			Expect(a2aedge.Settled(s)).To(BeTrue(), string(s))
		}
		Expect(a2aedge.Ended(store.TaskStateInputRequired)).To(BeFalse())
		Expect(a2aedge.Settled(store.TaskStateInputRequired)).To(BeTrue())
		for _, s := range []store.TaskState{store.TaskStateSubmitted, store.TaskStateWorking} {
			Expect(a2aedge.Settled(s)).To(BeFalse(), string(s))
		}
	})
})
