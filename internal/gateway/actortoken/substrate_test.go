// SPDX-License-Identifier: Apache-2.0

package actortoken_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/gateway/actortoken"
)

const (
	subAtespace = "team-a"
	subActor    = "sbx-1-0a1b2c3d"
	subUID      = "koinon-test-uid-0001"
	ecKID       = "ec-1"
	rsaKID      = "rsa-1"
)

var (
	testKeysOnce sync.Once
	ecKey        *ecdsa.PrivateKey
	ecKey2       *ecdsa.PrivateKey
	rsaKey       *rsa.PrivateKey
)

func testKeys() {
	testKeysOnce.Do(func() {
		var err error
		ecKey, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		Expect(err).NotTo(HaveOccurred())
		ecKey2, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		Expect(err).NotTo(HaveOccurred())
		rsaKey, err = rsa.GenerateKey(rand.Reader, 2048)
		Expect(err).NotTo(HaveOccurred())
	})
}

// fakeIDP is an ate-idp-server stand-in: OIDC discovery and a JWK set over TLS.
type fakeIDP struct {
	srv *httptest.Server

	mu       sync.Mutex
	issuer   string // the discovery issuer; "" means srv.URL
	jwksURI  string // "" means srv.URL + /openid/v1/jwks
	redirect bool
	down     bool
	keys     []jose.JSONWebKey

	discoveryHits  atomic.Int32
	jwksHits       atomic.Int32
	redirectedHits atomic.Int32
}

func newFakeIDP() *fakeIDP {
	f := &fakeIDP{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		f.discoveryHits.Add(1)
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case f.down:
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		case f.redirect:
			http.Redirect(w, r, "/moved/openid-configuration", http.StatusFound)
			return
		}
		f.writeDiscovery(w)
	})
	mux.HandleFunc("GET /moved/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		f.redirectedHits.Add(1)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.writeDiscovery(w)
	})
	mux.HandleFunc("GET /openid/v1/jwks", func(w http.ResponseWriter, _ *http.Request) {
		f.jwksHits.Add(1)
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.down {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: f.keys})
	})
	f.srv = httptest.NewTLSServer(mux)
	return f
}

func (f *fakeIDP) writeDiscovery(w http.ResponseWriter) {
	iss, jwks := f.issuer, f.jwksURI
	if iss == "" {
		iss = f.srv.URL
	}
	if jwks == "" {
		jwks = f.srv.URL + "/openid/v1/jwks"
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"issuer": iss, "jwks_uri": jwks})
}

func (f *fakeIDP) set(fn func(f *fakeIDP)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeIDP) pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(f.srv.Certificate())
	return p
}

func publicJWK(key any, kid string, alg jose.SignatureAlgorithm) jose.JSONWebKey {
	var pub any
	switch k := key.(type) {
	case *ecdsa.PrivateKey:
		pub = &k.PublicKey
	case *rsa.PrivateKey:
		pub = &k.PublicKey
	}
	return jose.JSONWebKey{Key: pub, KeyID: kid, Algorithm: string(alg), Use: "sig"}
}

func mintWith(key any, alg jose.SignatureAlgorithm, kid string, claims map[string]any) string {
	opts := (&jose.SignerOptions{}).WithType("JWT")
	if kid != "" {
		opts = opts.WithHeader("kid", kid)
	}
	s, err := jose.NewSigner(jose.SigningKey{Algorithm: alg, Key: key}, opts)
	Expect(err).NotTo(HaveOccurred())
	tok, err := jwt.Signed(s).Claims(claims).Serialize()
	Expect(err).NotTo(HaveOccurred())
	return tok
}

// substrateClaims returns the claims MintActorJwt writes for a token issued at iat that lives
// life: nbf is iat - 5 min, aud is an array.
func substrateClaims(issuer string, iat time.Time, life time.Duration) map[string]any {
	return map[string]any{
		"iss": issuer,
		"sub": "actor/" + subAtespace + "/" + subActor,
		"aud": []string{actortoken.AudienceEgress},
		"exp": iat.Add(life).Unix(),
		"nbf": iat.Add(-5 * time.Minute).Unix(),
		"iat": iat.Unix(),
		"jti": "koinon-test-jti",
		"actor.ate.dev": map[string]any{
			"atespace": subAtespace, "actorName": subActor, "actorUID": subUID,
		},
	}
}

func selfSignedCAPool() *x509.CertPool {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	Expect(err).NotTo(HaveOccurred())
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "koinon-test-untrusted-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	Expect(err).NotTo(HaveOccurred())
	cert, err := x509.ParseCertificate(der)
	Expect(err).NotTo(HaveOccurred())
	p := x509.NewCertPool()
	p.AddCert(cert)
	return p
}

var _ = Describe("Substrate actor token verifier", func() {
	var (
		idp   *fakeIDP
		now   time.Time
		clock func() time.Time
		v     *actortoken.SubstrateVerifier
		iss   string
	)

	newVerifier := func() *actortoken.SubstrateVerifier {
		sv, err := actortoken.NewSubstrateVerifier(context.Background(), actortoken.SubstrateConfig{
			Issuer: iss, RootCAs: idp.pool(), Now: clock, Audience: actortoken.AudienceEgress,
		})
		Expect(err).NotTo(HaveOccurred())
		return sv
	}
	good := func() string {
		return mintWith(ecKey, jose.ES256, ecKID, substrateClaims(iss, now, 900*time.Second))
	}

	BeforeEach(func() {
		testKeys()
		idp = newFakeIDP()
		DeferCleanup(idp.srv.Close)
		idp.set(func(f *fakeIDP) {
			f.keys = []jose.JSONWebKey{publicJWK(ecKey, ecKID, jose.ES256), publicJWK(rsaKey, rsaKID, jose.RS256)}
		})
		iss = idp.srv.URL
		now = time.Unix(1_800_000_000, 0)
		clock = func() time.Time { return now }
		v = newVerifier()
	})

	Describe("construction", func() {
		It("needs a CA pool or a CA file and accepts no empty pool", func() {
			_, err := actortoken.NewSubstrateVerifier(context.Background(), actortoken.SubstrateConfig{Issuer: iss, Audience: actortoken.AudienceEgress})
			Expect(err).To(MatchError(ContainSubstring("CA file or a CA pool")))
			_, err = actortoken.NewSubstrateVerifier(context.Background(), actortoken.SubstrateConfig{
				Issuer: iss, RootCAs: x509.NewCertPool(), Audience: actortoken.AudienceEgress,
			})
			Expect(err).To(MatchError(ContainSubstring("empty")))
			_, err = actortoken.NewSubstrateVerifier(context.Background(), actortoken.SubstrateConfig{
				Issuer: iss, RootCAs: idp.pool(), CAFile: "/nonexistent", Audience: actortoken.AudienceEgress,
			})
			Expect(err).To(MatchError(ContainSubstring("not both")))
		})

		It("dials the test address but keeps the issuer name for the certificate check", func() {
			// The fake IDP certificate is valid for example.com, not for wrong.example.net.
			addr := idp.srv.Listener.Addr().String()
			dialed := func(name string) *actortoken.SubstrateVerifier {
				idp.set(func(f *fakeIDP) { f.issuer, f.jwksURI = name, name+"/openid/v1/jwks" })
				sv, err := actortoken.NewSubstrateVerifier(context.Background(), actortoken.SubstrateConfig{
					Issuer: name, RootCAs: idp.pool(), Now: clock, Audience: actortoken.AudienceEgress, IssuerAddr: addr,
				})
				Expect(err).NotTo(HaveOccurred())
				return sv
			}
			_, err := dialed("https://example.com").Verify(mintWith(ecKey, jose.ES256, ecKID, substrateClaims("https://example.com", now, 900*time.Second)))
			Expect(err).NotTo(HaveOccurred())

			_, err = dialed("https://wrong.example.net").Verify(mintWith(ecKey, jose.ES256, ecKID, substrateClaims("https://wrong.example.net", now, 900*time.Second)))
			Expect(err).To(MatchError(actortoken.ErrUnavailable))
			var hostErr x509.HostnameError
			Expect(errors.As(err, &hostErr)).To(BeTrue(), "the cause is the certificate name check: %v", err)
		})

		DescribeTable("refuses a bad issuer address",
			func(addr, want string) {
				_, err := actortoken.NewSubstrateVerifier(context.Background(), actortoken.SubstrateConfig{
					Issuer: iss, RootCAs: idp.pool(), Audience: actortoken.AudienceEgress, IssuerAddr: addr,
				})
				Expect(err).To(MatchError(ContainSubstring(want)))
			},
			Entry("no port", "127.0.0.1", "host:port"),
			Entry("empty host", ":443", "empty host"),
			Entry("named port", "127.0.0.1:https", "numeric port"),
			Entry("port 0", "127.0.0.1:0", "numeric port"),
			Entry("port too large", "127.0.0.1:70000", "numeric port"),
		)

		It("reads a PEM CA file and refuses a file with no certificate", func() {
			dir := GinkgoT().TempDir()
			good := filepath.Join(dir, "ca.pem")
			Expect(os.WriteFile(good, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: idp.srv.Certificate().Raw}), 0o600)).To(Succeed())
			sv, err := actortoken.NewSubstrateVerifier(context.Background(), actortoken.SubstrateConfig{Issuer: iss, CAFile: good, Now: clock, Audience: actortoken.AudienceEgress})
			Expect(err).NotTo(HaveOccurred())
			_, err = sv.Verify(mintWith(ecKey, jose.ES256, ecKID, substrateClaims(iss, now, 900*time.Second)))
			Expect(err).NotTo(HaveOccurred())

			bad := filepath.Join(dir, "bad.pem")
			Expect(os.WriteFile(bad, []byte("not a certificate"), 0o600)).To(Succeed())
			_, err = actortoken.NewSubstrateVerifier(context.Background(), actortoken.SubstrateConfig{Issuer: iss, CAFile: bad, Audience: actortoken.AudienceEgress})
			Expect(err).To(MatchError(ContainSubstring("no PEM certificate")))
		})

		It("needs an https issuer with no query or fragment", func() {
			for _, bad := range []string{"", "http://ate-idp.example", "https://", "https://u@ate-idp.example", "https://ate-idp.example?x=1", "https://ate-idp.example#f"} {
				_, err := actortoken.NewSubstrateVerifier(context.Background(), actortoken.SubstrateConfig{Issuer: bad, RootCAs: idp.pool(), Audience: actortoken.AudienceEgress})
				Expect(err).To(HaveOccurred(), bad)
			}
		})

		It("does not contact the issuer before the first Verify", func() {
			Expect(idp.discoveryHits.Load()).To(BeZero())
		})
	})

	Describe("a valid token", func() {
		It("accepts an ES256 token as MintActorJwt writes it", func() {
			c, err := v.Verify(good())
			Expect(err).NotTo(HaveOccurred())
			Expect(c).To(Equal(actortoken.SubstrateClaims{
				Atespace: subAtespace, ActorName: subActor, ActorUID: subUID,
				Expiry: now.Add(900 * time.Second),
			}))
		})

		It("refuses an RS256 token, also when the issuer publishes an RS256 key, and names the alg", func() {
			_, err := v.Verify(mintWith(rsaKey, jose.RS256, rsaKID, substrateClaims(iss, now, 900*time.Second)))
			Expect(err).To(MatchError(actortoken.ErrAlgorithm))
			Expect(err).To(MatchError(actortoken.ErrInvalid))
			Expect(err.Error()).To(ContainSubstring(`alg "RS256" is not ES256`))
			Expect(actortoken.Reason(err)).To(Equal("alg"))
		})

		It("fetches the keys once for many concurrent requests", func() {
			tok := good()
			var wg sync.WaitGroup
			errs := make([]error, 16)
			for i := range errs {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, errs[i] = v.Verify(tok)
				}()
			}
			wg.Wait()
			for _, err := range errs {
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(idp.jwksHits.Load()).To(Equal(int32(1)))
		})
	})

	Describe("the issuer's documents", func() {
		It("refuses a discovery document with another issuer", func() {
			idp.set(func(f *fakeIDP) { f.issuer = iss + "/other" })
			_, err := v.Verify(good())
			Expect(err).To(MatchError(actortoken.ErrUnavailable))
			Expect(errors.Is(err, actortoken.ErrInvalid)).To(BeFalse())
			Expect(idp.jwksHits.Load()).To(BeZero())
		})

		It("refuses a jwks_uri that is not https", func() {
			idp.set(func(f *fakeIDP) { f.jwksURI = strings.Replace(iss, "https://", "http://", 1) + "/openid/v1/jwks" })
			_, err := v.Verify(good())
			Expect(err).To(MatchError(actortoken.ErrUnavailable))
			Expect(idp.jwksHits.Load()).To(BeZero())
		})

		It("does not follow a redirect", func() {
			idp.set(func(f *fakeIDP) { f.redirect = true })
			_, err := v.Verify(good())
			Expect(err).To(MatchError(actortoken.ErrUnavailable))
			Expect(err).To(MatchError(ContainSubstring("redirect")))
			Expect(idp.redirectedHits.Load()).To(BeZero())
		})

		It("refuses a server certificate the CA pool does not sign", func() {
			sv, err := actortoken.NewSubstrateVerifier(context.Background(), actortoken.SubstrateConfig{
				Issuer: iss, RootCAs: selfSignedCAPool(), Now: clock, Audience: actortoken.AudienceEgress,
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = sv.Verify(good())
			Expect(err).To(MatchError(actortoken.ErrUnavailable))
			Expect(err.Error()).To(ContainSubstring("certificate"))
			Expect(idp.discoveryHits.Load()).To(BeZero())
		})

		It("ignores a published key whose alg does not fit its type", func() {
			idp.set(func(f *fakeIDP) {
				f.keys = []jose.JSONWebKey{publicJWK(ecKey, ecKID, jose.ES256), publicJWK(rsaKey, rsaKID, jose.ES256)}
			})
			_, err := v.Verify(mintWith(ecKey, jose.ES256, rsaKID, substrateClaims(iss, now, 900*time.Second)))
			Expect(err).To(MatchError(actortoken.ErrUnknownKey))
		})

		It("does not use an RS256 key from the JWK set", func() {
			idp.set(func(f *fakeIDP) { f.keys = []jose.JSONWebKey{publicJWK(rsaKey, rsaKID, jose.RS256)} })
			_, err := v.Verify(good())
			Expect(err).To(MatchError(actortoken.ErrUnavailable))
		})

		It("refuses while the issuer is down and recovers after the refetch interval", func() {
			idp.set(func(f *fakeIDP) { f.down = true })
			_, err := v.Verify(good())
			Expect(err).To(MatchError(actortoken.ErrUnavailable))
			Expect(actortoken.Reason(err)).To(Equal("unavailable"))

			idp.set(func(f *fakeIDP) { f.down = false })
			now = now.Add(30 * time.Second)
			_, err = v.Verify(good())
			Expect(err).To(MatchError(actortoken.ErrUnavailable), "no second fetch within a minute")

			now = now.Add(31 * time.Second)
			_, err = v.Verify(good())
			Expect(err).NotTo(HaveOccurred())
		})

		It("stops using keys older than KeyMaxAge when the issuer is down", func() {
			_, err := v.Verify(good())
			Expect(err).NotTo(HaveOccurred())
			idp.set(func(f *fakeIDP) { f.down = true })
			now = now.Add(actortoken.KeyMaxAge - time.Second)
			_, err = v.Verify(good())
			Expect(err).NotTo(HaveOccurred(), "keys still fresh")
			now = now.Add(2 * time.Second)
			_, err = v.Verify(good())
			Expect(err).To(MatchError(actortoken.ErrUnavailable))
		})
	})

	Describe("algorithms and keys", func() {
		It("refuses alg none", func() {
			enc := base64.RawURLEncoding.EncodeToString
			hdr, _ := json.Marshal(map[string]string{"alg": "none", "kid": ecKID, "typ": "JWT"})
			body, _ := json.Marshal(substrateClaims(iss, now, 900*time.Second))
			_, err := v.Verify(enc(hdr) + "." + enc(body) + ".")
			Expect(err).To(MatchError(actortoken.ErrAlgorithm))
			Expect(err.Error()).To(ContainSubstring(`alg "none"`))
		})

		It("refuses HS256", func() {
			tok := mintWith([]byte("koinon_test_not-a-real-hmac-key-aaaaaaaa"), jose.HS256, ecKID, substrateClaims(iss, now, 900*time.Second))
			_, err := v.Verify(tok)
			Expect(err).To(MatchError(actortoken.ErrAlgorithm))
			Expect(err.Error()).To(ContainSubstring(`alg "HS256"`))
		})

		It("refuses RS384", func() {
			_, err := v.Verify(mintWith(rsaKey, jose.RS384, rsaKID, substrateClaims(iss, now, 900*time.Second)))
			Expect(err).To(MatchError(actortoken.ErrAlgorithm))
		})

		It("refuses an alg that is not the alg of the named key", func() {
			_, err := v.Verify(mintWith(rsaKey, jose.RS256, ecKID, substrateClaims(iss, now, 900*time.Second)))
			Expect(err).To(MatchError(actortoken.ErrAlgorithm))
		})

		It("refuses an empty alg and caps a long alg in the error", func() {
			enc := base64.RawURLEncoding.EncodeToString
			body, _ := json.Marshal(substrateClaims(iss, now, 900*time.Second))
			for _, alg := range []string{"", strings.Repeat("A", 200)} {
				hdr, _ := json.Marshal(map[string]string{"alg": alg, "kid": ecKID, "typ": "JWT"})
				_, err := v.Verify(enc(hdr) + "." + enc(body) + ".c2ln")
				Expect(err).To(MatchError(actortoken.ErrAlgorithm), "alg %q", alg)
				Expect(len(err.Error())).To(BeNumerically("<", 100))
			}
		})

		It("refuses a token with no kid", func() {
			_, err := v.Verify(mintWith(ecKey, jose.ES256, "", substrateClaims(iss, now, 900*time.Second)))
			Expect(err).To(MatchError(actortoken.ErrUnknownKey))
		})

		It("refuses a token signed by another key under a published kid", func() {
			_, err := v.Verify(mintWith(ecKey2, jose.ES256, ecKID, substrateClaims(iss, now, 900*time.Second)))
			Expect(err).To(MatchError(actortoken.ErrSignature))
		})

		It("refetches at most once a minute for an unknown kid", func() {
			_, err := v.Verify(good())
			Expect(err).NotTo(HaveOccurred())
			Expect(idp.jwksHits.Load()).To(Equal(int32(1)))

			now = now.Add(2 * time.Minute)
			_, err = v.Verify(mintWith(ecKey2, jose.ES256, "unknown-1", substrateClaims(iss, now, 900*time.Second)))
			Expect(err).To(MatchError(actortoken.ErrUnknownKey))
			Expect(idp.jwksHits.Load()).To(Equal(int32(2)))

			now = now.Add(30 * time.Second)
			_, err = v.Verify(mintWith(ecKey2, jose.ES256, "unknown-2", substrateClaims(iss, now, 900*time.Second)))
			Expect(err).To(MatchError(actortoken.ErrUnknownKey))
			Expect(idp.jwksHits.Load()).To(Equal(int32(2)), "no refetch within a minute")

			now = now.Add(30 * time.Second)
			_, err = v.Verify(mintWith(ecKey2, jose.ES256, "unknown-3", substrateClaims(iss, now, 900*time.Second)))
			Expect(err).To(MatchError(actortoken.ErrUnknownKey))
			Expect(idp.jwksHits.Load()).To(Equal(int32(3)), "a refetch after a minute")
		})

		It("accepts a rotated key once it is published", func() {
			_, err := v.Verify(good())
			Expect(err).NotTo(HaveOccurred())
			idp.set(func(f *fakeIDP) { f.keys = append(f.keys, publicJWK(ecKey2, "ec-2", jose.ES256)) })
			now = now.Add(61 * time.Second)
			c, err := v.Verify(mintWith(ecKey2, jose.ES256, "ec-2", substrateClaims(iss, now, 900*time.Second)))
			Expect(err).NotTo(HaveOccurred())
			Expect(c.ActorName).To(Equal(subActor))
			Expect(idp.jwksHits.Load()).To(Equal(int32(2)))
		})
	})

	Describe("claims", func() {
		withClaims := func(edit func(m map[string]any)) string {
			m := substrateClaims(iss, now, 900*time.Second)
			edit(m)
			return mintWith(ecKey, jose.ES256, ecKID, m)
		}

		It("refuses another iss", func() {
			_, err := v.Verify(withClaims(func(m map[string]any) { m["iss"] = iss + "/" }))
			Expect(err).To(MatchError(actortoken.ErrIssuer))
		})

		It("refuses a missing or other aud", func() {
			_, err := v.Verify(withClaims(func(m map[string]any) { delete(m, "aud") }))
			Expect(err).To(MatchError(actortoken.ErrAudience))
			_, err = v.Verify(withClaims(func(m map[string]any) { m["aud"] = []string{"other"} }))
			Expect(err).To(MatchError(actortoken.ErrAudience))
		})

		It("refuses an aud array that also names another audience (issue #1489)", func() {
			_, err := v.Verify(withClaims(func(m map[string]any) { m["aud"] = []string{"other", actortoken.AudienceEgress} }))
			Expect(err).To(MatchError(actortoken.ErrAudience))
			_, err = v.Verify(withClaims(func(m map[string]any) { m["aud"] = []string{actortoken.AudienceEgress, actortoken.AudienceEgress} }))
			Expect(err).To(MatchError(actortoken.ErrAudience))
		})

		It("accepts an aud string that equals the listener audience", func() {
			_, err := v.Verify(withClaims(func(m map[string]any) { m["aud"] = actortoken.AudienceEgress }))
			Expect(err).NotTo(HaveOccurred())
		})

		It("refuses the token of one listener on every other listener (issue #1489)", func() {
			for _, aud := range []string{actortoken.AudienceModel, actortoken.AudienceMCP, actortoken.AudienceEgress} {
				tok := withClaims(func(m map[string]any) { m["aud"] = []string{aud} })
				for _, listener := range []string{actortoken.AudienceModel, actortoken.AudienceMCP, actortoken.AudienceEgress} {
					lv, err := v.WithAudience(listener)
					Expect(err).NotTo(HaveOccurred())
					Expect(lv.Audience()).To(Equal(listener))
					_, err = lv.Verify(tok)
					if aud == listener {
						Expect(err).NotTo(HaveOccurred(), aud)
					} else {
						Expect(err).To(MatchError(actortoken.ErrAudience), aud+" on "+listener)
					}
				}
			}
		})

		It("refuses the Ed25519 audience and an audience that is not a listener's", func() {
			_, err := v.Verify(withClaims(func(m map[string]any) { m["aud"] = []string{actortoken.Audience} }))
			Expect(err).To(MatchError(actortoken.ErrAudience))
			for _, bad := range []string{"", actortoken.Audience, "other"} {
				_, err = v.WithAudience(bad)
				Expect(err).To(MatchError(ContainSubstring("not a gateway listener audience")), bad)
				_, err = actortoken.NewSubstrateVerifier(context.Background(), actortoken.SubstrateConfig{
					Issuer: iss, RootCAs: idp.pool(), Audience: bad,
				})
				Expect(err).To(MatchError(ContainSubstring("not a gateway listener audience")), bad)
			}
		})

		It("shares the key cache between the listener verifiers", func() {
			mv, err := v.WithAudience(actortoken.AudienceMCP)
			Expect(err).NotTo(HaveOccurred())
			_, err = v.Verify(good())
			Expect(err).NotTo(HaveOccurred())
			fetches := idp.jwksHits.Load()
			_, err = mv.Verify(withClaims(func(m map[string]any) { m["aud"] = []string{actortoken.AudienceMCP} }))
			Expect(err).NotTo(HaveOccurred())
			Expect(idp.jwksHits.Load()).To(Equal(fetches), "the MCP verifier used the keys the egress verifier fetched")
		})

		It("refuses an expired token beyond the leeway", func() {
			tok := good()
			now = now.Add(900*time.Second + actortoken.SubstrateLeeway - time.Second)
			_, err := v.Verify(tok)
			Expect(err).NotTo(HaveOccurred(), "inside the leeway")
			now = now.Add(2 * time.Second)
			_, err = v.Verify(tok)
			Expect(err).To(MatchError(actortoken.ErrExpired))
		})

		It("refuses nbf or iat in the future", func() {
			_, err := v.Verify(withClaims(func(m map[string]any) { m["nbf"] = now.Add(time.Minute).Unix() }))
			Expect(err).To(MatchError(actortoken.ErrNotYetValid))
			_, err = v.Verify(mintWith(ecKey, jose.ES256, ecKID, substrateClaims(iss, now.Add(2*time.Minute), 900*time.Second)))
			Expect(err).To(MatchError(actortoken.ErrNotYetValid))
		})

		It("checks exp - iat, not exp - nbf, against 300 to 3600 seconds", func() {
			for life, ok := range map[time.Duration]bool{299 * time.Second: false, 300 * time.Second: true, 3600 * time.Second: true, 3601 * time.Second: false} {
				_, err := v.Verify(mintWith(ecKey, jose.ES256, ecKID, substrateClaims(iss, now, life)))
				if ok {
					Expect(err).NotTo(HaveOccurred(), life.String())
				} else {
					Expect(err).To(MatchError(actortoken.ErrLifetime), life.String())
				}
			}
		})

		It("refuses a token with a missing time claim or jti", func() {
			for _, c := range []string{"exp", "iat", "nbf", "jti"} {
				_, err := v.Verify(withClaims(func(m map[string]any) { delete(m, c) }))
				Expect(err).To(MatchError(actortoken.ErrLifetime), c)
			}
		})

		It("refuses a sub that does not name the claim's actor", func() {
			_, err := v.Verify(withClaims(func(m map[string]any) { m["sub"] = "actor/" + subAtespace + "/other" }))
			Expect(err).To(MatchError(actortoken.ErrSubject))
			_, err = v.Verify(withClaims(func(m map[string]any) { m["sub"] = "actor/other/" + subActor }))
			Expect(err).To(MatchError(actortoken.ErrSubject))
			_, err = v.Verify(withClaims(func(m map[string]any) { delete(m, "actor.ate.dev") }))
			Expect(err).To(MatchError(actortoken.ErrSubject))
			_, err = v.Verify(withClaims(func(m map[string]any) {
				m["actor.ate.dev"] = map[string]any{"atespace": subAtespace, "actorName": subActor}
			}))
			Expect(err).To(MatchError(actortoken.ErrSubject), "no actorUID")
		})

		It("refuses a worker-for-actor token", func() {
			claim := map[string]any{"atespace": subAtespace, "actorName": subActor, "actorUID": subUID, "node": "node-1"}
			_, err := v.Verify(withClaims(func(m map[string]any) {
				m["sub"] = "worker-for-actor/" + subAtespace + "/" + subActor
				delete(m, "actor.ate.dev")
				m["worker-for-actor.ate.dev"] = claim
			}))
			Expect(err).To(MatchError(actortoken.ErrSubject))
			_, err = v.Verify(withClaims(func(m map[string]any) { m["worker-for-actor.ate.dev"] = claim }))
			Expect(err).To(MatchError(actortoken.ErrSubject), "both claims")
			_, err = v.Verify(withClaims(func(m map[string]any) { m["sub"] = "worker-for-actor/" + subAtespace + "/" + subActor }))
			Expect(err).To(MatchError(actortoken.ErrSubject), "worker sub with an actor claim")
		})
	})

	Describe("the request value", func() {
		It("refuses the placeholder", func() {
			_, err := v.Verify(actortoken.Placeholder)
			Expect(err).To(MatchError(actortoken.ErrPlaceholder))
			Expect(errors.Is(err, actortoken.ErrInvalid)).To(BeTrue())
			Expect(actortoken.Reason(err)).To(Equal("placeholder"))
			Expect(idp.discoveryHits.Load()).To(BeZero())
		})

		It("is refused by the Ed25519 verifier too", func() {
			pub, _, err := ed25519.GenerateKey(rand.Reader)
			Expect(err).NotTo(HaveOccurred())
			ev, err := actortoken.NewVerifier("koinon-test-issuer", map[string]ed25519.PublicKey{"k1": pub})
			Expect(err).NotTo(HaveOccurred())
			_, err = ev.Verify(actortoken.Placeholder)
			Expect(err).To(MatchError(actortoken.ErrInvalid))
		})

		It("refuses an oversized token before it parses it", func() {
			_, err := v.Verify(strings.Repeat("a", actortoken.MaxTokenSize+1))
			Expect(err).To(MatchError(actortoken.ErrTokenTooLarge))
			tok := mintWith(ecKey, jose.ES256, ecKID, func() map[string]any {
				m := substrateClaims(iss, now, 900*time.Second)
				m["pad"] = strings.Repeat("x", actortoken.MaxTokenSize)
				return m
			}())
			_, err = v.Verify(tok)
			Expect(err).To(MatchError(actortoken.ErrTokenTooLarge))
			Expect(idp.discoveryHits.Load()).To(BeZero())
		})

		It("refuses garbage as malformed", func() {
			_, err := v.Verify("koinon_test_not-a-real-token")
			Expect(err).To(MatchError(actortoken.ErrMalformed))
			Expect(actortoken.Reason(err)).To(Equal("malformed"))
		})
	})
})
