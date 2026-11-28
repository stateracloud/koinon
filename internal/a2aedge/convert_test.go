// SPDX-License-Identifier: Apache-2.0

package a2aedge_test

import (
	"github.com/a2aproject/a2a-go/v2/a2a"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/a2aedge"
	"github.com/stateracloud/koinon/internal/db/store"
)

var _ = Describe("History", func() {
	DescribeTable("maps a timeline message to an A2A message by its actor",
		func(actor store.TaskActorKind, want a2a.MessageRole) {
			e := store.TaskEvent{Seq: 3, Kind: "message", ActorKind: actor, Payload: []byte(`{"text":"hello"}`)}
			m := a2aedge.HistoryMessage("thread", "task", e)
			if want == "" {
				Expect(m).To(BeNil())
				return
			}
			Expect(m).NotTo(BeNil())
			Expect(m.Role).To(Equal(want))
			Expect(m.Parts[0].Text()).To(Equal("hello"))
		},
		Entry("a Koinon user", store.TaskActorKindUser, a2a.MessageRoleUser),
		Entry("a person without a Koinon account (ADR 0130)", store.TaskActorKindExternal, a2a.MessageRoleUser),
		Entry("the agent", store.TaskActorKindAgent, a2a.MessageRoleAgent),
		Entry("the system: not a conversation entry", store.TaskActorKindSystem, a2a.MessageRole("")),
	)

	It("opens a turn with the first message of a person without a Koinon account (ADR 0130)", func() {
		msg := func(seq int64, actor store.TaskActorKind) store.TaskEvent {
			return store.TaskEvent{Seq: seq, Kind: "message", ActorKind: actor, Payload: []byte(`{"text":"hello"}`)}
		}
		events := []store.TaskEvent{msg(1, store.TaskActorKindExternal), msg(2, store.TaskActorKindAgent), msg(3, store.TaskActorKindExternal)}
		Expect(a2aedge.Turns(events)).To(Equal(1), "a later message answers the open turn")
		Expect(a2aedge.TurnStarts(events)).To(Equal([]int{0}))
	})
})
