// SPDX-License-Identifier: Apache-2.0

package preflight_test

import (
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/preflight"
)

// usableJWKS is a JWKS with one key of a form Substrate's parseJWK accepts (the probe checks the fields only).
const usableJWKS = `{"keys":[{"kid":"k1","kty":"RSA","n":"AQAB","e":"AQAB"}]}`

// issuerServer serves a discovery document whose issuer is the server's own URL.
func issuerServer(useTLS bool, discovery, jwks int, issuerOverride string) *httptest.Server {
	return issuerServerWithKeys(useTLS, discovery, jwks, issuerOverride, usableJWKS)
}

func issuerServerWithKeys(useTLS bool, discovery, jwks int, issuerOverride, keys string) *httptest.Server {
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
			_, _ = w.Write([]byte(keys))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	return srv
}

// Substrate accepts HTTP 200 only and fails on a JWKS without a key it can use.
func TestIssuerProbeNeedsHTTP200AndAUsableKey(t *testing.T) {
	for name, tc := range map[string]struct {
		discovery, jwks int
		keys, detail    string
		reachable       bool
	}{
		"usable key":         {200, 200, usableJWKS, "1 usable keys", true},
		"discovery 203":      {http.StatusNonAuthoritativeInfo, 200, usableJWKS, "HTTP 203", false},
		"jwks 204":           {200, http.StatusNoContent, usableJWKS, "HTTP 204", false},
		"empty jwks":         {200, 200, `{"keys":[]}`, "no usable key", false},
		"key without kid":    {200, 200, `{"keys":[{"kty":"RSA","n":"AQAB","e":"AQAB"}]}`, "no usable key", false},
		"unsupported curve":  {200, 200, `{"keys":[{"kid":"k","kty":"EC","crv":"P-224","x":"AQ","y":"AQ"}]}`, "no usable key", false},
		"usable ec key":      {200, 200, `{"keys":[{"kid":"k","kty":"EC","crv":"P-256","x":"AQ","y":"AQ"}]}`, "1 usable keys", true},
		"jwks is not a jwks": {200, 200, `<html>`, "not a JSON JWKS", false},
	} {
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)
			srv := issuerServerWithKeys(false, tc.discovery, tc.jwks, "", tc.keys)
			defer srv.Close()
			res := preflight.ProbeIssuerFromPod(t.Context(), srv.URL, 2*time.Second)
			g.Expect(res.Reachable).To(Equal(tc.reachable), res.Detail)
			g.Expect(res.Detail).To(ContainSubstring(tc.detail))
		})
	}
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
// authRecorder records the path and Authorization header of each request; the handler runs on server
// goroutines, so it is locked.
type authRecorder struct {
	mu   sync.Mutex
	seen []string
}

func (a *authRecorder) add(r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seen = append(a.seen, r.URL.Path+" "+r.Header.Get("Authorization"))
}

// take returns what was recorded and starts again.
func (a *authRecorder) take() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.seen
	a.seen = nil
	return s
}

const probeToken = "koinon_test_not-a-real-token"

// tokenIssuerServer is an issuer that answers only requests with probeToken, as the API server does.
// jwksURI, when set, replaces the JWKS URL in its discovery document.
func tokenIssuerServer(rec *authRecorder, jwksURI func(srv *httptest.Server) string) *httptest.Server {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, r.URL.Query().Get("to"), http.StatusFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+probeToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			jwks := srv.URL + "/openid/v1/jwks"
			if jwksURI != nil {
				jwks = jwksURI(srv)
			}
			_, _ = fmt.Fprintf(w, `{"issuer":%q,"jwks_uri":%q}`, srv.URL, jwks)
		case "/openid/v1/jwks":
			_, _ = w.Write([]byte(usableJWKS))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	return srv
}

// podFiles writes the pod's CA (trusting caOf when set) and token.
func podFiles(t *testing.T, caOf *httptest.Server, inCluster bool) preflight.IssuerProbeFiles {
	t.Helper()
	dir := t.TempDir()
	caFile, tokenFile := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "token")
	var ca []byte
	if caOf != nil {
		ca = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caOf.Certificate().Raw})
	}
	if err := os.WriteFile(caFile, ca, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenFile, []byte(probeToken), 0o600); err != nil {
		t.Fatal(err)
	}
	return preflight.IssuerProbeFiles{CAFile: caFile, TokenFile: tokenFile, InCluster: inCluster}
}

func TestInClusterIssuerUsesThePodsCAAndToken(t *testing.T) {
	g := NewWithT(t)
	rec := &authRecorder{}
	srv := tokenIssuerServer(rec, nil)
	defer srv.Close()
	files := podFiles(t, srv, true)
	tokenFile := files.TokenFile

	res := preflight.ProbeIssuerWith(t.Context(), srv.URL, 2*time.Second, files)
	g.Expect(res.Reachable).To(BeTrue(), res.Detail)
	g.Expect(res.Unverified).To(BeFalse())
	g.Expect(rec.take()).To(Equal([]string{
		"/.well-known/openid-configuration Bearer " + probeToken, "/openid/v1/jwks Bearer " + probeToken,
	}))

	// An issuer outside the cluster gets no credential and only the system roots.
	res = preflight.ProbeIssuerWith(t.Context(), srv.URL, 2*time.Second, preflight.IssuerProbeFiles{CAFile: files.CAFile, TokenFile: tokenFile})
	g.Expect(res.Reachable).To(BeFalse())
	for _, a := range rec.take() {
		g.Expect(a).NotTo(ContainSubstring("Bearer"))
	}

	// The pod's own token issuer comes back for the comparison.
	g.Expect(os.WriteFile(tokenFile, []byte(jwtWithIssuer("https://[fdae:1::1]:10001")), 0o600)).To(Succeed())
	res = preflight.ProbeIssuerWith(t.Context(), srv.URL, 2*time.Second, files)
	g.Expect(res.TokenIssuer).To(Equal("https://[fdae:1::1]:10001"))
}

// When the in-cluster issuer fails TLS verification, the diagnostic retry accepts any certificate, so it
// must not carry the token: the server it reaches is not authenticated.
func TestInClusterTLSRetrySendsNoToken(t *testing.T) {
	g := NewWithT(t)
	rec := &authRecorder{}
	srv := tokenIssuerServer(rec, nil)
	defer srv.Close()
	files := podFiles(t, nil, true) // no CA in the file: srv's certificate does not verify

	res := preflight.ProbeIssuerWith(t.Context(), srv.URL, 2*time.Second, files)
	g.Expect(res.Reachable).To(BeFalse())
	g.Expect(res.Detail).To(ContainSubstring("certificate"))
	seen := rec.take()
	g.Expect(seen).NotTo(BeEmpty()) // the retry reached the server, without the token: it answered 401
	for _, a := range seen {
		g.Expect(a).NotTo(ContainSubstring("Bearer"))
	}
}

// The token goes only on requests in scope, decided per request: a redirect to another scheme on the same
// host, or a jwks_uri on another path, gets none.
func TestInClusterTokenStaysInScopeOnRedirectsAndOtherPaths(t *testing.T) {
	g := NewWithT(t)
	rec := &authRecorder{}
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer plain.Close()
	srv := tokenIssuerServer(rec, func(srv *httptest.Server) string {
		return srv.URL + "/redirect?to=" + url.QueryEscape(plain.URL+"/openid/v1/jwks")
	})
	defer srv.Close()
	files := podFiles(t, srv, true)

	res := preflight.ProbeIssuerWith(t.Context(), srv.URL, 2*time.Second, files)
	g.Expect(res.Reachable).To(BeFalse())
	g.Expect(rec.take()).To(Equal([]string{
		"/.well-known/openid-configuration Bearer " + probeToken,
		"/redirect Bearer " + probeToken, // under the issuer
		"/openid/v1/jwks ",               // http: out of scope, so no token after the redirect
	}))

	srv2 := tokenIssuerServer(rec, func(*httptest.Server) string { return plain.URL + "/keys" })
	defer srv2.Close()
	res = preflight.ProbeIssuerWith(t.Context(), srv2.URL, 2*time.Second, podFiles(t, srv2, true))
	g.Expect(res.Reachable).To(BeFalse())
	g.Expect(rec.take()).To(Equal([]string{"/.well-known/openid-configuration Bearer " + probeToken, "/keys "}))
}
