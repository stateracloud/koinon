// SPDX-License-Identifier: Apache-2.0

package secretstore_test

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/secretstore"
)

func key() []byte {
	k := make([]byte, secretstore.KeySize)
	_, _ = rand.Read(k)
	return k
}

var bind = secretstore.Binding{OrgID: "org-1", Table: "providers", RowID: "row-1", Column: "api_key"}

var _ = Describe("Keyring", func() {
	var ring *secretstore.Keyring
	BeforeEach(func() {
		var err error
		ring, err = secretstore.NewKeyring(map[string][]byte{"k1": key(), "k2": key()}, "k1")
		Expect(err).NotTo(HaveOccurred())
	})

	It("round-trips and stores no plaintext", func() {
		env, err := ring.Seal(bind, []byte("sk-secret"))
		Expect(err).NotTo(HaveOccurred())
		Expect(env.KEKID).To(Equal("k1"))
		Expect(env.Format).To(Equal(1))
		Expect(env.Alg).To(Equal("AES-256-GCM"))
		raw, _ := json.Marshal(env)
		Expect(string(raw)).NotTo(ContainSubstring("sk-secret"))
		Expect(raw).To(ContainSubstring(`"kek_id":"k1"`))
		got, err := ring.Open(bind, env)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(got)).To(Equal("sk-secret"))
	})

	It("uses a fresh DEK and nonce for every value", func() {
		a, _ := ring.Seal(bind, []byte("same"))
		b, _ := ring.Seal(bind, []byte("same"))
		Expect(a.WrappedDEK).NotTo(Equal(b.WrappedDEK))
		Expect(a.Nonce).NotTo(Equal(b.Nonce))
		Expect(a.Ciphertext).NotTo(Equal(b.Ciphertext))
	})

	DescribeTable("rejects a changed binding",
		func(mutate func(*secretstore.Binding)) {
			env, _ := ring.Seal(bind, []byte("x"))
			other := bind
			mutate(&other)
			_, err := ring.Open(other, env)
			Expect(err).To(MatchError(secretstore.ErrOpen))
			// The wrapped DEK is bound too: a rewrap under the wrong binding fails.
			_, err = ring.Rewrap(other, env)
			Expect(err).To(MatchError(secretstore.ErrOpen))
		},
		Entry("org", func(b *secretstore.Binding) { b.OrgID = "org-2" }),
		Entry("table", func(b *secretstore.Binding) { b.Table = "connections" }),
		Entry("row", func(b *secretstore.Binding) { b.RowID = "row-2" }),
		Entry("column", func(b *secretstore.Binding) { b.Column = "other" }),
	)

	It("rejects modified data", func() {
		env, _ := ring.Seal(bind, []byte("x"))
		bad := env
		bad.Ciphertext = bytes.Clone(env.Ciphertext)
		bad.Ciphertext[0] ^= 1
		_, err := ring.Open(bind, bad)
		Expect(err).To(MatchError(secretstore.ErrOpen))
		bad = env
		bad.WrappedDEK = bytes.Clone(env.WrappedDEK)
		bad.WrappedDEK[0] ^= 1
		_, err = ring.Open(bind, bad)
		Expect(err).To(MatchError(secretstore.ErrOpen))
	})

	It("rejects parts swapped between two envelopes sealed under the same binding", func() {
		a, _ := ring.Seal(bind, []byte("aaaa"))
		b, _ := ring.Seal(bind, []byte("bbbb"))
		for name, swap := range map[string]func(x, y secretstore.Envelope) secretstore.Envelope{
			"wrapped DEK and its nonce": func(x, y secretstore.Envelope) secretstore.Envelope {
				x.WrappedDEK, x.DEKNonce = y.WrappedDEK, y.DEKNonce
				return x
			},
			"payload nonce": func(x, y secretstore.Envelope) secretstore.Envelope {
				x.Nonce = y.Nonce
				return x
			},
			"ciphertext": func(x, y secretstore.Envelope) secretstore.Envelope {
				x.Ciphertext = y.Ciphertext
				return x
			},
			"wrapped DEK and payload nonce": func(x, y secretstore.Envelope) secretstore.Envelope {
				x.WrappedDEK, x.DEKNonce, x.Nonce = y.WrappedDEK, y.DEKNonce, y.Nonce
				return x
			},
		} {
			_, err := ring.Open(bind, swap(a, b))
			Expect(err).To(MatchError(secretstore.ErrOpen), name)
			_, err = ring.Open(bind, swap(b, a))
			Expect(err).To(MatchError(secretstore.ErrOpen), name)
		}
	})

	It("rejects an envelope relabelled with another KEK of the keyring", func() {
		env, _ := ring.Seal(bind, []byte("x"))
		Expect(env.KEKID).To(Equal("k1"))
		env.KEKID = "k2"
		_, err := ring.Open(bind, env)
		Expect(err).To(MatchError(secretstore.ErrOpen))
		_, err = ring.Rewrap(bind, env)
		Expect(err).To(MatchError(secretstore.ErrOpen))
	})

	It("rewraps under the active KEK without touching the payload", func() {
		k1, k2 := key(), key()
		both, _ := secretstore.NewKeyring(map[string][]byte{"k1": k1, "k2": k2}, "k1")
		env, _ := both.Seal(bind, []byte("keep"))
		Expect(both.Replace(map[string][]byte{"k1": k1, "k2": k2}, "k2")).To(Succeed())
		re, err := both.Rewrap(bind, env)
		Expect(err).NotTo(HaveOccurred())
		Expect(re.KEKID).To(Equal("k2"))
		Expect(re.Ciphertext).To(Equal(env.Ciphertext))
		Expect(re.Nonce).To(Equal(env.Nonce))
		Expect(re.WrappedDEK).NotTo(Equal(env.WrappedDEK))
		// Only k2 is needed now.
		only2, _ := secretstore.NewKeyring(map[string][]byte{"k2": k2}, "k2")
		got, err := only2.Open(bind, re)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(got)).To(Equal("keep"))
		_, err = only2.Open(bind, env)
		Expect(err).To(MatchError(secretstore.ErrUnknownKEK))
	})

	It("re-encryption makes a new DEK", func() {
		env, _ := ring.Seal(bind, []byte("v"))
		plain, _ := ring.Open(bind, env)
		fresh, _ := ring.Seal(bind, plain)
		Expect(fresh.Ciphertext).NotTo(Equal(env.Ciphertext))
		Expect(fresh.WrappedDEK).NotTo(Equal(env.WrappedDEK))
	})

	It("leaves the keyring alone when a replacement is invalid", func() {
		Expect(ring.Replace(map[string][]byte{"k1": nil}, "")).NotTo(Succeed())
		Expect(ring.Active()).To(Equal("k1"))
		Expect(ring.Has("k2")).To(BeTrue())
	})

	It("refuses to seal without an active KEK", func() {
		empty, err := secretstore.NewKeyring(map[string][]byte{"k1": key()}, "")
		Expect(err).NotTo(HaveOccurred())
		_, err = empty.Seal(bind, []byte("x"))
		Expect(err).To(MatchError(secretstore.ErrNoActiveKEK))
	})
})

var _ = Describe("KEK directory", func() {
	It("reads a mounted Secret and reloads it", func(ctx SpecContext) {
		dir := GinkgoT().TempDir()
		write := func(name string, v []byte) {
			Expect(os.WriteFile(filepath.Join(dir, name), v, 0o600)).To(Succeed())
		}
		var local secretstore.Wrappers
		write("k1", secretstore.EncodeKey(key()))
		write("active", []byte("k1\n"))
		ring, err := secretstore.LoadDir(ctx, dir, local)
		Expect(err).NotTo(HaveOccurred())
		Expect(ring.Active()).To(Equal("k1"))

		write("k2", secretstore.EncodeKey(key()))
		write("active", []byte("k2"))
		Expect(ring.Reload(ctx, dir, local)).To(Succeed())
		Expect(ring.Active()).To(Equal("k2"))
		Expect(ring.IDs()).To(ConsistOf("k1", "k2"))

		write("active", []byte("missing"))
		Expect(ring.Reload(ctx, dir, local)).NotTo(Succeed())
		Expect(ring.Active()).To(Equal("k2")) // a failed reload keeps the old keys
	})

	It("never quotes a bad key", func() {
		_, _, err := secretstore.ParseKeySet(map[string][]byte{"k1": []byte("hunter2hunter2"), "active": []byte("k1")})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).NotTo(ContainSubstring("hunter2"))
	})

	It("reserves the name active", func() {
		Expect(secretstore.ValidKEKID("active")).To(BeFalse())
		Expect(secretstore.ValidKEKID("2026-10")).To(BeTrue())
	})
})

var _ = Describe("UnsupportedFormat", func() {
	It("accepts the one known format and algorithm and refuses any other", func() {
		ok := secretstore.Envelope{Format: secretstore.FormatV1, Alg: secretstore.AlgAES256GCM}
		Expect(secretstore.UnsupportedFormat(ok)).To(BeFalse())
		Expect(secretstore.UnsupportedFormat(secretstore.Envelope{Format: 2, Alg: secretstore.AlgAES256GCM})).To(BeTrue())
		Expect(secretstore.UnsupportedFormat(secretstore.Envelope{Format: secretstore.FormatV1, Alg: "ROT13"})).To(BeTrue())
		Expect(secretstore.UnsupportedFormat(secretstore.Envelope{})).To(BeTrue())
	})
})
