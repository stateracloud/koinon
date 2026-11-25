// SPDX-License-Identifier: Apache-2.0

package guestauth_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/guestauth"
)

// seed is an obviously fake signing key: 32 bytes of 0x11.
const seed = "1111111111111111111111111111111111111111111111111111111111111111"

func newKey() ed25519.PrivateKey {
	_, k, err := ed25519.GenerateKey(rand.Reader)
	Expect(err).NotTo(HaveOccurred())
	return k
}

var _ = Describe("keys", func() {
	It("parses a hex seed and rejects anything else", func() {
		k, err := guestauth.ParseSeed(seed + "\n")
		Expect(err).NotTo(HaveOccurred())
		Expect(k).To(HaveLen(ed25519.PrivateKeySize))
		for _, bad := range []string{"", "11", seed + "11", strings.Repeat("zz", 32)} {
			_, err := guestauth.ParseSeed(bad)
			Expect(err).To(HaveOccurred(), bad)
		}
	})

	It("round-trips a public key set and rejects an empty or bad one", func() {
		a, b := newKey(), newKey()
		pa, pb := a.Public().(ed25519.PublicKey), b.Public().(ed25519.PublicKey)
		keys, err := guestauth.DecodeKeys(guestauth.EncodeKeys(pa, pb))
		Expect(err).NotTo(HaveOccurred())
		Expect(keys).To(Equal([]ed25519.PublicKey{pa, pb}))
		for _, bad := range []string{"", " ", "abc", guestauth.EncodeKeys(pa) + ",", "!!"} {
			_, err := guestauth.DecodeKeys(bad)
			Expect(err).To(HaveOccurred(), bad)
		}
		_, err = guestauth.NewVerifier(nil)
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("Verifier", func() {
	const a, b, c = "org-1/sbx-a", "org-1/sbx-b", "org-2/sbx-c"
	var (
		key    ed25519.PrivateKey
		signer *guestauth.Signer
		v      *guestauth.Verifier
		now    time.Time
	)

	sign := func(actor, prev string) string {
		tok, err := signer.Sign(actor, prev)
		Expect(err).NotTo(HaveOccurred())
		return tok
	}
	// raw signs arbitrary claims with key, to build tokens the Signer never makes.
	raw := func(k ed25519.PrivateKey, alg jose.SignatureAlgorithm, kid string, cl any) string {
		s, err := jose.NewSigner(jose.SigningKey{Algorithm: alg, Key: k}, (&jose.SignerOptions{}).WithHeader("kid", kid))
		Expect(err).NotTo(HaveOccurred())
		tok, err := jwt.Signed(s).Claims(cl).Serialize()
		Expect(err).NotTo(HaveOccurred())
		return tok
	}

	BeforeEach(func() {
		var err error
		key = newKey()
		signer, err = guestauth.NewSigner(key)
		Expect(err).NotTo(HaveOccurred())
		v, err = guestauth.NewVerifier([]ed25519.PublicKey{signer.PublicKey()})
		Expect(err).NotTo(HaveOccurred())
		now = time.Unix(1_800_000_000, 0)
		guestauth.SetSignerClock(signer, func() time.Time { return now })
		guestauth.SetVerifierClock(v, func() time.Time { return now })
	})

	It("pins the first verified aud and then accepts only it", func() {
		Expect(v.Pin()).To(BeEmpty())
		Expect(v.Verify(sign(a, ""))).To(Succeed())
		Expect(v.Pin()).To(Equal(guestauth.Audience(a)))
		Expect(v.Verify(sign(a, ""))).To(Succeed())
		Expect(v.Verify(sign(b, ""))).To(MatchError(guestauth.ErrDenied))
		Expect(v.Pin()).To(Equal(guestauth.Audience(a)))
	})

	It("moves the pin only when koinon_prev names the pin it has", func() {
		Expect(v.Verify(sign(a, ""))).To(Succeed())
		Expect(v.Verify(sign(b, c))).To(MatchError(guestauth.ErrDenied))
		Expect(v.Verify(sign(b, a))).To(Succeed())
		Expect(v.Pin()).To(Equal(guestauth.Audience(b)))
		By("the old actor's tokens no longer pass")
		Expect(v.Verify(sign(a, ""))).To(MatchError(guestauth.ErrDenied))
		By("a later call that still carries prev passes")
		Expect(v.Verify(sign(b, a))).To(Succeed())
	})

	It("accepts each token once", func() {
		tok := sign(a, "")
		Expect(v.Verify(tok)).To(Succeed())
		Expect(v.Verify(tok)).To(MatchError(ContainSubstring("replayed")))
	})

	It("forgets a jti once its token has expired", func() {
		Expect(v.Verify(sign(a, ""))).To(Succeed())
		Expect(guestauth.ReplayEntries(v)).To(Equal(1))
		now = now.Add(3 * time.Minute)
		Expect(v.Verify(sign(a, ""))).To(Succeed())
		Expect(guestauth.ReplayEntries(v)).To(Equal(1))
	})

	It("refuses expired and future tokens", func() {
		tok := sign(a, "")
		now = now.Add(guestauth.TokenTTL + time.Minute)
		Expect(v.Verify(tok)).To(MatchError(ContainSubstring("expired")))
		now = now.Add(-10 * time.Minute)
		Expect(v.Verify(sign(a, ""))).To(Succeed()) // the signer's clock moved too
		guestauth.SetSignerClock(signer, func() time.Time { return now.Add(5 * time.Minute) })
		Expect(v.Verify(sign(a, ""))).To(MatchError(ContainSubstring("future")))
	})

	It("refuses tokens the driver did not sign", func() {
		kid := guestauth.KeyID(signer.PublicKey())
		good := jwt.Claims{
			Audience: jwt.Audience{guestauth.Audience(a)}, ID: "x",
			IssuedAt: jwt.NewNumericDate(now), Expiry: jwt.NewNumericDate(now.Add(time.Minute)),
		}
		other := newKey()
		cases := map[string]string{
			"empty":         "",
			"garbage":       "not.a.token",
			"other key":     raw(other, jose.EdDSA, kid, good),
			"unknown kid":   raw(other, jose.EdDSA, guestauth.KeyID(other.Public().(ed25519.PublicKey)), good),
			"too long":      strings.Repeat("a", 5000),
			"no jti":        raw(key, jose.EdDSA, kid, jwt.Claims{Audience: good.Audience, IssuedAt: good.IssuedAt, Expiry: good.Expiry}),
			"long lifetime": raw(key, jose.EdDSA, kid, jwt.Claims{Audience: good.Audience, ID: "y", IssuedAt: good.IssuedAt, Expiry: jwt.NewNumericDate(now.Add(time.Hour))}),
			"wrong prefix":  raw(key, jose.EdDSA, kid, jwt.Claims{Audience: jwt.Audience{a}, ID: "z", IssuedAt: good.IssuedAt, Expiry: good.Expiry}),
			"two auds":      raw(key, jose.EdDSA, kid, jwt.Claims{Audience: jwt.Audience{guestauth.Audience(a), guestauth.Audience(b)}, ID: "w", IssuedAt: good.IssuedAt, Expiry: good.Expiry}),
			"bare prefix":   raw(key, jose.EdDSA, kid, jwt.Claims{Audience: jwt.Audience{guestauth.AudiencePrefix}, ID: "v", IssuedAt: good.IssuedAt, Expiry: good.Expiry}),
		}
		for name, tok := range cases {
			Expect(v.Verify(tok)).To(MatchError(guestauth.ErrDenied), name)
		}
		Expect(v.Pin()).To(BeEmpty(), "no refused token sets the pin")
		Expect(v.Verify(raw(key, jose.EdDSA, kid, good))).To(Succeed())
	})

	It("refuses an alg other than EdDSA", func() {
		hs, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.HS256, Key: []byte(strings.Repeat("k", 32))}, nil)
		Expect(err).NotTo(HaveOccurred())
		tok, err := jwt.Signed(hs).Claims(jwt.Claims{Audience: jwt.Audience{guestauth.Audience(a)}}).Serialize()
		Expect(err).NotTo(HaveOccurred())
		Expect(v.Verify(tok)).To(MatchError(guestauth.ErrDenied))
	})

	It("accepts any key of the set (rotation)", func() {
		next, err := guestauth.NewSigner(newKey())
		Expect(err).NotTo(HaveOccurred())
		guestauth.SetSignerClock(next, func() time.Time { return now })
		v2, err := guestauth.NewVerifier([]ed25519.PublicKey{signer.PublicKey(), next.PublicKey()})
		Expect(err).NotTo(HaveOccurred())
		guestauth.SetVerifierClock(v2, func() time.Time { return now })
		Expect(v2.Verify(sign(a, ""))).To(Succeed())
		tok, err := next.Sign(a, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(v2.Verify(tok)).To(Succeed())
	})

	It("refuses a bad actor reference at signing", func() {
		_, err := signer.Sign("", "")
		Expect(err).To(HaveOccurred())
		_, err = signer.Sign(strings.Repeat("a", 600), "")
		Expect(err).To(HaveOccurred())
	})
})
