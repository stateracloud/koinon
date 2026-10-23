// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing/fstest"

	connect "connectrpc.com/connect/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	authv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/auth/v1alpha1"
)

func (b *browser) get(ctx context.Context, path string) (int, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.e.srv.URL+path, nil)
	Expect(err).NotTo(HaveOccurred())
	res, err := b.http.Do(req)
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	Expect(err).NotTo(HaveOccurred())
	return res.StatusCode, string(body)
}

func (b *browser) methods(ctx context.Context) map[string]bool {
	code, body := b.get(ctx, "/auth/methods")
	Expect(code).To(Equal(http.StatusOK))
	var out map[string]bool
	Expect(json.Unmarshal([]byte(body), &out)).To(Succeed())
	return out
}

var _ = Describe("GET /auth/methods", func() {
	It("reports password login only by default", func(ctx SpecContext) {
		e := newEnv(ctx, envOptions{})
		Expect(e.browser().methods(ctx)).To(Equal(map[string]bool{"password": true, "oidc": false}))
	})

	It("reports OIDC when configured and password login when disabled", func(ctx SpecContext) {
		e := newEnv(ctx, envOptions{oidc: true, bootstrapLoginDisabled: true})
		Expect(e.browser().methods(ctx)).To(Equal(map[string]bool{"password": false, "oidc": true}))
	})
})

var _ = Describe("serving the web UI", func() {
	It("serves the SPA for client routes next to the API and login endpoints", func(ctx SpecContext) {
		ui := fstest.MapFS{"index.html": {Data: []byte("<title>Koinon</title>")}}
		e := newEnv(ctx, envOptions{ui: ui})
		b := e.browser()

		for _, p := range []string{"/", "/tickets/abc", "/login"} {
			code, body := b.get(ctx, p)
			Expect(code).To(Equal(http.StatusOK), p)
			Expect(body).To(ContainSubstring("<title>Koinon</title>"), p)
		}
		code, _ := b.get(ctx, "/healthz")
		Expect(code).To(Equal(http.StatusOK))
		Expect(b.methods(ctx)).To(HaveKeyWithValue("password", true))

		_, err := b.auth().WhoAmI(ctx, &authv1alpha1.WhoAmIRequest{})
		Expect(codeOf(err)).To(Equal(connect.CodeUnauthenticated), "RPCs still reach connect, not the SPA")
		Expect(b.post(ctx, "/tickets/abc", nil).StatusCode).To(Equal(http.StatusMethodNotAllowed))
	})
})
