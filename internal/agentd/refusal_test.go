// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"time"

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
})
