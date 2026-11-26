// SPDX-License-Identifier: Apache-2.0

package stateclient_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/route"
	"github.com/stateracloud/koinon/internal/gateway/stateclient"
	"github.com/stateracloud/koinon/internal/gateway/staterev"
	"github.com/stateracloud/koinon/internal/gateway/stateserver"
	gwv1 "github.com/stateracloud/koinon/internal/gen/koinon/gateway/v1alpha1"
)

func TestStateClient(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Gateway State client")
}

const token = "koinon_test_not-a-real-token-gateway-0001"

// memStore is an in-memory stateserver.Store.
type memStore struct {
	mu   sync.Mutex
	cur  *gwv1.StateUpdate
	acks []ack
	// failAcks makes Ack fail while > 0, counting down.
	failAcks int
	// unknownAcks makes Ack answer ErrUnknownRevision (FailedPrecondition on the wire) while > 0.
	unknownAcks int
}

type ack struct {
	copyID string
	rev    int64
	half   gwv1.Half
}

func (m *memStore) publish(rev int64, name string) {
	var s gateway.State
	s.MCP = []gateway.MCPTarget{{Name: name, URL: "http://" + name + "/mcp"}}
	u := staterev.Snapshot(s)
	u.Revision = rev
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cur = u
}

func (m *memStore) Latest(context.Context) (*gwv1.StateUpdate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cur, nil
}

func (m *memStore) Ack(_ context.Context, copyID string, rev int64, half gwv1.Half) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.unknownAcks > 0 {
		m.unknownAcks--
		return stateserver.ErrUnknownRevision
	}
	if m.failAcks > 0 {
		m.failAcks--
		return errors.New("database unavailable")
	}
	m.acks = append(m.acks, ack{copyID, rev, half})
	return nil
}

func (m *memStore) ackList() []ack {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]ack(nil), m.acks...)
}

// applier records what each half was handed, and when, relative to the acks.
type applier struct {
	mu      sync.Mutex
	seen    []string // MCP target names, in order
	err     error
	onApply func()
}

func (a *applier) record(name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.onApply != nil {
		a.onApply()
	}
	if a.err != nil {
		return a.err
	}
	a.seen = append(a.seen, name)
	return nil
}

func (a *applier) names() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.seen...)
}

func (a *applier) setErr(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.err = err
}

type modelApplier struct{ applier }

func (m *modelApplier) ApplyModel(_ context.Context, c gateway.ModelConfig) (gateway.Revision, error) {
	return "m", m.record(modelMarker(c))
}

// modelMarker names a model config by its keys' count, which the fixture does not set: the model half
// carries no MCP target, so the spec checks the MCP half by name and the model half by call count.
func modelMarker(gateway.ModelConfig) string { return "model" }

type mcpApplier struct{ applier }

func (m *mcpApplier) ApplyMCP(_ context.Context, c gateway.MCPConfig) (gateway.Revision, error) {
	return "c", m.record(c.MCP[0].Name)
}

// egressApplier records the org egress routes of each revision.
type egressApplier struct {
	mu     sync.Mutex
	got    [][]route.Route
	creds  [][]route.Credential
	actors [][]gateway.EgressActor
	err    error
}

func (e *egressApplier) ApplyEgress(_ context.Context, st gateway.EgressState) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.got = append(e.got, st.EgressRoutes)
	e.creds = append(e.creds, st.EgressCredentials)
	e.actors = append(e.actors, st.EgressActors)
	return e.err
}

func (e *egressApplier) calls() [][]route.Route {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([][]route.Route(nil), e.got...)
}

func (m *memStore) publishEgress(rev int64, name string, routes []route.Route, creds ...route.Credential) {
	var s gateway.State
	s.MCP = []gateway.MCPTarget{{Name: name, URL: "http://" + name + "/mcp"}}
	s.EgressRoutes = routes
	s.EgressCredentials = creds
	m.publishState(rev, s)
}

func (m *memStore) publishState(rev int64, s gateway.State) {
	u := staterev.Snapshot(s)
	u.Revision = rev
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cur = u
}

var _ = Describe("the gateway State client", func() {
	var (
		st     *memStore
		ts     *httptest.Server
		model  *modelApplier
		mcp    *mcpApplier
		cancel context.CancelFunc
		done   chan error
	)

	start := func(poll time.Duration) {
		st = &memStore{}
		srv, err := stateserver.New(stateserver.Config{
			Store: st, PollInterval: poll,
			Auth: func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") != "Bearer "+token {
						w.WriteHeader(http.StatusUnauthorized)
						return
					}
					next.ServeHTTP(w, r)
				})
			},
		})
		Expect(err).NotTo(HaveOccurred())
		srvCtx, stopSrv := context.WithCancel(context.Background())
		srvDone := make(chan struct{})
		go func() { defer close(srvDone); srv.Run(srvCtx) }()
		ts = httptest.NewServer(srv.Mount()(http.NotFoundHandler()))
		DeferCleanup(func() { ts.CloseClientConnections(); stopSrv(); <-srvDone; ts.Close() })
		model, mcp = &modelApplier{}, &mcpApplier{}
	}

	var egress stateclient.EgressApplier
	BeforeEach(func() { egress = nil })

	run := func(m *stateclient.Metrics) {
		c, err := stateclient.New(stateclient.Config{
			URL: ts.URL, Token: func() (string, error) { return token, nil }, CopyID: "copy-test-1",
			Model: model, MCP: mcp, Egress: egress, MinBackoff: 10 * time.Millisecond, MaxBackoff: 50 * time.Millisecond, Metrics: m,
		})
		Expect(err).NotTo(HaveOccurred())
		var ctx context.Context
		ctx, cancel = context.WithCancel(context.Background())
		done = make(chan error, 1)
		go func() { done <- c.Run(ctx) }()
		DeferCleanup(func() { cancel(); Eventually(done).Should(Receive()) })
	}

	acked := func(rev int64, h gwv1.Half) func() bool {
		return func() bool {
			for _, a := range st.ackList() {
				if a.rev == rev && a.half == h && a.copyID == "copy-test-1" {
					return true
				}
			}
			return false
		}
	}

	Context("the org egress routes (#1086 part B)", func() {
		forgeRoute := route.Route{
			Name: "org/acme/agent/a1/forge-0-0", Class: route.ClassForge, Org: "acme", Agent: "a1", Host: "github.com",
			Methods: []string{"GET"}, Paths: []string{"/acme/app.git/info/refs"}, QueryValues: map[string]string{"service": "git-upload-pack"},
			AnyQuery: true, Credential: "org/acme/agent/a1/codehost",
		}

		It("applies them on each revision, with no ack half of their own", func() {
			start(20 * time.Millisecond)
			eg := &egressApplier{}
			egress = eg
			cred := route.Credential{
				Name: "org/acme/agent/a1/codehost", Org: "acme", Agent: "a1", File: "/keys/forgev2-x",
				Scheme: route.SchemeBasic, User: "x-access-token",
			}
			st.publishEgress(3, "wiki", []route.Route{forgeRoute}, cred)
			m := stateclient.NewMetrics(nil)
			run(m)
			Eventually(acked(3, gwv1.Half_HALF_MCP), 10*time.Second, 10*time.Millisecond).Should(BeTrue())
			Eventually(eg.calls).Should(HaveLen(1))
			Expect(eg.calls()[0]).To(Equal([]route.Route{forgeRoute}))
			eg.mu.Lock()
			Expect(eg.creds[0]).To(Equal([]route.Credential{cred}))
			eg.mu.Unlock()
			for _, a := range st.ackList() {
				Expect(a.half).To(BeElementOf(gwv1.Half_HALF_MODEL, gwv1.Half_HALF_MCP))
			}

			// A revision without org routes removes them.
			st.publishEgress(4, "docs", nil)
			Eventually(eg.calls, 10*time.Second, 10*time.Millisecond).Should(HaveLen(2))
			Expect(eg.calls()[1]).To(BeEmpty())
			Expect(testutil.ToFloat64(m.Applied.WithLabelValues("egress", "ok"))).To(Equal(2.0))
		})

		It("carries the Substrate actors of a revision to the egress applier (issue #1386)", func() {
			start(20 * time.Millisecond)
			eg := &egressApplier{}
			egress = eg
			actor := gateway.EgressActor{Atespace: "ws-acme", Name: "ka-acme-0a1b2c3d", UID: "uid-1", Org: "acme", Agent: "a1"}
			var s gateway.State
			s.MCP = []gateway.MCPTarget{{Name: "wiki", URL: "http://wiki/mcp"}}
			s.EgressActors = []gateway.EgressActor{actor}
			st.publishState(3, s)
			run(stateclient.NewMetrics(nil))
			Eventually(eg.calls, 10*time.Second, 10*time.Millisecond).Should(HaveLen(1))
			eg.mu.Lock()
			defer eg.mu.Unlock()
			Expect(eg.actors[0]).To(Equal([]gateway.EgressActor{actor}))
		})

		It("counts a refused set and still applies and acks the halves", func() {
			start(20 * time.Millisecond)
			eg := &egressApplier{err: errors.New("egress: the org routes: bad")}
			egress = eg
			st.publishEgress(5, "wiki", []route.Route{forgeRoute})
			m := stateclient.NewMetrics(nil)
			run(m)
			Eventually(acked(5, gwv1.Half_HALF_MODEL), 10*time.Second, 10*time.Millisecond).Should(BeTrue())
			Eventually(acked(5, gwv1.Half_HALF_MCP), 10*time.Second, 10*time.Millisecond).Should(BeTrue())
			Eventually(func() float64 { return testutil.ToFloat64(m.Applied.WithLabelValues("egress", "failed")) }).Should(Equal(1.0))
			// The session does not end: no reconnect re-applies the same revision.
			Consistently(eg.calls, 200*time.Millisecond, 20*time.Millisecond).Should(HaveLen(1))
			Expect(mcp.names()).To(Equal([]string{"wiki"}))
		})
	})

	It("applies each half of the newest revision, then acks each half", func() {
		start(20 * time.Millisecond)
		st.publish(3, "wiki")
		var order []string
		var mu sync.Mutex
		note := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }
		model.onApply = func() { note("apply-model") }
		mcp.onApply = func() { note("apply-mcp") }
		run(nil)

		Eventually(acked(3, gwv1.Half_HALF_MODEL), 10*time.Second, 10*time.Millisecond).Should(BeTrue())
		Eventually(acked(3, gwv1.Half_HALF_MCP), 10*time.Second, 10*time.Millisecond).Should(BeTrue())
		Expect(mcp.names()).To(Equal([]string{"wiki"}))
		Expect(model.names()).To(HaveLen(1))
		mu.Lock()
		Expect(order).To(ConsistOf("apply-model", "apply-mcp"))
		mu.Unlock()

		st.publish(4, "docs")
		Eventually(acked(4, gwv1.Half_HALF_MCP), 10*time.Second, 10*time.Millisecond).Should(BeTrue())
		Expect(mcp.names()).To(Equal([]string{"wiki", "docs"}))
	})

	It("acks only the half that applied", func() {
		start(20 * time.Millisecond)
		st.publish(1, "wiki")
		model.setErr(errors.New("limit: USD budget limits support at most 9 fractional digits"))
		m := stateclient.NewMetrics(nil)
		run(m)

		Eventually(acked(1, gwv1.Half_HALF_MCP), 10*time.Second, 10*time.Millisecond).Should(BeTrue())
		Consistently(acked(1, gwv1.Half_HALF_MODEL), 300*time.Millisecond, 20*time.Millisecond).Should(BeFalse())
		Expect(testutil.ToFloat64(m.Applied.WithLabelValues("model", "failed"))).To(BeNumerically(">=", 1))
		Expect(testutil.ToFloat64(m.Applied.WithLabelValues("mcp", "ok"))).To(Equal(1.0))
	})

	It("backs off while an apply keeps failing instead of reconnecting at the minimum interval", func() {
		start(20 * time.Millisecond)
		st.publish(1, "wiki")
		model.setErr(errors.New("gateway admin endpoint unreachable"))
		m := stateclient.NewMetrics(nil)
		run(m)

		time.Sleep(600 * time.Millisecond)
		// Backoff 10, 20, 40, 50, 50 ms (with jitter) gives about 10 attempts in 600 ms; a backoff
		// stuck at the 10 ms minimum would give 50 or more.
		failed := testutil.ToFloat64(m.Applied.WithLabelValues("model", "failed"))
		Expect(failed).To(BeNumerically(">=", 2))
		Expect(failed).To(BeNumerically("<", 25))
	})

	It("reports a half that has failed to apply for longer than StaleAfter, and clears it on the next success (#1347)", func() {
		start(20 * time.Millisecond)
		st.publish(7, "wiki")
		mcp.setErr(errors.New("gateway rejected the config"))
		var mu sync.Mutex
		clock := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
		now := func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
		advance := func(d time.Duration) { mu.Lock(); defer mu.Unlock(); clock = clock.Add(d) }
		m := stateclient.NewMetrics(nil)
		c, err := stateclient.New(stateclient.Config{
			URL: ts.URL, Token: func() (string, error) { return token, nil }, CopyID: "copy-test-1",
			Model: model, MCP: mcp, MinBackoff: 10 * time.Millisecond, MaxBackoff: 50 * time.Millisecond, Metrics: m,
			StaleAfter: time.Minute, Now: now,
		})
		Expect(err).NotTo(HaveOccurred())
		ctx, stop := context.WithCancel(context.Background())
		fin := make(chan error, 1)
		go func() { fin <- c.Run(ctx) }()
		DeferCleanup(func() { stop(); Eventually(fin).Should(Receive()) })

		Eventually(func() float64 { return testutil.ToFloat64(m.Applied.WithLabelValues("mcp", "failed")) }, 10*time.Second, 10*time.Millisecond).
			Should(BeNumerically(">=", 1))
		Expect(c.Stale()).To(Succeed(), "a first failure is inside the bound")
		advance(2 * time.Minute)
		Eventually(c.Stale, 5*time.Second, 10*time.Millisecond).Should(MatchError(ContainSubstring("mcp half has failed")))

		mcp.setErr(nil)
		Eventually(acked(7, gwv1.Half_HALF_MCP), 10*time.Second, 10*time.Millisecond).Should(BeTrue())
		Expect(c.Stale()).To(Succeed(), "an applied revision clears it")
	})

	It("retries a half whose apply failed, without a new publish", func() {
		start(20 * time.Millisecond)
		st.publish(5, "wiki")
		model.setErr(errors.New("gateway admin endpoint unreachable"))
		calls := 0
		model.onApply = func() { // runs under the applier lock, before the error check: the second call succeeds
			if calls++; calls == 2 {
				model.err = nil
			}
		}
		m := stateclient.NewMetrics(nil)
		run(m)

		Eventually(acked(5, gwv1.Half_HALF_MODEL), 10*time.Second, 10*time.Millisecond).Should(BeTrue())
		Eventually(acked(5, gwv1.Half_HALF_MCP), 10*time.Second, 10*time.Millisecond).Should(BeTrue())
		Expect(testutil.ToFloat64(m.Applied.WithLabelValues("model", "failed"))).To(Equal(1.0))
		Expect(testutil.ToFloat64(m.Applied.WithLabelValues("model", "ok"))).To(Equal(1.0))
		Expect(mcp.names()).To(Equal([]string{"wiki"}), "the half that applied is not applied again")
	})

	It("reconnects after a drop and gets the newest revision, not the ones it missed", func() {
		start(20 * time.Millisecond)
		st.publish(1, "one")
		m := stateclient.NewMetrics(nil)
		run(m)
		Eventually(acked(1, gwv1.Half_HALF_MCP), 10*time.Second, 10*time.Millisecond).Should(BeTrue())

		// Revisions 2 and 3 appear while the connection is cut; only 3 may be applied.
		st.publish(2, "two")
		st.publish(3, "three")
		ts.CloseClientConnections()
		Eventually(acked(3, gwv1.Half_HALF_MCP), 10*time.Second, 10*time.Millisecond).Should(BeTrue())
		Expect(mcp.names()[len(mcp.names())-1]).To(Equal("three"))
		Expect(testutil.ToFloat64(m.Connects.WithLabelValues("established"))).To(BeNumerically(">=", 1))
	})

	It("does not re-apply a revision it already serves when it reconnects", func() {
		start(20 * time.Millisecond)
		st.publish(1, "one")
		run(nil)
		Eventually(acked(1, gwv1.Half_HALF_MCP), 10*time.Second, 10*time.Millisecond).Should(BeTrue())
		ts.CloseClientConnections()
		st.publish(2, "two")
		Eventually(acked(2, gwv1.Half_HALF_MCP), 10*time.Second, 10*time.Millisecond).Should(BeTrue())
		Expect(mcp.names()).To(Equal([]string{"one", "two"}))
	})

	It("re-acks after a lost ack by reconnecting, without applying again", func() {
		start(20 * time.Millisecond)
		st.publish(1, "one")
		st.failAcks = 3 // the whole retry budget of the first ack
		m := stateclient.NewMetrics(nil)
		run(m)
		Eventually(acked(1, gwv1.Half_HALF_MODEL), 10*time.Second, 10*time.Millisecond).Should(BeTrue())
		Eventually(acked(1, gwv1.Half_HALF_MCP), 10*time.Second, 10*time.Millisecond).Should(BeTrue())
		Expect(mcp.names()).To(Equal([]string{"one"}), "applied once")
		Expect(testutil.ToFloat64(m.AckFailures.WithLabelValues("model"))).To(Equal(1.0))
	})

	It("follows a lower revision after a database restore: applies both halves again and acks it", func() {
		start(20 * time.Millisecond)
		st.publish(5, "five")
		run(nil)
		Eventually(acked(5, gwv1.Half_HALF_MCP), 10*time.Second, 10*time.Millisecond).Should(BeTrue())
		Eventually(acked(5, gwv1.Half_HALF_MODEL), 10*time.Second, 10*time.Millisecond).Should(BeTrue())

		st.publish(3, "three") // the restored database's newest revision
		Eventually(acked(3, gwv1.Half_HALF_MCP), 10*time.Second, 10*time.Millisecond).Should(BeTrue())
		Eventually(acked(3, gwv1.Half_HALF_MODEL), 10*time.Second, 10*time.Millisecond).Should(BeTrue())
		Expect(mcp.names()).To(Equal([]string{"five", "three"}))
		Expect(model.names()).To(HaveLen(2))
	})

	It("does not retry an ack that cannot succeed: it ends the session and reconnects", func() {
		start(20 * time.Millisecond)
		st.publish(1, "one")
		st.unknownAcks = 1 // FailedPrecondition once; a retry inside the loop would then succeed
		m := stateclient.NewMetrics(nil)
		run(m)
		Eventually(acked(1, gwv1.Half_HALF_MODEL), 10*time.Second, 10*time.Millisecond).Should(BeTrue())
		Eventually(acked(1, gwv1.Half_HALF_MCP), 10*time.Second, 10*time.Millisecond).Should(BeTrue())
		failures := testutil.ToFloat64(m.AckFailures.WithLabelValues("model")) + testutil.ToFloat64(m.AckFailures.WithLabelValues("mcp"))
		Expect(failures).To(Equal(1.0), "one failed ack ended the session; no retry masked it")
		Expect(mcp.names()).To(Equal([]string{"one"}), "applied once")
	})

	It("keeps retrying while the server is down", func() {
		start(20 * time.Millisecond)
		st.publish(1, "one")
		url := ts.URL
		ts.CloseClientConnections()
		ts.Close()
		m := stateclient.NewMetrics(nil)
		c, err := stateclient.New(stateclient.Config{
			URL: url, Token: func() (string, error) { return token, nil }, CopyID: "copy-test-1",
			MCP: mcp, MinBackoff: 5 * time.Millisecond, MaxBackoff: 10 * time.Millisecond, Metrics: m,
		})
		Expect(err).NotTo(HaveOccurred())
		ctx, stop := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer stop()
		Expect(c.Run(ctx)).To(MatchError(context.DeadlineExceeded))
		Expect(testutil.ToFloat64(m.Connects.WithLabelValues("failed"))).To(BeNumerically(">", 2))
	})

	It("refuses a configuration that could not work", func() {
		_, err := stateclient.New(stateclient.Config{URL: "http://x", Token: func() (string, error) { return token, nil }, CopyID: "c"})
		Expect(err).To(HaveOccurred(), "no half to serve")
		id, err := stateclient.NewCopyID("pod-uid")
		Expect(err).NotTo(HaveOccurred())
		id2, _ := stateclient.NewCopyID("pod-uid")
		Expect(id).NotTo(Equal(id2), "a boot nonce makes each incarnation's id its own")

		// A base that would be cut inside a multi-byte rune is cut before it.
		long := strings.Repeat("a", 235) + strings.Repeat("é", 10) // byte 236 is the second byte of an é
		id, err = stateclient.NewCopyID(long)
		Expect(err).NotTo(HaveOccurred())
		Expect(utf8.ValidString(id)).To(BeTrue())
		Expect(len(id)).To(BeNumerically("<=", stateserver.MaxCopyIDLen))
		Expect(id).To(HavePrefix(strings.Repeat("a", 235) + "-"))
	})
})
