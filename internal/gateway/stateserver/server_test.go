// SPDX-License-Identifier: Apache-2.0

package stateserver_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/staterev"
	"github.com/stateracloud/koinon/internal/gateway/stateserver"
	gwv1 "github.com/stateracloud/koinon/internal/gen/koinon/gateway/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/gateway/v1alpha1/gatewayv1alpha1connect"
	"github.com/stateracloud/koinon/internal/otlpauth"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

func TestStateServer(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Gateway State server")
}

const (
	gatewayToken = "koinon_test_not-a-real-token-gateway-0001"
	traceToken   = "koinon_test_not-a-real-token-traces-00002"
)

func tokens(dir, token string) *otlpauth.Tokens {
	Expect(os.MkdirAll(dir, 0o700)).To(Succeed())
	Expect(os.WriteFile(filepath.Join(dir, otlpauth.TokenFile), []byte(token), 0o600)).To(Succeed())
	t, err := otlpauth.NewTokens(dir, nil)
	Expect(err).NotTo(HaveOccurred())
	return t
}

type withToken struct{ token string }

func (w withToken) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if w.token != "" {
		r.Header.Set("Authorization", "Bearer "+w.token)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func stateOf(name string) gateway.State {
	var s gateway.State
	s.MCP = []gateway.MCPTarget{{Name: name, URL: "http://" + name + "/mcp"}}
	return s
}

var _ = Describe("the gateway State stream", Ordered, func() {
	var (
		pool *pgxpool.Pool
		pub  staterev.Publisher
	)

	BeforeAll(func(ctx SpecContext) {
		pool = pgtest.Start(ctx)
		pub = staterev.Publisher{DB: pool}
	}, NodeTimeout(3*time.Minute))

	BeforeEach(func(ctx SpecContext) {
		_, err := pool.Exec(ctx, "DELETE FROM gateway_state_revisions")
		Expect(err).NotTo(HaveOccurred())
	})

	publish := func(ctx context.Context, name string) int64 {
		p, err := pub.Publish(ctx, stateOf(name))
		Expect(err).NotTo(HaveOccurred())
		Expect(p.Changed).To(BeTrue())
		return p.Revision
	}

	// serve starts a Server behind the real token check on an httptest listener.
	serve := func(cfg stateserver.Config) (*httptest.Server, gatewayv1alpha1connect.GatewayStateServiceClient, *stateserver.Server) {
		dir := GinkgoT().TempDir()
		cfg.Store = stateserver.NewPGStore(pool)
		cfg.Auth = otlpauth.ScopedMiddleware(tokens(filepath.Join(dir, "gw"), gatewayToken),
			map[string]*otlpauth.Tokens{"/v1/traces": tokens(filepath.Join(dir, "tr"), traceToken)}, nil)
		srv, err := stateserver.New(cfg)
		Expect(err).NotTo(HaveOccurred())
		runCtx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); srv.Run(runCtx) }()
		ts := httptest.NewServer(srv.Mount()(http.NotFoundHandler()))
		DeferCleanup(func() { ts.CloseClientConnections(); cancel(); <-done; ts.Close() })
		return ts, clientFor(ts.URL, gatewayToken), srv
	}

	receive := func(ctx context.Context, st gatewayv1alpha1connect.GatewayStateServiceStreamStateClientStream) (*gwv1.StateUpdate, error) {
		type res struct {
			u   *gwv1.StateUpdate
			err error
		}
		ch := make(chan res, 1)
		go func() { u, err := st.Receive(); ch <- res{u, err} }()
		select {
		case r := <-ch:
			return r.u, r.err
		case <-time.After(10 * time.Second):
			Fail("no message within 10 s")
		case <-ctx.Done():
		}
		return nil, ctx.Err()
	}

	It("sends the newest revision at connect, then each new one (poll only, notifications off)", func(ctx SpecContext) {
		publish(ctx, "one")
		r2 := publish(ctx, "two")
		_, c, _ := serve(stateserver.Config{PollInterval: 50 * time.Millisecond})

		st, err := c.StreamState(ctx, &gwv1.StreamStateRequest{CopyId: "copy-1"})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(st.Close)
		u, err := receive(ctx, st)
		Expect(err).NotTo(HaveOccurred())
		Expect(u.GetRevision()).To(Equal(r2), "a full snapshot of the newest revision, not a replay")
		Expect(u.GetMcp().GetTargets()).To(HaveLen(1))
		Expect(u.GetMcp().GetTargets()[0].GetName()).To(Equal("two"))

		r3 := publish(ctx, "three")
		u, err = receive(ctx, st)
		Expect(err).NotTo(HaveOccurred())
		Expect(u.GetRevision()).To(Equal(r3))
	})

	It("is woken by a publish notification well before the poll would run", func(ctx SpecContext) {
		publish(ctx, "one")
		_, c, _ := serve(stateserver.Config{PollInterval: time.Hour, Notify: stateserver.PoolNotify(pool)})
		st, err := c.StreamState(ctx, &gwv1.StreamStateRequest{CopyId: "copy-1"})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(st.Close)
		_, err = receive(ctx, st)
		Expect(err).NotTo(HaveOccurred())

		var got int64
		Eventually(func() bool { // the LISTEN may not be in place yet; publish until it is seen
			got = publish(ctx, "n"+time.Now().Format("150405.000000"))
			u, err := receive(ctx, st)
			return err == nil && u.GetRevision() >= got
		}, 20*time.Second).Should(BeTrue())
	})

	It("follows a lower revision after a database restore, on the same stream", func(ctx SpecContext) {
		publish(ctx, "one")
		r2 := publish(ctx, "two")
		_, c, _ := serve(stateserver.Config{PollInterval: 50 * time.Millisecond})
		st, err := c.StreamState(ctx, &gwv1.StreamStateRequest{CopyId: "copy-1"})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(st.Close)
		u, err := receive(ctx, st)
		Expect(err).NotTo(HaveOccurred())
		Expect(u.GetRevision()).To(Equal(r2))

		_, err = pool.Exec(ctx, "DELETE FROM gateway_state_revisions WHERE revision = $1", r2)
		Expect(err).NotTo(HaveOccurred())
		u, err = receive(ctx, st)
		Expect(err).NotTo(HaveOccurred())
		Expect(u.GetRevision()).To(BeNumerically("<", r2))
		Expect(u.GetMcp().GetTargets()[0].GetName()).To(Equal("one"))
	})

	It("sends nothing before the first revision exists, then the first one", func(ctx SpecContext) {
		_, c, _ := serve(stateserver.Config{PollInterval: 50 * time.Millisecond})
		st, err := c.StreamState(ctx, &gwv1.StreamStateRequest{CopyId: "copy-1"})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(st.Close)
		r1 := publish(ctx, "first")
		u, err := receive(ctx, st)
		Expect(err).NotTo(HaveOccurred())
		Expect(u.GetRevision()).To(Equal(r1))
	})

	It("stores an ack, idempotently, and rejects a bad one", func(ctx SpecContext) {
		r1 := publish(ctx, "one")
		ts, c, srv := serve(stateserver.Config{PollInterval: 50 * time.Millisecond})
		_ = srv
		for range 2 {
			_, err := c.AckRevision(ctx, &gwv1.AckRevisionRequest{CopyId: "copy-1", Revision: r1, Half: gwv1.Half_HALF_MODEL})
			Expect(err).NotTo(HaveOccurred())
		}
		ok, err := staterev.AllAcked(ctx, pool, r1, gwv1.Half_HALF_MODEL, []string{"copy-1"})
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		ok, err = staterev.AllAcked(ctx, pool, r1, gwv1.Half_HALF_MCP, []string{"copy-1"})
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse(), "the MCP half was not acked")

		code := func(req *gwv1.AckRevisionRequest) connect.Code {
			_, err := c.AckRevision(ctx, req)
			Expect(err).To(HaveOccurred())
			return connect.CodeOf(err)
		}
		Expect(code(&gwv1.AckRevisionRequest{CopyId: "copy-1", Revision: r1 + 1000, Half: gwv1.Half_HALF_MCP})).To(Equal(connect.CodeFailedPrecondition))
		Expect(code(&gwv1.AckRevisionRequest{CopyId: "copy-1", Revision: r1})).To(Equal(connect.CodeInvalidArgument), "no half")
		Expect(code(&gwv1.AckRevisionRequest{Revision: r1, Half: gwv1.Half_HALF_MCP})).To(Equal(connect.CodeInvalidArgument), "no copy id")
		Expect(code(&gwv1.AckRevisionRequest{CopyId: "copy-1", Half: gwv1.Half_HALF_MCP})).To(Equal(connect.CodeInvalidArgument), "no revision")
		_ = ts
	})

	It("refuses a caller without the gateway's token, and serves nothing else", func(ctx SpecContext) {
		publish(ctx, "one")
		ts, _, _ := serve(stateserver.Config{PollInterval: 50 * time.Millisecond})
		for name, tok := range map[string]string{"no token": "", "a wrong token": "koinon_test_not-a-real-token-wrong-00003", "the traces token": traceToken} {
			c := clientFor(ts.URL, tok)
			st, err := c.StreamState(ctx, &gwv1.StreamStateRequest{CopyId: "copy-1"})
			if err == nil {
				_, err = st.Receive()
				_ = st.Close()
			}
			Expect(err).To(HaveOccurred(), name)
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnauthenticated), name)
			_, err = c.AckRevision(ctx, &gwv1.AckRevisionRequest{CopyId: "copy-1", Revision: 1, Half: gwv1.Half_HALF_MODEL})
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnauthenticated), name)
		}
		var n int
		Expect(pool.QueryRow(ctx, "SELECT count(*) FROM gateway_state_acks").Scan(&n)).To(Succeed())
		Expect(n).To(BeZero(), "a refused caller records nothing")
	})

	It("does not start without an authentication wrapper", func() {
		_, err := stateserver.New(stateserver.Config{Store: stateserver.NewPGStore(pool)})
		Expect(err).To(MatchError(ContainSubstring("authentication")))
	})

	It("keeps copy ids and counts out of metric labels", func(ctx SpecContext) {
		m := stateserver.NewMetrics(nil)
		r1 := publish(ctx, "one")
		_, c, _ := serve(stateserver.Config{PollInterval: 50 * time.Millisecond, Metrics: m})
		_, err := c.AckRevision(ctx, &gwv1.AckRevisionRequest{CopyId: "copy-secret-id", Revision: r1, Half: gwv1.Half_HALF_MCP})
		Expect(err).NotTo(HaveOccurred())
		Expect(testutil.ToFloat64(m.Acks.WithLabelValues("mcp", "ok"))).To(Equal(1.0))
	})
})

func clientFor(url, token string) gatewayv1alpha1connect.GatewayStateServiceClient {
	return gatewayv1alpha1connect.NewGatewayStateServiceClient(
		connect.NewClient(connecthttp.NewTransport(&http.Client{Transport: withToken{token}}, url)))
}
