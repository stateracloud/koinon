// SPDX-License-Identifier: Apache-2.0

package route_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/gateway/actortoken"
	"github.com/stateracloud/koinon/internal/gateway/route"
)

// idp is a small ate-idp-server stand-in: OIDC discovery and a JWK set of one EC key, over TLS.
type idp struct {
	srv  *httptest.Server
	key  *ecdsa.PrivateKey
	down atomic.Bool
}

func newIDP() *idp {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	Expect(err).NotTo(HaveOccurred())
	f := &idp{key: k}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		if f.down.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": f.srv.URL, "jwks_uri": f.srv.URL + "/openid/v1/jwks"})
	})
	mux.HandleFunc("GET /openid/v1/jwks", func(w http.ResponseWriter, _ *http.Request) {
		if f.down.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		jwk := jose.JSONWebKey{Key: &f.key.PublicKey, KeyID: "ec-1", Algorithm: string(jose.ES256), Use: "sig"}
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{jwk}})
	})
	f.srv = httptest.NewTLSServer(mux)
	DeferCleanup(f.srv.Close)
	return f
}

func (f *idp) pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(f.srv.Certificate())
	return p
}

// mint returns a token as MintActorJwt writes it for the actor atespace/name with uid.
func (f *idp) mint(atespace, name, uid string, iat time.Time) string {
	s, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: f.key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "ec-1"))
	Expect(err).NotTo(HaveOccurred())
	tok, err := jwt.Signed(s).Claims(map[string]any{
		"iss": f.srv.URL,
		"sub": "actor/" + atespace + "/" + name,
		"aud": []string{actortoken.AudienceEgress},
		"exp": iat.Add(15 * time.Minute).Unix(),
		"nbf": iat.Add(-5 * time.Minute).Unix(),
		"iat": iat.Unix(),
		"jti": "koinon-test-jti",
		"actor.ate.dev": map[string]any{
			"atespace": atespace, "actorName": name, "actorUID": uid,
		},
	}).Serialize()
	Expect(err).NotTo(HaveOccurred())
	return tok
}

var _ = Describe("actor identity (issue #1386)", func() {
	const (
		space = "acme-space"
		name  = "ka-0123456789abcdef-0a1b2c3d"
		uid   = "koinon-test-uid-0001"
	)
	var (
		now    = time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
		acme   = actortoken.Actor{Org: "acme", Agent: "builder", Run: "run-1"}
		key    = actortoken.SubstrateActor{Atespace: space, Name: name, UID: uid}
		forge  = route.Route{Name: "acme/forge", Class: route.ClassForge, Org: "acme", Host: "git.example.com", Methods: []string{"GET"}, PathPrefixes: []string{"/acme"}}
		issuer *idp
		actors *route.ActorTable
		gate   *route.Gate
	)

	req := func(tok string) route.Request {
		return route.Request{Scheme: "https", Token: tok, Authority: "git.example.com", Method: "GET", Path: "/acme/app.git"}
	}

	Describe("the Substrate identifier", func() {
		BeforeEach(func() {
			issuer = newIDP()
			ver, err := actortoken.NewSubstrateVerifier(context.Background(), actortoken.SubstrateConfig{
				Issuer: issuer.srv.URL, RootCAs: issuer.pool(), Now: func() time.Time { return now }, Audience: actortoken.AudienceEgress,
			})
			Expect(err).NotTo(HaveOccurred())
			actors = &route.ActorTable{}
			actors.Set(map[actortoken.SubstrateActor]actortoken.Actor{key: acme})
			id, err := route.NewSubstrateIdentifier(ver, actors)
			Expect(err).NotTo(HaveOccurred())
			t, err := route.Compile([]route.Route{forge})
			Expect(err).NotTo(HaveOccurred())
			gate = route.NewGate(id, t)
		})

		It("allows a known actor, with the org, agent and run of Koinon's record", func() {
			d := gate.Check(req(issuer.mint(space, name, uid, now)))
			Expect(d.Allowed).To(BeTrue(), string(d.Reason))
			Expect(d.Actor).To(Equal(acme))
			Expect(d.TokenReason).To(BeEmpty())
		})

		DescribeTable("refuses an actor that Koinon's record does not hold",
			func(atespace, actor, actorUID string) {
				d := gate.Check(req(issuer.mint(atespace, actor, actorUID, now)))
				Expect(d.Allowed).To(BeFalse())
				Expect(d.Reason).To(Equal(route.ReasonUnknownActor))
				Expect(d.TokenReason).To(Equal("unknown_actor"))
				Expect(d.Actor).To(Equal(actortoken.Actor{}))
			},
			Entry("another UID for the same name", space, name, "koinon-test-uid-0002"),
			Entry("an unknown actor name", space, "ka-0123456789abcdef-ffffffff", uid),
			Entry("the same name in another workspace", "globex-space", name, uid),
		)

		It("refuses an actor that a later State revision removes, and reads the table on every call", func() {
			tok := issuer.mint(space, name, uid, now)
			Expect(gate.Check(req(tok)).Allowed).To(BeTrue())
			actors.Set(map[actortoken.SubstrateActor]actortoken.Actor{})
			d := gate.Check(req(tok))
			Expect(d.Allowed).To(BeFalse())
			Expect(d.Reason).To(Equal(route.ReasonUnknownActor))
			actors.Set(map[actortoken.SubstrateActor]actortoken.Actor{key: acme})
			Expect(gate.Check(req(tok)).Allowed).To(BeTrue())
			actors.Set(nil)
			Expect(gate.Check(req(tok)).Reason).To(Equal(route.ReasonUnknownActor))
		})

		It("refuses every token before the first State revision", func() {
			ver, err := actortoken.NewSubstrateVerifier(context.Background(), actortoken.SubstrateConfig{
				Issuer: issuer.srv.URL, RootCAs: issuer.pool(), Now: func() time.Time { return now }, Audience: actortoken.AudienceEgress,
			})
			Expect(err).NotTo(HaveOccurred())
			id, err := route.NewSubstrateIdentifier(ver, &route.ActorTable{})
			Expect(err).NotTo(HaveOccurred())
			_, err = id.Identify(issuer.mint(space, name, uid, now))
			Expect(err).To(MatchError(actortoken.ErrUnknownActor))
		})

		It("refuses the placeholder", func() {
			d := gate.Check(req(actortoken.Placeholder))
			Expect(d.Allowed).To(BeFalse())
			Expect(d.Reason).To(Equal(route.ReasonBadToken))
			Expect(d.TokenReason).To(Equal("placeholder"))
		})

		It("refuses a request with two token values, also when both are valid", func() {
			tok := issuer.mint(space, name, uid, now)
			r := httptest.NewRequest(http.MethodGet, "https://git.example.com/acme/app.git", nil)
			r.Header.Add(route.TokenHeader, tok)
			r.Header.Add(route.TokenHeader, tok)
			d := gate.Check(route.FromHTTP(r))
			Expect(d.Allowed).To(BeFalse())
			Expect(d.Reason).To(Equal(route.ReasonBadToken))
			Expect(d.TokenReason).To(Equal("multiple_tokens"))
		})

		It("refuses with identity_unavailable, never allows, when the issuer keys cannot be read", func() {
			issuer.down.Store(true)
			d := gate.Check(req(issuer.mint(space, name, uid, now)))
			Expect(d.Allowed).To(BeFalse())
			Expect(d.Reason).To(Equal(route.ReasonIdentityUnavailable))
			Expect(d.TokenReason).To(Equal("unavailable"))
		})

		It("refuses a token the verifier refuses, with its reason", func() {
			d := gate.Check(req(issuer.mint(space, name, uid, now.Add(-2*time.Hour))))
			Expect(d.Reason).To(Equal(route.ReasonBadToken))
			Expect(d.TokenReason).To(Equal("expired"))
		})

		It("needs a verifier and a table", func() {
			_, err := route.NewSubstrateIdentifier(nil, &route.ActorTable{})
			Expect(err).To(HaveOccurred())
			ver, err := actortoken.NewSubstrateVerifier(context.Background(), actortoken.SubstrateConfig{Issuer: issuer.srv.URL, RootCAs: issuer.pool(), Audience: actortoken.AudienceEgress})
			Expect(err).NotTo(HaveOccurred())
			_, err = route.NewSubstrateIdentifier(ver, nil)
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("the Ed25519 identifier (exit: #1386 PR B, after #1094)", func() {
		var minter *actortoken.Minter

		BeforeEach(func() {
			pub, priv, err := ed25519.GenerateKey(rand.Reader)
			Expect(err).NotTo(HaveOccurred())
			minter, err = actortoken.NewMinter("koinon-test", "k1", priv)
			Expect(err).NotTo(HaveOccurred())
			ver, err := actortoken.NewVerifier("koinon-test", map[string]ed25519.PublicKey{"k1": pub})
			Expect(err).NotTo(HaveOccurred())
			t, err := route.Compile([]route.Route{forge})
			Expect(err).NotTo(HaveOccurred())
			gate = route.NewGate(ver, t)
		})

		It("still takes the actor from the token", func() {
			tok, err := minter.Mint(acme, 0)
			Expect(err).NotTo(HaveOccurred())
			d := gate.Check(req(tok))
			Expect(d.Allowed).To(BeTrue(), string(d.Reason))
			Expect(d.Actor).To(Equal(acme))
		})

		It("refuses the placeholder and two token values", func() {
			Expect(gate.Check(req(actortoken.Placeholder)).TokenReason).To(Equal("placeholder"))
			tok, err := minter.Mint(acme, 0)
			Expect(err).NotTo(HaveOccurred())
			r := req(tok)
			r.RepeatedToken = true
			d := gate.Check(r)
			Expect(d.Allowed).To(BeFalse())
			Expect(d.Reason).To(Equal(route.ReasonBadToken))
			Expect(d.TokenReason).To(Equal("multiple_tokens"))
		})

		It("refuses a bad token as bad_token, and a nil verifier refuses", func() {
			Expect(gate.Check(req("not-a-token")).Reason).To(Equal(route.ReasonBadToken))
			var nilVer *actortoken.Verifier
			_, err := nilVer.Identify("x")
			Expect(err).To(MatchError(actortoken.ErrInvalid))
		})
	})
})
