// SPDX-License-Identifier: Apache-2.0

package runreply_test

import (
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/runreply"
)

var _ = Describe("Reply", func() {
	var (
		run store.Run
		seq int64
	)
	event := func(ctx SpecContext, kind, payload string) {
		GinkgoHelper()
		seq++
		_, err := q.AppendRunEventOnce(ctx, store.AppendRunEventOnceParams{
			OrgID: run.OrgID, RunID: run.ID, Epoch: run.Epoch, Kind: kind, Payload: []byte(payload), SourceSeq: seq,
		})
		Expect(err).NotTo(HaveOccurred())
	}
	chunk := func(ctx SpecContext, text string) {
		GinkgoHelper()
		event(ctx, "acp.agent_message_chunk", `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"`+text+`"}}`)
	}
	BeforeEach(func(ctx SpecContext) {
		run, _ = newRun(ctx)
		seq = 0
	})

	It("joins the agent's chunks and starts again after a park", func(ctx SpecContext) {
		chunk(ctx, "before ")
		chunk(ctx, "the park")
		event(ctx, runreply.KindParked, `{"reason":"input_required"}`)
		chunk(ctx, "after ")
		chunk(ctx, "the park")
		got, err := runreply.Reply(ctx, q, run.OrgID, run.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal("after the park"))
	})

	tool := func(ctx SpecContext) {
		GinkgoHelper()
		event(ctx, "acp.tool_call", `{"sessionUpdate":"tool_call","toolCallId":"t1","title":"read"}`)
		event(ctx, "acp.tool_call_update", `{"sessionUpdate":"tool_call_update","toolCallId":"t1","status":"completed"}`)
	}

	It("replies with the last message, not the narration before tool calls (finops-haiku)", func(ctx SpecContext) {
		chunk(ctx, "I'll pull cost data ")
		chunk(ctx, "first.")
		tool(ctx)
		chunk(ctx, "The metrics view works. Next I'll draft the report.")
		tool(ctx)
		chunk(ctx, "# Daily cost report")
		got, err := runreply.Reply(ctx, q, run.OrgID, run.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal("# Daily cost report"))
	})

	It("replies with the last message, not the narration before tool calls (internal-sonnet)", func(ctx SpecContext) {
		chunk(ctx, "Fresh start.")
		tool(ctx)
		chunk(ctx, "Fix a few line refs, then write the comparison.")
		tool(ctx)
		chunk(ctx, "# How it compares")
		got, err := runreply.Reply(ctx, q, run.OrgID, run.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal("# How it compares"))
	})

	It("uses the message before when the turn ended on a tool call", func(ctx SpecContext) {
		chunk(ctx, "All done.")
		tool(ctx)
		got, err := runreply.Reply(ctx, q, run.OrgID, run.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal("All done."))
	})

	It("separates the messages in the time-limit transcript", func(ctx SpecContext) {
		chunk(ctx, "Fresh start.")
		tool(ctx)
		chunk(ctx, "Half done.")
		got, err := runreply.Transcript(ctx, q, run.OrgID, run.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal("Fresh start.\n\nHalf done."))
	})

	It("says what the agent said in the time-limit note, from its reply", func(ctx SpecContext) {
		chunk(ctx, "Done so far: A.")
		note, err := runreply.TimeLimitNote(ctx, q, run)
		Expect(err).NotTo(HaveOccurred())
		Expect(note).To(HaveKeyWithValue("reason", "wall_clock_cap"))
		Expect(note).To(HaveKeyWithValue("message", "Stopped at the time limit of 30m."))
		Expect(note).To(HaveKeyWithValue("maxWallClockSeconds", int32(1800)))
		Expect(note).To(HaveKeyWithValue("summary", "Done so far: A."))
		Expect(note).To(HaveKeyWithValue("summarySource", "reply"))
	})

	It("keeps the end of a long reply in the time-limit note, where the wrap-up reply is", func(ctx SpecContext) {
		for range 20 {
			chunk(ctx, strings.Repeat("early working text ", 40))
		}
		chunk(ctx, " Finished A. Left: B. Next: C.")
		note, err := runreply.TimeLimitNote(ctx, q, run)
		Expect(err).NotTo(HaveOccurred())
		sum := note["summary"].(string)
		Expect(len(sum)).To(BeNumerically("<=", runreply.MaxSummaryBytes))
		Expect(sum).To(HavePrefix("[truncated]\n"))
		Expect(sum).To(HaveSuffix("Finished A. Left: B. Next: C."))
	})

	It("falls back to the task's notes, then to none", func(ctx SpecContext) {
		note, err := runreply.TimeLimitNote(ctx, q, run)
		Expect(err).NotTo(HaveOccurred())
		Expect(note).To(HaveKeyWithValue("summarySource", "none"))
		Expect(note).NotTo(HaveKey("summary"))

		_, err = pool.Exec(ctx, `UPDATE tasks SET agent_summary = 'notes text', agent_summary_at = now() WHERE org_id = $1 AND id = $2`, run.OrgID, run.TaskID)
		Expect(err).NotTo(HaveOccurred())
		note, err = runreply.TimeLimitNote(ctx, q, run)
		Expect(err).NotTo(HaveOccurred())
		Expect(note).To(HaveKeyWithValue("summary", "notes text"))
		Expect(note).To(HaveKeyWithValue("summarySource", "notes"))
	})
})

var _ = Describe("Truncate", func() {
	It("cuts on a character boundary and says so", func() {
		s := strings.Repeat("é", 100) // two bytes each
		got := runreply.Truncate(s, 51)
		Expect(len(got)).To(BeNumerically("<=", 51))
		Expect(got).To(HaveSuffix("\n[truncated]"))
		Expect(strings.ToValidUTF8(got, "")).To(Equal(got))
		Expect(runreply.Truncate("  short  ", 51)).To(Equal("short"))
	})
})

var _ = Describe("TruncateTail", func() {
	It("keeps the end on a character boundary and says so", func() {
		s := strings.Repeat("é", 100)
		got := runreply.TruncateTail(s, 51)
		Expect(len(got)).To(BeNumerically("<=", 51))
		Expect(got).To(HavePrefix("[truncated]\n"))
		Expect(strings.ToValidUTF8(got, "")).To(Equal(got))
		Expect(runreply.TruncateTail("  short  ", 51)).To(Equal("short"))
	})
})

var _ = DescribeTable("Human",
	func(d time.Duration, want string) { Expect(runreply.Human(d)).To(Equal(want)) },
	Entry("an hour", time.Hour, "1h"),
	Entry("an hour and a half", 90*time.Minute, "1h30m"),
	Entry("half an hour", 30*time.Minute, "30m"),
	Entry("seconds", 45*time.Second, "45s"),
	Entry("a minute and a half", 90*time.Second, "1m30s"),
)
