// SPDX-License-Identifier: Apache-2.0

package router_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/stateracloud/koinon/internal/otlpauth"
	"github.com/stateracloud/koinon/internal/tracing/export"
	"github.com/stateracloud/koinon/internal/tracing/router"
	"github.com/stateracloud/koinon/internal/tracing/semconv"
)

const (
	orgA = "0192f5a0-0000-7000-8000-00000000000a"
	orgB = "0192f5a0-0000-7000-8000-00000000000b"
	runA = "0192f5a0-0000-7000-8000-0000000000a1"
	runB = "0192f5a0-0000-7000-8000-0000000000b1"
)

func settle(e *env) { e.r.Sweep(context.Background(), e.clock.Now()) }

// finishTurn releases the root of a held turn: two polls that agree.
func finishTurn(e *env) {
	settle(e)
	e.clock.Advance(2 * time.Second)
	settle(e)
}

func secretsFor(orgs ...string) *fakeSecrets {
	f := &fakeSecrets{sets: map[pgtype.UUID][]string{}}
	for _, o := range orgs {
		f.sets[uid(o)] = []string{fakeOrgSecret}
	}
	return f
}

var _ = Describe("Router", func() {
	Describe("content levels", func() {
		DescribeTable("one tool span per level",
			func(level string, wantContent bool, wantEmail bool) {
				be := newBackend()
				e := newEnv(nil, secretsFor(orgA))
				e.routes.set(uid(orgA), be.route("d1", false, level))
				tr := newTurn(e, orgA, runA, 1)
				e.post(tr.tool(true))
				Eventually(func() int { return len(be.spans()) }).Should(Equal(1))

				sp := be.spans()[0]
				Expect(has(sp, semconv.KoinonContentAttributes)).To(BeFalse(), "the marker never leaves")
				args, hasArgs := attr(sp, semconv.GenAIToolCallArguments)
				res, hasRes := attr(sp, semconv.GenAIToolCallResult)
				Expect(hasArgs).To(Equal(wantContent))
				Expect(hasRes).To(Equal(wantContent))
				if wantContent {
					Expect(args).To(ContainSubstring("[redacted:"))
					Expect(res).To(ContainSubstring("sent to"))
					Expect(strings.Contains(res, fakeEmail)).To(Equal(wantEmail), "an email is removed at redacted only")
				}
				// A credential is removed from everything, at every level, content or not.
				raw := string(be.raw())
				Expect(raw).NotTo(ContainSubstring(fakeGatewayKey))
				Expect(raw).NotTo(ContainSubstring(fakeOrgSecret))
				if !wantContent {
					Expect(raw).NotTo(ContainSubstring("curl"), "no content at none")
					Expect(raw).NotTo(ContainSubstring("sent to"))
				}
				status, _ := attr(sp, "koinon.tool.status")
				Expect(status).To(Equal("failed with [redacted:secret]"))
			},
			Entry("none", "none", false, false),
			Entry("redacted", "redacted", true, false),
			Entry("full", "full", true, true),
		)

		It("treats a content attribute the span did not list as content, so none still removes it", func() {
			be := newBackend()
			e := newEnv(nil, secretsFor(orgA))
			e.routes.set(uid(orgA), be.route("d1", false, "none"))
			tr := newTurn(e, orgA, runA, 1)
			sp := tr.tool(false)
			sp.Attributes = append(sp.Attributes, str(semconv.GenAIToolCallResult, "unlisted"))
			e.post(sp)
			Eventually(func() int { return len(be.spans()) }).Should(Equal(1))
			Expect(has(be.spans()[0], semconv.GenAIToolCallResult)).To(BeFalse())
		})

		It("cuts a content attribute to 16 KiB and keeps it valid JSON", func() {
			be := newBackend()
			e := newEnv(nil, secretsFor(orgA))
			e.routes.set(uid(orgA), be.route("d1", false, "full"))
			tr := newTurn(e, orgA, runA, 1)
			sp := tr.tool(false)
			big := `{"output":"` + strings.Repeat("x", 60<<10) + `"}`
			sp.Attributes = append(sp.Attributes, str(semconv.GenAIToolCallResult, big), strs(semconv.KoinonContentAttributes, semconv.GenAIToolCallResult))
			e.post(sp)
			Eventually(func() int { return len(be.spans()) }).Should(Equal(1))
			res, _ := attr(be.spans()[0], semconv.GenAIToolCallResult)
			Expect(len(res)).To(BeNumerically("<=", 16<<10))
			Expect(res).To(HavePrefix(`{"output":"xxx`))
		})

		It("sends no content when the org's secrets cannot be loaded", func() {
			be := newBackend()
			e := newEnv(nil, &fakeSecrets{err: context.DeadlineExceeded})
			e.routes.set(uid(orgA), be.route("d1", false, "full"))
			tr := newTurn(e, orgA, runA, 1)
			e.post(tr.tool(true))
			Eventually(func() int { return len(be.spans()) }).Should(Equal(1))
			Expect(has(be.spans()[0], semconv.GenAIToolCallArguments)).To(BeFalse(), "fail closed")
		})
	})

	Describe("attribution", func() {
		It("drops spans of an unknown run, a foreign trace and an untraced run, and counts them", func() {
			be := newBackend()
			e := newEnv(nil, secretsFor(orgA))
			e.routes.set(uid(orgA), be.route("d1", false, "full"))
			tr := newTurn(e, orgA, runA, 1)

			unknownRun := tr.tool(false)
			unknownRun.Attributes = append(unknownRun.Attributes[:0:0], unknownRun.Attributes...)
			unknownRun.Attributes[0] = str(semconv.KoinonRun, "0192f5a0-0000-7000-8000-0000000000ff")

			foreign := newTurn(e, orgA, runA, 2) // a real turn of runA ...
			other := tr.tool(false)              // ... but the span says it is trace 1 of org B
			other.Attributes[1] = str(semconv.KoinonOrg, orgB)

			wrongTrace := tr.tool(false)
			wrongTrace.TraceId = []byte("0123456789abcdef") // not one of the run's turns
			_ = foreign

			untracedRun := uid(runB)
			var trace [16]byte
			trace[0] = 9
			e.store.addRun(uid(orgA), untracedRun, false, trace, router.Turn{Seq: 1, StartedAt: t0})
			untraced := (turn{org: uid(orgA), run: untracedRun, trace: trace, start: t0, end: t0}).tool(false)

			noIDs := tr.tool(false)
			noIDs.Attributes = noIDs.Attributes[2:]

			e.post(unknownRun, other, wrongTrace, untraced, noIDs)
			m := e.r.Metrics()
			Eventually(func() float64 {
				return testutil.ToFloat64(m.Discarded.WithLabelValues("unknown_run")) + testutil.ToFloat64(m.Discarded.WithLabelValues("unknown_trace")) +
					testutil.ToFloat64(m.Discarded.WithLabelValues("untraced")) + testutil.ToFloat64(m.Discarded.WithLabelValues("unattributed"))
			}).Should(Equal(5.0))
			Expect(testutil.ToFloat64(m.Discarded.WithLabelValues("untraced"))).To(Equal(1.0))
			Expect(testutil.ToFloat64(m.Discarded.WithLabelValues("unattributed"))).To(Equal(1.0))
			Consistently(func() int { return len(be.spans()) }, 100*time.Millisecond).Should(BeZero())
			Expect(testutil.ToFloat64(m.Received.WithLabelValues("agentd"))).To(Equal(5.0))
		})
	})

	Describe("model calls from the ledger", func() {
		var (
			be *backend
			e  *env
			tr turn
		)
		BeforeEach(func() {
			be = newBackend()
			e = newEnv(nil, secretsFor(orgA))
			e.routes.set(uid(orgA), be.route("d1", false, "full"))
			tr = newTurn(e, orgA, runA, 1)
			e.clock.SetNow(t0.Add(11 * time.Second))
		})

		It("holds the root, then sends it with the chat spans and the turn's totals", func() {
			e.store.setCalls([]router.ModelCall{call("req-1", 2*time.Second, 0.25), call("req-2", 5*time.Second, 0.5), call("outside", 20*time.Second, 9)})
			e.post(tr.root(false), tr.tool(false))
			Eventually(func() int { return len(be.spans()) }).Should(Equal(1), "the tool span goes at once")
			Expect(be.named("invoke_agent Support Bot")).To(BeNil(), "the root waits for the ledger")
			// The root is held asynchronously, by another intake worker than the tool span's: a sweep
			// before it is held finds nothing, and no later sweep would release it (old issue 396, old issue 612).
			Eventually(func() int { return e.r.HeldRoots() }).Should(Equal(1))

			settle(e) // the first poll finds two rows
			Consistently(func() int { return len(be.spans()) }, 50*time.Millisecond).Should(Equal(1))
			e.clock.Advance(2 * time.Second)
			settle(e) // the second agrees: released
			Eventually(func() int { return len(be.spans()) }).Should(Equal(4))

			root := be.named("invoke_agent Support Bot")
			chats := 0
			for _, sp := range be.spans() {
				if sp.Name != "chat claude-sonnet" {
					continue
				}
				chats++
				Expect(sp.Kind).To(Equal(tracepb.Span_SPAN_KIND_CLIENT))
				Expect(sp.ParentSpanId).To(Equal(root.SpanId))
				Expect(sp.TraceId).To(Equal(root.TraceId))
				Expect(av(sp, semconv.GenAIProviderName)).To(Equal("anthropic"))
				Expect(av(sp, semconv.GenAIUsageInputTokens)).To(Equal("1000"))
				Expect(av(sp, semconv.GenAIUsageCacheReadTokens)).To(Equal("600"))
				Expect(av(sp, semconv.GenAIUsageCacheWriteTokens)).To(Equal("100"))
				Expect(av(sp, semconv.GenAIUsageReasoningTokens)).To(Equal("50"))
				Expect(has(sp, semconv.KoinonCostPending)).To(BeFalse())
				Expect(av(sp, semconv.KoinonOrg)).To(Equal(orgA))
				Expect(av(sp, semconv.KoinonRun)).To(Equal(runA))
			}
			Expect(chats).To(Equal(2))
			Expect(av(root, semconv.GenAIUsageCost)).To(Equal("0.75"))
			Expect(av(root, semconv.KoinonCostUSD)).To(Equal("0.75"))
			Expect(av(root, semconv.GenAIUsageInputTokens)).To(Equal("2000"))
			Expect(av(root, "koinon.model_calls")).To(Equal("2"))
			Expect(has(root, semconv.KoinonCostPending)).To(BeFalse())
			Expect(testutil.ToFloat64(e.r.Metrics().Received.WithLabelValues("ledger"))).To(Equal(2.0))
		})

		It("leaves the reasoning tokens out when no call reported a split, rather than write 0 (old issue 316)", func() {
			unsplit := func(id string, at time.Duration) router.ModelCall {
				c := call(id, at, 0.25)
				c.Reasoning = 0
				return c
			}
			e.store.setCalls([]router.ModelCall{unsplit("req-1", 2*time.Second), unsplit("req-2", 5*time.Second)})
			e.post(tr.root(false))
			// The root is held asynchronously after the POST returns. A sweep that runs
			// before it is held finds nothing, and the fake clock never sweeps again, so
			// the spec would wait for spans that no later sweep releases (old issue 396).
			Eventually(func() int { return e.r.HeldRoots() }).Should(Equal(1))
			finishTurn(e)
			Eventually(func() int { return len(be.spans()) }).Should(Equal(3))
			for _, sp := range be.spans() {
				Expect(has(sp, semconv.GenAIUsageReasoningTokens)).To(BeFalse(), sp.Name)
			}
			Expect(av(be.named("invoke_agent Support Bot"), semconv.GenAIUsageOutputTokens)).NotTo(BeEmpty(), "the output tokens, thinking included, are still there")
		})

		It("writes a reported split of 0, since the provider said so (#78)", func() {
			reported := func(id string, at time.Duration) router.ModelCall {
				c := call(id, at, 0.25)
				c.Reasoning, c.ReasoningKnown = 0, true
				return c
			}
			e.store.setCalls([]router.ModelCall{reported("req-1", 2*time.Second), reported("req-2", 5*time.Second)})
			e.post(tr.root(false))
			Eventually(func() int { return e.r.HeldRoots() }).Should(Equal(1))
			finishTurn(e)
			Eventually(func() int { return len(be.spans()) }).Should(Equal(3))
			for _, sp := range be.spans() {
				Expect(av(sp, semconv.GenAIUsageReasoningTokens)).To(Equal("0"), sp.Name)
			}
		})

		It("waits up to 30 seconds for a late row and marks a cost still missing as pending", func() {
			nocost := call("req-late", 3*time.Second, 0)
			nocost.Cost = nil
			e.store.setCalls([]router.ModelCall{call("req-1", 2*time.Second, 0.25), nocost})
			// The 30 s are counted on the router's clock from when the root was held. A clock that also
			// ran with real time would leave the "still waiting at 27 s" check a margin of 3 s of the
			// spec's own run time, which a loaded machine can use up (#134); frozen, it moves only by
			// the steps below.
			e.clock.Freeze()
			e.post(tr.root(false))
			Eventually(func() int { return e.r.HeldRoots() }).Should(Equal(1))

			for _, step := range []time.Duration{0, 2 * time.Second, 10 * time.Second, 15 * time.Second} { // 27 s in all
				e.clock.Advance(step)
				settle(e)
			}
			Consistently(func() int { return len(be.spans()) }, 50*time.Millisecond).Should(BeZero(), "still waiting at 27 s")

			e.clock.Advance(4 * time.Second) // 31 s
			settle(e)
			Eventually(func() int { return len(be.spans()) }).Should(Equal(3))
			late := be.named("chat claude-sonnet")
			var pending, priced int
			for _, sp := range be.spans() {
				if sp.Name != "chat claude-sonnet" {
					continue
				}
				if has(sp, semconv.KoinonCostPending) {
					pending++
					Expect(has(sp, semconv.GenAIUsageCost)).To(BeFalse())
				} else {
					priced++
				}
			}
			_ = late
			Expect([]int{pending, priced}).To(Equal([]int{1, 1}))
			root := be.named("invoke_agent Support Bot")
			Expect(av(root, semconv.KoinonCostPending)).To(Equal("true"))
			Expect(av(root, semconv.GenAIUsageCost)).To(Equal("0.25"))
		})

		It("releases at once when a late row arrives and the figures settle", func() {
			e.store.setCalls([]router.ModelCall{call("req-1", 2*time.Second, 0.25)})
			e.post(tr.root(false))
			Eventually(func() int { return e.r.HeldRoots() }).Should(Equal(1))
			settle(e)
			e.store.setCalls([]router.ModelCall{call("req-1", 2*time.Second, 0.25), call("req-2", 4*time.Second, 0.25)}) // arrives after the first poll
			e.clock.Advance(2 * time.Second)
			settle(e)
			Consistently(func() int { return len(be.spans()) }, 50*time.Millisecond).Should(BeZero(), "the set changed, so one more poll")
			e.clock.Advance(2 * time.Second)
			settle(e)
			Eventually(func() int { return len(be.spans()) }).Should(Equal(3))
			Expect(av(be.named("invoke_agent Support Bot"), semconv.GenAIUsageCost)).To(Equal("0.5"))
		})

		It("sends the root without totals when the ledger cannot be read, rather than losing the turn", func() {
			e.store.failCalls = true
			e.post(tr.root(false))
			Eventually(func() int { return e.r.HeldRoots() }).Should(Equal(1))
			e.clock.Advance(31 * time.Second)
			settle(e)
			Eventually(func() int { return len(be.spans()) }).Should(Equal(1))
		})

		It("holds a bounded number of roots and releases the rest at once", func() {
			e2 := newEnv(func(c *router.Config) { c.MaxHeld = 1 }, secretsFor(orgA))
			e2.routes.set(uid(orgA), be.route("d1", false, "full"))
			t1 := newTurn(e2, orgA, runA, 1)
			t2 := newTurn(e2, orgA, runA, 2)
			e2.post(t1.root(false))
			Eventually(func() int { return e2.r.HeldRoots() }).Should(Equal(1))
			e2.post(t2.root(false))
			Eventually(func() int { return len(be.spans()) }).Should(Equal(1), "the second root did not fit and went out without waiting")
			Expect(e2.r.HeldRoots()).To(Equal(1))
		})
	})

	Describe("isolation", func() {
		It("sends each org's spans only to its own destinations, and the platform's copy has IDs and its own level", func() {
			beA, beB, beP := newBackend(), newBackend(), newBackend()
			e := newEnv(nil, secretsFor(orgA, orgB))
			// Org A: full; its platform copy none. Org B: redacted; its platform copy redacted.
			e.routes.set(uid(orgA), beA.route("da", false, "full"), beP.route("", true, "none"))
			e.routes.set(uid(orgB), beB.route("db", false, "redacted"), beP.route("", true, "redacted"))
			ta := newTurn(e, orgA, runA, 1)
			tb := newTurn(e, orgB, runB, 2)
			// Content unique to each org, to find any crossing.
			ra, rb := ta.root(true), tb.root(true)
			ra.Attributes[len(ra.Attributes)-2] = str(semconv.GenAIInputMessages, `[{"role":"user","parts":[{"type":"text","content":"alpha-only-secret-plan"}]}]`)
			rb.Attributes[len(rb.Attributes)-2] = str(semconv.GenAIInputMessages, `[{"role":"user","parts":[{"type":"text","content":"bravo-only-secret-plan"}]}]`)
			e.store.setCalls([]router.ModelCall{call("req-a", time.Second, 0.1)})
			e.post(ra, ta.tool(true), rb, tb.tool(true))
			e.clock.Advance(11 * time.Second)
			Eventually(func() int { return e.r.HeldRoots() }).Should(Equal(2))
			finishTurn(e)

			Eventually(func() int { return len(beA.spans()) }).Should(Equal(3)) // root, tool, chat
			Eventually(func() int { return len(beB.spans()) }).Should(Equal(3))
			Eventually(func() int { return len(beP.spans()) }).Should(Equal(6))

			for _, sp := range beA.spans() {
				Expect(av(sp, semconv.KoinonOrg)).To(Equal(orgA))
			}
			for _, sp := range beB.spans() {
				Expect(av(sp, semconv.KoinonOrg)).To(Equal(orgB))
			}
			Expect(string(beA.raw())).NotTo(ContainSubstring("bravo-only"))
			Expect(string(beB.raw())).NotTo(ContainSubstring("alpha-only"))
			Expect(string(beB.raw())).NotTo(ContainSubstring(orgA))
			Expect(string(beA.raw())).NotTo(ContainSubstring(orgB))

			By("the org's own copy has names, the platform's has IDs")
			Expect(beA.named("invoke_agent Support Bot")).NotTo(BeNil())
			Expect(string(beP.raw())).NotTo(ContainSubstring("Support Bot"))
			pRoot := beP.named("invoke_agent 0192f5a0-0000-7000-8000-0000000000a1")
			Expect(pRoot).NotTo(BeNil())
			Expect(av(pRoot, semconv.GenAIAgentName)).To(Equal("0192f5a0-0000-7000-8000-0000000000a1"))

			By("the platform's content follows each org's opt-in")
			seenA, seenB := false, false
			for _, sp := range beP.spans() {
				org, _ := attr(sp, semconv.KoinonOrg)
				_, content := attr(sp, semconv.GenAIInputMessages)
				_, toolContent := attr(sp, semconv.GenAIToolCallArguments)
				switch org {
				case orgA:
					seenA = true
					Expect(content || toolContent).To(BeFalse(), "org A opted in to none")
				case orgB:
					seenB = true
				}
			}
			Expect(seenA && seenB).To(BeTrue())
			Expect(string(beP.raw())).NotTo(ContainSubstring("alpha-only"))
			Expect(string(beP.raw())).To(ContainSubstring("bravo-only"))
			Expect(string(beP.raw())).NotTo(ContainSubstring(fakeEmail), "redacted for org B")
			Expect(string(beP.raw())).NotTo(ContainSubstring(fakeGatewayKey))
		})
	})

	Describe("the receiver", func() {
		It("takes OTLP protobuf, also gzipped, and refuses JSON", func() {
			be := newBackend()
			e := newEnv(nil, secretsFor(orgA))
			e.routes.set(uid(orgA), be.route("d1", false, "none"))
			tr := newTurn(e, orgA, runA, 1)

			req := &coltracepb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{tr.tool(false)}}}}}}
			body, _ := proto.Marshal(req)
			var gz bytes.Buffer
			zw := gzip.NewWriter(&gz)
			_, _ = zw.Write(body)
			_ = zw.Close()
			r, _ := http.NewRequest(http.MethodPost, e.srv.URL, &gz)
			r.Header.Set("Content-Type", "application/x-protobuf")
			r.Header.Set("Content-Encoding", "gzip")
			resp, err := http.DefaultClient.Do(r)
			Expect(err).NotTo(HaveOccurred())
			_ = resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(200))
			Eventually(func() int { return len(be.spans()) }).Should(Equal(1))

			resp = httpDo(http.MethodPost, e.srv.URL, "application/json", strings.NewReader("{}"))
			_ = resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(415))
			resp = httpDo(http.MethodPost, e.srv.URL, "application/x-protobuf", strings.NewReader("not protobuf \xff\xff"))
			_ = resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(400))
			resp = httpDo(http.MethodGet, e.srv.URL, "", nil)
			_ = resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(405))
		})

		It("answers at once and reports a full intake as rejected spans", func() {
			// Not started: nothing drains the intake.
			client, _ := export.NewClient((&net.Dialer{}).DialContext, time.Second)
			r, err := router.New(router.Config{Store: newStore(), Routes: &fakeRoutes{}, Health: &fakeHealth{}, Sender: client, IngestQueue: 2, Metrics: router.NewMetrics(nil)})
			Expect(err).NotTo(HaveOccurred())
			srv := httptest.NewServer(r.Handler())
			DeferCleanup(srv.Close)
			tr := turn{org: uid(orgA), run: uid(runA), trace: [16]byte{1}}
			var spans []*tracepb.Span
			for range 5 {
				spans = append(spans, tr.tool(false))
			}
			req := &coltracepb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: spans}}}}}
			body, _ := proto.Marshal(req)
			// Nothing drains the intake, so a handler that waited for room would never answer: the
			// answer itself is the proof (httpDo gives up after hangBound) (old issue 612).
			resp := httpDo(http.MethodPost, srv.URL, "application/x-protobuf", bytes.NewReader(body))
			defer func() { _ = resp.Body.Close() }()
			var out coltracepb.ExportTraceServiceResponse
			buf := new(bytes.Buffer)
			_, _ = buf.ReadFrom(resp.Body)
			Expect(proto.Unmarshal(buf.Bytes(), &out)).To(Succeed())
			Expect(out.PartialSuccess.RejectedSpans).To(Equal(int64(3)))
		})

		It("sits inside the listener's bearer-token check", func() {
			be := newBackend()
			e := newEnv(nil, secretsFor(orgA))
			e.routes.set(uid(orgA), be.route("d1", false, "none"))
			tr := newTurn(e, orgA, runA, 1)
			dir := GinkgoT().TempDir()
			const token = "koinon_test_not-a-real-token-otlp-0001"
			Expect(os.WriteFile(filepath.Join(dir, "token"), []byte(token), 0o600)).To(Succeed())
			tokens, err := otlpauth.NewTokens(dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
			Expect(err).NotTo(HaveOccurred())
			// The order agentgateway.Serve applies in koinon-api: the token check outermost.
			h := otlpauth.Middleware(tokens, otlpauth.NewMetrics(nil))(e.r.Mount()(http.NotFoundHandler()))
			srv := httptest.NewServer(h)
			DeferCleanup(srv.Close)

			body, _ := proto.Marshal(&coltracepb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{tr.tool(false)}}}}}})
			do := func(auth string) int {
				r, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/traces", bytes.NewReader(body))
				r.Header.Set("Content-Type", "application/x-protobuf")
				if auth != "" {
					r.Header.Set("Authorization", auth)
				}
				resp, err := http.DefaultClient.Do(r)
				Expect(err).NotTo(HaveOccurred())
				_ = resp.Body.Close()
				return resp.StatusCode
			}
			Expect(do("")).To(Equal(401))
			Expect(do("Bearer wrong")).To(Equal(401))
			Consistently(func() int { return len(be.spans()) }, 50*time.Millisecond).Should(BeZero())
			Expect(do("Bearer " + token)).To(Equal(200))
			Eventually(func() int { return len(be.spans()) }).Should(Equal(1))
		})

		It("mounts at /v1/traces and passes other paths on", func() {
			e := newEnv(nil, secretsFor())
			var other atomic.Int32
			h := e.r.Mount()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { other.Add(1) }))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/logs", nil))
			Expect(other.Load()).To(Equal(int32(1)))
			rec = httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/traces", strings.NewReader("")))
			Expect(other.Load()).To(Equal(int32(1)))
			Expect(rec.Code).To(Equal(415))
		})
	})
})
