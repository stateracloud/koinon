// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"context"
	"time"

	sdk "github.com/coder/acp-go-sdk"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/agentd"
	"github.com/stateracloud/koinon/internal/db/store"
)

var _ = Describe("A refused permission request", func() {
	It("tells the agent what was refused and continues the run when the harness cancelled the turn", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		r := e.enqueue(ctx, e.task(ctx, markRefuse+" write the notes"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateSucceeded))

		prompts, _, _, _ := e.harness.snapshot()
		Expect(prompts).To(HaveLen(2), "the task's prompt, then the note")
		Expect(prompts[1]).To(HavePrefix("[Koinon] Your last action was refused"))
		Expect(prompts[1]).To(ContainSubstring("/sandbox/.koinon/notes/external/report.md is outside your session directory /sandbox/work"))
		Expect(prompts[1]).To(ContainSubstring("/sandbox/work/notes/"))

		evs := events(ctx, r)
		refusals := eventOf(evs, "run.refusal")
		Expect(refusals).To(HaveLen(1))
		Expect(payload(refusals[0])).To(HaveKeyWithValue("count", BeEquivalentTo(1)))
		Expect(chunks(evs)).To(ContainElement("resumed"))
		Expect(getRun(ctx, r).FailureClass.Valid).To(BeFalse())
		Expect(eventOf(evs, "run.failed")).To(BeEmpty())
	})

	It("fails the run, as a policy failure, when the agent is refused again and again", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		e.harness.set(func(h *harness) { h.refuseAll = true })
		r := e.enqueue(ctx, e.task(ctx, "write the notes"))
		e.pool("agentd-a", func(c *agentd.Config) { c.MaxRefusedTurns = 2 })
		Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateFailed))

		prompts, _, _, _ := e.harness.snapshot()
		Expect(prompts).To(HaveLen(3), "the task's prompt and two notes")
		got := getRun(ctx, r)
		Expect(got.FailureClass.RunFailureClass).To(Equal(store.RunFailureClassPolicy))
		Expect(got.OutcomeReason.String).To(ContainSubstring("Koinon refused write on /sandbox/.koinon/notes/external/report.md"))
		Expect(got.OutcomeReason.String).To(ContainSubstring("after 2 refusals"))
	})

	It("counts only consecutive refused turns", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		// Prompts 1 and 3 are refused; the model fails once in between (prompt 2) and is retried.
		e.harness.set(func(h *harness) { h.refuseOn = map[int]bool{1: true, 3: true} })
		calls := 0
		e.harness.modelFault = func(context.Context, string) error {
			calls++
			if calls == 2 {
				return &sdk.RequestError{Code: -32603, Message: "boom", Data: map[string]any{"statusCode": 503}}
			}
			return nil
		}
		r := e.enqueue(ctx, e.task(ctx, "write the notes"))
		// agentd's own retry layer, the one for a flavor whose harness does not retry.
		e.pool("agentd-a", agentd.OwnRetryLayer, func(c *agentd.Config) { c.MaxRefusedTurns = 1; c.RetryBackoff = 10 * time.Millisecond })
		Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateSucceeded))
		Expect(eventOf(events(ctx, r), "run.refusal")).To(HaveLen(2))
	})

	It("does not prompt again when the run was canceled during the refused turn", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		r := e.enqueue(ctx, e.task(ctx, markRefuse+" write the notes"))
		e.harness.set(func(h *harness) {
			h.onRefuse = func() {
				_, err := q.CancelRun(context.Background(), store.CancelRunParams{OrgID: e.org, ID: r.ID, OutcomeReason: "user canceled"})
				Expect(err).NotTo(HaveOccurred())
			}
		})
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateCanceled))
		Consistently(func() int { p, _, _, _ := e.harness.snapshot(); return len(p) }, 500*time.Millisecond).Should(Equal(1))
		Expect(eventOf(events(ctx, r), "run.refusal")).To(BeEmpty())
	})
})
