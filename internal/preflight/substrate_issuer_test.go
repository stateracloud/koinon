// SPDX-License-Identifier: Apache-2.0

package preflight_test

import (
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
		"discovery refused": {http.StatusUnauthorized, 200, "", "HTTP 401"},
		"jwks refused":      {200, http.StatusForbidden, "", "HTTP 403"},
		"issuer mismatch":   {200, 200, "https://other.example", "differs"},
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

func jwtWithIssuer(iss string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"RS256"}`)) + "." + enc([]byte(fmt.Sprintf(`{"iss":%q}`, iss))) + "." + enc([]byte("sig"))
}

// The in-cluster issuer is read the way Substrate's api server reads it (internal/oidcjwt/http.go): the pod's
// CA replaces the roots, the pod's token goes on the discovery request and on the /openid/v1/jwks request, and
// the pod's own token issuer is reported for the comparison with the configured one.
func TestInClusterIssuerUsesThePodsCAAndToken(t *testing.T) {
	g := NewWithT(t)
	const token = "koinon_test_not-a-real-token"
	var seenAuth []string
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = append(seenAuth, r.URL.Path+" "+r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_, _ = fmt.Fprintf(w, `{"issuer":%q,"jwks_uri":%q}`, srv.URL, srv.URL+"/openid/v1/jwks")
		case "/openid/v1/jwks":
			_, _ = w.Write([]byte(`{"keys":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	caFile, tokenFile := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "token")
	g.Expect(os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600)).To(Succeed())
	g.Expect(os.WriteFile(tokenFile, []byte(token), 0o600)).To(Succeed())

	files := preflight.IssuerProbeFiles{CAFile: caFile, TokenFile: tokenFile, InCluster: true}
	res := preflight.ProbeIssuerWith(t.Context(), srv.URL, 2*time.Second, files)
	g.Expect(res.Reachable).To(BeTrue(), res.Detail)
	g.Expect(res.Unverified).To(BeFalse())
	g.Expect(seenAuth).To(Equal([]string{
		"/.well-known/openid-configuration Bearer " + token, "/openid/v1/jwks Bearer " + token,
	}))

	// An issuer outside the cluster gets no credential and only the system roots.
	seenAuth = nil
	res = preflight.ProbeIssuerWith(t.Context(), srv.URL, 2*time.Second, preflight.IssuerProbeFiles{CAFile: caFile, TokenFile: tokenFile})
	g.Expect(res.Reachable).To(BeFalse())
	for _, a := range seenAuth {
		g.Expect(a).NotTo(ContainSubstring("Bearer"))
	}

	// The pod's own token issuer comes back for the comparison.
	g.Expect(os.WriteFile(tokenFile, []byte(jwtWithIssuer("https://[fdae:1::1]:10001")), 0o600)).To(Succeed())
	res = preflight.ProbeIssuerWith(t.Context(), srv.URL, 2*time.Second, files)
	g.Expect(res.TokenIssuer).To(Equal("https://[fdae:1::1]:10001"))
}
