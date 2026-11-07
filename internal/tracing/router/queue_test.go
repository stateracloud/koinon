// SPDX-License-Identifier: Apache-2.0

package router_test

import (
	"context"
	"net"
	"strconv"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/stateracloud/koinon/internal/tracing/export"
	"github.com/stateracloud/koinon/internal/tracing/router"
	"github.com/stateracloud/koinon/internal/tracing/semconv"
)

// gate is a Sender that can hold a destination's requests, fail them, and note when each came.
type gate struct {
	mu       sync.Mutex
	inner    router.Sender
	hold     map[string]chan struct{}
	failing  map[string]*export.Error // fails every request
	once     map[string]*export.Error // fails the next n requests
	onceN    map[string]int
	attempts map[string][]time.Time
}

func newGate() *gate {
	client, err := export.NewClient((&net.Dialer{Timeout: time.Second}).DialContext, 2*time.Second)
	Expect(err).NotTo(HaveOccurred())
	return &gate{
		inner: client, hold: map[string]chan struct{}{}, failing: map[string]*export.Error{}, once: map[string]*export.Error{},
		onceN: map[string]int{}, attempts: map[string][]time.Time{},
	}
}

func (g *gate) Export(ctx context.Context, d export.Destination, req *coltracepb.ExportTraceServiceRequest) (export.Result, error) {
	g.mu.Lock()
	g.attempts[d.Endpoint] = append(g.attempts[d.Endpoint], time.Now())
	hold, fe := g.hold[d.Endpoint], g.failing[d.Endpoint]
	if n := g.onceN[d.Endpoint]; n > 0 {
		g.onceN[d.Endpoint] = n - 1
		fe = g.once[d.Endpoint]
	}
	g.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return export.Result{}, &export.Error{Kind: export.KindNetwork, Message: "canceled"}
		}
	}
	if fe != nil {
		return export.Result{}, fe
	}
	return g.inner.Export(ctx, d, req)
}

func (g *gate) times(ep string) []time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]time.Time(nil), g.attempts[ep]...)
}

func named(sp *tracepb.Span, name string) *tracepb.Span { sp.Name = name; return sp }

var _ = Describe("Destination queues", func() {
	var (
		g  *gate
		be *backend
		e  *env
		tr turn
	)
	setup := func(mod func(*router.Config)) {
		g = newGate()
		be = newBackend()
		e = newEnv(func(c *router.Config) {
			c.Sender = g
			if mod != nil {
				mod(c)
			}
		}, secretsFor(orgA, orgB))
		e.routes.set(uid(orgA), be.route("d1", false, "none"))
		tr = newTurn(e, orgA, runA, 1)
	}
	sentNames := func() []string {
		var out []string
		for _, sp := range be.spans() {
			out = append(out, sp.Name)
		}
		return out
	}

	It("drops the oldest span when the queue is full, and the next root says how many", func() {
		setup(func(c *router.Config) { c.QueueSize = 3; c.BatchSize = 1 })
		ep := be.URL + "/v1/traces"
		hold := make(chan struct{})
		g.mu.Lock()
		g.hold[ep] = hold
		g.mu.Unlock()

		e.post(named(tr.tool(false), "s0"))
		Eventually(func() int { return len(g.times(ep)) }).Should(Equal(1), "s0 is in flight")
		m := e.r.Metrics()
		depth := func() float64 { return testutil.ToFloat64(m.QueueDepth.WithLabelValues("org", "generic")) }
		dropped := func() float64 { return testutil.ToFloat64(m.Dropped.WithLabelValues("org", "generic", "queue_full")) }
		// A post returns once its spans are in the intake, and the intake workers attribute spans
		// concurrently, so two posts in a row can reach the queue in either order (old issue 567). Each span is
		// queued (or has pushed the oldest out) before the next is posted, so arrival order is s1..s5.
		for i := 1; i <= 5; i++ {
			e.post(named(tr.tool(false), "s"+strconv.Itoa(i)))
			Eventually(func() float64 { return depth() + dropped() }).Should(Equal(float64(i)), "s%d reached the queue", i)
		}
		Expect(dropped()).To(Equal(2.0))
		Expect(depth()).To(Equal(3.0))
		close(hold)
		Eventually(sentNames).Should(Equal([]string{"s0", "s3", "s4", "s5"}), "s1 and s2, the oldest, were dropped")
		Eventually(depth).Should(BeZero())
		Eventually(func() float64 { return testutil.ToFloat64(m.Exported.WithLabelValues("org", "generic")) }).Should(Equal(4.0))

		e.clock.SetNow(t0.Add(11 * time.Second))
		e.post(tr.root(false))
		Eventually(func() int { return e.r.HeldRoots() }).Should(Equal(1))
		finishTurn(e)
		Eventually(func() *tracepb.Span { return be.named("invoke_agent Support Bot") }).ShouldNot(BeNil())
		Expect(av(be.named("invoke_agent Support Bot"), semconv.KoinonSpansDropped)).To(Equal("2"))

		By("and the count is reported once")
		tr2 := newTurn(e, orgA, runA, 2)
		e.post(tr2.root(false))
		Eventually(func() int { return e.r.HeldRoots() }).Should(Equal(1))
		finishTurn(e)
		Eventually(func() int { return len(be.spans()) }).Should(Equal(6))
		for _, sp := range be.spans() {
			if string(sp.TraceId) == string(tr2.trace[:]) {
				Expect(has(sp, semconv.KoinonSpansDropped)).To(BeFalse())
			}
		}
	})

	It("retries with backoff, pauses a failing destination with a growing breaker, and recovers", func() {
		// The waits the queue asks for after the first attempt, recorded rather than timed: on a loaded
		// machine a 2 ms wait can take longer than the breaker's pause, so only the pauses' lower
		// bounds are measured on the clock (a wait never ends early) (old issue 612).
		var ep string
		var mu sync.Mutex
		var afterFirst []time.Duration
		setup(func(c *router.Config) {
			c.BreakerThreshold, c.BreakerBase, c.BreakerMax = 2, 40*time.Millisecond, time.Second
			c.BackoffBase, c.BackoffMax = time.Millisecond, 2*time.Millisecond
			c.HealthInterval = 20 * time.Millisecond
			c.Sleep = func(ctx context.Context, d time.Duration) error {
				if len(g.times(ep)) == 1 {
					mu.Lock()
					afterFirst = append(afterFirst, d)
					mu.Unlock()
				}
				return timerSleep(ctx, d)
			}
		})
		ep = be.URL + "/v1/traces"
		g.mu.Lock()
		g.failing[ep] = &export.Error{Kind: export.KindServer, Status: 503, Message: "the backend answered HTTP 503 (the backend failed)"}
		g.mu.Unlock()
		e.post(named(tr.tool(false), "s0"))

		Eventually(func() int { return len(g.times(ep)) }).Should(Equal(6), "MaxAttempts")
		ts := g.times(ep)
		gap := func(i int) time.Duration { return ts[i+1].Sub(ts[i]) }
		mu.Lock()
		Expect(afterFirst).NotTo(BeEmpty())
		Expect(afterFirst).To(HaveEach(BeNumerically("<=", 2*time.Millisecond)), "below the threshold only the backoff (and the linger) applies")
		mu.Unlock()
		Expect(gap(1)).To(BeNumerically(">=", 35*time.Millisecond), "the breaker opened")
		Expect(gap(2)).To(BeNumerically(">=", 70*time.Millisecond), "and the pause grows")
		Expect(gap(3)).To(BeNumerically(">=", 140*time.Millisecond))
		m := e.r.Metrics()
		Eventually(func() float64 {
			return testutil.ToFloat64(m.Dropped.WithLabelValues("org", "generic", "export_failed"))
		}).Should(Equal(1.0))
		Eventually(func() int { _, fail, _ := e.health.counts(); return fail }).Should(BeNumerically(">=", 1))

		By("a later span waits out the pause, then goes through and resets the breaker")
		g.mu.Lock()
		delete(g.failing, ep)
		g.mu.Unlock()
		e.post(named(tr.tool(false), "s1"))
		Eventually(sentNames).Should(Equal([]string{"s1"}))
		Eventually(func() int { ok, _, _ := e.health.counts(); return ok }).Should(BeNumerically(">=", 1))
	})

	It("honours Retry-After", func() {
		setup(nil)
		ep := be.URL + "/v1/traces"
		g.mu.Lock()
		g.once[ep] = &export.Error{Kind: export.KindServer, Status: 429, RetryAfter: 150 * time.Millisecond, Message: "rate limited"}
		g.onceN[ep] = 1
		g.mu.Unlock()
		e.post(named(tr.tool(false), "s0"))
		Eventually(sentNames).Should(Equal([]string{"s0"}))
		ts := g.times(ep)
		Expect(ts).To(HaveLen(2))
		Expect(ts[1].Sub(ts[0])).To(BeNumerically(">=", 150*time.Millisecond))
	})

	It("drops a batch the backend refuses, at once, and records the error", func() {
		setup(nil)
		be.setStatus(401)
		e.post(named(tr.tool(false), "s0"))
		m := e.r.Metrics()
		Eventually(func() float64 {
			return testutil.ToFloat64(m.Rejected.WithLabelValues("org", "generic", "batch_refused"))
		}).Should(Equal(1.0))
		Eventually(func() string { e.health.mu.Lock(); defer e.health.mu.Unlock(); return e.health.last }).Should(ContainSubstring("401"))
		_, _, dropped := e.health.counts()
		Expect(dropped).To(BeNumerically(">=", 1))
		Expect(testutil.ToFloat64(m.Dropped.WithLabelValues("org", "generic", "export_failed"))).To(BeZero(), "a refusal is not retried")
	})

	It("does not let one slow destination delay another, or the sender", func() {
		setup(nil)
		beB := newBackend()
		e.routes.set(uid(orgB), beB.route("db", false, "none"))
		tb := newTurn(e, orgB, runB, 2)
		epA := be.URL + "/v1/traces"
		hold := make(chan struct{})
		g.mu.Lock()
		g.hold[epA] = hold
		g.mu.Unlock()
		defer func() {
			select {
			case <-hold:
			default:
				close(hold)
			}
		}()

		e.post(named(tr.tool(false), "a0"))
		Eventually(func() int { return len(g.times(epA)) }).Should(Equal(1), "org A's export is stuck")
		for i := range 20 {
			e.post(named(tr.tool(false), "a"+strconv.Itoa(i+1)))
		}
		e.post(named(tb.tool(false), "b0"))
		// A's export is still held: B's span arriving, and every post above answering, is the proof that
		// neither B's queue nor the sender waited for A. No wall-clock bound (old issue 612).
		Eventually(func() int { return len(beB.spans()) }).Should(Equal(1), "org B's destination is unaffected")
		Expect(be.spans()).To(BeEmpty())
		close(hold)
		Eventually(func() int { return len(be.spans()) }).Should(Equal(21))
	})

	It("writes a destination's health at most every interval", func() {
		// The router's clock is frozen and moved by whole intervals, so the number of intervals the
		// traffic spans is fixed however slowly the machine runs (old issue 612).
		setup(func(c *router.Config) { c.HealthInterval = 300 * time.Millisecond })
		e.clock.Freeze()
		const rounds = 3
		for round := range rounds {
			if round > 0 {
				e.clock.Advance(300 * time.Millisecond)
			}
			for i := range 10 {
				e.post(named(tr.tool(false), "s"+strconv.Itoa(round*10+i)))
			}
			Eventually(func() int { return len(be.spans()) }).Should(Equal((round + 1) * 10))
		}
		ok, _, _ := e.health.counts()
		Expect(ok).To(BeNumerically(">=", 1))
		Expect(ok).To(BeNumerically("<=", rounds), "one write per interval at most, over three intervals")
	})

	It("stops sending to a destination that was removed, even a request in flight", func() {
		setup(nil)
		ep := be.URL + "/v1/traces"
		hold := make(chan struct{})
		g.mu.Lock()
		g.hold[ep] = hold
		g.mu.Unlock()
		e.post(named(tr.tool(false), "s0"))
		Eventually(func() int { return len(g.times(ep)) }).Should(Equal(1))
		e.post(named(tr.tool(false), "s1")) // queued behind the stuck one
		Eventually(func() float64 {
			return testutil.ToFloat64(e.r.Metrics().QueueDepth.WithLabelValues("org", "generic"))
		}).Should(Equal(1.0), "s1 is queued before the destination is deleted")
		e.routes.set(uid(orgA)) // the admin deletes the destination
		e.clock.Advance(time.Second)
		e.post(named(tr.tool(false), "s2"))
		Eventually(func() float64 {
			return testutil.ToFloat64(e.r.Metrics().Discarded.WithLabelValues("no_destination"))
		}).Should(Equal(1.0))
		Eventually(func() float64 {
			return testutil.ToFloat64(e.r.Metrics().Dropped.WithLabelValues("org", "generic", "destination_removed"))
		}).Should(Equal(2.0), "the span in flight and the one behind it")
		close(hold)
		Consistently(sentNames, 100*time.Millisecond).Should(BeEmpty())
	})
})
