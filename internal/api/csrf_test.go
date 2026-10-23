// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"net/http"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	authv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/auth/v1alpha1"
)

var _ = Describe("CSRF protection", func() {
	var (
		e *env
		b *browser
	)

	BeforeEach(func(ctx SpecContext) {
		e = newEnv(ctx, envOptions{trustedOrigins: []string{"https://dev.example.test"}})
		b = e.browser()
		Expect(b.login(ctx, "admin", bootstrapPassword).StatusCode).To(Equal(http.StatusOK))
	})

	stillLoggedIn := func(ctx SpecContext) {
		_, err := b.auth().WhoAmI(ctx, &authv1alpha1.WhoAmIRequest{})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	}

	rpc := func(ctx SpecContext, header http.Header) *http.Response {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			e.srv.URL+"/koinon.auth.v1alpha1.AuthService/WhoAmI", strings.NewReader("{}"))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Content-Type", "application/json")
		for k, v := range header {
			req.Header[k] = v
		}
		res, err := b.http.Do(req)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(res.Body.Close)
		return res
	}

	It("refuses a cross-site logout", func(ctx SpecContext) {
		res := b.post(ctx, "/auth/logout", http.Header{"Sec-Fetch-Site": {"cross-site"}})
		Expect(res.StatusCode).To(Equal(http.StatusForbidden))
		stillLoggedIn(ctx)
	})

	It("refuses a logout whose Origin is another site (browsers without Sec-Fetch-Site)", func(ctx SpecContext) {
		res := b.post(ctx, "/auth/logout", http.Header{"Origin": {"https://evil.example.test"}})
		Expect(res.StatusCode).To(Equal(http.StatusForbidden))
		stillLoggedIn(ctx)
	})

	It("refuses a cross-site login form post (login CSRF)", func(ctx SpecContext) {
		attacker := e.browser()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.srv.URL+"/auth/login",
			strings.NewReader("username=admin&password="+bootstrapPassword))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", "https://evil.example.test")
		res, err := attacker.http.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = res.Body.Close() }()
		Expect(res.StatusCode).To(Equal(http.StatusForbidden))
		Expect(attacker.sessionToken()).To(BeEmpty())
	})

	It("allows same-origin and trusted-origin requests", func(ctx SpecContext) {
		Expect(rpc(ctx, http.Header{
			"Connect-Protocol-Version": {"1"}, "Sec-Fetch-Site": {"same-origin"},
		}).StatusCode).To(Equal(http.StatusOK))
		Expect(rpc(ctx, http.Header{
			"Connect-Protocol-Version": {"1"}, "Origin": {"https://dev.example.test"}, "Sec-Fetch-Site": {"cross-site"},
		}).StatusCode).To(Equal(http.StatusOK))
		Expect(rpc(ctx, http.Header{
			"Connect-Protocol-Version": {"1"}, "Origin": {e.srv.URL},
		}).StatusCode).To(Equal(http.StatusOK))
	})

	It("refuses cross-site RPCs", func(ctx SpecContext) {
		Expect(rpc(ctx, http.Header{
			"Connect-Protocol-Version": {"1"}, "Sec-Fetch-Site": {"cross-site"},
		}).StatusCode).To(Equal(http.StatusForbidden))
		Expect(rpc(ctx, http.Header{
			"Connect-Protocol-Version": {"1"}, "Origin": {"https://evil.example.test"},
		}).StatusCode).To(Equal(http.StatusForbidden))
	})

	It("refuses RPCs without the Connect-Protocol-Version header", func(ctx SpecContext) {
		res := rpc(ctx, nil)
		Expect(res.StatusCode).NotTo(Equal(http.StatusOK))
		Expect(res.StatusCode).To(BeNumerically(">=", 400))
	})
})
