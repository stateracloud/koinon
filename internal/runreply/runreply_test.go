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

var _ = DescribeTable("Human",
	func(d time.Duration, want string) { Expect(runreply.Human(d)).To(Equal(want)) },
	Entry("an hour", time.Hour, "1h"),
	Entry("an hour and a half", 90*time.Minute, "1h30m"),
	Entry("half an hour", 30*time.Minute, "30m"),
	Entry("seconds", 45*time.Second, "45s"),
	Entry("a minute and a half", 90*time.Second, "1m30s"),
)
