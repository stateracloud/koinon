// SPDX-License-Identifier: Apache-2.0

package ensuresecrets_test

import (
	"bytes"
	"crypto/ed25519"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/stateracloud/koinon/internal/ensuresecrets"
	"github.com/stateracloud/koinon/internal/gateway/actortoken"
	"github.com/stateracloud/koinon/internal/guestauth"
)

var _ = Describe("Ensure with an Ed25519 key and its public key (#1086 part C2)", func() {
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
	secret := func(data map[string][]byte) *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "actor", Namespace: "koinon"}, Data: data}
	}

	It("creates a seed under key and its public key, that a minter, a verifier and the guest signer accept, and never logs them", func(ctx SpecContext) {
		var out bytes.Buffer
		client := fake.NewClientset()
		res, err := ensuresecrets.Ensure(ctx, client, "koinon", spec, &out)
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal([]ensuresecrets.Result{{Name: "actor", Created: true}}))
		got, err := client.CoreV1().Secrets("koinon").Get(ctx, "actor", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Data).To(HaveLen(2))
		Expect(string(got.Data["key"])).To(MatchRegexp(`^[0-9a-f]{64}$`))
		Expect(out.String()).NotTo(ContainSubstring(string(got.Data["key"])))
		Expect(out.String()).NotTo(ContainSubstring("PUBLIC KEY"))

		key, err := actortoken.ParseSeedKey(got.Data["key"])
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
		// One layout for both users of the kind: the guest API signer reads the same seed.
		gk, err := guestauth.ParseSeed(string(got.Data["key"]))
		Expect(err).NotTo(HaveOccurred())
		Expect(gk.Public()).To(Equal(key.Public()))
	})

	It("keeps a matching pair exactly as it is", func(ctx SpecContext) {
		kept := secret(map[string][]byte{"key": fakeSeed("01"), "public.pem": pemOf(fakeSeed("01"))})
		client := fake.NewClientset(kept)
		var out bytes.Buffer
		res, err := ensuresecrets.Ensure(ctx, client, "koinon", spec, &out)
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal([]ensuresecrets.Result{{Name: "actor"}}))
		got, _ := client.CoreV1().Secrets("koinon").Get(ctx, "actor", metav1.GetOptions{})
		Expect(got).To(Equal(kept))
		for _, a := range client.Actions() {
			Expect(a.GetVerb()).To(Equal("get"))
		}
	})

	It("adds the public key of a kept seed without one, and changes nothing else", func(ctx SpecContext) {
		old := secret(map[string][]byte{"key": fakeSeed("01"), "other": []byte("left alone")})
		old.Labels = map[string]string{"app": "x"}
		client := fake.NewClientset(old)
		var out bytes.Buffer
		res, err := ensuresecrets.Ensure(ctx, client, "koinon", spec, &out)
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal([]ensuresecrets.Result{{Name: "actor", PublicKeyAdded: true}}))
		Expect(out.String()).To(Equal("secret actor: kept, public key added\n"))

		got, _ := client.CoreV1().Secrets("koinon").Get(ctx, "actor", metav1.GetOptions{})
		want := old.DeepCopy()
		want.Data["public.pem"] = pemOf(fakeSeed("01"))
		// The patch also writes managedFields; the Secret's content is all that is compared.
		Expect(got.Data).To(Equal(want.Data))
		Expect(got.Labels).To(Equal(want.Labels))
		Expect(got.Type).To(Equal(want.Type))

		// Idempotent: a second run keeps the pair and makes no patch.
		client.ClearActions()
		res, err = ensuresecrets.Ensure(ctx, client, "koinon", spec, &out)
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal([]ensuresecrets.Result{{Name: "actor"}}))
		for _, a := range client.Actions() {
			Expect(a.GetVerb()).To(Equal("get"))
		}
	})

	It("patches only after a test that the seed is still the one it read", func(ctx SpecContext) {
		client := fake.NewClientset(secret(map[string][]byte{"key": fakeSeed("01")}))
		// The apiserver fails the JSON patch's test op when the seed changed after the read.
		client.PrependReactor("patch", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
			Expect(string(a.(k8stesting.PatchAction).GetPatch())).To(HavePrefix(`[{"op":"test","path":"/data/key"`))
			return true, nil, apierrors.NewInvalid(schema.GroupKind{Kind: "Secret"}, "actor", nil)
		})
		var out bytes.Buffer
		_, err := ensuresecrets.Ensure(ctx, client, "koinon", spec, &out)
		Expect(err).To(MatchError(ContainSubstring("its key changed while the public key was added")))
		Expect(err.Error()).NotTo(ContainSubstring(string(fakeSeed("01"))))
	})

	It("refuses a bad Secret, with or without a public key, and never changes it or logs the key", func(ctx SpecContext) {
		for name, data := range map[string]map[string][]byte{
			`short "key", no public key`:     {"key": []byte("0101")},
			`upper-case "key", no public key`: {"key": []byte(strings.Repeat("AB", 32))},
			`short "key"`:                     {"key": []byte("0101"), "public.pem": pemOf(fakeSeed("01"))},
			`"public.pem" not a key`:          {"key": fakeSeed("01"), "public.pem": []byte("not a key")},
			"not the public key of its seed":  {"key": fakeSeed("01"), "public.pem": pemOf(fakeSeed("02"))},
			"the old seed key only":           {"seed": fakeSeed("01"), "public.pem": pemOf(fakeSeed("01"))},
		} {
			bad := secret(data)
			c := fake.NewClientset(bad)
			var out bytes.Buffer
			_, err := ensuresecrets.Ensure(ctx, c, "koinon", spec, &out)
			Expect(err).To(MatchError(ContainSubstring("it is not changed here")), name)
			Expect(err.Error()).NotTo(ContainSubstring(string(fakeSeed("01"))), name)
			got, _ := c.CoreV1().Secrets("koinon").Get(ctx, "actor", metav1.GetOptions{})
			Expect(got).To(Equal(bad), name)
			for _, a := range c.Actions() {
				Expect(a.GetVerb()).To(Equal("get"), name)
			}
		}
	})
})
