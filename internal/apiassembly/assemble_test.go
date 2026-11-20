// SPDX-License-Identifier: Apache-2.0

package apiassembly_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"

	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/stateracloud/koinon/internal/adminapi"
	"github.com/stateracloud/koinon/internal/api"
	"github.com/stateracloud/koinon/internal/apiassembly"
	"github.com/stateracloud/koinon/internal/auth"
	agentv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/agent/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/agent/v1alpha1/agentv1alpha1connect"
	"github.com/stateracloud/koinon/internal/installbinding"
	"github.com/stateracloud/koinon/internal/runwatch"
)

// start assembles the request path on the suite's database, with no Kubernetes and no keyring, and
// serves its handler. notReady makes /readyz answer 503.
func start(ready http.HandlerFunc, settings apiassembly.Settings) (*httptest.Server, error) {
	authSvc, err := auth.New(pool, auth.Config{})
	Expect(err).NotTo(HaveOccurred())
	asm, err := apiassembly.Assemble(context.Background(), apiassembly.Outside{
		Pool: pool, Binding: installbinding.New(pool, "", false, nil), Ready: ready,
	}, settings, apiassembly.Prebuilt{
		Auth: authSvc, Hub: runwatch.New(pool.Config().ConnConfig, nil), Metrics: api.NewMetrics(prometheus.NewRegistry()),
	})
	if err != nil {
		return nil, err
	}
	srv := httptest.NewServer(asm.Handler)
	DeferCleanup(srv.Close)
	return srv, nil
}

var _ = Describe("Assemble", func() {
	var notReady atomic.Bool
	ready := func(w http.ResponseWriter, _ *http.Request) {
		if notReady.Load() {
			http.Error(w, "schema out of range", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
	get := func(ctx context.Context, srv *httptest.Server, path string) int {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+path, nil)
		Expect(err).NotTo(HaveOccurred())
		res, err := srv.Client().Do(req)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(res.Body.Close)
		return res.StatusCode
	}

	BeforeEach(func() { notReady.Store(false) })

	It("answers /healthz", func(ctx SpecContext) {
		srv, err := start(ready, apiassembly.Settings{})
		Expect(err).NotTo(HaveOccurred())
		Expect(get(ctx, srv, "/healthz")).To(Equal(http.StatusOK))
	})

	It("lets /readyz follow Outside.Ready", func(ctx SpecContext) {
		srv, err := start(ready, apiassembly.Settings{})
		Expect(err).NotTo(HaveOccurred())
		Expect(get(ctx, srv, "/readyz")).To(Equal(http.StatusOK))
		notReady.Store(true)
		Expect(get(ctx, srv, "/readyz")).To(Equal(http.StatusServiceUnavailable))
	})

	It("mounts the admin REST routes behind a token", func(ctx SpecContext) {
		srv, err := start(ready, apiassembly.Settings{})
		Expect(err).NotTo(HaveOccurred())
		Expect(get(ctx, srv, adminapi.BasePath+"/providers")).To(Equal(http.StatusUnauthorized))
	})

	It("registers the Connect services, which refuse a call with no session", func(ctx SpecContext) {
		srv, err := start(ready, apiassembly.Settings{})
		Expect(err).NotTo(HaveOccurred())
		agents := agentv1alpha1connect.NewAgentServiceClient(connect.NewClient(connecthttp.NewTransport(srv.Client(), srv.URL)))
		_, err = agents.ListAgents(ctx, &agentv1alpha1.ListAgentsRequest{})
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnauthenticated))
	})

	It("refuses a cluster domain that is not a DNS name as an *InputError (#1054)", func() {
		_, err := start(ready, apiassembly.Settings{ClusterDomain: "Cluster.Local."})
		var input *apiassembly.InputError
		Expect(errors.As(err, &input)).To(BeTrue(), "got %v", err)
		Expect(input.Setting).To(Equal("cluster domain"))
	})

	It("refuses invalid allowed hosts as an *InputError", func() {
		_, err := start(ready, apiassembly.Settings{EgressAllowedHosts: []string{"*.example.com"}})
		var input *apiassembly.InputError
		Expect(errors.As(err, &input)).To(BeTrue(), "got %v", err)
		Expect(err).To(MatchError(ContainSubstring("*.example.com")))
	})
})
