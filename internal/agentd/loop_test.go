// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	sdk "github.com/coder/acp-go-sdk"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/stateracloud/koinon/internal/agentd"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/testing/racescale"
)

// A looping agent is told once and then stopped with failure class loop (ADR 0030 amendment
// 2026-11-11, issue 119). The fake harness plays each pattern on its first prompt; what it does on the
// nudge decides whether the run goes on.
var _ = Describe("a looping agent", func() {
	var ids atomic.Int64
	toolCall := func(say func(sdk.SessionUpdate) error, cmd string, st sdk.ToolCallStatus, out string) {
		GinkgoHelper()
		id := sdk.ToolCallId(fmt.Sprintf("loop-%d", ids.Add(1)))
		Expect(say(sdk.StartToolCall(id, "bash", sdk.WithStartKind(sdk.ToolKindExecute), sdk.WithStartRawInput(map[string]any{"cmd": cmd})))).To(Succeed())
		Expect(say(sdk.UpdateToolCall(id, sdk.WithUpdateStatus(st), sdk.WithUpdateRawOutput(out)))).To(Succeed())
	}
	// patterns plays one stuck pattern, in the steps the default thresholds ask for.
	patterns := map[string]func(say func(sdk.SessionUpdate) error){
		"same_action": func(say func(sdk.SessionUpdate) error) {
			for range 4 {
				toolCall(say, "ls", sdk.ToolCallStatusCompleted, "a b")
			}
		},
		"same_error": func(say func(sdk.SessionUpdate) error) {
			for i := range 3 {
				toolCall(say, "make", sdk.ToolCallStatusFailed, fmt.Sprintf("error %d", i))
			}
		},
		"monologue": func(say func(sdk.SessionUpdate) error) {
			for i := range 3 {
				Expect(say(sdk.UpdateAgentThoughtText("hm"))).To(Succeed())
				Expect(say(sdk.UpdateAgentMessageText(fmt.Sprintf("I will look, attempt %d. ", i)))).To(Succeed())
			}
		},
		"alternating": func(say func(sdk.SessionUpdate) error) {
			for i := range 6 {
				if i%2 == 0 {
					toolCall(say, "cat a", sdk.ToolCallStatusCompleted, "A")
				} else {
					toolCall(say, "cat b", sdk.ToolCallStatusCompleted, "B")
				}
			}
		},
	}
	// loops makes the harness play the pattern on its first prompt and, when again is set, on the nudge
	// that follows it. The nudge then waits for a cancel; otherwise the agent changes course and ends.
	loops := func(e *env, pattern string, again bool) {
		e.harness.set(func(h *harness) {
			h.holdLoop = again
			h.script = func(ctx context.Context, text string, say func(sdk.SessionUpdate) error) bool {
				defer GinkgoRecover()
				switch {
				case strings.Contains(text, markWait):
					patterns[pattern](say)
				case again && strings.HasPrefix(text, loopNudgeMarker):
					patterns[pattern](say)
				}
				return false
			}
		})
	}
	nudges := func(ctx SpecContext, r store.Run) []map[string]any {
		var out []map[string]any
		for _, ev := range events(ctx, r) {
			if ev.Kind == "run.loop_nudge" {
				var p map[string]any
				Expect(json.Unmarshal(ev.Payload, &p)).To(Succeed())
				out = append(out, p)
			}
		}
		return out
	}

	DescribeTable("is told once, and the run goes on when it changes course",
		func(ctx SpecContext, pattern string) {
			e := newEnv(ctx, store.AgentFlavorOpencode)
			loops(e, pattern, false)
			t := e.task(ctx, markWait)
			r := e.enqueue(ctx, t)
			m := agentd.NewMetrics(prometheus.NewRegistry())
			e.pool("agentd-a", func(c *agentd.Config) { c.Metrics = m })

			Eventually(runState(ctx, r)).WithTimeout(racescale.D(20 * time.Second)).Should(Equal(store.RunStateSucceeded))
			prompts, _, cancels, _ := e.harness.snapshot()
			Expect(prompts).To(HaveLen(2))
			Expect(prompts[1]).To(HavePrefix(loopNudgeMarker))
			Expect(cancels).To(Equal(1), "the turn is interrupted once, for the nudge")
			Expect(nudges(ctx, r)).To(ConsistOf(HaveKeyWithValue("pattern", pattern)))
			Expect(testutil.ToFloat64(m.LoopDetections.WithLabelValues("nudge", pattern))).To(Equal(1.0))
			Expect(testutil.ToFloat64(m.LoopDetections.WithLabelValues("stop", pattern))).To(BeZero())
			Expect(getRun(ctx, r).FailureClass.Valid).To(BeFalse())
		},
		Entry("the same action with the same result", "same_action"),
		Entry("the same error", "same_error"),
		Entry("a monologue without a tool call", "monologue"),
		Entry("two actions taking turns", "alternating"),
	)

	DescribeTable("is stopped with failure class loop when the pattern continues after the nudge",
		func(ctx SpecContext, pattern string) {
			e := newEnv(ctx, store.AgentFlavorOpencode)
			loops(e, pattern, true)
			t := e.task(ctx, markWait)
			r := e.enqueue(ctx, t)
			m := agentd.NewMetrics(prometheus.NewRegistry())
			e.pool("agentd-a", func(c *agentd.Config) { c.Metrics = m })

			Eventually(runState(ctx, r)).WithTimeout(racescale.D(20 * time.Second)).Should(Equal(store.RunStateFailed))
			got := getRun(ctx, r)
			Expect(got.FailureClass.RunFailureClass).To(Equal(store.RunFailureClassLoop))
			Expect(got.OutcomeReason.String).To(ContainSubstring("kept looping"))
			prompts, _, cancels, _ := e.harness.snapshot()
			Expect(prompts).To(HaveLen(2), "one nudge, no second")
			Expect(cancels).To(Equal(2), "one to interrupt the turn for the nudge, one to stop")
			Expect(nudges(ctx, r)).To(HaveLen(1))
			Expect(testutil.ToFloat64(m.LoopDetections.WithLabelValues("nudge", pattern))).To(Equal(1.0))
			Expect(testutil.ToFloat64(m.LoopDetections.WithLabelValues("stop", pattern))).To(Equal(1.0))
			// A person can still answer: the task stays open (ADR 0065).
			task, err := q.GetTask(ctx, store.GetTaskParams{OrgID: e.org, ID: t.ID})
			Expect(err).NotTo(HaveOccurred())
			Expect(task.State).NotTo(Equal(store.TaskStateFailed))
		},
		Entry("the same action with the same result", "same_action"),
		Entry("the same error", "same_error"),
		Entry("a monologue without a tool call", "monologue"),
		Entry("two actions taking turns", "alternating"),
	)

	It("leaves a run alone that stays under the thresholds", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		e.harness.set(func(h *harness) {
			h.script = func(ctx context.Context, text string, say func(sdk.SessionUpdate) error) bool {
				defer GinkgoRecover()
				for range 3 {
					toolCall(say, "ls", sdk.ToolCallStatusCompleted, "a b")
				}
				return true
			}
		})
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(20 * time.Second)).Should(Equal(store.RunStateSucceeded))
		Expect(nudges(ctx, r)).To(BeEmpty())
	})
})
