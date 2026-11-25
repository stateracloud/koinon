// SPDX-License-Identifier: Apache-2.0

package ensuresecrets_test

import (
	"bytes"
	"crypto/ed25519"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/ensuresecrets"
	"github.com/stateracloud/koinon/internal/gateway/actortoken"
)

var _ = Describe("Ensure with an Ed25519 key (#1086 part C2)", func() {
	spec := []ensuresecrets.Spec{{Name: "actor", Type: corev1.SecretTypeOpaque, Kind: ensuresecrets.KindEd25519Key}}
	// Obviously fake seeds: one repeated byte.
	fakeSeed := func(b string) []byte { return []byte(strings.Repeat(b, 32)) }
	pemOf := func(seedHex []byte) []byte {
		k, err := actortoken.ParseSeedKey(seedHex)
		Expect(err).NotTo(HaveOccurred())
		p, err := actortoken.MarshalPublicKey(k.Public().(ed25519.PublicKey))
		Expect(err).NotTo(HaveOccurred())
		return p
	}

	It("creates a seed and its public key that a minter and a verifier accept, and never logs them", func(ctx SpecContext) {
		var out bytes.Buffer
		client := fake.NewClientset()
		_, err := ensuresecrets.Ensure(ctx, client, "koinon", spec, &out)
		Expect(err).NotTo(HaveOccurred())
		got, err := client.CoreV1().Secrets("koinon").Get(ctx, "actor", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Data).To(HaveLen(2))
		Expect(string(got.Data["seed"])).To(MatchRegexp(`^[0-9a-f]{64}$`))
		Expect(out.String()).NotTo(ContainSubstring(string(got.Data["seed"])))
		Expect(out.String()).NotTo(ContainSubstring("PUBLIC KEY"))

		key, err := actortoken.ParseSeedKey(got.Data["seed"])
		Expect(err).NotTo(HaveOccurred())
		pub, err := actortoken.ParsePublicKey(got.Data["public.pem"])
		Expect(err).NotTo(HaveOccurred())
		m, err := actortoken.NewMinter("koinon-agentd", "k1", key)
		Expect(err).NotTo(HaveOccurred())
		v, err := actortoken.NewVerifier("koinon-agentd", map[string]ed25519.PublicKey{"k1": pub})
		Expect(err).NotTo(HaveOccurred())
		tok, err := m.Mint(actortoken.Actor{Org: "acme", Agent: "a", Run: "r"}, actortoken.DefaultTTL)
		Expect(err).NotTo(HaveOccurred())
		_, err = v.Verify(tok)
		Expect(err).NotTo(HaveOccurred())

		_, err = ensuresecrets.Ensure(ctx, fake.NewClientset(), "koinon", spec, &out)
		Expect(err).NotTo(HaveOccurred())
	})

	It("keeps a matching pair and refuses a broken one unchanged, without the key text in the error", func(ctx SpecContext) {
		kept := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "actor", Namespace: "koinon"},
			Data:       map[string][]byte{"seed": fakeSeed("01"), "public.pem": pemOf(fakeSeed("01"))},
		}
		client := fake.NewClientset(kept)
		var out bytes.Buffer
		_, err := ensuresecrets.Ensure(ctx, client, "koinon", spec, &out)
		Expect(err).NotTo(HaveOccurred())
		got, _ := client.CoreV1().Secrets("koinon").Get(ctx, "actor", metav1.GetOptions{})
		Expect(got).To(Equal(kept))

		for name, data := range map[string]map[string][]byte{
			`"seed"`:             {"seed": []byte("0101"), "public.pem": pemOf(fakeSeed("01"))},
			`"public.pem"`:       {"seed": fakeSeed("01"), "public.pem": []byte("not a key")},
			"not the public key": {"seed": fakeSeed("01"), "public.pem": pemOf(fakeSeed("02"))},
		} {
			bad := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "actor", Namespace: "koinon"}, Data: data}
			c := fake.NewClientset(bad)
			_, err := ensuresecrets.Ensure(ctx, c, "koinon", spec, &out)
			Expect(err).To(MatchError(ContainSubstring(name)), name)
			Expect(err.Error()).NotTo(ContainSubstring(string(fakeSeed("01"))))
			got, _ := c.CoreV1().Secrets("koinon").Get(ctx, "actor", metav1.GetOptions{})
			Expect(got).To(Equal(bad))
		}
	})
})
