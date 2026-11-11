// SPDX-License-Identifier: Apache-2.0

package loopguard_test

import (
	"fmt"

	sdk "github.com/coder/acp-go-sdk"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/agentd/loopguard"
)

var _ = Describe("the loop guard", func() {
	var d *loopguard.Detector
	var seq int

	BeforeEach(func() {
		d = loopguard.New(loopguard.DefaultThresholds)
		seq = 0
	})

	feed := func(u sdk.SessionUpdate) { d.Observe(sdk.SessionNotification{SessionId: "s", Update: u}) }
	id := func() sdk.ToolCallId { seq++; return sdk.ToolCallId(fmt.Sprintf("t%d", seq)) }

	// start announces a call; finish ends it with a status and an output.
	start := func(tid sdk.ToolCallId, cmd string) {
		feed(sdk.StartToolCall(tid, "bash", sdk.WithStartKind(sdk.ToolKindExecute), sdk.WithStartRawInput(map[string]any{"cmd": cmd})))
	}
	finish := func(tid sdk.ToolCallId, st sdk.ToolCallStatus, out string) {
		feed(sdk.UpdateToolCall(tid, sdk.WithUpdateStatus(st), sdk.WithUpdateRawOutput(out)))
	}
	call := func(cmd string, st sdk.ToolCallStatus, out string) {
		tid := id()
		start(tid, cmd)
		finish(tid, st, out)
	}
	detected := func() loopguard.Pattern {
		select {
		case p := <-d.Signal():
			return p
		default:
			return ""
		}
	}

	It("finds the same action with the same result after four steps", func() {
		for range 3 {
			call("ls", sdk.ToolCallStatusCompleted, "a b")
		}
		Expect(detected()).To(BeEmpty())
		call("ls", sdk.ToolCallStatusCompleted, "a b")
		Expect(detected()).To(Equal(loopguard.SameAction))
	})

	It("does not call the same action a loop when the result changes", func() {
		for i := range 8 {
			call("tail log", sdk.ToolCallStatusCompleted, fmt.Sprintf("line %d", i))
		}
		Expect(detected()).To(BeEmpty())
	})

	It("finds the same failing action after three steps, whatever the error text", func() {
		call("make", sdk.ToolCallStatusFailed, "error 1")
		call("make", sdk.ToolCallStatusFailed, "error 2")
		Expect(detected()).To(BeEmpty())
		call("make", sdk.ToolCallStatusFailed, "error 3")
		Expect(detected()).To(Equal(loopguard.SameError))
	})

	It("does not mix a failure with a success", func() {
		call("make", sdk.ToolCallStatusFailed, "boom")
		call("make", sdk.ToolCallStatusCompleted, "ok")
		call("make", sdk.ToolCallStatusFailed, "boom")
		Expect(detected()).To(BeEmpty())
	})

	It("finds messages with no tool call between them after three", func() {
		feed(sdk.UpdateAgentMessageText("I will look"))
		feed(sdk.UpdateAgentThoughtText("hm"))
		feed(sdk.UpdateAgentMessageText("I will look again"))
		Expect(detected()).To(BeEmpty())
		feed(sdk.UpdateAgentThoughtText("hm"))
		feed(sdk.UpdateAgentMessageText("Still looking"))
		Expect(detected()).To(Equal(loopguard.Monologue))
	})

	It("counts the chunks of one message once", func() {
		for range 20 {
			feed(sdk.UpdateAgentMessageText("word "))
		}
		Expect(detected()).To(BeEmpty())
	})

	It("ends a monologue with a tool call", func() {
		feed(sdk.UpdateAgentMessageText("one"))
		feed(sdk.UpdateAgentThoughtText("t"))
		feed(sdk.UpdateAgentMessageText("two"))
		call("ls", sdk.ToolCallStatusCompleted, "x")
		feed(sdk.UpdateAgentThoughtText("t"))
		feed(sdk.UpdateAgentMessageText("three"))
		Expect(detected()).To(BeEmpty())
	})

	It("finds two actions taking turns after six steps", func() {
		for i := range 6 {
			if i%2 == 0 {
				call("cat a", sdk.ToolCallStatusCompleted, "A")
			} else {
				call("cat b", sdk.ToolCallStatusCompleted, "B")
			}
			if i < 5 {
				Expect(detected()).To(BeEmpty(), "step %d", i)
			}
		}
		Expect(detected()).To(Equal(loopguard.Alternating))
	})

	It("counts parallel calls as one step", func() {
		// Three calls announced together, then finished, are one step: nine calls are three steps.
		round := func() {
			a, b, c := id(), id(), id()
			start(a, "ls")
			start(b, "ls")
			start(c, "ls")
			finish(c, sdk.ToolCallStatusCompleted, "x")
			finish(a, sdk.ToolCallStatusCompleted, "x")
			finish(b, sdk.ToolCallStatusCompleted, "x")
		}
		for range 3 {
			round()
		}
		Expect(detected()).To(BeEmpty())
		round()
		Expect(detected()).To(Equal(loopguard.SameAction))
	})

	It("starts counting again after a reset (a new user message)", func() {
		for range 3 {
			call("ls", sdk.ToolCallStatusCompleted, "a b")
		}
		d.Reset()
		call("ls", sdk.ToolCallStatusCompleted, "a b")
		Expect(detected()).To(BeEmpty())
		for range 3 {
			call("ls", sdk.ToolCallStatusCompleted, "a b")
		}
		Expect(detected()).To(Equal(loopguard.SameAction))
	})

	It("reports a stretch once and drops a signal on reset", func() {
		for range 4 {
			call("ls", sdk.ToolCallStatusCompleted, "a b")
		}
		d.Reset()
		Expect(detected()).To(BeEmpty())
		for range 4 {
			call("ls", sdk.ToolCallStatusCompleted, "a b")
		}
		Expect(detected()).To(Equal(loopguard.SameAction))
		call("ls", sdk.ToolCallStatusCompleted, "a b")
		Expect(detected()).To(BeEmpty(), "the stretch was reported and forgotten")
	})

	It("turns a pattern off with a threshold of zero", func() {
		d = loopguard.New(loopguard.Thresholds{})
		for range 10 {
			call("ls", sdk.ToolCallStatusCompleted, "a b")
		}
		Expect(detected()).To(BeEmpty())
	})
})
