// SPDX-License-Identifier: Apache-2.0

package secretstore_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/secretstore/secretstoretest"
)

var recipientBinding = secretstore.Binding{OrgID: "org-1", Table: "mcp_servers", RowID: "row-1", Column: "credential"}

const recipientInstall = "00000000-0000-4000-8000-000000000685"

var _ = Describe("envelope v2 (sealed to a recipient key)", func() {
	var key *secretstore.RecipientKey
	plain := []byte("koinon_test_not-a-real-token")

	BeforeEach(func() {
		var err error
		key, err = secretstore.GenerateRecipientKey(secretstore.SuiteX25519)
		Expect(err).NotTo(HaveOccurred())
	})

	DescribeTable("seals with the public key only and opens with the private key",
		func(suite string) {
			k, err := secretstore.GenerateRecipientKey(suite)
			Expect(err).NotTo(HaveOccurred())
			pub, err := secretstore.NewRecipientPublicKey(suite, k.Public().Bytes())
			Expect(err).NotTo(HaveOccurred())
			Expect(pub.CheckID(k.ID())).To(Succeed())

			env, err := secretstore.SealV2(pub, recipientBinding, plain)
			Expect(err).NotTo(HaveOccurred())
			Expect(env.Format).To(Equal(secretstore.FormatV2))
			Expect(env.Recipient).To(Equal(suite))
			Expect(env.RecipientKeyID).To(Equal(k.ID()))
			Expect(secretstore.ValidRecipientKeyID(env.RecipientKeyID)).To(BeTrue())
			Expect(secretstore.ValidKEKID(env.RecipientKeyID)).To(BeTrue())
			raw, err := json.Marshal(env)
			Expect(err).NotTo(HaveOccurred())
			Expect(raw).NotTo(ContainSubstring(string(plain)))

			got, err := k.Open(recipientBinding, env)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(plain))
		},
		Entry("X25519", secretstore.SuiteX25519),
		Entry("P-256", secretstore.SuiteP256),
	)

	It("gives each value its own DEK and encapsulated key", func() {
		a, err := secretstore.SealV2(key.Public(), recipientBinding, plain)
		Expect(err).NotTo(HaveOccurred())
		b, err := secretstore.SealV2(key.Public(), recipientBinding, plain)
		Expect(err).NotTo(HaveOccurred())
		Expect(a.Enc).NotTo(Equal(b.Enc))
		Expect(a.SealedDEK).NotTo(Equal(b.SealedDEK))
		Expect(a.Ciphertext).NotTo(Equal(b.Ciphertext))
	})

	It("derives the key ID from the public key and refuses another ID", func() {
		Expect(key.ID()).To(Equal(secretstore.RecipientKeyID(key.Public().Bytes())))
		Expect(key.Public().CheckID("gk-0000000000000000")).To(MatchError(secretstore.ErrRecipientKeyID))
		Expect(secretstore.ValidRecipientKeyID("gk-XYZ")).To(BeFalse())
		Expect(secretstore.ValidRecipientKeyID("kek-1f3a9c2e5b7d4e60")).To(BeFalse())
	})

	Describe("fails closed", func() {
		var env secretstore.EnvelopeV2
		BeforeEach(func() {
			var err error
			env, err = secretstore.SealV2(key.Public(), recipientBinding, plain)
			Expect(err).NotTo(HaveOccurred())
		})
		flip := func(b []byte) []byte { c := bytes.Clone(b); c[0] ^= 1; return c }

		DescribeTable("on changed bytes",
			func(mutate func(*secretstore.EnvelopeV2)) {
				e := env
				mutate(&e)
				got, err := key.Open(recipientBinding, e)
				Expect(err).To(MatchError(secretstore.ErrOpen))
				Expect(got).To(BeNil())
			},
			Entry("enc", func(e *secretstore.EnvelopeV2) { e.Enc = flip(e.Enc) }),
			Entry("sealed_dek", func(e *secretstore.EnvelopeV2) { e.SealedDEK = flip(e.SealedDEK) }),
			Entry("nonce", func(e *secretstore.EnvelopeV2) { e.Nonce = flip(e.Nonce) }),
			Entry("short nonce", func(e *secretstore.EnvelopeV2) { e.Nonce = e.Nonce[:3] }),
			Entry("ciphertext", func(e *secretstore.EnvelopeV2) { e.Ciphertext = flip(e.Ciphertext) }),
			Entry("empty enc", func(e *secretstore.EnvelopeV2) { e.Enc = nil }),
		)

		DescribeTable("on another binding",
			func(mutate func(*secretstore.Binding)) {
				b := recipientBinding
				mutate(&b)
				_, err := key.Open(b, env)
				Expect(err).To(MatchError(secretstore.ErrOpen))
			},
			Entry("org", func(b *secretstore.Binding) { b.OrgID = "org-2" }),
			Entry("table", func(b *secretstore.Binding) { b.Table = "providers" }),
			Entry("row", func(b *secretstore.Binding) { b.RowID = "row-2" }),
			Entry("column", func(b *secretstore.Binding) { b.Column = "other" }),
		)

		It("refuses another key's pair, with the right key ID or a wrong one", func() {
			other, err := secretstore.GenerateRecipientKey(secretstore.SuiteX25519)
			Expect(err).NotTo(HaveOccurred())
			_, err = other.Open(recipientBinding, env)
			Expect(err).To(MatchError(secretstore.ErrUnknownRecipientKey))

			e := env
			e.RecipientKeyID = other.ID()
			_, err = key.Open(recipientBinding, e)
			Expect(err).To(MatchError(secretstore.ErrUnknownRecipientKey))
			_, err = other.Open(recipientBinding, e)
			Expect(err).To(MatchError(secretstore.ErrOpen), "the key ID is bound into the data: a relabelled envelope does not open")
		})

		It("refuses an unknown format, algorithm or suite", func() {
			for _, mutate := range []func(*secretstore.EnvelopeV2){
				func(e *secretstore.EnvelopeV2) { e.Format = 1 },
				func(e *secretstore.EnvelopeV2) { e.Format = 3 },
				func(e *secretstore.EnvelopeV2) { e.Alg = "AES-128-GCM" },
				func(e *secretstore.EnvelopeV2) { e.Recipient = "hpke-647a-0001-0002" },
				func(e *secretstore.EnvelopeV2) { e.Recipient = "" },
			} {
				e := env
				mutate(&e)
				_, err := key.Open(recipientBinding, e)
				Expect(err).To(MatchError(secretstore.ErrUnsupportedFormat))
			}
			_, err := secretstore.GenerateRecipientKey("hpke-647a-0001-0002")
			Expect(err).To(MatchError(secretstore.ErrUnsupportedFormat))
			_, err = secretstore.NewRecipientPublicKey("nope", key.Public().Bytes())
			Expect(err).To(MatchError(secretstore.ErrUnsupportedFormat))
		})

		It("is refused by the v1 reader, which never mis-reads it", func() {
			raw, err := json.Marshal(env)
			Expect(err).NotTo(HaveOccurred())
			var v1 secretstore.Envelope
			Expect(json.Unmarshal(raw, &v1)).To(Succeed())
			Expect(secretstore.UnsupportedFormat(v1)).To(BeTrue())
			ring, err := secretstore.NewKeyring(map[string][]byte{"k1": bytes.Repeat([]byte("k"), secretstore.KeySize)}, "k1")
			Expect(err).NotTo(HaveOccurred())
			_, err = ring.Open(recipientBinding, v1)
			Expect(err).To(MatchError(secretstore.ErrUnsupportedFormat))
			Expect(secretstore.EnvelopeFormat(raw)).To(Equal(secretstore.FormatV2))
		})

		It("refuses a v1 envelope where v2 is required", func() {
			ring, err := secretstore.NewKeyring(map[string][]byte{"k1": bytes.Repeat([]byte("k"), secretstore.KeySize)}, "k1")
			Expect(err).NotTo(HaveOccurred())
			v1, err := ring.Seal(recipientBinding, plain)
			Expect(err).NotTo(HaveOccurred())
			raw, err := json.Marshal(v1)
			Expect(err).NotTo(HaveOccurred())
			Expect(secretstore.EnvelopeFormat(raw)).To(Equal(secretstore.FormatV1))
			var as2 secretstore.EnvelopeV2
			Expect(json.Unmarshal(raw, &as2)).To(Succeed())
			_, err = key.Open(recipientBinding, as2)
			Expect(err).To(MatchError(secretstore.ErrUnsupportedFormat))
		})
	})

	Describe("the keyring", func() {
		It("picks the key by ID, never tries another, and holds old and new keys together", func() {
			oldKey, err := secretstore.GenerateRecipientKey(secretstore.SuiteX25519)
			Expect(err).NotTo(HaveOccurred())
			ring := secretstore.NewRecipientKeyring(oldKey, key)
			for _, k := range []*secretstore.RecipientKey{oldKey, key} {
				env, err := secretstore.SealV2(k.Public(), recipientBinding, plain)
				Expect(err).NotTo(HaveOccurred())
				got, err := ring.Open(recipientBinding, env)
				Expect(err).NotTo(HaveOccurred())
				Expect(got).To(Equal(plain))
			}
			ring.Replace(key)
			Expect(ring.Has(oldKey.ID())).To(BeFalse())
			env, err := secretstore.SealV2(oldKey.Public(), recipientBinding, plain)
			Expect(err).NotTo(HaveOccurred())
			_, err = ring.Open(recipientBinding, env)
			Expect(err).To(MatchError(secretstore.ErrUnknownRecipientKey))
			env.Format = 1
			_, err = ring.Open(recipientBinding, env)
			Expect(err).To(MatchError(secretstore.ErrUnsupportedFormat))
		})
	})

	Describe("wrapping the private key", func() {
		var ctx context.Context
		BeforeEach(func() { ctx = context.Background() })
		wrappers := func(admin bool, kms *secretstoretest.FakeKMS) secretstore.Wrappers {
			GinkgoHelper()
			src := secretstore.StaticBoundOnly(secretstore.BoundOnly{})
			mk := secretstore.NewWrappers
			if admin {
				mk = secretstore.NewKeyAdminWrappers
			}
			ws, err := mk(src, kms)
			Expect(err).NotTo(HaveOccurred())
			return ws.WithInstallID(secretstore.StaticInstallID(recipientInstall))
		}

		It("wraps with the key-admin identity and unwraps with the service identity (KMS)", func() {
			kms := &secretstoretest.FakeKMS{Name: "koinon-gateway"}
			s, err := wrappers(true, kms).WrapRecipientKey(ctx, key)
			Expect(err).NotTo(HaveOccurred())
			Expect(s.Bound()).To(BeTrue())
			Expect(s.KeyID).To(Equal("koinon-gateway"))

			got, err := wrappers(false, kms).UnwrapRecipientKey(ctx, secretstore.SuiteX25519, key.ID(), s)
			Expect(err).NotTo(HaveOccurred())
			env, err := secretstore.SealV2(key.Public(), recipientBinding, plain)
			Expect(err).NotTo(HaveOccurred())
			out, err := got.Open(recipientBinding, env)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(Equal(plain))
		})

		It("refuses a KMS wrap by a service identity", func() {
			_, err := wrappers(false, &secretstoretest.FakeKMS{}).WrapRecipientKey(ctx, key)
			Expect(err).To(MatchError(secretstore.ErrKeyAdminWraps))
		})

		It("never reads a wrapped recipient key as a KEK, or a KEK as a recipient key", func() {
			kms := &secretstoretest.FakeKMS{}
			admin := wrappers(true, kms)
			s, err := admin.WrapRecipientKey(ctx, key)
			Expect(err).NotTo(HaveOccurred())
			_, err = admin.Unwrap(ctx, key.ID(), s)
			Expect(err).To(MatchError(secretstore.ErrOpen))

			kek, err := admin.Wrap(ctx, key.ID(), bytes.Repeat([]byte("k"), secretstore.KeySize))
			Expect(err).NotTo(HaveOccurred())
			_, err = admin.UnwrapRecipientKey(ctx, secretstore.SuiteX25519, key.ID(), kek)
			Expect(err).To(MatchError(secretstore.ErrOpen))
		})

		It("refuses another install, another key ID and another key's ID", func() {
			kms := &secretstoretest.FakeKMS{}
			s, err := wrappers(true, kms).WrapRecipientKey(ctx, key)
			Expect(err).NotTo(HaveOccurred())

			ws := wrappers(false, kms).WithInstallID(secretstore.StaticInstallID("00000000-0000-4000-8000-000000000001"))
			_, err = ws.UnwrapRecipientKey(ctx, secretstore.SuiteX25519, key.ID(), s)
			Expect(err).To(MatchError(secretstore.ErrOpen))
			_, err = wrappers(false, kms).UnwrapRecipientKey(ctx, secretstore.SuiteX25519, "gk-0000000000000000", s)
			Expect(err).To(MatchError(secretstore.ErrOpen))

			By("a wrap of another key under this key's ID")
			other, err := secretstore.GenerateRecipientKey(secretstore.SuiteX25519)
			Expect(err).NotTo(HaveOccurred())
			otherWrapped, err := wrappers(true, kms).WrapRecipientKey(ctx, other)
			Expect(err).NotTo(HaveOccurred())
			_, err = wrappers(false, kms).UnwrapRecipientKey(ctx, secretstore.SuiteX25519, key.ID(), otherWrapped)
			Expect(err).To(MatchError(secretstore.ErrOpen))
		})

		It("refuses a KMS key that is not bound to an install", func() {
			s := secretstore.StoredKEK{Backend: secretstoretest.FakeKMSBackend, KeyID: "koinon-test", Wrapped: []byte("x")}
			_, err := wrappers(false, &secretstoretest.FakeKMS{}).UnwrapRecipientKey(ctx, secretstore.SuiteX25519, key.ID(), s)
			Expect(err).To(MatchError(secretstore.ErrOpen))
		})

		It("reports an outage as unreachable, not as a wrong key", func() {
			kms := &secretstoretest.FakeKMS{}
			s, err := wrappers(true, kms).WrapRecipientKey(ctx, key)
			Expect(err).NotTo(HaveOccurred())
			kms.Down.Store(true)
			_, err = wrappers(false, kms).UnwrapRecipientKey(ctx, secretstore.SuiteX25519, key.ID(), s)
			Expect(errors.Is(err, secretstore.ErrUnreachable)).To(BeTrue())
			Expect(errors.Is(err, secretstore.ErrOpen)).To(BeFalse())
		})

		It("round-trips on the local backend, where the wrap is the identity", func() {
			ws, err := secretstore.NewWrappers(secretstore.StaticBoundOnly(secretstore.BoundOnly{}), secretstore.Local{})
			Expect(err).NotTo(HaveOccurred())
			s, err := ws.WrapRecipientKey(ctx, key)
			Expect(err).NotTo(HaveOccurred())
			Expect(s.Backend).To(Equal(secretstore.BackendLocal))
			got, err := ws.UnwrapRecipientKey(ctx, secretstore.SuiteX25519, key.ID(), s)
			Expect(err).NotTo(HaveOccurred())
			Expect(got.ID()).To(Equal(key.ID()))
		})
	})
})

func FuzzOpenV2(f *testing.F) {
	// The seed is the golden file, not a fresh seal: sealing is random, and the fuzz engine runs this
	// setup again in each worker.
	key, err := secretstore.NewRecipientKey(secretstore.SuiteX25519, goldenRecipientSeed)
	if err != nil {
		f.Fatal(err)
	}
	seed, err := os.ReadFile(goldenPath("envelope_v2.json"))
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Add([]byte(`{"fmt":2}`))
	f.Add([]byte(`{`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		var e secretstore.EnvelopeV2
		if json.Unmarshal(raw, &e) != nil {
			return
		}
		got, err := key.Open(goldenBinding, e)
		if err != nil {
			return
		}
		// Only the sealed envelope may open: any other spelling that opens must decode to the same fields.
		var want secretstore.EnvelopeV2
		_ = json.Unmarshal(seed, &want)
		if !reflect.DeepEqual(e, want) {
			t.Fatalf("opened a changed envelope %+v vs %+v: %q", e, want, got)
		}
	})
}

var _ = Describe("the default recipient suite", func() {
	It("is X25519, or P-256 under FIPS 140-only mode, and works", func() {
		suite := secretstore.DefaultRecipientSuite()
		Expect([]string{secretstore.SuiteX25519, secretstore.SuiteP256}).To(ContainElement(suite))
		k, err := secretstore.GenerateRecipientKey(suite)
		Expect(err).NotTo(HaveOccurred())
		env, err := secretstore.SealV2(k.Public(), recipientBinding, []byte("koinon_test_not-a-real-token"))
		Expect(err).NotTo(HaveOccurred())
		_, err = k.Open(recipientBinding, env)
		Expect(err).NotTo(HaveOccurred())
	})
})
