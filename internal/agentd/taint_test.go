// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sdk "github.com/coder/acp-go-sdk"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/agentd"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/mcpguard"
	"github.com/stateracloud/koinon/internal/testing/racescale"
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

	It("counts a tool of kind other as both legs, cautiously, when its name is not a Koinon MCP tool", func() {
		legs, texts, ok := finished(update(`{"sessionUpdate":"tool_call","toolCallId":"t5","title":"mcp__acme-wiki_read_page","kind":"other","status":"completed",
			"content":[{"type":"content","content":{"type":"text","text":"a wiki page"}}]}`))
		Expect(ok).To(BeTrue())
		Expect(legs).To(Equal(mcpguard.Legs{UntrustedInput: true, PrivateData: true}))
		Expect(texts).To(ConsistOf("a wiki page"))
	})

	Describe("with the flavor's MCP tool prefix", func() {
		BeforeEach(func() { finished = agentd.NewTaintCallsFor("koinon_") })

		It("reports nothing for a finished Koinon MCP call: the gateway set its legs", func() {
			_, _, ok := finished(update(`{"sessionUpdate":"tool_call","toolCallId":"m1","title":"koinon_create_pull_request","kind":"other","status":"pending"}`))
			Expect(ok).To(BeFalse())
			_, _, ok = finished(update(`{"sessionUpdate":"tool_call_update","toolCallId":"m1","status":"completed",
				"content":[{"type":"content","content":{"type":"text","text":"opened"}}]}`))
			Expect(ok).To(BeFalse())
		})

		It("still sets both legs for another MCP naming and for an unprefixed call of kind other", func() {
			for i, title := range []string{"mcp__x", "task", "koinon_", "xkoinon_y", "koinon_a b"} {
				legs, _, ok := finished(update(fmt.Sprintf(`{"sessionUpdate":"tool_call","toolCallId":"o%d","title":%q,"kind":"other","status":"completed"}`, i, title)))
				Expect(ok).To(BeTrue(), title)
				Expect(legs).To(Equal(mcpguard.Legs{UntrustedInput: true, PrivateData: true}), title)
			}
		})

		It("keeps its own legs for a call of another kind titled like a Koinon tool", func() {
			legs, _, ok := finished(update(`{"sessionUpdate":"tool_call","toolCallId":"r1","title":"koinon_x","kind":"read","status":"completed"}`))
			Expect(ok).To(BeTrue())
			Expect(legs).To(Equal(mcpguard.Legs{PrivateData: true}))
		})

		It("does not let a later title rename the call: a task that finishes as koinon_x sets both legs", func() {
			_, _, ok := finished(update(`{"sessionUpdate":"tool_call","toolCallId":"k1","title":"task","kind":"other","status":"pending"}`))
			Expect(ok).To(BeFalse())
			legs, _, ok := finished(update(`{"sessionUpdate":"tool_call_update","toolCallId":"k1","title":"koinon_x","status":"completed"}`))
			Expect(ok).To(BeTrue())
			Expect(legs).To(Equal(mcpguard.Legs{UntrustedInput: true, PrivateData: true}))
		})
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

// recordingFeed is a TaintFeed that records what agentd tells the leak guard, in order. With hold
// set, the first Report waits until hold is closed: a slow database.
type recordingFeed struct {
	mu     sync.Mutex
	events []string
	epochs []int64
	counts [][2]int64
	texts  []string
	hold   chan struct{}
	held   bool
}

func (f *recordingFeed) add(e string, epoch int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
	f.epochs = append(f.epochs, epoch)
}

func (f *recordingFeed) Report(ctx context.Context, _, _ pgtype.UUID, legs mcpguard.Legs, texts []string) error {
	f.mu.Lock()
	hold := f.hold
	if f.held {
		hold = nil
	}
	f.held = true
	f.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.add(fmt.Sprintf("report private=%t untrusted=%t", legs.PrivateData, legs.UntrustedInput), -1)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.texts = append(f.texts, texts...)
	return nil
}

func (f *recordingFeed) Start(_ context.Context, _, _ pgtype.UUID, epoch int64) error {
	f.add("start", epoch)
	return nil
}

func (f *recordingFeed) Counts(_ context.Context, _, _ pgtype.UUID, epoch, read, done int64) error {
	f.add(fmt.Sprintf("counts %d/%d", read, done), epoch)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counts = append(f.counts, [2]int64{read, done})
	return nil
}

func (f *recordingFeed) snapshot() ([]string, []int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.events), slices.Clone(f.epochs)
}

// fencedFeed is a feed a newer worker of the run already started.
type fencedFeed struct{ recordingFeed }

func (f *fencedFeed) Start(_ context.Context, _, _ pgtype.UUID, epoch int64) error {
	f.add("start", epoch)
	return mcpguard.ErrFeedFenced
}

// errFeed is a feed whose Report fails with err: every time, or only the first failFirst times.
type errFeed struct {
	recordingFeed
	err       error
	failFirst int32
	reports   atomic.Int32
}

func (f *errFeed) Report(ctx context.Context, org, agent pgtype.UUID, legs mcpguard.Legs, texts []string) error {
	n := f.reports.Add(1)
	if f.failFirst == 0 || n <= f.failFirst {
		return f.err
	}
	return f.recordingFeed.Report(ctx, org, agent, legs, texts)
}

// latest is the last counts written, or none.
func (f *recordingFeed) latest() [2]int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.counts) == 0 {
		return [2]int64{}
	}
	return f.counts[len(f.counts)-1]
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
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		Eventually(feed.latest).Should(Equal([2]int64{2, 2}))
		events, epochs := feed.snapshot()
		Expect(events[0]).To(Equal("start"), "the counts start before the harness runs")
		claimed := getRun(ctx, r)
		reports := 0
		var last [2]int64
		for i, ev := range events {
			if strings.HasPrefix(ev, "report") {
				reports++
				continue
			}
			Expect(epochs[i]).To(Equal(claimed.Epoch), ev)
			var c [2]int64
			if _, err := fmt.Sscanf(ev, "counts %d/%d", &c[0], &c[1]); err != nil {
				continue
			}
			// The counts only grow, done never passes read, and done counts only reported calls.
			Expect(c[0]).To(BeNumerically(">=", last[0]), ev)
			Expect(c[1]).To(BeNumerically(">=", last[1]), ev)
			Expect(c[1]).To(BeNumerically("<=", c[0]), ev)
			Expect(c[1]).To(BeNumerically("<=", reports), ev)
			last = c
		}
		Expect(reports).To(Equal(2))
		Expect(events).To(ContainElements("report private=true untrusted=false", "report private=false untrusted=true"))
	})

	It("counts a result read while an earlier report is slow, before that report finishes", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		e.harness.set(func(h *harness) {
			h.script = func(ctx context.Context, text string, say func(sdk.SessionUpdate) error) bool {
				defer GinkgoRecover()
				// Two shell reads finish back to back; the first one's report is slow.
				Expect(say(sdk.StartToolCall("c1", "bash", sdk.WithStartKind(sdk.ToolKindExecute), sdk.WithStartStatus(sdk.ToolCallStatusCompleted)))).To(Succeed())
				Expect(say(sdk.StartToolCall("c2", "bash", sdk.WithStartKind(sdk.ToolKindExecute), sdk.WithStartStatus(sdk.ToolCallStatusCompleted)))).To(Succeed())
				return true
			}
		})
		feed := &recordingFeed{hold: make(chan struct{})}
		r := e.enqueue(ctx, e.task(ctx, "feed"))
		e.pool("agentd-feed-slow", func(c *agentd.Config) { c.Taint = feed })
		// Both results are counted as read while the first report still waits: the guard sees the
		// session two results behind, and waits.
		Eventually(feed.latest).WithTimeout(racescale.D(10 * time.Second)).Should(Equal([2]int64{2, 0}))
		events, _ := feed.snapshot()
		Expect(events).NotTo(ContainElement(HavePrefix("report")))
		close(feed.hold)
		Eventually(feed.latest).WithTimeout(racescale.D(10 * time.Second)).Should(Equal([2]int64{2, 2}))
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
	})

	It("feeds a tool that finishes during the end-of-turn summary before it counts it as done", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		e.harness.set(func(h *harness) {
			// The summary prompt blocks permission requests, not tools: a fetch can finish then.
			h.summaryUpdates = []sdk.SessionUpdate{
				sdk.StartToolCall("s1", "webfetch", sdk.WithStartKind(sdk.ToolKindFetch)),
				sdk.UpdateToolCall("s1", sdk.WithUpdateStatus(sdk.ToolCallStatusCompleted),
					sdk.WithUpdateRawOutput(map[string]any{"output": "koinon-test page fetched during the summary"})),
			}
		})
		feed := &recordingFeed{}
		r := e.enqueue(ctx, e.task(ctx, "feed"))
		e.pool("agentd-feed-summary", func(c *agentd.Config) { c.Taint = feed })
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		Eventually(feed.latest).Should(Equal([2]int64{1, 1}))
		events, _ := feed.snapshot()
		report := slices.Index(events, "report private=false untrusted=true")
		Expect(report).To(BeNumerically(">=", 0), "the summary's fetch set the untrusted leg: %v", events)
		for i, ev := range events {
			if ev == "counts 1/1" {
				Expect(i).To(BeNumerically(">", report), "done advanced only after the report")
			}
		}
		feed.mu.Lock()
		defer feed.mu.Unlock()
		Expect(feed.texts).To(ContainElement("koinon-test page fetched during the summary"), "its text reached the fingerprints")
	})

	It("records, through the real feed, a result that carries its text twice, and the run carries on", func(ctx SpecContext) {
		// Lab e2e step 6b on 1657dd6: opencode sends a finished tool's text as its content and
		// again as its raw output, the two winnowed to the same fingerprints, the fingerprints'
		// upsert refused the statement (SQLSTATE 21000), and every worker gave the run up until it
		// failed.
		e := newEnv(ctx, store.AgentFlavorOpencode)
		out := "koinon-test private repository file\nwith a second line that the harness reports twice\n"
		e.harness.set(func(h *harness) {
			h.script = func(ctx context.Context, text string, say func(sdk.SessionUpdate) error) bool {
				defer GinkgoRecover()
				Expect(say(sdk.StartToolCall("c1", "bash", sdk.WithStartKind(sdk.ToolKindExecute)))).To(Succeed())
				Expect(say(sdk.UpdateToolCall("c1", sdk.WithUpdateStatus(sdk.ToolCallStatusCompleted),
					sdk.WithUpdateContent([]sdk.ToolCallContent{sdk.ToolContent(sdk.TextBlock(out))}),
					sdk.WithUpdateRawOutput(map[string]any{"output": out})))).To(Succeed())
				return true
			}
		})
		r := e.enqueue(ctx, e.task(ctx, "feed"))
		e.pool("agentd-feed-real", func(c *agentd.Config) { c.Taint = mcpguard.NewFeed(db, nil) })
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		Expect(getRun(ctx, r).Epoch).To(Equal(int64(1)), "one worker ran it: no takeover")
		var private bool
		var fps int
		Expect(db.QueryRow(ctx, `SELECT s.private_data, (SELECT count(*) FROM leak_guard_fingerprints f
			WHERE f.org_id = s.org_id AND f.agent_id = s.agent_id AND f.generation = s.generation)
			FROM leak_guard_sessions s WHERE s.org_id = $1 AND s.agent_id = $2`, e.org, e.agent.ID).Scan(&private, &fps)).To(Succeed())
		Expect(private).To(BeTrue())
		Expect(fps).To(BeNumerically(">", 0))
	})

	finishOneTool := func(e *env) {
		e.harness.set(func(h *harness) {
			h.script = func(ctx context.Context, text string, say func(sdk.SessionUpdate) error) bool {
				defer GinkgoRecover()
				Expect(say(sdk.StartToolCall("c1", "bash", sdk.WithStartKind(sdk.ToolKindExecute)))).To(Succeed())
				_ = say(sdk.UpdateToolCall("c1", sdk.WithUpdateStatus(sdk.ToolCallStatusCompleted),
					sdk.WithUpdateRawOutput(map[string]any{"output": "koinon-test private repository line"})))
				return true
			}
		})
	}

	It("fails the run once, in one epoch, with a named reason when the feed's error is permanent (koinon#123)", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		finishOneTool(e)
		feed := &errFeed{err: fmt.Errorf("upsert fingerprints: %w", &pgconn.PgError{Code: "21000", Message: "koinon-test cardinality"})}
		r := e.enqueue(ctx, e.task(ctx, "feed"))
		e.pool("agentd-feed-permanent", func(c *agentd.Config) { c.Taint = feed })
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateFailed))
		got := getRun(ctx, r)
		Expect(got.Epoch).To(Equal(int64(1)), "no takeover per worker")
		Expect(got.OutcomeReason.String).To(And(ContainSubstring("leak guard"), ContainSubstring("21000")))
		Expect(feed.reports.Load()).To(Equal(int32(1)), "a permanent refusal is not retried")
		Expect(kinds(events(ctx, r))).To(ContainElement("run.failed"))
	})

	It("retries a transient feed error and carries on", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		finishOneTool(e)
		feed := &errFeed{err: &pgconn.PgError{Code: "40001", Message: "koinon-test serialization"}, failFirst: 2}
		r := e.enqueue(ctx, e.task(ctx, "feed"))
		e.pool("agentd-feed-transient", func(c *agentd.Config) { c.Taint = feed })
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		Expect(getRun(ctx, r).Epoch).To(Equal(int64(1)))
		Expect(feed.reports.Load()).To(Equal(int32(3)), "two failures, then the third try recorded it")
	})

	It("leaves the run to the newer worker when its feed is already fenced, as a takeover, not an interruption", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		feed := &fencedFeed{}
		r := e.enqueue(ctx, e.task(ctx, "feed"))
		e.pool("agentd-feed-fenced", func(c *agentd.Config) { c.Taint = feed })
		Eventually(func() []string { ev, _ := feed.snapshot(); return ev }).WithTimeout(racescale.D(10 * time.Second)).Should(ContainElement("start"))
		Consistently(func() []string { return kinds(events(ctx, r)) }).WithTimeout(time.Second).
			ShouldNot(ContainElement("run.interrupted"), "the newer worker drives the run; this one records nothing")
		Expect(runState(ctx, r)()).NotTo(Equal(store.RunStateSucceeded))
	})
})
