// SPDX-License-Identifier: Apache-2.0

package escape_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/gateway/actortoken"
)

// fakeIssuerKID is the key id of the fake issuer's one signing key.
const fakeIssuerKID = "koinon-escape-ec-1"

// fakeIssuer stands in for ate-idp-server in the contract specs: OIDC discovery and a JWK set over TLS
// with one ES256 key, the shape that actortoken.SubstrateVerifier reads from the install's issuer.
type fakeIssuer struct {
	srv *httptest.Server
	key *ecdsa.PrivateKey
}

func newFakeIssuer() *fakeIssuer {
	GinkgoHelper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	Expect(err).NotTo(HaveOccurred())
	f := &fakeIssuer{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": f.srv.URL, "jwks_uri": f.srv.URL + "/openid/v1/jwks"})
	})
	mux.HandleFunc("GET /openid/v1/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key: &key.PublicKey, KeyID: fakeIssuerKID, Algorithm: string(jose.ES256), Use: "sig",
		}}})
	})
	f.srv = httptest.NewTLSServer(mux)
	return f
}

func (f *fakeIssuer) close() { f.srv.Close() }

// verifier returns a SubstrateVerifier of the listener audience that trusts this issuer only, with the
// clock now (nil: time.Now).
func (f *fakeIssuer) verifier(ctx context.Context, audience string, now func() time.Time) *actortoken.SubstrateVerifier {
	GinkgoHelper()
	pool := x509.NewCertPool()
	pool.AddCert(f.srv.Certificate())
	v, err := actortoken.NewSubstrateVerifier(context.WithoutCancel(ctx), actortoken.SubstrateConfig{
		Issuer: f.srv.URL, RootCAs: pool, Audience: audience, Now: now,
	})
	Expect(err).NotTo(HaveOccurred())
	return v
}

// claims returns the claims that Substrate's MintActorJwt writes for one actor (Substrate v1.0.1
// internal/actoridjwt/actoridjwt.go): aud an array, nbf 5 minutes before iat, the actor.ate.dev claim.
func (f *fakeIssuer) claims(aud []string, iat time.Time, life time.Duration) map[string]any {
	return map[string]any{
		"iss": f.srv.URL,
		"sub": "actor/koinon-escape/kesi-1",
		"aud": aud,
		"exp": iat.Add(life).Unix(),
		"nbf": iat.Add(-5 * time.Minute).Unix(),
		"iat": iat.Unix(),
		"jti": "koinon_test_not-a-real-jti",
		"actor.ate.dev": map[string]any{
			"atespace": "koinon-escape", "actorName": "kesi-1", "actorUID": "koinon_test_not-a-real-uid",
		},
	}
}

// mint signs claims with the issuer's key.
func (f *fakeIssuer) mint(claims map[string]any) string {
	GinkgoHelper()
	return signES256(f.key, claims)
}

// signES256 signs claims with key under the fake issuer's key id.
func signES256(key *ecdsa.PrivateKey, claims map[string]any) string {
	GinkgoHelper()
	s, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", fakeIssuerKID))
	Expect(err).NotTo(HaveOccurred())
	tok, err := jwt.Signed(s).Claims(claims).Serialize()
	Expect(err).NotTo(HaveOccurred())
	return tok
}
