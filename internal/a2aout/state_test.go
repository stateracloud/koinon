// SPDX-License-Identifier: Apache-2.0

package a2aout_test

import (
	"errors"

	"github.com/a2aproject/a2a-go/v2/a2a"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/a2aedge"
	"github.com/stateracloud/koinon/internal/a2aout"
	"github.com/stateracloud/koinon/internal/db/store"
)

var _ = Describe("State", func() {
	DescribeTable("maps an external task state to a Koinon state",
		func(in a2a.TaskState, want store.TaskState, reason a2aout.Reason) {
			got, r, err := a2aout.State(in)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(want))
			Expect(r).To(Equal(reason))
		},
		Entry("submitted", a2a.TaskStateSubmitted, store.TaskStateSubmitted, a2aout.ReasonNone),
		Entry("working", a2a.TaskStateWorking, store.TaskStateWorking, a2aout.ReasonNone),
		Entry("input-required", a2a.TaskStateInputRequired, store.TaskStateInputRequired, a2aout.ReasonNone),
		Entry("auth-required waits for a person", a2a.TaskStateAuthRequired, store.TaskStateInputRequired, a2aout.ReasonAuthRequired),
		Entry("completed", a2a.TaskStateCompleted, store.TaskStateCompleted, a2aout.ReasonNone),
		Entry("canceled", a2a.TaskStateCanceled, store.TaskStateCanceled, a2aout.ReasonNone),
		Entry("failed", a2a.TaskStateFailed, store.TaskStateFailed, a2aout.ReasonNone),
		Entry("rejected is failed with a reason", a2a.TaskStateRejected, store.TaskStateFailed, a2aout.ReasonRejected),
	)

	It("refuses a state it does not know, and the empty state", func() {
		for _, s := range []a2a.TaskState{"", "TASK_STATE_SLEEPING"} {
			_, _, err := a2aout.State(s)
			Expect(errors.Is(err, a2aout.ErrUnknownState)).To(BeTrue(), string(s))
		}
	})

	It("is the inverse of the inbound mapping for every state the edge sends", func() {
		for _, s := range []store.TaskState{
			store.TaskStateSubmitted, store.TaskStateWorking, store.TaskStateInputRequired,
			store.TaskStateCompleted, store.TaskStateCanceled, store.TaskStateFailed,
		} {
			back, r, err := a2aout.State(a2aedge.State(s))
			Expect(err).NotTo(HaveOccurred())
			Expect(back).To(Equal(s))
			Expect(r).To(Equal(a2aout.ReasonNone))
		}
	})

	It("treats completed, failed, canceled and rejected as ended", func() {
		for _, s := range []a2a.TaskState{a2a.TaskStateCompleted, a2a.TaskStateFailed, a2a.TaskStateCanceled, a2a.TaskStateRejected} {
			Expect(a2aout.Ended(s)).To(BeTrue(), string(s))
		}
		for _, s := range []a2a.TaskState{a2a.TaskStateSubmitted, a2a.TaskStateWorking, a2a.TaskStateInputRequired, a2a.TaskStateAuthRequired} {
			Expect(a2aout.Ended(s)).To(BeFalse(), string(s))
		}
	})
})
