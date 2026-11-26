// SPDX-License-Identifier: Apache-2.0

package actortoken_test

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/gateway/actortoken"
)

var _ = Describe("verifier key files", func() {
	var dir string

	write := func(name string, b []byte) {
		Expect(os.WriteFile(filepath.Join(dir, name), b, 0o600)).To(Succeed())
	}
	newKey := func() (ed25519.PublicKey, []byte) {
		pub, _, err := ed25519.GenerateKey(rand.Reader)
		Expect(err).NotTo(HaveOccurred())
		b, err := actortoken.MarshalPublicKey(pub)
		Expect(err).NotTo(HaveOccurred())
		return pub, b
	}

	BeforeEach(func() { dir = GinkgoT().TempDir() })

	It("reads one key per <kid>.pub file and skips dot names", func() {
		k1, b1 := newKey()
		k2, b2 := newKey()
		write("2026-11.pub", b1)
		write("2026-12.pub", b2)
		Expect(os.Mkdir(filepath.Join(dir, "..data"), 0o700)).To(Succeed())
		write(".hidden", []byte("ignored"))
		keys, err := actortoken.LoadPublicKeys(dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(keys).To(HaveLen(2))
		Expect(keys["2026-11"]).To(Equal(k1))
		Expect(keys["2026-12"]).To(Equal(k2))
	})

	It("refuses an empty directory", func() {
		_, err := actortoken.LoadPublicKeys(dir)
		Expect(err).To(MatchError(ContainSubstring("no key")))
	})

	It("refuses a file with another name", func() {
		_, b := newKey()
		write("key.pem", b)
		_, err := actortoken.LoadPublicKeys(dir)
		Expect(err).To(MatchError(ContainSubstring("<key id>.pub")))
	})

	It("refuses a private key, so a minting key cannot enter the gateway", func() {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		Expect(err).NotTo(HaveOccurred())
		der, err := x509.MarshalPKCS8PrivateKey(priv)
		Expect(err).NotTo(HaveOccurred())
		write("k.pub", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
		_, err = actortoken.LoadPublicKeys(dir)
		Expect(err).To(MatchError(ContainSubstring("private key is never accepted")))
	})

	It("refuses a key of another type", func() {
		ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		Expect(err).NotTo(HaveOccurred())
		der, err := x509.MarshalPKIXPublicKey(&ec.PublicKey)
		Expect(err).NotTo(HaveOccurred())
		write("k.pub", pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
		_, err = actortoken.LoadPublicKeys(dir)
		Expect(err).To(MatchError(ContainSubstring("not an Ed25519 key")))
	})

	It("refuses text after the PEM block", func() {
		_, b := newKey()
		write("k.pub", append(b, []byte("trailing")...))
		_, err := actortoken.LoadPublicKeys(dir)
		Expect(err).To(MatchError(ContainSubstring("text after")))
	})

	It("gives a verifier that accepts a token of the matching minter", func() {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		Expect(err).NotTo(HaveOccurred())
		b, err := actortoken.MarshalPublicKey(pub)
		Expect(err).NotTo(HaveOccurred())
		write("k1.pub", b)
		keys, err := actortoken.LoadPublicKeys(dir)
		Expect(err).NotTo(HaveOccurred())
		v, err := actortoken.NewVerifier("koinon-test", keys)
		Expect(err).NotTo(HaveOccurred())
		m, err := actortoken.NewMinter("koinon-test", "k1", priv)
		Expect(err).NotTo(HaveOccurred())
		tok, err := m.Mint(actortoken.Actor{Org: "acme", Agent: "builder"}, 0)
		Expect(err).NotTo(HaveOccurred())
		c, err := v.Verify(tok)
		Expect(err).NotTo(HaveOccurred())
		Expect(c.Actor.Org).To(Equal("acme"))
	})
})

var _ = Describe("minting key files", func() {
	// A fixed, obviously fake seed: 32 bytes of 0x01.
	const seedHex = "0101010101010101010101010101010101010101010101010101010101010101"

	It("reads a hex seed and mints a token that the public key checks", func() {
		file := filepath.Join(GinkgoT().TempDir(), "seed")
		Expect(os.WriteFile(file, []byte(seedHex+"\n"), 0o600)).To(Succeed())
		priv, err := actortoken.LoadSeedKey(file)
		Expect(err).NotTo(HaveOccurred())
		m, err := actortoken.NewMinter("koinon-agentd", "k1", priv)
		Expect(err).NotTo(HaveOccurred())
		tok, err := m.Mint(actortoken.Actor{Org: "acme", Agent: "a1"}, 0)
		Expect(err).NotTo(HaveOccurred())
		pub, ok := priv.Public().(ed25519.PublicKey)
		Expect(ok).To(BeTrue())
		v, err := actortoken.NewVerifier("koinon-agentd", map[string]ed25519.PublicKey{"k1": pub})
		Expect(err).NotTo(HaveOccurred())
		_, err = v.Verify(tok)
		Expect(err).NotTo(HaveOccurred())
	})

	DescribeTable("refuses a seed that is not 64 hex characters, without its text in the error",
		func(text string) {
			_, err := actortoken.ParseSeedKey([]byte(text))
			Expect(err).To(HaveOccurred())
			if text != "" {
				Expect(err.Error()).NotTo(ContainSubstring(text))
			}
		},
		Entry("empty", ""),
		Entry("short", seedHex[:62]),
		Entry("long", seedHex+"01"),
		Entry("not hex", "zz"+seedHex[2:]),
	)

	It("refuses a missing file and a directory", func() {
		_, err := actortoken.LoadSeedKey(filepath.Join(GinkgoT().TempDir(), "none"))
		Expect(err).To(HaveOccurred())
		_, err = actortoken.LoadSeedKey(GinkgoT().TempDir())
		Expect(err).To(HaveOccurred())
	})
})
