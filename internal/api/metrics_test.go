// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"net/http"
	"strings"

	connect "connectrpc.com/connect/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/stateracloud/koinon/internal/api"
	authv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/auth/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/auth/v1alpha1/authv1alpha1connect"
)

var _ = Describe("RPC metrics", func() {
	It("counts RPCs by procedure and Connect code, refused ones included", func(ctx SpecContext) {
		m := api.NewMetrics(prometheus.NewRegistry())
		e := newEnv(ctx, envOptions{metrics: m})
		b := e.browser()
		count := func(code string) float64 {
			return testutil.ToFloat64(m.RPCRequests.WithLabelValues(probeProcedure, code))
		}

		err := b.probe(ctx)
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnauthenticated))
		Expect(count("unauthenticated")).To(Equal(1.0))
		Expect(count("ok")).To(Equal(0.0))

		Expect(b.login(ctx, "admin", bootstrapPassword).StatusCode).To(Equal(http.StatusOK))
		Expect(connect.CodeOf(b.probe(ctx))).To(Equal(connect.CodePermissionDenied), "the password must be changed first")
		Expect(count("permission_denied")).To(Equal(1.0))

		_, err = b.auth().ChangePassword(ctx, &authv1alpha1.ChangePasswordRequest{
			CurrentPassword: bootstrapPassword, NewPassword: "a-new-long-password-1",
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(b.probe(ctx)).To(Succeed())
		Expect(b.probe(ctx)).To(Succeed())
		Expect(count("ok")).To(Equal(2.0))
		Expect(count("unauthenticated")).To(Equal(1.0))
		Expect(testutil.ToFloat64(m.RPCRequests.WithLabelValues(authv1alpha1connect.AuthServiceChangePasswordProcedure, "ok"))).To(Equal(1.0))
	})

	It("adds no series for an unknown procedure", func(ctx SpecContext) {
		reg := prometheus.NewRegistry()
		m := api.NewMetrics(reg)
		e := newEnv(ctx, envOptions{metrics: m})
		before := testutil.CollectAndCount(m.RPCRequests)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.srv.URL+"/no.such.v1.Service/Nope", strings.NewReader("{}"))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Connect-Protocol-Version", "1")
		resp, err := e.srv.Client().Do(req)
		Expect(err).NotTo(HaveOccurred())
		_ = resp.Body.Close()
		Expect(testutil.CollectAndCount(m.RPCRequests)).To(Equal(before))
	})
})
