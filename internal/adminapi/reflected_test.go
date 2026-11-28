// SPDX-License-Identifier: Apache-2.0

package adminapi_test

import (
	"io"
	"net/http"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Issue 1365 (CodeQL alert 54, go/reflected-xss at statusRecorder.Write): no response of the admin API
// shows a request value as markup. Each case sends an HTML payload through one place where a request
// value can reach a response: a path value, a query value, a body that does not decode, an unknown
// path and a wrong method. A body that echoes the value is JSON with Content-Type application/json,
// and every response carries nosniff, so a browser never renders any of them.
var _ = Describe("the admin API and request values in responses (issue 1365)", func() {
	const payload = `<img src=x onerror=alert(1)>`

	DescribeTable("never answers with markup",
		func(ctx SpecContext, method, path, body string, authenticated, echoes bool) {
			e := newEnv(ctx)
			var rd io.Reader
			if body != "" {
				rd = strings.NewReader(body)
			}
			req, err := http.NewRequestWithContext(ctx, method, e.srv.URL+path, rd)
			Expect(err).NotTo(HaveOccurred())
			if body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			if authenticated {
				req.Header.Set("Authorization", "Bearer "+e.token)
			}
			resp, err := http.DefaultClient.Do(req)
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = resp.Body.Close() }()
			raw, err := io.ReadAll(resp.Body)
			Expect(err).NotTo(HaveOccurred())

			Expect(resp.StatusCode).To(BeNumerically(">=", 400), string(raw))
			Expect(resp.Header.Get("X-Content-Type-Options")).To(Equal("nosniff"))
			Expect(resp.Header.Get("Content-Type")).NotTo(ContainSubstring("html"))
			Expect(string(raw)).NotTo(ContainSubstring("<img"), "a raw request value in the body")
			// A value that the error echoes is JSON-escaped (\u003c) and has the JSON type. The entries that
			// echo must do so: else the check below proves nothing.
			if echoes {
				Expect(string(raw)).To(ContainSubstring("u003c"), "the error no longer echoes the value; update this entry")
			}
			if strings.Contains(string(raw), "u003c") {
				Expect(resp.Header.Get("Content-Type")).To(Equal("application/json"), "an echoed value needs the JSON type")
			}
		},
		Entry("a path value that is not a UUID", http.MethodGet, "/admin/v1/agents/%3Cimg%20src=x%20onerror=alert(1)%3E/budget", "", true, true),
		Entry("a query value that is not a date-time", http.MethodGet, "/admin/v1/audit?since=%3Cimg%20src=x%20onerror=alert(1)%3E", "", true, true),
		Entry("a body that does not decode", http.MethodPost, "/admin/v1/tokens", `{"name": `+payload, true, true),
		Entry("an unknown path", http.MethodGet, "/admin/v1/%3Cimg%20src=x%20onerror=alert(1)%3E", "", true, false),
		Entry("a wrong method", http.MethodPatch, "/admin/v1/agents/%3Cimg%20src=x%20onerror=alert(1)%3E", "", true, false),
		Entry("a path value without a token", http.MethodGet, "/admin/v1/agents/%3Cimg%20src=x%20onerror=alert(1)%3E/budget", "", false, false),
	)
})
