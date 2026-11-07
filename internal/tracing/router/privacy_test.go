// SPDX-License-Identifier: Apache-2.0

package router_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/stateracloud/koinon/internal/keyscli"
	"github.com/stateracloud/koinon/internal/tracedest"
	"github.com/stateracloud/koinon/internal/tracing/export"
	"github.com/stateracloud/koinon/internal/tracing/router"
	"github.com/stateracloud/koinon/internal/tracing/semconv"
)

var _ = Describe("Settings that change while spans wait", func() {
	var (
		g  *gate
		be *backend
		e  *env
		tr turn
		ep string
	)
	setup := func(level string, mod func(*router.Config)) {
		g = newGate()
		be = newBackend()
		e = newEnv(func(c *router.Config) {
			c.Sender = g
			if mod != nil {
				mod(c)
			}
		}, secretsFor(orgA, orgB))
		e.routes.set(uid(orgA), be.route("d1", false, level))
		tr = newTurn(e, orgA, runA, 1)
		ep = be.URL + "/v1/traces"
	}

	It("sends a queued span at the level that holds when it is sent, not when it was queued", func() {
		setup("full", nil)
		hold := make(chan struct{})
		g.mu.Lock()
		g.hold[ep] = hold
		g.mu.Unlock()
		e.post(named(tr.tool(false), "s0"))
		Eventually(func() int { return len(g.times(ep)) }).Should(Equal(1))
		e.post(tr.tool(true)) // queued at full, with content
		Eventually(func() float64 {
			return testutil.ToFloat64(e.r.Metrics().QueueDepth.WithLabelValues("org", "generic"))
		}).Should(Equal(1.0), "the span is queued before the level changes")

		e.routes.set(uid(orgA), be.route("d1", false, "none")) // the owner lowers the level
		close(hold)
		Eventually(func() int { return len(be.spans()) }).Should(Equal(2))
		for _, sp := range be.spans() {
			Expect(has(sp, semconv.GenAIToolCallArguments)).To(BeFalse())
		}
		Expect(string(be.raw())).NotTo(ContainSubstring("curl"))
	})

	It("makes the copy again on every retry", func() {
		setup("full", func(c *router.Config) { c.BackoffBase, c.BackoffMax = 150*time.Millisecond, 150*time.Millisecond })
		g.mu.Lock()
		g.once[ep] = &export.Error{Kind: export.KindServer, Status: 503, Message: "down"}
		g.onceN[ep] = 1
		g.mu.Unlock()
		e.post(tr.tool(true))
		Eventually(func() int { return len(g.times(ep)) }).Should(Equal(1), "the first attempt failed, a retry is waiting")
		e.routes.set(uid(orgA), be.route("d1", false, "none"))
		Eventually(func() int { return len(be.spans()) }).Should(Equal(1))
		Expect(has(be.spans()[0], semconv.GenAIToolCallArguments)).To(BeFalse())
	})

	It("sends nothing to a destination deleted while it fails", func() {
		setup("none", func(c *router.Config) {
			c.RecheckInterval = 10 * time.Millisecond
			c.BackoffBase, c.BackoffMax = 30*time.Millisecond, 30*time.Millisecond
			c.BreakerBase = time.Hour // a paused destination: the sleep must end with the destination
			c.BreakerThreshold = 1
			c.MaxAttempts = 100
		})
		g.mu.Lock()
		g.failing[ep] = &export.Error{Kind: export.KindServer, Status: 503, Message: "down"}
		g.mu.Unlock()
		e.post(named(tr.tool(false), "s0"))
		Eventually(func() int { return len(g.times(ep)) }).Should(Equal(1))

		e.routes.set(uid(orgA)) // deleted; no span of the org arrives to notice
		e.clock.Advance(time.Second)
		Eventually(func() float64 {
			return testutil.ToFloat64(e.r.Metrics().Dropped.WithLabelValues("org", "generic", "destination_removed"))
		}).Should(Equal(1.0))
		g.mu.Lock()
		delete(g.failing, ep)
		g.mu.Unlock()
		Consistently(func() int { return len(be.spans()) }, 200*time.Millisecond).Should(BeZero())
		Eventually(e.r.QueueCount).Should(BeZero())
	})

	It("writes a failing destination's error once and lets its queue go idle", func() {
		setup("none", func(c *router.Config) {
			c.HealthInterval = 10 * time.Millisecond
			c.IdleTTL = 50 * time.Millisecond
			c.MaxAttempts = 3
			c.BreakerBase = time.Millisecond
			c.BackoffBase, c.BackoffMax = time.Millisecond, time.Millisecond
		})
		g.mu.Lock()
		g.failing[ep] = &export.Error{Kind: export.KindServer, Status: 503, Message: "the backend answered HTTP 503 (the backend failed)"}
		g.mu.Unlock()
		e.post(named(tr.tool(false), "s0"))
		// The post is asynchronous: first the batch is tried and lost, then the idle queue goes.
		Eventually(func() float64 {
			return testutil.ToFloat64(e.r.Metrics().Dropped.WithLabelValues("org", "generic", "export_failed"))
		}).Should(Equal(1.0))
		Eventually(e.r.QueueCount).Should(BeZero(), "the worker idled out")
		_, fail, _ := e.health.counts()
		Expect(fail).To(BeNumerically("<=", 2), "the error, and the drop; not one write per interval")
		Consistently(func() int { _, f, _ := e.health.counts(); return f }, 100*time.Millisecond).Should(Equal(fail))
	})

	It("keeps a transfer target's name out at level none", func() {
		setup("none", nil)
		sp := tr.tool(false)
		sp.Attributes = append(sp.Attributes, str(semconv.GenAITransferTargetName, "Payroll Bot"))
		e.post(sp)
		Eventually(func() int { return len(be.spans()) }).Should(Equal(1))
		Expect(string(be.raw())).NotTo(ContainSubstring("Payroll Bot"))
	})

	It("reloads the secret set when a credential changes, and fails closed when it cannot be confirmed", func() {
		secrets := secretsFor(orgA)
		secrets.versioned = true
		g = newGate()
		be = newBackend()
		// The router's clock decides when the set is checked again (ConfigCheck, 1 ms) and when the
		// grace ends: the spec moves it past each, rather than sleeping and hoping the machine kept up
		// (old issue 612).
		e = newEnv(func(c *router.Config) {
			c.Sender = g
			c.SecretsGrace = 30 * time.Second
		}, secrets)
		e.routes.set(uid(orgA), be.route("d1", false, "full"))
		tr = newTurn(e, orgA, runA, 1)
		leak := func(name string, secret string) {
			sp := tr.tool(false)
			sp.Name = name
			sp.Attributes = append(sp.Attributes, str(semconv.GenAIToolCallResult, "found "+secret), strs(semconv.KoinonContentAttributes, semconv.GenAIToolCallResult))
			e.post(sp)
		}
		leak("a", "koinon_test_not-a-real-token-added-later")
		Eventually(func() int { return len(be.spans()) }).Should(Equal(1))
		Expect(av(be.named("a"), semconv.GenAIToolCallResult)).To(ContainSubstring("koinon_test_not-a-real-token-added-later"), "not yet a known secret")

		secrets.set(uid(orgA), fakeOrgSecret, "koinon_test_not-a-real-token-added-later")
		e.clock.Advance(time.Second) // past ConfigCheck: the fingerprint is read again
		leak("b", "koinon_test_not-a-real-token-added-later")
		Eventually(func() int { return len(be.spans()) }).Should(Equal(2))
		Expect(av(be.named("b"), semconv.GenAIToolCallResult)).To(Equal("found [redacted:secret]"))

		secrets.setErr(errors.New("database down"))
		e.clock.Advance(time.Minute) // past the grace
		leak("c", "anything")
		Eventually(func() int { return len(be.spans()) }).Should(Equal(3))
		Expect(has(be.named("c"), semconv.GenAIToolCallResult)).To(BeFalse(), "no content for an org whose secrets are unknown")
	})

	It("counts the spans lost per org on the platform's shared queue", func() {
		g = newGate()
		be = newBackend()
		e = newEnv(func(c *router.Config) { c.Sender = g; c.QueueSize = 2; c.BatchSize = 1 }, secretsFor(orgA, orgB))
		e.routes.set(uid(orgA), be.route("", true, "none"))
		e.routes.set(uid(orgB), be.route("", true, "none"))
		ta := newTurn(e, orgA, runA, 1)
		tb := newTurn(e, orgB, runB, 2)
		ep = be.URL + "/v1/traces"
		hold := make(chan struct{})
		g.mu.Lock()
		g.hold[ep] = hold
		g.mu.Unlock()
		e.post(named(ta.tool(false), "a0"))
		Eventually(func() int { return len(g.times(ep)) }).Should(Equal(1))
		for i := range 5 {
			e.post(named(ta.tool(false), "a"+strconv.Itoa(i+1))) // org A overflows the queue: 3 lost
		}
		Eventually(func() float64 {
			return testutil.ToFloat64(e.r.Metrics().Dropped.WithLabelValues("platform", "generic", "queue_full"))
		}).Should(Equal(3.0))
		e.clock.SetNow(t0.Add(11 * time.Second))
		close(hold)
		e.post(tb.root(false), ta.root(false))
		Eventually(func() int { return e.r.HeldRoots() }).Should(Equal(2))
		finishTurn(e)
		roots := func() map[string]*tracepb.Span {
			out := map[string]*tracepb.Span{}
			for _, sp := range be.spans() {
				if sp.Name == "invoke_agent 0192f5a0-0000-7000-8000-0000000000a1" {
					org, _ := attr(sp, semconv.KoinonOrg)
					out[org] = sp
				}
			}
			return out
		}
		Eventually(func() int { return len(roots()) }).Should(Equal(2))
		Expect(has(roots()[orgB], semconv.KoinonSpansDropped)).To(BeFalse(), "org B lost nothing")
		Expect(has(roots()[orgA], semconv.KoinonSpansDropped)).To(BeTrue(), "org A is told what it lost")
	})

	It("drops a queue's oldest spans when its share of the byte budget is full", func() {
		setup("none", func(c *router.Config) { c.BatchSize = 1; c.QueueBytes = 300 << 10 })
		hold := make(chan struct{})
		g.mu.Lock()
		g.hold[ep] = hold
		g.mu.Unlock()
		padded := func(name string) *tracepb.Span {
			sp := named(tr.tool(false), name)
			sp.Attributes = append(sp.Attributes, str("pad", strings.Repeat("a", 60<<10)))
			return sp
		}
		e.post(padded("s0"))
		Eventually(func() int { return len(g.times(ep)) }).Should(Equal(1))
		for i := range 20 {
			e.post(padded("s" + strconv.Itoa(i+1)))
		}
		Eventually(func() float64 {
			return testutil.ToFloat64(e.r.Metrics().Dropped.WithLabelValues("org", "generic", "queue_full"))
		}).Should(BeNumerically(">=", 12))
		Expect(testutil.ToFloat64(e.r.Metrics().QueueDepth.WithLabelValues("org", "generic"))).To(BeNumerically("<=", 5))
		close(hold)
	})
})

var _ = Describe("Hostile or repeated input", func() {
	It("refuses a gzip bomb before decoding it", func() {
		be := newBackend()
		e := newEnv(nil, secretsFor(orgA))
		e.routes.set(uid(orgA), be.route("d1", false, "none"))

		bomb := func(spans int) []byte {
			// ScopeSpans { Spans: n × (empty span) } inside one ResourceSpans.
			scope := bytes.Repeat([]byte{0x12, 0x00}, spans)
			rs := append([]byte{0x12}, varint(len(scope))...)
			rs = append(rs, scope...)
			req := append([]byte{0x0a}, varint(len(rs))...)
			req = append(req, rs...)
			var gz bytes.Buffer
			zw := gzip.NewWriter(&gz)
			_, _ = zw.Write(req)
			_ = zw.Close()
			return gz.Bytes()
		}
		for _, n := range []int{2_000_000, 12_000_000} { // 4 MB and 24 MB of nothing, a few KB on the wire
			body := bomb(n)
			Expect(len(body)).To(BeNumerically("<", 100<<10))
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, e.srv.URL, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/x-protobuf")
			req.Header.Set("Content-Encoding", "gzip")
			resp, err := http.DefaultClient.Do(req)
			Expect(err).NotTo(HaveOccurred())
			_ = resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusRequestEntityTooLarge))
		}
		Expect(testutil.ToFloat64(e.r.Metrics().Received.WithLabelValues("agentd"))).To(BeZero(), "nothing was decoded")
	})

	It("drops structurally invalid spans before the intake", func() {
		e := newEnv(nil, secretsFor(orgA))
		tr := newTurn(e, orgA, runA, 1)
		short := tr.tool(false)
		short.TraceId = short.TraceId[:5]
		zero := tr.tool(false)
		zero.TraceId = make([]byte, 16)
		badParent := tr.tool(false)
		badParent.ParentSpanId = []byte{1, 2, 3}
		many := tr.tool(false)
		for i := range 300 {
			many.Attributes = append(many.Attributes, &commonpb.KeyValue{Key: "k" + strconv.Itoa(i), Value: strVal("v")})
		}
		resp := e.post(short, zero, badParent, many, tr.tool(false))
		Expect(resp.PartialSuccess.RejectedSpans).To(Equal(int64(4)))
		Expect(testutil.ToFloat64(e.r.Metrics().Discarded.WithLabelValues("invalid"))).To(Equal(4.0))
	})

	It("limits requests in progress", func() {
		e := newEnv(func(c *router.Config) { c.MaxConcurrentRequests = 1 }, secretsFor(orgA))
		pr, pw := newBlockingBody()
		defer func() { _ = pw.Close() }() // never leave the request blocked if the spec fails: the server's Close would wait for it
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, e.srv.URL, pr)
		req.Header.Set("Content-Type", "application/x-protobuf")
		done := make(chan struct{})
		var outcome atomic.Value
		go func() {
			defer close(done)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				outcome.Store("error: " + err.Error())
				return
			}
			outcome.Store("status " + resp.Status)
			_ = resp.Body.Close()
		}()
		defer func() { GinkgoWriter.Println("blocking request outcome:", outcome.Load()) }()
		// Probe only once the receiver says the blocking request holds the slot, not on a timer.
		Eventually(e.r.RequestsInProgress).Should(Equal(1), "the blocking request reached the handler (outcome: %v)", outcome.Load())
		resp := httpDo(http.MethodPost, e.srv.URL, "application/x-protobuf", bytes.NewReader(nil))
		_ = resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusServiceUnavailable))
		_ = pw.Close()
		<-done
	})

	It("emits a turn's model calls once when its root arrives twice", func() {
		be := newBackend()
		e := newEnv(nil, secretsFor(orgA))
		e.routes.set(uid(orgA), be.route("d1", false, "none"))
		tr := newTurn(e, orgA, runA, 1)
		e.clock.SetNow(t0.Add(11 * time.Second))
		e.store.setCalls([]router.ModelCall{call("req-1", 2*time.Second, 0.25)})
		e.post(tr.root(false), tr.root(false)) // a retried export, while the first is held
		Eventually(func() int { return e.r.HeldRoots() }).Should(Equal(1))
		finishTurn(e)
		Eventually(func() int { return len(be.spans()) }).Should(Equal(2))
		e.post(tr.root(false)) // and again, long after it was sent
		Eventually(func() float64 {
			return testutil.ToFloat64(e.r.Metrics().Discarded.WithLabelValues("duplicate_root"))
		}).Should(Equal(2.0))
		finishTurn(e)
		Consistently(func() int { return len(be.spans()) }, 100*time.Millisecond).Should(Equal(2))
	})

	It("counts a row logged just after the turn ended, and not one well after", func() {
		be := newBackend()
		e := newEnv(nil, secretsFor(orgA))
		e.routes.set(uid(orgA), be.route("d1", false, "none"))
		tr := newTurn(e, orgA, runA, 1) // ends at 10 s
		e.clock.SetNow(t0.Add(11 * time.Second))
		e.store.setCalls([]router.ModelCall{call("in", 11*time.Second, 0.25), call("late", 13*time.Second, 0.5)})
		e.post(tr.root(false))
		Eventually(func() int { return e.r.HeldRoots() }).Should(Equal(1))
		finishTurn(e)
		Eventually(func() int { return len(be.spans()) }).Should(Equal(2))
		Expect(av(be.named("invoke_agent Support Bot"), "koinon.model_calls")).To(Equal("1"))
	})
})

var _ = Describe("The secret columns", func() {
	It("cover every envelope Koinon stores, saved work and the platform's own destination excepted", func() {
		have := map[[2]string]bool{}
		for _, c := range router.SecretColumnNames() {
			have[c] = true
		}
		// Saved work is files, not credentials; the platform destination has no org.
		except := map[[2]string]bool{
			{"saved_work", "envelope"}:                             true,
			{tracedest.PlatformTable, tracedest.CredentialsColumn}: true,
		}
		for _, c := range keyscli.Columns {
			k := [2]string{c.Table, c.Column}
			if except[k] {
				continue
			}
			Expect(have).To(HaveKey(k), "%s.%s holds an org's envelopes: the router must scrub them", c.Table, c.Column)
		}
	})
})

func varint(n int) []byte {
	var out []byte
	for n >= 0x80 {
		out = append(out, byte(n)|0x80)
		n >>= 7
	}
	return append(out, byte(n))
}
