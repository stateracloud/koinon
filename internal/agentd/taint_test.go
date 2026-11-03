// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	sdk "github.com/coder/acp-go-sdk"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/agentd"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/mcpguard"
)

var _ = Describe("The leak guard's feed (ADR 0091 decision 4)", func() {
	// update is a session update on the wire, as agentd marshals it after the SDK decoded it.
	update := func(wire string) []byte {
		GinkgoHelper()
		var u sdk.SessionUpdate
		Expect(json.Unmarshal([]byte(wire), &u)).To(Succeed())
		b, err := json.Marshal(u)
		Expect(err).NotTo(HaveOccurred())
		return b
	}
	var finished func([]byte) (mcpguard.Legs, []string, bool)
	BeforeEach(func() { finished = agentd.NewTaintCalls() })

	It("reports a repository read when it finishes, with its output, as private data", func() {
		_, _, ok := finished(update(`{"sessionUpdate":"tool_call","toolCallId":"t1","title":"read","kind":"read","status":"pending"}`))
		Expect(ok).To(BeFalse())
		_, _, ok = finished(update(`{"sessionUpdate":"tool_call_update","toolCallId":"t1","status":"in_progress",
			"content":[{"type":"content","content":{"type":"text","text":"customer list: koinon-test-acme-row-1"}}]}`))
		Expect(ok).To(BeFalse(), "not finished yet")
		legs, texts, ok := finished(update(`{"sessionUpdate":"tool_call_update","toolCallId":"t1","status":"completed"}`))
		Expect(ok).To(BeTrue())
		Expect(legs).To(Equal(mcpguard.Legs{PrivateData: true}))
		Expect(texts).To(ConsistOf("customer list: koinon-test-acme-row-1"))
	})

	It("reports shell output from the raw output too, and a failed command as well", func() {
		legs, texts, ok := finished(update(`{"sessionUpdate":"tool_call","toolCallId":"t2","title":"bash","kind":"execute","status":"failed",
			"rawOutput":{"output":"cat: secrets.env line 1","metadata":{"exit":1}}}`))
		Expect(ok).To(BeTrue())
		Expect(legs.PrivateData).To(BeTrue())
		Expect(texts).To(ContainElement("cat: secrets.env line 1"))
	})

	It("reports both sides of an edit's diff", func() {
		_, texts, ok := finished(update(`{"sessionUpdate":"tool_call","toolCallId":"t3","title":"edit","kind":"edit","status":"completed",
			"content":[{"type":"diff","path":"/sandbox/work/a.go","oldText":"old private line","newText":"new private line"}]}`))
		Expect(ok).To(BeTrue())
		Expect(texts).To(ConsistOf("old private line", "new private line"))
	})

	It("reports a fetch as untrusted input", func() {
		legs, _, ok := finished(update(`{"sessionUpdate":"tool_call","toolCallId":"t4","title":"webfetch","kind":"fetch","status":"completed",
			"content":[{"type":"content","content":{"type":"text","text":"a page someone wrote"}}]}`))
		Expect(ok).To(BeTrue())
		Expect(legs).To(Equal(mcpguard.Legs{UntrustedInput: true}))
	})

	It("counts a tool of kind other (MCP tools among them) as both legs, cautiously", func() {
		legs, texts, ok := finished(update(`{"sessionUpdate":"tool_call","toolCallId":"t5","title":"mcp__acme-wiki_read_page","kind":"other","status":"completed",
			"content":[{"type":"content","content":{"type":"text","text":"a wiki page"}}]}`))
		Expect(ok).To(BeTrue())
		Expect(legs).To(Equal(mcpguard.Legs{UntrustedInput: true, PrivateData: true}))
		Expect(texts).To(ConsistOf("a wiki page"))
	})

	It("counts a tool call without a kind, or of a kind it does not know, as both legs", func() {
		for i, wire := range []string{
			`{"sessionUpdate":"tool_call","toolCallId":"t6","title":"mystery","status":"completed","content":[{"type":"content","content":{"type":"text","text":"out"}}]}`,
			`{"sessionUpdate":"tool_call","toolCallId":"t7","title":"newer","kind":"teleport","status":"completed"}`,
		} {
			legs, _, ok := finished([]byte(wire))
			Expect(ok).To(BeTrue(), "case %d", i)
			Expect(legs).To(Equal(mcpguard.Legs{UntrustedInput: true, PrivateData: true}), "case %d", i)
		}
		legs, _, ok := finished(update(`{"sessionUpdate":"tool_call","toolCallId":"t8","title":"mystery","status":"completed"}`))
		Expect(ok).To(BeTrue(), "a missing kind after the SDK's round trip")
		Expect(legs).To(Equal(mcpguard.Legs{UntrustedInput: true, PrivateData: true}))
	})

	It("counts, for the guard's wait, every update that finishes a tool call, and the same ones on both sides", func() {
		line := func(update string) []byte {
			return []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":` + update + `}}`)
		}
		for _, c := range []struct {
			update string
			counts bool
		}{
			{`{"sessionUpdate":"tool_call_update","toolCallId":"t1","status":"completed"}`, true},
			{`{"sessionUpdate":"tool_call","toolCallId":"t2","kind":"execute","status":"failed"}`, true},
			{`{"sessionUpdate":"tool_call_update","toolCallId":"t3","kind":"switch_mode","status":"completed"}`, true},
			{`{"sessionUpdate":"tool_call_update","toolCallId":"t1","status":"in_progress"}`, false},
			{`{"sessionUpdate":"tool_call","toolCallId":"t4","kind":"read"}`, false},
			{`{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"completed"}}`, false},
		} {
			read, handled := agentd.FeedCounts(line(c.update))
			Expect(read).To(Equal(c.counts), c.update)
			Expect(handled).To(Equal(c.counts), c.update)
			// The handler sees the update after the SDK decoded it: the count must not change.
			_, handled = agentd.FeedCounts(line(string(update(c.update))))
			Expect(handled).To(Equal(c.counts), "after the SDK's round trip: "+c.update)
		}
	})

	It("counts delete, move and think as private data, and reports nothing for a mode switch or other updates", func() {
		for _, kind := range []string{"delete", "move", "think"} {
			legs, _, ok := finished(update(`{"sessionUpdate":"tool_call","toolCallId":"k-` + kind + `","title":"x","kind":"` + kind + `","status":"completed"}`))
			Expect(ok).To(BeTrue(), kind)
			Expect(legs).To(Equal(mcpguard.Legs{PrivateData: true}), kind)
		}
		_, _, ok := finished(update(`{"sessionUpdate":"tool_call","toolCallId":"t9","title":"plan mode","kind":"switch_mode","status":"completed"}`))
		Expect(ok).To(BeFalse())
		_, _, ok = finished(update(`{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hi"}}`))
		Expect(ok).To(BeFalse())
	})
})

// recordingFeed is a TaintFeed that records what agentd tells the leak guard, in order.
type recordingFeed struct {
	mu     sync.Mutex
	events []string
	epochs []int64
}

func (f *recordingFeed) add(e string, epoch int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
	f.epochs = append(f.epochs, epoch)
}

func (f *recordingFeed) Report(_ context.Context, _, _ pgtype.UUID, legs mcpguard.Legs, _ []string) error {
	f.add(fmt.Sprintf("report private=%t untrusted=%t", legs.PrivateData, legs.UntrustedInput), -1)
	return nil
}

func (f *recordingFeed) Start(_ context.Context, _, _ pgtype.UUID, epoch int64) error {
	f.add("start", epoch)
	return nil
}

func (f *recordingFeed) Read(_ context.Context, _, _ pgtype.UUID, epoch int64) error {
	f.add("read", epoch)
	return nil
}

func (f *recordingFeed) Done(_ context.Context, _, _ pgtype.UUID, epoch int64) error {
	f.add("done", epoch)
	return nil
}

func (f *recordingFeed) snapshot() ([]string, []int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.events), slices.Clone(f.epochs)
}

var _ = Describe("The leak guard's feed counts (ADR 0091 decision 4, amendment 2026-11-03)", func() {
	It("counts each finished tool call when it is read and again once it is reported, under the run's epoch", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		e.harness.set(func(h *harness) {
			h.script = func(ctx context.Context, text string, say func(sdk.SessionUpdate) error) bool {
				defer GinkgoRecover()
				// A shell read of repository text, then a fetch, then a call still running.
				Expect(say(sdk.StartToolCall("c1", "bash", sdk.WithStartKind(sdk.ToolKindExecute)))).To(Succeed())
				Expect(say(sdk.UpdateToolCall("c1", sdk.WithUpdateStatus(sdk.ToolCallStatusCompleted),
					sdk.WithUpdateRawOutput(map[string]any{"output": "koinon-test private repository line"})))).To(Succeed())
				Expect(say(sdk.StartToolCall("c2", "webfetch", sdk.WithStartKind(sdk.ToolKindFetch), sdk.WithStartStatus(sdk.ToolCallStatusCompleted)))).To(Succeed())
				Expect(say(sdk.StartToolCall("c3", "bash", sdk.WithStartKind(sdk.ToolKindExecute)))).To(Succeed())
				return true
			}
		})
		feed := &recordingFeed{}
		r := e.enqueue(ctx, e.task(ctx, "feed"))
		e.pool("agentd-feed", func(c *agentd.Config) { c.Taint = feed })
		Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateSucceeded))
		events, epochs := feed.snapshot()
		Expect(events).To(HaveLen(7))
		Expect(events[0]).To(Equal("start"), "the counts start before the harness runs")
		claimed := getRun(ctx, r)
		for i, ev := range events {
			if ev == "start" || ev == "read" || ev == "done" {
				Expect(epochs[i]).To(Equal(claimed.Epoch), ev)
			}
		}
		// Each finished call: read before it is reported, reported before it is done.
		reads, reports, dones := []int{}, []int{}, []int{}
		for i, ev := range events {
			switch {
			case ev == "read":
				reads = append(reads, i)
			case strings.HasPrefix(ev, "report"):
				reports = append(reports, i)
			case ev == "done":
				dones = append(dones, i)
			}
		}
		Expect(reads).To(HaveLen(2))
		Expect(reports).To(HaveLen(2))
		Expect(dones).To(HaveLen(2))
		for k := range 2 {
			Expect(reads[k]).To(BeNumerically("<", reports[k]))
			Expect(reports[k]).To(BeNumerically("<", dones[k]))
		}
		Expect(events[reports[0]]).To(Equal("report private=true untrusted=false"))
		Expect(events[reports[1]]).To(Equal("report private=false untrusted=true"))
	})
})
