// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"context"
	"strings"
	"time"

	sdk "github.com/coder/acp-go-sdk"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
)

const markChunky = "CHUNKY"

var _ = Describe("Items, not chunks (old issue 344)", func() {
	It("stores a number of run events proportional to the items, not to the ACP chunks", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		// 3n+1 = 1201 chunks exceed the SDK's 1024-notification queue; acp.NewConn holds the
		// harness back while the handler lags, so the burst must not overflow it.
		const n = 400
		e.harness.set(func(h *harness) {
			h.script = func(ctx context.Context, text string, say func(sdk.SessionUpdate) error) bool {
				defer GinkgoRecover()
				if !strings.Contains(text, markChunky) {
					return false
				}
				for range n {
					Expect(say(sdk.UpdateAgentThoughtText("th "))).To(Succeed())
				}
				for range n {
					Expect(say(sdk.UpdateAgentMessageText("word "))).To(Succeed())
				}
				Expect(say(sdk.StartToolCall("chunky-1", "read", sdk.WithStartKind(sdk.ToolKindRead)))).To(Succeed())
				for range n {
					Expect(say(sdk.UpdateAgentMessageText("more "))).To(Succeed())
				}
				return true
			}
		})
		t := e.task(ctx, markChunky)
		r := e.enqueue(ctx, t)
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(15 * time.Second).Should(Equal(store.RunStateSucceeded))

		evs := events(ctx, r)
		expectOrdered(evs)
		Expect(kinds(evs)).To(Equal([]string{
			"run.session", "run.prompt",
			"acp.agent_thought_chunk", "acp.agent_message_chunk", "acp.tool_call", "acp.agent_message_chunk",
			"run.summary", "run.succeeded",
		}), "3 items and a tool call for %d chunks", 3*n+1)
		Expect(chunks(evs)).To(Equal([]string{strings.Repeat("word ", n), strings.Repeat("more ", n)}))
		for _, ev := range evs {
			Expect(payload(ev)).NotTo(HaveKey("partial"), "an item the prompt ended is complete (%s)", ev.Kind)
		}
		// The task's reply is the last message, whole.
		Expect(string(taskEvents(ctx, e, t)[1].Payload)).To(ContainSubstring(strings.TrimSpace(strings.Repeat("more ", n))))
	})
})
