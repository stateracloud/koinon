// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sdk "github.com/coder/acp-go-sdk"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/stateracloud/koinon/internal/agentd"
	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/questions"
	"github.com/stateracloud/koinon/internal/testing/racescale"
	"github.com/stateracloud/koinon/internal/tracing/semconv"
)

// fakeToken is a low-entropy fake of the OTLP listener's bearer token.
const fakeToken = "koinon_test_not-a-real-token-0123456789"

// fakeDecider decides every run the same way.
type fakeDecider struct {
	traced bool
	level  string
}

func (d fakeDecider) DecideRun(context.Context, pgtype.UUID, pgtype.UUID) (bool, error) {
	return d.traced, nil
}

// levelDecider is a decider whose content level changes while a run is parked.
type levelDecider struct {
	mu    sync.Mutex
	level string
}

func (d *levelDecider) set(l string) { d.mu.Lock(); d.level = l; d.mu.Unlock() }

func (d *levelDecider) DecideRun(context.Context, pgtype.UUID, pgtype.UUID) (bool, error) {
	return true, nil
}

func (d *levelDecider) ContentLevel(context.Context, pgtype.UUID) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.level, nil
}

func (d fakeDecider) ContentLevel(context.Context, pgtype.UUID) (string, error) { return d.level, nil }

// otlpReceiver is a fake of koinon-api's /v1/traces: it checks the bearer token and keeps the spans.
type otlpReceiver struct {
	*httptest.Server
	mu       sync.Mutex
	spans    []*tracepb.Span
	schemas  []string
	badAuth  int
	requests atomic.Int64
	// maxBody, when set, is the request cap on the wire: a larger request gets 413 and is counted.
	maxBody  int64
	tooLarge atomic.Int64
	// reject, when set, is the number of spans every response says it rejected (a partial success).
	reject int64
	// block makes every request wait until it is closed (a dead or slow receiver).
	block chan struct{}
}

func newReceiver() *otlpReceiver {
	r := &otlpReceiver{}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.requests.Add(1)
		if r.block != nil {
			select {
			case <-r.block:
			case <-req.Context().Done():
				return
			}
		}
		if req.URL.Path != "/v1/traces" {
			http.NotFound(w, req)
			return
		}
		r.mu.Lock()
		if req.Header.Get("Authorization") != "Bearer "+fakeToken {
			r.badAuth++
			r.mu.Unlock()
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		r.mu.Unlock()
		var rd io.Reader = req.Body
		if r.maxBody > 0 {
			rd = io.LimitReader(req.Body, r.maxBody+1)
		}
		body, _ := io.ReadAll(rd)
		if r.maxBody > 0 && int64(len(body)) > r.maxBody {
			r.tooLarge.Add(1)
			http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
			return
		}
		if req.Header.Get("Content-Encoding") == "gzip" {
			zr, err := gzip.NewReader(bytes.NewReader(body))
			if err == nil {
				body, _ = io.ReadAll(zr)
			}
		}
		var msg coltracepb.ExportTraceServiceRequest
		if err := proto.Unmarshal(body, &msg); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		r.mu.Lock()
		for _, rs := range msg.ResourceSpans {
			r.schemas = append(r.schemas, "resource:"+rs.SchemaUrl)
			for _, ss := range rs.ScopeSpans {
				r.schemas = append(r.schemas, "scope:"+ss.SchemaUrl)
				r.spans = append(r.spans, ss.Spans...)
			}
		}
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
		if r.reject > 0 {
			out, _ := proto.Marshal(&coltracepb.ExportTraceServiceResponse{
				PartialSuccess: &coltracepb.ExportTracePartialSuccess{RejectedSpans: r.reject, ErrorMessage: "fake rejection"},
			})
			_, _ = w.Write(out)
		}
	}))
	DeferCleanup(r.Close)
	return r
}

func (r *otlpReceiver) snapshot() []*tracepb.Span {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.spans)
}

func (r *otlpReceiver) count() int { return len(r.snapshot()) }

func tokenFile() string {
	f := filepath.Join(GinkgoT().TempDir(), "token")
	Expect(os.WriteFile(f, []byte(fakeToken+"\n"), 0o600)).To(Succeed())
	return f
}

// tracingOn is a pool mutation that traces runs to rcv.
func tracingOn(rcv *otlpReceiver, d agentd.TraceDecider, mutate ...func(*agentd.Tracing)) func(*agentd.Config) {
	return func(c *agentd.Config) {
		t := &agentd.Tracing{
			Decider: d, Endpoint: rcv.URL, TokenFile: tokenFile(),
			Interval: 50 * time.Millisecond, ExportTimeout: racescale.D(time.Second),
		}
		for _, m := range mutate {
			m(t)
		}
		c.Tracing = t
	}
}

func attrMap(kvs []*commonpb.KeyValue) map[string]string {
	out := map[string]string{}
	for _, kv := range kvs {
		switch v := kv.Value.Value.(type) {
		case *commonpb.AnyValue_StringValue:
			out[kv.Key] = v.StringValue
		case *commonpb.AnyValue_IntValue:
			out[kv.Key] = fmt.Sprint(v.IntValue)
		case *commonpb.AnyValue_BoolValue:
			out[kv.Key] = fmt.Sprint(v.BoolValue)
		case *commonpb.AnyValue_DoubleValue:
			out[kv.Key] = "<number>"
		case *commonpb.AnyValue_ArrayValue:
			var items []string
			for _, it := range v.ArrayValue.Values {
				items = append(items, it.GetStringValue())
			}
			out[kv.Key] = strings.Join(items, ",")
		}
	}
	return out
}

func spanByName(spans []*tracepb.Span, name string) []*tracepb.Span {
	var out []*tracepb.Span
	for _, s := range spans {
		if s.Name == name {
			out = append(out, s)
		}
	}
	return out
}

// Scripted turns: the first prompt thinks, reads a file, asks the user a question and parks; the
// resume says it deploys.
func traceScript(a *askEnv, qid *string) func(ctx context.Context, text string, say func(sdk.SessionUpdate) error) bool {
	return func(ctx context.Context, text string, say func(sdk.SessionUpdate) error) bool {
		defer GinkgoRecover()
		switch {
		case strings.Contains(text, markQuestion):
			Expect(say(sdk.UpdateAgentThoughtText("I should ask which region. "))).To(Succeed())
			Expect(say(sdk.StartToolCall("call-read", "read", sdk.WithStartKind(sdk.ToolKindRead),
				sdk.WithStartRawInput(map[string]any{"path": "README.md"})))).To(Succeed())
			time.Sleep(20 * time.Millisecond)
			Expect(say(sdk.UpdateToolCall("call-read", sdk.WithUpdateStatus(sdk.ToolCallStatusCompleted),
				sdk.WithUpdateRawOutput(map[string]any{"text": "# the widget"})))).To(Succeed())
			Expect(say(sdk.StartToolCall("call-ask", "koinon_ask_user", sdk.WithStartKind(sdk.ToolKindOther),
				sdk.WithStartRawInput(a.args)))).To(Succeed())
			out, id := a.call(ctx, a.args)
			*qid = id
			Expect(say(sdk.UpdateToolCall("call-ask", sdk.WithUpdateStatus(sdk.ToolCallStatusCompleted),
				sdk.WithUpdateRawOutput(map[string]any{"text": out})))).To(Succeed())
			Expect(say(sdk.UpdateAgentMessageText("Asked you."))).To(Succeed())
			return true
		case strings.HasPrefix(text, "[Koinon] Your run was paused"):
			Expect(say(sdk.UpdateAgentThoughtText("They chose a region. "))).To(Succeed())
			Expect(say(sdk.UpdateAgentMessageText("Deploying now."))).To(Succeed())
			return true
		}
		return false
	}
}

// parkedRun runs the scripted task to its park, answers the question and waits for the run.
func parkedRun(ctx SpecContext, a *askEnv, rcv *otlpReceiver, d agentd.TraceDecider, mutate ...func(*agentd.Tracing)) (store.Run, string) {
	return parkedRunWith(ctx, a, rcv, d, nil, mutate...)
}

// parkedRunWith is parkedRun with between, which runs while the run is parked.
func parkedRunWith(ctx SpecContext, a *askEnv, rcv *otlpReceiver, d agentd.TraceDecider, between func(), mutate ...func(*agentd.Tracing)) (store.Run, string) {
	var qid string
	a.harness.set(func(h *fakeHarness) { h.script = traceScript(a, &qid) })
	t := a.task(ctx, markQuestion+": deploy the app")
	r := a.enqueue(ctx, t)
	a.pool("agentd-trace", tracingOn(rcv, d, mutate...))
	Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateParked))
	time.Sleep(300 * time.Millisecond) // a real wait between the turns
	if between != nil {
		between()
	}
	pool := a.serve()
	Expect(resolveIn(ctx, pool, func(q *store.Queries, b audit.Batch) error {
		_, err := questions.Respond(ctx, q, b, a.org, mustUUID(qid), a.user, questions.Answer{Kind: store.QuestionAnswerKindChoice, Text: "us-east-1"})
		return err
	})).To(Succeed())
	Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
	return r, qid
}

var _ = Describe("run spans (ADR 0074)", func() {
	DescribeTable("two turns split by a park",
		func(ctx SpecContext, level string) {
			a := newAskEnv(ctx)
			rcv := newReceiver()
			r, qid := parkedRun(ctx, a, rcv, fakeDecider{traced: true, level: level})
			Eventually(rcv.count).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(4))
			spans := rcv.snapshot()

			rcv.mu.Lock()
			Expect(rcv.badAuth).To(BeZero())
			Expect(rcv.schemas).NotTo(BeEmpty())
			for _, sch := range rcv.schemas {
				Expect(sch).To(HaveSuffix(":" + semconv.SchemaURL))
			}
			rcv.mu.Unlock()

			// The turns, from Postgres: Koinon assigned the trace IDs.
			turns, err := q.ListRunTurns(ctx, store.ListRunTurnsParams{OrgID: a.org, RunID: r.ID})
			Expect(err).NotTo(HaveOccurred())
			Expect(turns).To(HaveLen(2))
			Expect(turns[0].EndReason.String).To(Equal("park:input_required"))
			Expect(turns[1].EndReason.String).To(Equal("end_turn"))
			Expect(turns[0].EndedAt.Valid && turns[1].EndedAt.Valid).To(BeTrue())

			rootName := "invoke_agent " + a.agent.DisplayName
			roots := spanByName(spans, rootName)
			Expect(roots).To(HaveLen(2))
			byTurn := map[string]*tracepb.Span{}
			for _, s := range roots {
				byTurn[attrMap(s.Attributes)[semconv.KoinonTurn]] = s
			}
			root1, root2 := byTurn["1"], byTurn["2"]
			Expect(hex.EncodeToString(root1.TraceId)).To(Equal(hex.EncodeToString(turns[0].TraceID)))
			Expect(hex.EncodeToString(root2.TraceId)).To(Equal(hex.EncodeToString(turns[1].TraceID)))
			Expect(root1.TraceId).NotTo(Equal(root2.TraceId))
			var t1 [16]byte
			copy(t1[:], turns[0].TraceID)
			wantRoot := agentd.RootSpanID(t1)
			Expect(root1.SpanId).To(Equal(wantRoot[:]))
			Expect(root1.ParentSpanId).To(BeEmpty())
			Expect(root1.Kind).To(Equal(tracepb.Span_SPAN_KIND_INTERNAL))

			ra := attrMap(root1.Attributes)
			Expect(ra).To(HaveKeyWithValue(semconv.GenAIOperationName, "invoke_agent"))
			Expect(ra).To(HaveKeyWithValue(semconv.GenAIAgentID, a.agent.ID.String()))
			Expect(ra).To(HaveKeyWithValue(semconv.GenAIAgentName, a.agent.DisplayName))
			Expect(ra).To(HaveKeyWithValue(semconv.GenAIAgentVersion, "1"))
			Expect(ra).To(HaveKeyWithValue(semconv.GenAIRequestModel, model))
			Expect(ra).To(HaveKeyWithValue(semconv.GenAIConversationID, r.ID.String()))
			Expect(ra).To(HaveKeyWithValue(semconv.SessionID, r.ID.String()))
			Expect(ra).To(HaveKeyWithValue(semconv.KoinonRun, r.ID.String()))
			Expect(ra).To(HaveKeyWithValue(semconv.KoinonOrg, a.org.String()))
			Expect(ra).To(HaveKeyWithValue(semconv.KoinonTask, r.TaskID.String()))
			Expect(ra).To(HaveKeyWithValue(semconv.KoinonRequestedByKind, "user"))
			Expect(ra).To(HaveKeyWithValue(semconv.KoinonRequestedByID, a.user.String()))
			Expect(ra).NotTo(HaveKey(semconv.KoinonOutcome), "only the last turn has the outcome")

			// Tool spans are children of their turn, and nothing is open across the park.
			for _, name := range []string{"execute_tool read", "execute_tool koinon_ask_user"} {
				ts := spanByName(spans, name)
				Expect(ts).To(HaveLen(1), name)
				Expect(ts[0].ParentSpanId).To(Equal(root1.SpanId))
				Expect(ts[0].TraceId).To(Equal(root1.TraceId))
				Expect(ts[0].EndTimeUnixNano).To(BeNumerically("<=", root1.EndTimeUnixNano))
				ta := attrMap(ts[0].Attributes)
				Expect(ta).To(HaveKeyWithValue(semconv.GenAIOperationName, "execute_tool"))
				Expect(ta).To(HaveKeyWithValue(semconv.GenAIToolType, "function"))
				Expect(ta).To(HaveKeyWithValue(semconv.KoinonToolStatus, "completed"))
				Expect(ta).To(HaveKeyWithValue(semconv.KoinonRun, r.ID.String()))
			}
			read := spanByName(spans, "execute_tool read")[0]
			Expect(attrMap(read.Attributes)).To(HaveKeyWithValue(semconv.GenAIToolCallID, "call-read"))
			Expect(attrMap(read.Attributes)).To(HaveKeyWithValue(semconv.KoinonToolKind, "read"))
			Expect(read.EndTimeUnixNano).To(BeNumerically(">", read.StartTimeUnixNano), "timed from the first update to the final status")

			// The park ends turn 1; turn 2 links back and says how long the run waited.
			Expect(eventNames(root1)).To(Equal([]string{semconv.EventPark}))
			Expect(attrMap(root1.Events[0].Attributes)).To(HaveKeyWithValue(semconv.KoinonParkReason, "input_required"))
			Expect(root1.Links).To(BeEmpty())
			Expect(root2.Links).To(HaveLen(1))
			Expect(root2.Links[0].TraceId).To(Equal(root1.TraceId))
			Expect(root2.Links[0].SpanId).To(Equal(root1.SpanId))
			Expect(eventNames(root2)).To(Equal([]string{semconv.EventWait}))
			waited := root2.Events[0].TimeUnixNano
			Expect(waited).To(BeNumerically(">=", turns[0].EndedAt.Time.UnixNano()))
			Expect(attrMap(root2.Attributes)).To(HaveKeyWithValue(semconv.KoinonOutcome, "succeeded"))
			Expect(root2.StartTimeUnixNano).To(BeNumerically(">", root1.EndTimeUnixNano), "no span covers the wait")

			// Content: only above none, and marked for the router.
			all := allText(spans)
			if level == "none" {
				for _, key := range []string{semconv.GenAIInputMessages, semconv.GenAIOutputMessages, semconv.GenAIToolCallArguments, semconv.GenAIToolCallResult, semconv.KoinonContentAttributes} {
					for _, s := range spans {
						Expect(attrMap(s.Attributes)).NotTo(HaveKey(key), s.Name)
					}
				}
				for _, secret := range []string{"I should ask which region", "Asked you.", "# the widget", "Which region should I deploy to?", "README.md", "Deploying now"} {
					Expect(all).NotTo(ContainSubstring(secret))
				}
				return
			}
			Expect(ra).To(HaveKeyWithValue(semconv.KoinonContentAttributes, semconv.GenAIInputMessages+","+semconv.GenAIOutputMessages))
			var in, out []map[string]any
			Expect(json.Unmarshal([]byte(ra[semconv.GenAIInputMessages]), &in)).To(Succeed())
			Expect(in).To(HaveLen(1))
			Expect(in[0]["role"]).To(Equal("user"))
			Expect(fmt.Sprint(in[0]["parts"])).To(ContainSubstring(markQuestion))
			Expect(json.Unmarshal([]byte(ra[semconv.GenAIOutputMessages]), &out)).To(Succeed())
			Expect(out).To(HaveLen(1))
			Expect(out[0]["role"]).To(Equal("assistant"))
			Expect(out[0]["finish_reason"]).To(Equal("stop"))
			Expect(out[0]["parts"]).To(Equal([]any{
				map[string]any{"type": "reasoning", "content": "I should ask which region. "},
				map[string]any{"type": "text", "content": "Asked you."},
			}))
			ta := attrMap(read.Attributes)
			Expect(ta).To(HaveKeyWithValue(semconv.KoinonContentAttributes, semconv.GenAIToolCallArguments+","+semconv.GenAIToolCallResult))
			Expect(ta[semconv.GenAIToolCallArguments]).To(MatchJSON(`{"path":"README.md"}`))
			Expect(ta[semconv.GenAIToolCallResult]).To(MatchJSON(`{"text":"# the widget"}`))
			Expect(attrMap(root2.Attributes)[semconv.GenAIOutputMessages]).To(ContainSubstring("Deploying now."))
			Expect(attrMap(root2.Attributes)[semconv.GenAIOutputMessages]).To(ContainSubstring(`"type":"reasoning"`))
			_ = qid
		},
		Entry("at none, no content leaves agentd", "none"),
		Entry("at redacted, content is sent for the router to scrub", "redacted"),
		Entry("at full", "full"),
	)

	It("sends a traceparent hint in the _meta of session/prompt, naming the turn's root", func(ctx SpecContext) {
		a := newAskEnv(ctx)
		rcv := newReceiver()
		r, _ := parkedRun(ctx, a, rcv, fakeDecider{traced: true, level: "none"})
		turns, err := q.ListRunTurns(ctx, store.ListRunTurnsParams{OrgID: a.org, RunID: r.ID})
		Expect(err).NotTo(HaveOccurred())
		metas := a.harness.promptMetas()
		Expect(metas).To(HaveLen(2))
		for i, m := range metas {
			var tid [16]byte
			copy(tid[:], turns[i].TraceID)
			root := agentd.RootSpanID(tid)
			Expect(m["traceparent"]).To(Equal("00-" + hex.EncodeToString(tid[:]) + "-" + hex.EncodeToString(root[:]) + "-01"))
		}
	})

	It("reads the content level for every turn: lowered while parked, the resumed turn sends no content", func(ctx SpecContext) {
		a := newAskEnv(ctx)
		rcv := newReceiver()
		d := &levelDecider{level: "full"}
		r, _ := parkedRunWith(ctx, a, rcv, d, func() { d.set("none") })
		Eventually(rcv.count).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(4))
		_ = r
		for _, s := range rcv.snapshot() {
			a := attrMap(s.Attributes)
			if a[semconv.KoinonTurn] == "1" && s.Name == "execute_tool koinon_ask_user" {
				Expect(a).To(HaveKey(semconv.GenAIToolCallArguments))
			}
			if a[semconv.KoinonTurn] == "2" {
				for _, k := range []string{semconv.GenAIInputMessages, semconv.GenAIOutputMessages, semconv.GenAIToolCallArguments, semconv.KoinonContentAttributes} {
					Expect(a).NotTo(HaveKey(k), s.Name)
				}
			}
		}
		Expect(allText(rcv.snapshot())).NotTo(ContainSubstring("Deploying now"))
		Expect(allText(rcv.snapshot())).To(ContainSubstring("Asked you."), "turn 1 was sent at full")
	})

	It("emits nothing for a run that is not traced", func(ctx SpecContext) {
		e := newEnv(ctx, harness.OpenCode)
		rcv := newReceiver()
		t := e.task(ctx, "say hello")
		r := e.enqueue(ctx, t)
		e.pool("agentd-untraced", tracingOn(rcv, fakeDecider{traced: false, level: "full"}))
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		Consistently(rcv.requests.Load).WithTimeout(500 * time.Millisecond).Should(BeZero())
		turns, err := q.ListRunTurns(ctx, store.ListRunTurnsParams{OrgID: e.org, RunID: r.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(turns).To(BeEmpty())
	})

	It("never delays a run when the receiver does not answer", func(ctx SpecContext) {
		e := newEnv(ctx, harness.OpenCode)
		rcv := newReceiver()
		rcv.block = make(chan struct{})
		DeferCleanup(func() { close(rcv.block) })
		reg := agentd.NewMetrics(nil)
		t := e.task(ctx, "say hello")
		started := time.Now()
		r := e.enqueue(ctx, t)
		stop := e.pool("agentd-blocked", tracingOn(rcv, fakeDecider{traced: true, level: "full"}, func(tr *agentd.Tracing) {
			tr.QueueSize = 1
			tr.ExportTimeout = 400 * time.Millisecond
		}), func(c *agentd.Config) { c.Metrics = reg })
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		Expect(time.Since(started)).To(BeNumerically("<", 5*time.Second), "the run did not wait for the export")
		// The export is tried, times out and the batch is dropped and counted.
		Eventually(rcv.requests.Load).WithTimeout(racescale.D(5 * time.Second)).Should(BeNumerically(">=", 1))
		Eventually(func() float64 {
			return testutil.ToFloat64(reg.TraceSpansDropped.WithLabelValues("export_failed"))
		}).WithTimeout(racescale.D(5 * time.Second)).Should(BeNumerically(">=", 1))
		// Stopping the pool is bounded by the export timeout, not by the receiver.
		begin := time.Now()
		stop()
		Expect(time.Since(begin)).To(BeNumerically("<", 5*time.Second))
	})

	It("drops spans when its queue is full and counts them", func(ctx SpecContext) {
		a := newAskEnv(ctx)
		rcv := newReceiver()
		rcv.block = make(chan struct{})
		DeferCleanup(func() { close(rcv.block) })
		reg := agentd.NewMetrics(nil)
		var qid string
		a.harness.set(func(h *fakeHarness) { h.script = traceScript(a, &qid) })
		t := a.task(ctx, markQuestion+": deploy the app")
		r := a.enqueue(ctx, t)
		a.pool("agentd-full", tracingOn(rcv, fakeDecider{traced: true, level: "none"}, func(tr *agentd.Tracing) {
			tr.QueueSize = 1
			tr.BatchSize = 1
			tr.Interval = time.Hour
			tr.ExportTimeout = 2 * time.Second
		}), func(c *agentd.Config) { c.Metrics = reg })
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateParked))
		Eventually(func() float64 {
			return testutil.ToFloat64(reg.TraceSpansDropped.WithLabelValues("queue_full"))
		}).WithTimeout(racescale.D(5 * time.Second)).Should(BeNumerically(">=", 1))
	})

	It("pins the emitted spans (golden)", func(ctx SpecContext) {
		a := newAskEnv(ctx)
		rcv := newReceiver()
		r, qid := parkedRun(ctx, a, rcv, fakeDecider{traced: true, level: "full"})
		Eventually(rcv.count).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(4))
		golden("trace_spans", normalizeSpans(rcv.snapshot(), map[string]string{
			r.ID.String(): "<run>", r.TaskID.String(): "<task>", a.org.String(): "<org>", a.agent.ID.String(): "<agent-id>",
			a.agent.DisplayName: "<agent-name>", a.user.String(): "<user>", qid: "<question>",
		}))
	})
})

var (
	timeRE      = regexp.MustCompile(`\d{4}-\d\d-\d\dT[\d:.]+Z`)
	taskTitleRE = regexp.MustCompile(`task-\d+`)
)

func eventNames(s *tracepb.Span) []string {
	var out []string
	for _, e := range s.Events {
		out = append(out, e.Name)
	}
	return out
}

func allText(spans []*tracepb.Span) string {
	var b strings.Builder
	for _, s := range spans {
		b.WriteString(s.Name)
		for k, v := range attrMap(s.Attributes) {
			b.WriteString(k + "=" + v + "\n")
		}
	}
	return b.String()
}

// normalizeSpans renders spans as stable text: IDs and times are replaced, spans keep their order.
func normalizeSpans(spans []*tracepb.Span, replace map[string]string) string {
	spans = slices.Clone(spans)
	slices.SortFunc(spans, func(x, y *tracepb.Span) int {
		switch {
		case x.StartTimeUnixNano < y.StartTimeUnixNano:
			return -1
		case x.StartTimeUnixNano > y.StartTimeUnixNano:
			return 1
		}
		return strings.Compare(x.Name, y.Name)
	})
	norm := func(s string) string {
		for from, to := range replace {
			if from != "" {
				s = strings.ReplaceAll(s, from, to)
			}
		}
		s = timeRE.ReplaceAllString(s, "<time>")
		return taskTitleRE.ReplaceAllString(s, "<task-title>")
	}
	traceNo, spanNo := map[string]int{}, map[string]int{}
	tname := func(id []byte) string {
		k := hex.EncodeToString(id)
		if _, ok := traceNo[k]; !ok {
			traceNo[k] = len(traceNo) + 1
		}
		return fmt.Sprintf("trace-%d", traceNo[k])
	}
	sname := func(id []byte) string {
		if len(id) == 0 {
			return "-"
		}
		k := hex.EncodeToString(id)
		if _, ok := spanNo[k]; !ok {
			spanNo[k] = len(spanNo) + 1
		}
		return fmt.Sprintf("span-%d", spanNo[k])
	}
	var b strings.Builder
	for _, s := range spans {
		fmt.Fprintf(&b, "%s kind=%s %s parent=%s\n", norm(s.Name), strings.TrimPrefix(s.Kind.String(), "SPAN_KIND_"), tname(s.TraceId), sname(s.ParentSpanId))
		fmt.Fprintf(&b, "  id=%s\n", sname(s.SpanId))
		attrs := attrMap(s.Attributes)
		keys := make([]string, 0, len(attrs))
		for k := range attrs {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "  %s = %s\n", k, norm(attrs[k]))
		}
		for _, e := range s.Events {
			ea := attrMap(e.Attributes)
			fmt.Fprintf(&b, "  event %s", e.Name)
			ks := make([]string, 0, len(ea))
			for k := range ea {
				ks = append(ks, k)
			}
			slices.Sort(ks)
			for _, k := range ks {
				fmt.Fprintf(&b, " %s=%s", k, ea[k])
			}
			b.WriteString("\n")
		}
		for _, l := range s.Links {
			fmt.Fprintf(&b, "  link %s %s\n", tname(l.TraceId), sname(l.SpanId))
		}
	}
	return b.String()
}
