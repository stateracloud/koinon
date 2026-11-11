// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/agentd"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/testing/racescale"
)

var _ = Describe("The end-of-turn summary", func() {
	It("asks for a note after the turn, stores it on the task and keeps it out of the timeline", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		e.harness.set(func(h *harness) { h.summaryReply = "Notes: the build is green; next, open the pull request." })
		t := e.task(ctx, "hello")
		r := e.enqueue(ctx, t)
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))

		summaries, _ := e.harness.summarySnapshot()
		Expect(summaries).To(HaveLen(1))
		Expect(summaries[0]).To(And(ContainSubstring("Do not use any tool at all"), ContainSubstring("not shown as a reply")))
		got, err := q.GetTask(ctx, store.GetTaskParams{OrgID: e.org, ID: t.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.AgentSummary.String).To(Equal("Notes: the build is green; next, open the pull request."))
		Expect(got.AgentSummaryAt.Valid).To(BeTrue())

		// The one agent reply is the turn's, not the note; no run event carries the note.
		var replies []string
		for _, ev := range taskEvents(ctx, e, t) {
			if ev.Kind == "message" && ev.ActorKind == store.TaskActorKindAgent {
				replies = append(replies, string(ev.Payload))
			}
		}
		Expect(replies).To(HaveLen(1))
		Expect(replies[0]).NotTo(ContainSubstring("Notes:"))
		for _, ev := range events(ctx, r) {
			Expect(string(ev.Payload)).NotTo(ContainSubstring("Notes: the build"))
		}
		Expect(kinds(events(ctx, r))).To(ContainElement("run.summary"))
		Expect(getRun(ctx, r).Summarizing).To(BeFalse())
	})

	It("cuts a long note to 2,000 tokens (6,000 bytes)", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		e.harness.set(func(h *harness) { h.summaryReply = strings.Repeat("é", 20000) })
		t := e.task(ctx, "hello")
		r := e.enqueue(ctx, t)
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		got, err := q.GetTask(ctx, store.GetTaskParams{OrgID: e.org, ID: t.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(len(got.AgentSummary.String)).To(BeNumerically("<=", agentd.SummaryMaxBytes))
		Expect(got.AgentSummary.String).To(HaveSuffix("[truncated]"))
	})

	It("marks the run summarizing, so Koinon's tool servers see no running run, and clears the mark", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		var during error
		e.harness.set(func(h *harness) {
			h.onSummary = func() {
				_, during = q.GetRunningRunForAgent(ctx, store.GetRunningRunForAgentParams{OrgID: e.org, AgentID: e.agent.ID})
			}
		})
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		Expect(during).To(HaveOccurred(), "tools find no running run while the summary is written")
		Expect(getRun(ctx, r).Summarizing).To(BeFalse())
	})

	It("rejects permission requests during the summary and discards a note from an agent that used a tool", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		e.harness.set(func(h *harness) { h.summaryTool = true })
		t := e.task(ctx, "hello")
		r := e.enqueue(ctx, t)
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		_, answer := e.harness.summarySnapshot()
		Expect(answer).To(Equal("reject"))
		got, err := q.GetTask(ctx, store.GetTaskParams{OrgID: e.org, ID: t.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.AgentSummary.Valid).To(BeFalse())
		Expect(kinds(events(ctx, r))).To(ContainElement("run.summary_refused"))
		for _, ev := range events(ctx, r) {
			Expect(ev.Kind).NotTo(Equal("acp.permission"), "the summary's permission request is not recorded as the turn's")
		}
	})

	It("does not fail the run when the agent does not answer in time", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		e.harness.set(func(h *harness) { h.summaryHang = true })
		t := e.task(ctx, "hello")
		r := e.enqueue(ctx, t)
		e.pool("agentd-a", func(c *agentd.Config) { c.SummaryTimeout = 300 * time.Millisecond })
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		Expect(kinds(events(ctx, r))).To(ContainElement("run.summary_failed"))
		got, err := q.GetTask(ctx, store.GetTaskParams{OrgID: e.org, ID: t.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.AgentSummary.Valid).To(BeFalse())
	})

	It("asks again at the end of every turn and keeps the latest note", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		t := e.task(ctx, "first")
		r1 := e.enqueue(ctx, t)
		e.pool("agentd-a")
		Eventually(runState(ctx, r1)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		e.harness.set(func(h *harness) { h.summaryReply = "Second note." })
		e.message(ctx, t, "second")
		r2 := e.enqueue(ctx, t)
		Eventually(runState(ctx, r2)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		summaries, _ := e.harness.summarySnapshot()
		Expect(summaries).To(HaveLen(2))
		got, err := q.GetTask(ctx, store.GetTaskParams{OrgID: e.org, ID: t.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.AgentSummary.String).To(Equal("Second note."))
	})
})

var _ = Describe("A fresh session with a handoff", func() {
	It("starts a new session instead of loading, with the notes, the last replies and the messages", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		e.harness.set(func(h *harness) { h.summaryReply = "Notes: waiting on the review." })
		t := e.task(ctx, "first message")
		r1 := e.enqueue(ctx, t)
		e.pool("agentd-a")
		Eventually(runState(ctx, r1)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		before, err := q.GetTask(ctx, store.GetTaskParams{OrgID: e.org, ID: t.ID})
		Expect(err).NotTo(HaveOccurred())

		// A run that succeeded completed its task (old issue 324); the restart marks open tasks, so a
		// follow-up reopens it first.
		_, err = db.Exec(ctx, `UPDATE tasks SET state = 'submitted', finished_at = NULL WHERE org_id = $1 AND id = $2`, e.org, t.ID)
		Expect(err).NotTo(HaveOccurred())
		marked, err := q.RequestSessionRestartForAgent(ctx, store.RequestSessionRestartForAgentParams{OrgID: e.org, AgentID: e.agent.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(marked).To(HaveLen(1))
		e.message(ctx, t, "second message")
		r2 := e.enqueue(ctx, t)
		Eventually(runState(ctx, r2)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))

		prompts, loads, _, _ := e.harness.snapshot()
		Expect(loads).To(BeEmpty(), "the old session is not loaded")
		Expect(prompts).To(HaveLen(2))
		Expect(prompts[1]).To(And(
			HavePrefix("Task: "+t.Title),
			ContainSubstring("Notes: waiting on the review."),
			ContainSubstring("first message"),
			ContainSubstring("second message"),
			ContainSubstring("instructions changed"),
		))
		// The agent's last reply is part of it ("asking" is not scripted here; the fake answers "ok"-like text).
		after, err := q.GetTask(ctx, store.GetTaskParams{OrgID: e.org, ID: t.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(after.HarnessSessionID.String).NotTo(Equal(before.HarnessSessionID.String))
		Expect(after.SessionRestartPending).To(BeFalse())
		var note string
		for _, ev := range taskEvents(ctx, e, t) {
			if ev.Kind == "run.session_restarted" {
				note = string(ev.Payload)
			}
		}
		Expect(note).To(ContainSubstring("Session restarted: new instructions."))
		Expect(kindsOf(taskEvents(ctx, e, t))).NotTo(ContainElement("run.session_reset"))
	})

	It("loads the session as usual when nothing was applied", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		t := e.task(ctx, "one")
		r1 := e.enqueue(ctx, t)
		e.pool("agentd-a")
		Eventually(runState(ctx, r1)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		e.message(ctx, t, "two")
		r2 := e.enqueue(ctx, t)
		Eventually(runState(ctx, r2)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		_, loads, _, _ := e.harness.snapshot()
		Expect(loads).To(HaveLen(1))
	})
})

var _ = Describe("The handoff", func() {
	h := func(n int, size int) agentd.Handoff {
		x := agentd.Handoff{Title: "Fix the build", Summary: "Notes."}
		for i := range n {
			x.Replies = append(x.Replies, strings.Repeat(string(rune('a'+i)), size))
			x.Messages = append(x.Messages, strings.Repeat(string(rune('A'+i)), size))
		}
		return x
	}

	It("has the parts in order", func() {
		out := h(2, 5).Build()
		Expect(out).To(MatchRegexp(`(?s)Task: Fix the build.*Notes\..*aaaaa.*bbbbb.*AAAAA.*BBBBB`))
	})

	It("stays within about 8,000 tokens (bytes / 3), dropping the oldest replies first", func() {
		x := h(3, 6000) // 3 replies and a message of 6 kB each: just over the cap
		x.Messages = x.Messages[:1]
		out := x.Build()
		Expect(len(out)).To(BeNumerically("<=", agentd.HandoffMaxBytes))
		Expect(out).To(ContainSubstring("Notes."))
		Expect(out).To(ContainSubstring(strings.Repeat("A", 100)), "the message stays")
		Expect(out).NotTo(ContainSubstring(strings.Repeat("a", 100)), "the oldest reply went first")
		Expect(out).To(ContainSubstring(strings.Repeat("c", 100)), "the newest reply stays while it fits")
	})

	It("drops the oldest messages after the replies, and never the newest", func() {
		x := agentd.Handoff{Title: "T", Summary: "S", Replies: []string{"r1"}}
		for i := range 5 {
			x.Messages = append(x.Messages, strings.Repeat(string(rune('A'+i)), 9000))
		}
		out := x.Build()
		Expect(len(out)).To(BeNumerically("<=", agentd.HandoffMaxBytes))
		Expect(out).NotTo(ContainSubstring("r1"))
		Expect(out).To(ContainSubstring(strings.Repeat("E", 100)))
		Expect(out).NotTo(ContainSubstring(strings.Repeat("A", 100)))
	})

	It("cuts a note or message that alone is too long", func() {
		out := agentd.Handoff{Title: "T", Summary: strings.Repeat("s", 40000), Messages: []string{"m"}}.Build()
		Expect(len(out)).To(BeNumerically("<=", agentd.HandoffMaxBytes))
		Expect(out).To(ContainSubstring("[truncated]"))
		out = agentd.Handoff{Title: "T", Messages: []string{strings.Repeat("m", 40000)}}.Build()
		Expect(len(out)).To(BeNumerically("<=", agentd.HandoffMaxBytes))
	})

	It("truncates summaries on a character boundary", func() {
		out := agentd.TruncateSummary(strings.Repeat("日本語", 5000))
		Expect(len(out)).To(BeNumerically("<=", agentd.SummaryMaxBytes))
		Expect(out).To(HaveSuffix("[truncated]"))
		Expect(strings.ToValidUTF8(out, "")).To(Equal(out))
	})
})
