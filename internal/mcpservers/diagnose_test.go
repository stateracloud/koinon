// SPDX-License-Identifier: Apache-2.0

package mcpservers_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/mcpservers"
)

var _ = Describe("Diagnose", func() {
	run := func(h http.HandlerFunc, value string) (mcpservers.Diagnosis, *httptest.Server) {
		srv := httptest.NewServer(h)
		DeferCleanup(srv.Close)
		u, _ := url.Parse(srv.URL)
		policy := catalog.URLPolicy{AllowedHosts: []string{u.Host}}
		return mcpservers.Diagnose(context.Background(), policy, mcpservers.PreflightRequest{
			URL: srv.URL + "/mcp", Transport: mcpservers.TransportStreamableHTTP, Header: "Authorization", Prefix: "Bearer ", Value: value,
		}), srv
	}

	It("is happy with a server that answers", func() {
		var got string
		d, _ := run(func(w http.ResponseWriter, r *http.Request) {
			got = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusOK)
		}, "not-a-real-token")
		Expect(d.Reachable).To(BeTrue())
		Expect(d.Reason).To(BeEmpty())
		Expect(got).To(Equal("Bearer not-a-real-token"))
	})

	It("recognises a server that wants per-user OAuth", func() {
		d, _ := run(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="https://mcp.example.com/.well-known/oauth-protected-resource"`)
			w.WriteHeader(http.StatusUnauthorized)
		}, "")
		Expect(d.NeedsOAuth).To(BeTrue())
		Expect(d.Reason).To(ContainSubstring("OAuth"))
	})

	It("does not mistake a rejected static credential for OAuth", func() {
		d, _ := run(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="api"`)
			w.WriteHeader(http.StatusUnauthorized)
		}, "not-a-real-token")
		Expect(d.NeedsOAuth).To(BeFalse())
		Expect(d.Reason).To(ContainSubstring("rejected the credential"))
		Expect(d.Reason).NotTo(ContainSubstring("not-a-real-token"))
	})

	It("does not follow a redirect to another address", func() {
		hits := 0
		inner := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
		DeferCleanup(inner.Close)
		d, _ := run(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, inner.URL, http.StatusTemporaryRedirect)
		}, "x")
		Expect(hits).To(BeZero())
		Expect(d.Status).To(Equal(http.StatusTemporaryRedirect))
	})

	It("refuses to dial an address the policy denies", func() {
		srv := httptest.NewServer(http.NotFoundHandler())
		DeferCleanup(srv.Close)
		d := mcpservers.Diagnose(context.Background(), catalog.URLPolicy{}, mcpservers.PreflightRequest{URL: srv.URL, Transport: mcpservers.TransportStreamableHTTP})
		Expect(d.Reachable).To(BeFalse())
		Expect(d.Reason).To(ContainSubstring("egress policy"))
	})

	It("says a closed port is refused", func() {
		srv := httptest.NewServer(http.NotFoundHandler())
		u, _ := url.Parse(srv.URL)
		srv.Close()
		d := mcpservers.Diagnose(context.Background(), catalog.URLPolicy{AllowedHosts: []string{u.Host}}, mcpservers.PreflightRequest{URL: srv.URL, Transport: mcpservers.TransportStreamableHTTP})
		Expect(d.Reason).To(ContainSubstring("connection refused"))
	})
})
