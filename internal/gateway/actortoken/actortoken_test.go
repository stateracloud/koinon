// SPDX-License-Identifier: Apache-2.0

package actortoken_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/gateway/actortoken"
)

var _ = Describe("actor token", func() {
	var (
		pub    ed25519.PublicKey
		priv   ed25519.PrivateKey
		t0     time.Time
		minter *actortoken.Minter
		ver    *actortoken.Verifier
		actor  = actortoken.Actor{Org: "acme", Agent: "builder", Run: "run-1"}
	)

	BeforeEach(func() {
		var err error
		pub, priv, err = ed25519.GenerateKey(rand.Reader)
		Expect(err).NotTo(HaveOccurred())
		t0 = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
		minter, err = actortoken.NewMinter("koinon-test", "k1", priv)
		Expect(err).NotTo(HaveOccurred())
		minter = minter.WithClock(func() time.Time { return t0 })
		ver, err = actortoken.NewVerifier("koinon-test", map[string]ed25519.PublicKey{"k1": pub})
		Expect(err).NotTo(HaveOccurred())
		ver = ver.WithClock(func() time.Time { return t0 })
	})

	It("round-trips the actor with the gateway audience", func() {
		tok, err := minter.Mint(actor, 0)
		Expect(err).NotTo(HaveOccurred())
		c, err := ver.Verify(tok)
		Expect(err).NotTo(HaveOccurred())
		Expect(c.Actor).To(Equal(actor))
		Expect(c.ExpiresAt.Sub(c.IssuedAt)).To(Equal(actortoken.DefaultTTL))
		Expect(c.ID).NotTo(BeEmpty())
	})

	It("refuses to mint a lifetime outside 300 to 3600 seconds", func() {
		for _, ttl := range []time.Duration{time.Second, 299 * time.Second, 3601 * time.Second, 24 * time.Hour, -time.Minute} {
			_, err := minter.Mint(actor, ttl)
			Expect(err).To(MatchError(actortoken.ErrTTL), ttl.String())
		}
		for _, ttl := range []time.Duration{actortoken.MinTTL, actortoken.MaxTTL} {
			_, err := minter.Mint(actor, ttl)
			Expect(err).NotTo(HaveOccurred(), ttl.String())
		}
	})

	It("refuses an actor with no org or agent", func() {
		_, err := minter.Mint(actortoken.Actor{Org: "acme"}, 0)
		Expect(err).To(HaveOccurred())
	})

	It("expires the token, with a small leeway", func() {
		tok, err := minter.Mint(actor, 300*time.Second)
		Expect(err).NotTo(HaveOccurred())
		_, err = ver.WithClock(func() time.Time { return t0.Add(304 * time.Second) }).Verify(tok)
		Expect(err).NotTo(HaveOccurred())
		_, err = ver.WithClock(func() time.Time { return t0.Add(306 * time.Second) }).Verify(tok)
		Expect(err).To(MatchError(actortoken.ErrInvalid))
	})

	It("refuses a token from the future", func() {
		tok, err := minter.WithClock(func() time.Time { return t0.Add(time.Hour) }).Mint(actor, 0)
		Expect(err).NotTo(HaveOccurred())
		_, err = ver.Verify(tok)
		Expect(err).To(MatchError(actortoken.ErrInvalid))
	})

	It("refuses a token signed by another key, or with an unknown key id", func() {
		_, other, err := ed25519.GenerateKey(rand.Reader)
		Expect(err).NotTo(HaveOccurred())
		m, err := actortoken.NewMinter("koinon-test", "k1", other)
		Expect(err).NotTo(HaveOccurred())
		tok, err := m.WithClock(func() time.Time { return t0 }).Mint(actor, 0)
		Expect(err).NotTo(HaveOccurred())
		_, err = ver.Verify(tok)
		Expect(err).To(MatchError(actortoken.ErrInvalid))

		m2, err := actortoken.NewMinter("koinon-test", "k2", priv)
		Expect(err).NotTo(HaveOccurred())
		tok, err = m2.WithClock(func() time.Time { return t0 }).Mint(actor, 0)
		Expect(err).NotTo(HaveOccurred())
		_, err = ver.Verify(tok)
		Expect(err).To(MatchError(actortoken.ErrInvalid))
	})

	It("refuses another issuer", func() {
		m, err := actortoken.NewMinter("someone-else", "k1", priv)
		Expect(err).NotTo(HaveOccurred())
		tok, err := m.WithClock(func() time.Time { return t0 }).Mint(actor, 0)
		Expect(err).NotTo(HaveOccurred())
		_, err = ver.Verify(tok)
		Expect(err).To(MatchError(actortoken.ErrInvalid))
	})

	It("refuses a tampered payload", func() {
		tok, err := minter.Mint(actor, 0)
		Expect(err).NotTo(HaveOccurred())
		parts := strings.Split(tok, ".")
		Expect(parts).To(HaveLen(3))
		parts[1] = parts[1][:len(parts[1])-2] + "AA"
		_, err = ver.Verify(strings.Join(parts, "."))
		Expect(err).To(MatchError(actortoken.ErrInvalid))
	})

	It("refuses garbage, an empty token and alg none", func() {
		for _, s := range []string{"", "x", "a.b.c", "eyJhbGciOiJub25lIn0.e30."} {
			_, err := ver.Verify(s)
			Expect(err).To(MatchError(actortoken.ErrInvalid), s)
		}
	})

	// signRaw signs arbitrary claims with the right key, to test the verifier's own checks.
	signRaw := func(claims map[string]any) string {
		s, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: priv},
			(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
		Expect(err).NotTo(HaveOccurred())
		tok, err := jwt.Signed(s).Claims(claims).Serialize()
		Expect(err).NotTo(HaveOccurred())
		return tok
	}
	valid := func() map[string]any {
		return map[string]any{
			"iss": "koinon-test", "sub": "org/acme/agent/builder", "aud": []string{actortoken.Audience},
			"iat": t0.Unix(), "nbf": t0.Unix(), "exp": t0.Add(10 * time.Minute).Unix(), "jti": "j1",
			"org": "acme", "agent": "builder",
		}
	}

	It("accepts the hand-built baseline claims", func() {
		_, err := ver.Verify(signRaw(valid()))
		Expect(err).NotTo(HaveOccurred())
	})

	It("refuses another audience, a second audience and no audience", func() {
		for _, aud := range []any{[]string{"someone-else"}, []string{actortoken.Audience, "other"}, nil} {
			c := valid()
			c["aud"] = aud
			_, err := ver.Verify(signRaw(c))
			Expect(err).To(MatchError(actortoken.ErrInvalid), "%v", aud)
		}
	})

	It("refuses a lifetime outside the bounds even when signed", func() {
		for _, life := range []time.Duration{time.Minute, 2 * time.Hour, 365 * 24 * time.Hour} {
			c := valid()
			c["exp"] = t0.Add(life).Unix()
			_, err := ver.Verify(signRaw(c))
			Expect(err).To(MatchError(actortoken.ErrInvalid), life.String())
		}
	})

	It("refuses a missing jti, expiry or actor, and a subject that does not match", func() {
		for _, drop := range []string{"jti", "exp", "iat", "org", "agent"} {
			c := valid()
			delete(c, drop)
			_, err := ver.Verify(signRaw(c))
			Expect(err).To(MatchError(actortoken.ErrInvalid), drop)
		}
		c := valid()
		c["sub"] = "org/other/agent/builder"
		_, err := ver.Verify(signRaw(c))
		Expect(err).To(MatchError(actortoken.ErrInvalid))
	})

	It("needs an issuer and a key to build", func() {
		_, err := actortoken.NewVerifier("", map[string]ed25519.PublicKey{"k1": pub})
		Expect(err).To(HaveOccurred())
		_, err = actortoken.NewVerifier("koinon-test", nil)
		Expect(err).To(HaveOccurred())
		_, err = actortoken.NewMinter("koinon-test", "", priv)
		Expect(err).To(HaveOccurred())
		_, err = actortoken.NewMinter("koinon-test", "k1", ed25519.PrivateKey("short"))
		Expect(err).To(HaveOccurred())
	})
})
