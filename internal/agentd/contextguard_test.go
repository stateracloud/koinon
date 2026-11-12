// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/questions"
	"github.com/stateracloud/koinon/internal/testing/racescale"
)

// The model of the test env has no context window in the catalog, so the guard assumes 200000
// tokens and the cap is 160000 tokens (about 368000 bytes at 2.3 bytes per token).
var _ = Describe("the context guard (#22)", func() {
	It("sends an input under the cap unchanged", func(ctx SpecContext) {
		e := newEnv(ctx, harness.OpenCode)
		body := strings.Repeat("x", 300_000)
		t := e.task(ctx, body)
		r := e.enqueue(ctx, t)
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(20 * time.Second)).Should(Equal(store.RunStateSucceeded))
		prompts, _, _, _ := e.harness.snapshot()
		Expect(prompts).To(Equal([]string{"Task: " + t.Title + "\n\n" + body}))
		arts, err := q.ListRunArtifacts(ctx, store.ListRunArtifactsParams{OrgID: e.org, RunID: r.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(arts).To(BeEmpty())
	})

	It("keeps an input over the cap whole as an artifact, does not send it, and parks the run with the ask", func(ctx SpecContext) {
		e := newEnv(ctx, harness.OpenCode)
		body := strings.Repeat("webhook body ", 40_000) // 520000 bytes
		t := e.task(ctx, body)
		r := e.enqueue(ctx, t)
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(20 * time.Second)).Should(Equal(store.RunStateParked))

		prompts, _, _, _ := e.harness.snapshot()
		Expect(prompts).To(BeEmpty(), "the model never sees the input")
		parked := getRun(ctx, r)
		Expect(parked.ParkReason.RunParkReason).To(Equal(store.RunParkReasonInputRequired))
		got, err := q.GetTask(ctx, store.GetTaskParams{OrgID: e.org, ID: t.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.State).To(Equal(store.TaskStateInputRequired))

		arts, err := q.ListRunArtifacts(ctx, store.ListRunArtifactsParams{OrgID: e.org, RunID: r.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(arts).To(HaveLen(1))
		Expect(arts[0].SizeBytes).To(BeEquivalentTo(len(body)))
		full, err := q.GetRunArtifact(ctx, store.GetRunArtifactParams{OrgID: e.org, ID: arts[0].ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(string(full.Body)).To(Equal(body), "never truncated")

		inbox, err := q.ListOpenQuestionsForUser(ctx, store.ListOpenQuestionsForUserParams{OrgID: e.org, UserID: e.user})
		Expect(err).NotTo(HaveOccurred())
		Expect(inbox).To(HaveLen(1))
		Expect(inbox[0].Question.Question).To(ContainSubstring(arts[0].ID.String()))
		Expect(inbox[0].Question.Question).To(ContainSubstring("larger than 80%"))
		Expect(kindsOf(taskEvents(ctx, e, t))).To(ContainElements(questions.KindAsked, "run.parked"))
	})
})
