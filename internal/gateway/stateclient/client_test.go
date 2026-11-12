// SPDX-License-Identifier: Apache-2.0

package stateclient_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/stateracloud/koinon/internal/gateway"
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

	run := func(m *stateclient.Metrics) {
		c, err := stateclient.New(stateclient.Config{
			URL: ts.URL, Token: func() (string, error) { return token, nil }, CopyID: "copy-test-1",
			Model: model, MCP: mcp, MinBackoff: 10 * time.Millisecond, MaxBackoff: 50 * time.Millisecond, Metrics: m,
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
	})
})
