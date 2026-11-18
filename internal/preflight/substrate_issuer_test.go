// SPDX-License-Identifier: Apache-2.0

package preflight_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/preflight"
)

// issuerServer serves a discovery document whose issuer is the server's own URL.
func issuerServer(useTLS bool, discovery, jwks int, issuerOverride string) *httptest.Server {
	var srv *httptest.Server
	mk := httptest.NewServer
	if useTLS {
		mk = httptest.NewTLSServer
	}
	srv = mk(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			if discovery != http.StatusOK {
				w.WriteHeader(discovery)
				return
			}
			iss := srv.URL
			if issuerOverride != "" {
				iss = issuerOverride
			}
			_, _ = fmt.Fprintf(w, `{"issuer":%q,"jwks_uri":%q}`, iss, srv.URL+"/openid/v1/jwks")
		case "/openid/v1/jwks":
			w.WriteHeader(jwks)
			_, _ = w.Write([]byte(`{"keys":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	return srv
}

func TestIssuerProbeNeedsA2xxDocumentAndJWKS(t *testing.T) {
	for name, tc := range map[string]struct {
		discovery, jwks int
		issuer          string
		detail          string
	}{
		"anonymous discovery refused": {http.StatusUnauthorized, 200, "", "HTTP 401"},
		"anonymous jwks refused":      {200, http.StatusForbidden, "", "HTTP 403"},
		"issuer mismatch":             {200, 200, "https://other.example", "differs"},
	} {
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)
			srv := issuerServer(false, tc.discovery, tc.jwks, tc.issuer)
			defer srv.Close()
			res := preflight.ProbeIssuerFromPod(t.Context(), srv.URL, 2*time.Second)
			g.Expect(res.Reachable).To(BeFalse())
			g.Expect(res.Detail).To(ContainSubstring(tc.detail))
		})
	}
}

// The test server's certificate is signed by no CA the probe trusts: the answer is reachable but
// unverified, which the checker reports as a failure (Substrate rejects the tokens), never a pass.
func TestIssuerProbeFlagsAnUntrustedCertificate(t *testing.T) {
	g := NewWithT(t)
	srv := issuerServer(true, http.StatusOK, http.StatusOK, "")
	defer srv.Close()
	res := preflight.ProbeIssuerFromPod(t.Context(), srv.URL, 2*time.Second)
	g.Expect(res.Reachable).To(BeTrue())
	g.Expect(res.Unverified).To(BeTrue())
	g.Expect(res.Detail).To(ContainSubstring("TLS verification failed"))
}
