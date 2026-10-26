// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"net/http"

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
})
