// SPDX-License-Identifier: Apache-2.0

package mcpservers_test

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/faketls"
	"github.com/stateracloud/koinon/internal/mcpservers"
)

var _ = Describe("Diagnose", func() {
	run := func(h http.HandlerFunc) (mcpservers.Diagnosis, *httptest.Server) {
		srv := httptest.NewServer(h)
		DeferCleanup(srv.Close)
		u, _ := url.Parse(srv.URL)
		policy := catalog.URLPolicy{AllowedHosts: []string{u.Host}}
		return mcpservers.Diagnose(context.Background(), policy, mcpservers.PreflightRequest{
			URL: srv.URL + "/mcp", Transport: mcpservers.TransportStreamableHTTP,
		}), srv
	}

	It("is happy with a server that answers", func() {
		var got http.Header
		d, _ := run(func(w http.ResponseWriter, r *http.Request) {
			got = r.Header.Clone()
			w.WriteHeader(http.StatusOK)
		})
		Expect(d.Reachable).To(BeTrue())
		Expect(d.Reason).To(BeEmpty())
		Expect(got.Get("Authorization")).To(BeEmpty(), "the add-time check carries no credential")
	})

	It("recognises a server that wants per-user OAuth", func() {
		d, _ := run(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="https://mcp.example.com/.well-known/oauth-protected-resource"`)
			w.WriteHeader(http.StatusUnauthorized)
		})
		Expect(d.NeedsOAuth).To(BeTrue())
		Expect(d.Reason).To(ContainSubstring("OAuth"))
	})

	It("does not mistake a server that wants a static credential for OAuth", func() {
		d, _ := run(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="api"`)
			w.WriteHeader(http.StatusUnauthorized)
		})
		Expect(d.NeedsOAuth).To(BeFalse())
		Expect(d.Reason).To(ContainSubstring("asks for a credential"))
	})

	It("does not follow a redirect to another address", func() {
		hits := 0
		inner := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
		DeferCleanup(inner.Close)
		d, _ := run(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, inner.URL, http.StatusTemporaryRedirect)
		})
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

var _ = Describe("Diagnose over TLS with a private CA", func() {
	tlsServer := func() (*httptest.Server, catalog.URLPolicy, string) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
		DeferCleanup(srv.Close)
		u, _ := url.Parse(srv.URL)
		bundle := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
		return srv, catalog.URLPolicy{AllowedHosts: []string{u.Host}}, bundle
	}
	req := func(srv *httptest.Server, bundle string) mcpservers.PreflightRequest {
		return mcpservers.PreflightRequest{URL: srv.URL + "/mcp", Transport: mcpservers.TransportStreamableHTTP, CABundle: bundle}
	}

	It("trusts the server's own CA bundle", func() {
		srv, policy, bundle := tlsServer()
		d := mcpservers.Diagnose(context.Background(), policy, req(srv, bundle))
		Expect(d.Reachable).To(BeTrue(), d.Reason)
		Expect(d.BadCertificate).To(BeFalse())
	})

	It("reports a certificate no trusted CA signed, and names the CA field", func() {
		srv, policy, _ := tlsServer()
		d := mcpservers.Diagnose(context.Background(), policy, req(srv, ""))
		Expect(d.Reachable).To(BeFalse())
		Expect(d.BadCertificate).To(BeTrue())
		Expect(d.Reason).To(ContainSubstring("caBundle"))
	})

	It("does not trust a different CA's bundle for this server", func() {
		srv, policy, _ := tlsServer()
		other, err := faketls.Make("another test CA", []string{"example.test"})
		Expect(err).NotTo(HaveOccurred())
		d := mcpservers.Diagnose(context.Background(), policy, req(srv, string(other.CA)))
		Expect(d.BadCertificate).To(BeTrue())
	})

	It("does not call a down server a certificate problem", func() {
		srv, policy, bundle := tlsServer()
		srv.Close()
		d := mcpservers.Diagnose(context.Background(), policy, req(srv, bundle))
		Expect(d.BadCertificate).To(BeFalse())
		Expect(d.Reason).To(ContainSubstring("connection refused"))
	})

	It("refuses an add with a typed certificate error, and lets an unreachable server through", func() {
		srv, policy, bundle := tlsServer()
		pre := mcpservers.OAuthPreflight(policy)
		err := pre(context.Background(), req(srv, ""))
		var inv *mcpservers.InvalidError
		Expect(err).To(HaveOccurred())
		Expect(err).To(BeAssignableToTypeOf(inv))
		Expect(err.Error()).To(ContainSubstring("caBundle"))
		Expect(pre(context.Background(), req(srv, bundle))).To(Succeed())
		srv.Close()
		Expect(pre(context.Background(), req(srv, ""))).To(Succeed())
	})

	It("refuses a bundle that is not PEM certificates", func() {
		srv, policy, _ := tlsServer()
		d := mcpservers.Diagnose(context.Background(), policy, req(srv, "not a pem"))
		Expect(d.Reachable).To(BeFalse())
		Expect(d.Reason).To(ContainSubstring("CA bundle"))
	})
})

var _ = Describe("Diagnose trust (#278)", func() {
	tlsServer := func() (*httptest.Server, catalog.URLPolicy, string) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
		DeferCleanup(srv.Close)
		u, _ := url.Parse(srv.URL)
		bundle := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
		return srv, catalog.URLPolicy{AllowedHosts: []string{u.Host}}, bundle
	}

	It("trusts ONLY the bundle when one is set: the pool holds the bundle's certificates and no system root", func() {
		_, _, bundle := tlsServer()
		pool, err := mcpservers.CertPoolOf(bundle)
		Expect(err).NotTo(HaveOccurred())
		only := x509.NewCertPool()
		Expect(only.AppendCertsFromPEM([]byte(bundle))).To(BeTrue())
		Expect(pool.Equal(only)).To(BeTrue(), "the bundle replaces the public roots")
		system, err := x509.SystemCertPool()
		Expect(err).NotTo(HaveOccurred())
		withSystem := system.Clone()
		Expect(withSystem.AppendCertsFromPEM([]byte(bundle))).To(BeTrue())
		if !withSystem.Equal(only) { // a machine with no system roots makes the two pools equal
			Expect(pool.Equal(withSystem)).To(BeFalse())
		}
	})

	It("skips certificate verification when asked, and only then", func() {
		srv, policy, _ := tlsServer()
		r := mcpservers.PreflightRequest{URL: srv.URL + "/mcp", Transport: mcpservers.TransportStreamableHTTP}
		d := mcpservers.Diagnose(context.Background(), policy, r)
		Expect(d.BadCertificate).To(BeTrue(), "the default verifies")
		r.InsecureSkipVerify = true
		d = mcpservers.Diagnose(context.Background(), policy, r)
		Expect(d.Reachable).To(BeTrue(), d.Reason)
		Expect(d.BadCertificate).To(BeFalse())
	})
})
