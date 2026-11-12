// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"time"

	sdk "github.com/coder/acp-go-sdk"
	"github.com/jackc/pgx/v5"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/stateracloud/koinon/internal/agentd"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/testing/fakemodel"
	"github.com/stateracloud/koinon/internal/testing/racescale"
	"github.com/stateracloud/koinon/internal/tracing/semconv"
)

func turnRows(ctx context.Context, e *env, r store.Run) []store.RunTurn {
	GinkgoHelper()
	rows, err := q.ListRunTurns(ctx, store.ListRunTurnsParams{OrgID: e.org, RunID: r.ID})
	Expect(err).NotTo(HaveOccurred())
	return rows
}

func endReasons(rows []store.RunTurn) []string {
	var out []string
	for _, t := range rows {
		out = append(out, t.EndReason.String)
	}
	return out
}

func rootsOf(spans []*tracepb.Span) []*tracepb.Span {
	var out []*tracepb.Span
	for _, s := range spans {
		if strings.HasPrefix(s.Name, "invoke_agent ") {
			out = append(out, s)
		}
	}
	return out
}

var _ = Describe("run spans: turns, privacy and exporter", func() {
	const markPaths, markLong = "PATHS", "LONG"

	It("names tools only from the first message and only from known names, so no path leaks at level none", func(ctx SpecContext) {
		e := newEnv(ctx, harness.OpenCode)
		e.harness.set(func(h *fakeHarness) {
			h.script = func(ctx context.Context, text string, say func(sdk.SessionUpdate) error) bool {
				defer GinkgoRecover()
				if !strings.Contains(text, markPaths) {
					return false
				}
				// OpenCode: "write" first, the file path as the title of the finished call.
				Expect(say(sdk.StartToolCall("c1", "write", sdk.WithStartKind(sdk.ToolKindEdit), sdk.WithStartRawInput(map[string]any{"filePath": "/sandbox/work/notes.txt"})))).To(Succeed())
				Expect(say(sdk.UpdateToolCall("c1", sdk.WithUpdateTitle("sandbox/work/notes.txt"), sdk.WithUpdateStatus(sdk.ToolCallStatusCompleted)))).To(Succeed())
				// A call whose first title is already a path or a URL.
				Expect(say(sdk.StartToolCall("c2", "/etc/passwd", sdk.WithStartKind(sdk.ToolKindRead)))).To(Succeed())
				Expect(say(sdk.UpdateToolCall("c2", sdk.WithUpdateStatus(sdk.ToolCallStatusCompleted)))).To(Succeed())
				Expect(say(sdk.StartToolCall("c3", "https://example.test/secret?token=x", sdk.WithStartKind(sdk.ToolKindFetch)))).To(Succeed())
				Expect(say(sdk.UpdateToolCall("c3", sdk.WithUpdateStatus(sdk.ToolCallStatusCompleted)))).To(Succeed())
				return true
			}
		})
		rcv := newReceiver()
		r := e.enqueue(ctx, e.task(ctx, markPaths))
		e.pool("agentd-paths", tracingOn(rcv, fakeDecider{traced: true, level: "none"}))
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		Eventually(rcv.count).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(4))
		spans := rcv.snapshot()
		var names []string
		for _, s := range spans {
			names = append(names, s.Name)
		}
		Expect(names).To(ContainElements("execute_tool write", "execute_tool read", "execute_tool fetch"))
		all := allText(spans)
		for _, leak := range []string{"notes.txt", "/etc/passwd", "sandbox/work", "example.test", "token=x"} {
			Expect(all).NotTo(ContainSubstring(leak))
		}
	})

	DescribeTable("lists the ask_agent target as content, and sends it only above none", func(ctx SpecContext, level string) {
		{
			e := newEnv(ctx, harness.OpenCode)
			e.harness.set(func(h *fakeHarness) {
				h.script = func(ctx context.Context, text string, say func(sdk.SessionUpdate) error) bool {
					defer GinkgoRecover()
					Expect(say(sdk.StartToolCall("c1", "koinon_ask_agent", sdk.WithStartRawInput(map[string]any{"agent": "triage"})))).To(Succeed())
					Expect(say(sdk.UpdateToolCall("c1", sdk.WithUpdateStatus(sdk.ToolCallStatusCompleted)))).To(Succeed())
					return true
				}
			})
			rcv := newReceiver()
			r := e.enqueue(ctx, e.task(ctx, "ask"))
			e.pool("agentd-askagent-"+level, tracingOn(rcv, fakeDecider{traced: true, level: level}))
			Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
			Eventually(rcv.count).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(2))
			tool := spanByName(rcv.snapshot(), "execute_tool koinon_ask_agent")[0]
			a := attrMap(tool.Attributes)
			Expect(a).To(HaveKeyWithValue(semconv.GenAITransferMode, "return_to_caller"))
			if level == "none" {
				Expect(a).NotTo(HaveKey(semconv.GenAITransferTargetName))
				Expect(a).NotTo(HaveKey(semconv.KoinonContentAttributes))
			} else {
				Expect(a).To(HaveKeyWithValue(semconv.GenAITransferTargetName, "triage"))
				Expect(a[semconv.KoinonContentAttributes]).To(ContainSubstring(semconv.GenAITransferTargetName))
			}
		}
	},
		Entry("at none", "none"),
		Entry("at full", "full"),
	)

	It("ends a turn with the reason turn() saw, and marks failed turns as errors: retry", func(ctx SpecContext) {
		e := newEnv(ctx, harness.OpenCode)
		srv, err := fakemodel.New(fakemodel.Script{Model: "faulty", Faults: fakemodel.Faults{Unavailable: 1}, Turns: []fakemodel.Turn{{Text: "ok"}}})
		Expect(err).NotTo(HaveOccurred())
		ts := httptest.NewServer(srv)
		DeferCleanup(ts.Close)
		e.harness.modelFault = modelCaller(ts.URL)
		rcv := newReceiver()
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		// agentd's own retry layer, the one for a harness whose harness does not retry.
		e.pool("agentd-retry", tracingOn(rcv, fakeDecider{traced: true, level: "none"}), agentd.OwnRetryLayer, func(c *agentd.Config) { c.RetryBackoff = 10 * time.Millisecond })
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateSucceeded))
		Expect(endReasons(turnRows(ctx, e, r))).To(Equal([]string{"failed", "end_turn"}))
		Eventually(rcv.count).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(2))
		roots := map[string]*tracepb.Span{}
		for _, s := range rootsOf(rcv.snapshot()) {
			roots[attrMap(s.Attributes)[semconv.KoinonTurn]] = s
		}
		Expect(roots["1"].Status.GetCode()).To(Equal(tracepb.Status_STATUS_CODE_ERROR))
		Expect(roots["2"].Status.GetCode()).NotTo(Equal(tracepb.Status_STATUS_CODE_ERROR))
		Expect(attrMap(roots["2"].Attributes)).To(HaveKeyWithValue(semconv.KoinonOutcome, "succeeded"))
		Expect(attrMap(roots["1"].Attributes)).NotTo(HaveKey(semconv.KoinonOutcome))
		Expect(roots["2"].Links).To(HaveLen(1))
		Expect(eventNames(roots["2"])).To(BeEmpty(), "no wait event after a retry")
	})

	It("ends a refused turn as refused", func(ctx SpecContext) {
		e := newEnv(ctx, harness.OpenCode)
		rcv := newReceiver()
		r := e.enqueue(ctx, e.task(ctx, markRefuse+" write the notes"))
		e.pool("agentd-refuse", tracingOn(rcv, fakeDecider{traced: true, level: "none"}))
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		Expect(endReasons(turnRows(ctx, e, r))).To(Equal([]string{"refused", "end_turn"}))
	})

	It("ends a turn interrupted for the wrap-up notice as wrap_up, then the cap as canceled", func(ctx SpecContext) {
		e := newEnv(ctx, harness.OpenCode)
		rcv := newReceiver()
		t := e.task(ctx, markWait)
		r, err := q.CreateRun(ctx, store.CreateRunParams{OrgID: e.org, AgentID: e.agent.ID, TaskID: t.ID, MaxWallClockSeconds: 4})
		Expect(err).NotTo(HaveOccurred())
		e.pool("agentd-wrapup", tracingOn(rcv, fakeDecider{traced: true, level: "none"}), func(c *agentd.Config) { c.WrapUpNotice = 2 * time.Second })
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateCanceled))
		Eventually(func() []string { return endReasons(turnRows(ctx, e, r)) }).WithTimeout(racescale.D(5 * time.Second)).Should(Equal([]string{"wrap_up", "canceled"}))
		Eventually(rcv.count).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(2))
		for _, s := range rootsOf(rcv.snapshot()) {
			if attrMap(s.Attributes)[semconv.KoinonTurn] == "2" {
				Expect(attrMap(s.Attributes)).To(HaveKeyWithValue(semconv.KoinonOutcome, "canceled"))
			}
		}
	})

	It("ends the turn as canceled when the control plane cancels the run", func(ctx SpecContext) {
		e := newEnv(ctx, harness.OpenCode)
		rcv := newReceiver()
		r := e.enqueue(ctx, e.task(ctx, markWait))
		e.pool("agentd-cancel", tracingOn(rcv, fakeDecider{traced: true, level: "none"}))
		Eventually(func() int { p, _, _, _ := e.harness.snapshot(); return len(p) }).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(1))
		_, err := q.CancelRun(ctx, store.CancelRunParams{OrgID: e.org, ID: r.ID, OutcomeReason: "user canceled"})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() []string { return endReasons(turnRows(ctx, e, r)) }).WithTimeout(racescale.D(10 * time.Second)).Should(Equal([]string{"canceled"}))
		Eventually(rcv.count).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(1))
		a := attrMap(rcv.snapshot()[0].Attributes)
		Expect(a).To(HaveKeyWithValue(semconv.KoinonOutcome, "canceled"))
	})

	It("keeps the messages array when content is cut to the pre-cap", func(ctx SpecContext) {
		e := newEnv(ctx, harness.OpenCode)
		e.harness.set(func(h *fakeHarness) {
			h.script = func(ctx context.Context, text string, say func(sdk.SessionUpdate) error) bool {
				defer GinkgoRecover()
				if !strings.Contains(text, markLong) {
					return false
				}
				Expect(say(sdk.UpdateAgentThoughtText(strings.Repeat("think ", 2000)))).To(Succeed())
				Expect(say(sdk.UpdateAgentMessageText(strings.Repeat("\"quoted\"\n", 2000)))).To(Succeed())
				return true
			}
		})
		rcv := newReceiver()
		r := e.enqueue(ctx, e.task(ctx, markLong+strings.Repeat(" word", 3000)))
		e.pool("agentd-long", tracingOn(rcv, fakeDecider{traced: true, level: "full"}, func(t *agentd.Tracing) { t.ContentPreCap = 2048 }))
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		Eventually(rcv.count).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(1))
		a := attrMap(rcv.snapshot()[0].Attributes)
		for _, k := range []string{semconv.GenAIInputMessages, semconv.GenAIOutputMessages} {
			Expect(len(a[k])).To(BeNumerically("<=", 2048), k)
			var msgs []map[string]any
			Expect(json.Unmarshal([]byte(a[k]), &msgs)).To(Succeed(), k+" is still the messages array")
			Expect(msgs).To(HaveLen(1))
			Expect(msgs[0]["parts"]).NotTo(BeEmpty())
			Expect(a[k]).To(ContainSubstring("…[truncated]"))
		}
	})

	It("exports 256 tool spans of 64 KiB content in several batches under the receiver's request cap, gzipped", func(ctx SpecContext) {
		e := newEnv(ctx, harness.OpenCode)
		blob := func() string {
			b := make([]byte, 40<<10)
			_, _ = rand.Read(b) // incompressible, so the wire size is real
			return base64.StdEncoding.EncodeToString(b)
		}
		e.harness.set(func(h *fakeHarness) {
			h.script = func(ctx context.Context, text string, say func(sdk.SessionUpdate) error) bool {
				defer GinkgoRecover()
				if !strings.Contains(text, markLong) {
					return false
				}
				for i := range 256 {
					id := sdk.ToolCallId(fmt.Sprintf("c%d", i))
					Expect(say(sdk.StartToolCall(id, "read", sdk.WithStartKind(sdk.ToolKindRead), sdk.WithStartRawInput(map[string]any{"data": blob()})))).To(Succeed())
					Expect(say(sdk.UpdateToolCall(id, sdk.WithUpdateStatus(sdk.ToolCallStatusCompleted), sdk.WithUpdateRawOutput(map[string]any{"data": blob()})))).To(Succeed())
				}
				return true
			}
		})
		rcv := newReceiver()
		rcv.maxBody = 4 << 20
		reg := agentd.NewMetrics(nil)
		r := e.enqueue(ctx, e.task(ctx, markLong))
		e.pool("agentd-big", tracingOn(rcv, fakeDecider{traced: true, level: "full"}, func(t *agentd.Tracing) { t.QueueSize = 4096; t.BatchSize = 64; t.Interval = time.Second }), func(c *agentd.Config) { c.Metrics = reg })
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(60 * time.Second)).Should(Equal(store.RunStateSucceeded))
		Eventually(rcv.count).WithTimeout(racescale.D(30 * time.Second)).Should(Equal(257))
		Expect(rcv.tooLarge.Load()).To(BeZero(), "no request was refused as too large")
		Expect(rcv.requests.Load()).To(BeNumerically(">=", 3), "several batches")
		Expect(testutil.ToFloat64(reg.TraceSpansDropped.WithLabelValues("export_failed"))).To(BeZero())
	})

	It("counts spans the receiver rejected in a partial success", func(ctx SpecContext) {
		e := newEnv(ctx, harness.OpenCode)
		rcv := newReceiver()
		rcv.reject = 1
		reg := agentd.NewMetrics(nil)
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-partial", tracingOn(rcv, fakeDecider{traced: true, level: "none"}), func(c *agentd.Config) { c.Metrics = reg })
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		Eventually(func() float64 {
			return testutil.ToFloat64(reg.TraceSpansDropped.WithLabelValues("rejected"))
		}).WithTimeout(racescale.D(5 * time.Second)).Should(Equal(1.0))
		Expect(testutil.ToFloat64(reg.TraceSpansDropped.WithLabelValues("export_failed"))).To(BeZero())
	})

	It("ends a turn row the previous owner left open as interrupted, before it starts its own", func(ctx SpecContext) {
		e := newEnv(ctx, harness.OpenCode)
		rcv := newReceiver()
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		_, err := q.InsertRunTurn(ctx, store.InsertRunTurnParams{OrgID: e.org, RunID: r.ID, TraceID: []byte("0123456789abcdef")})
		Expect(err).NotTo(HaveOccurred())
		e.pool("agentd-takeover", tracingOn(rcv, fakeDecider{traced: true, level: "none"}))
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		Eventually(func() []string { return endReasons(turnRows(ctx, e, r)) }).WithTimeout(racescale.D(5 * time.Second)).Should(Equal([]string{"interrupted", "end_turn"}))
	})

	It("adds no turn for a worker that is not the run's owner", func(ctx SpecContext) {
		e := newEnv(ctx, harness.OpenCode)
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-owner")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		_, err := q.InsertOwnedRunTurn(ctx, store.InsertOwnedRunTurnParams{OrgID: e.org, RunID: r.ID, Epoch: getRun(ctx, r).Epoch + 1, TraceID: []byte("0123456789abcdef")})
		Expect(err).To(MatchError(pgx.ErrNoRows))
		_, err = q.InsertOwnedRunTurn(ctx, store.InsertOwnedRunTurnParams{OrgID: e.org, RunID: r.ID, Epoch: getRun(ctx, r).Epoch, TraceID: []byte("0123456789abcdef")})
		Expect(err).To(MatchError(pgx.ErrNoRows), "a finished run takes no turns")
	})
})
