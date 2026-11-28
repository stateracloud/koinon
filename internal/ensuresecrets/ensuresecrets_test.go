// SPDX-License-Identifier: Apache-2.0

package ensuresecrets_test

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"

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
	"github.com/stateracloud/koinon/internal/guestauth"
)

var _ = Describe("Ensure", func() {
	var out bytes.Buffer
	specs := []ensuresecrets.Spec{
		{Name: "db-api", Type: corev1.SecretTypeBasicAuth, Username: "koinon_api", Labels: map[string]string{"cnpg.io/reload": "true"}},
		{Name: "admin", Type: corev1.SecretTypeOpaque, Username: "admin"},
	}
	BeforeEach(func() { out.Reset() })

	It("creates a missing Secret with the type, labels, username and a random password", func(ctx SpecContext) {
		client := fake.NewClientset()
		res, err := ensuresecrets.Ensure(ctx, client, "koinon", specs, &out)
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(ConsistOf(
			ensuresecrets.Result{Name: "db-api", Created: true},
			ensuresecrets.Result{Name: "admin", Created: true}))

		got, err := client.CoreV1().Secrets("koinon").Get(ctx, "db-api", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Type).To(Equal(corev1.SecretTypeBasicAuth))
		Expect(got.Labels).To(Equal(map[string]string{"cnpg.io/reload": "true"}))
		Expect(string(got.Data["username"])).To(Equal("koinon_api"))
		Expect(string(got.Data["password"])).To(MatchRegexp(`^[A-Za-z0-9]{32}$`))
		other, err := client.CoreV1().Secrets("koinon").Get(ctx, "admin", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(other.Data["password"]).NotTo(Equal(got.Data["password"]))

		Expect(out.String()).To(ContainSubstring("secret db-api: created"))
		Expect(out.String()).NotTo(ContainSubstring(string(got.Data["password"])))
	})

	It("keeps an existing Secret and its password", func(ctx SpecContext) {
		existing := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "db-api", Namespace: "koinon"},
			Type:       corev1.SecretTypeBasicAuth,
			Data:       map[string][]byte{"username": []byte("koinon_api"), "password": []byte("not-a-real-password")},
		}
		client := fake.NewClientset(existing)
		res, err := ensuresecrets.Ensure(ctx, client, "koinon", specs[:1], &out)
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(ConsistOf(ensuresecrets.Result{Name: "db-api"}))

		got, err := client.CoreV1().Secrets("koinon").Get(ctx, "db-api", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(existing))
		Expect(out.String()).To(ContainSubstring("secret db-api: kept"))
	})

	It("fails on an existing Secret without a password and changes nothing", func(ctx SpecContext) {
		existing := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "db-api", Namespace: "koinon"},
			Data:       map[string][]byte{"username": []byte("someone")},
		}
		client := fake.NewClientset(existing)
		_, err := ensuresecrets.Ensure(ctx, client, "koinon", specs, &out)
		Expect(err).To(MatchError(ContainSubstring(`koinon/db-api exists but has no "password" key`)))
		got, _ := client.CoreV1().Secrets("koinon").Get(ctx, "db-api", metav1.GetOptions{})
		Expect(got).To(Equal(existing))
		_, err = client.CoreV1().Secrets("koinon").Get(ctx, "admin", metav1.GetOptions{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "nothing after the failure is created")
	})

	It("fails on an existing database role Secret with another username or type", func(ctx SpecContext) {
		for _, existing := range []*corev1.Secret{{
			ObjectMeta: metav1.ObjectMeta{Name: "db-api", Namespace: "koinon"},
			Type:       corev1.SecretTypeBasicAuth,
			Data:       map[string][]byte{"username": []byte("someone"), "password": []byte("not-a-real-password")},
		}, {
			ObjectMeta: metav1.ObjectMeta{Name: "db-api", Namespace: "koinon"},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{"username": []byte("koinon_api"), "password": []byte("not-a-real-password")},
		}} {
			client := fake.NewClientset(existing)
			_, err := ensuresecrets.Ensure(ctx, client, "koinon", specs[:1], &out)
			Expect(err).To(MatchError(ContainSubstring("koinon/db-api exists with")))
			got, _ := client.CoreV1().Secrets("koinon").Get(ctx, "db-api", metav1.GetOptions{})
			Expect(got).To(Equal(existing), "it is not changed")
		}
	})

	It("treats AlreadyExists on create as success", func(ctx SpecContext) {
		client := fake.NewClientset()
		client.PrependReactor("create", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "secrets"}, "db-api")
		})
		res, err := ensuresecrets.Ensure(ctx, client, "koinon", specs[:1], &out)
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(ConsistOf(ensuresecrets.Result{Name: "db-api"}))
	})

	It("generates different passwords", func() {
		a, err := ensuresecrets.NewPassword()
		Expect(err).NotTo(HaveOccurred())
		b, err := ensuresecrets.NewPassword()
		Expect(err).NotTo(HaveOccurred())
		Expect(a).NotTo(Equal(b))
	})
})

var _ = Describe("Ensure with an AES key", func() {
	spec := []ensuresecrets.Spec{{Name: "gw-session", Type: corev1.SecretTypeOpaque, Kind: ensuresecrets.KindAES256Key}}

	It("creates a 64-hex-character key and no password, and never logs it", func(ctx SpecContext) {
		var out bytes.Buffer
		client := fake.NewClientset()
		_, err := ensuresecrets.Ensure(ctx, client, "koinon", spec, &out)
		Expect(err).NotTo(HaveOccurred())
		got, err := client.CoreV1().Secrets("koinon").Get(ctx, "gw-session", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(string(got.Data["key"])).To(MatchRegexp(`^[0-9a-f]{64}$`))
		Expect(got.Data).NotTo(HaveKey("password"))
		Expect(out.String()).NotTo(ContainSubstring(string(got.Data["key"])))
	})

	It("keeps a valid key and rejects a malformed one without changing it", func(ctx SpecContext) {
		good := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "gw-session", Namespace: "koinon"},
			Data:       map[string][]byte{"key": []byte("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")},
		}
		client := fake.NewClientset(good)
		var out bytes.Buffer
		_, err := ensuresecrets.Ensure(ctx, client, "koinon", spec, &out)
		Expect(err).NotTo(HaveOccurred())
		got, _ := client.CoreV1().Secrets("koinon").Get(ctx, "gw-session", metav1.GetOptions{})
		Expect(got).To(Equal(good))

		bad := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "gw-session", Namespace: "koinon"},
			Data:       map[string][]byte{"key": []byte("short")},
		}
		_, err = ensuresecrets.Ensure(ctx, fake.NewClientset(bad), "koinon", spec, &out)
		Expect(err).To(MatchError(ContainSubstring("not 64 hex characters")))
	})
})

var _ = Describe("Ensure with an Ed25519 key", func() {
	spec := []ensuresecrets.Spec{{Name: "guest-key", Type: corev1.SecretTypeOpaque, Kind: ensuresecrets.KindEd25519Key}}

	It("creates a seed that the guest API signer takes, and never logs it", func(ctx SpecContext) {
		var out bytes.Buffer
		client := fake.NewClientset()
		_, err := ensuresecrets.Ensure(ctx, client, "koinon", spec, &out)
		Expect(err).NotTo(HaveOccurred())
		got, err := client.CoreV1().Secrets("koinon").Get(ctx, "guest-key", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		k, err := guestauth.ParseSeed(string(got.Data["key"]))
		Expect(err).NotTo(HaveOccurred())
		_, err = guestauth.NewSigner(k)
		Expect(err).NotTo(HaveOccurred())
		Expect(out.String()).NotTo(ContainSubstring(string(got.Data["key"])))
	})

	It("keeps a valid seed (adding only its public key) and rejects a malformed one without changing it", func(ctx SpecContext) {
		good := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "guest-key", Namespace: "koinon"},
			Data:       map[string][]byte{"key": []byte("1111111111111111111111111111111111111111111111111111111111111111")},
		}
		client := fake.NewClientset(good)
		var out bytes.Buffer
		_, err := ensuresecrets.Ensure(ctx, client, "koinon", spec, &out)
		Expect(err).NotTo(HaveOccurred())
		got, _ := client.CoreV1().Secrets("koinon").Get(ctx, "guest-key", metav1.GetOptions{})
		// The rc.30 guest key had no public.pem: Ensure adds it from the seed and keeps the seed
		// (#1086 part C2; ed25519_test.go has the details).
		Expect(got.Data["key"]).To(Equal(good.Data["key"]))
		Expect(got.Data).To(HaveKey("public.pem"))
		Expect(got.Data).To(HaveLen(2))
		bad := good.DeepCopy()
		bad.Data["key"] = []byte("short")
		_, err = ensuresecrets.Ensure(ctx, fake.NewClientset(bad), "koinon", spec, &out)
		Expect(err).To(MatchError(ContainSubstring("not 64 hex characters")))
	})
})

var _ = Describe("Ensure with a bearer token", func() {
	spec := []ensuresecrets.Spec{{Name: "otlp", Type: corev1.SecretTypeOpaque, Kind: ensuresecrets.KindBearerToken}}

	It("creates a 64-hex-character token under token, and never logs it", func(ctx SpecContext) {
		var out bytes.Buffer
		client := fake.NewClientset()
		_, err := ensuresecrets.Ensure(ctx, client, "koinon", spec, &out)
		Expect(err).NotTo(HaveOccurred())
		got, err := client.CoreV1().Secrets("koinon").Get(ctx, "otlp", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(string(got.Data["token"])).To(MatchRegexp(`^[0-9a-f]{64}$`))
		Expect(got.Data).To(HaveLen(1))
		Expect(out.String()).NotTo(ContainSubstring(string(got.Data["token"])))
	})

	It("keeps a rotated token and its previous key, and rejects a short one unchanged", func(ctx SpecContext) {
		rotated := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "otlp", Namespace: "koinon"},
			Data: map[string][]byte{
				"token":    []byte("koinon_test_not-a-real-token-rotated-0001\n"),
				"previous": []byte("koinon_test_not-a-real-token-previous-002"),
			},
		}
		client := fake.NewClientset(rotated)
		var out bytes.Buffer
		_, err := ensuresecrets.Ensure(ctx, client, "koinon", spec, &out)
		Expect(err).NotTo(HaveOccurred())
		got, _ := client.CoreV1().Secrets("koinon").Get(ctx, "otlp", metav1.GetOptions{})
		Expect(got).To(Equal(rotated))

		bad := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "otlp", Namespace: "koinon"},
			Data:       map[string][]byte{"token": []byte("short")},
		}
		_, err = ensuresecrets.Ensure(ctx, fake.NewClientset(bad), "koinon", spec, &out)
		Expect(err).To(MatchError(ContainSubstring("at least 32 characters")))

		badPrev := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "otlp", Namespace: "koinon"},
			Data: map[string][]byte{
				"token":    []byte("koinon_test_not-a-real-token-rotated-0001"),
				"previous": []byte("short"),
			},
		}
		_, err = ensuresecrets.Ensure(ctx, fake.NewClientset(badPrev), "koinon", spec, &out)
		Expect(err).To(MatchError(ContainSubstring(`"previous"`)))
		// An empty previous (a rotation finished by blanking it) is fine.
		badPrev.Data["previous"] = []byte("\n")
		_, err = ensuresecrets.Ensure(ctx, fake.NewClientset(badPrev), "koinon", spec, &out)
		Expect(err).NotTo(HaveOccurred())
	})
})

var _ = Describe("Ensure with an unknown kind", func() {
	It("fails instead of creating a password", func(ctx SpecContext) {
		client := fake.NewClientset()
		var out bytes.Buffer
		_, err := ensuresecrets.Ensure(ctx, client, "koinon", []ensuresecrets.Spec{{Name: "x", Kind: "aes512-key"}}, &out)
		Expect(err).To(MatchError(ContainSubstring("unknown kind")))
		_, err = client.CoreV1().Secrets("koinon").Get(ctx, "x", metav1.GetOptions{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	Describe("a tls Secret", func() {
		var out bytes.Buffer
		BeforeEach(func() { out.Reset() })
		names := []string{"cache", "cache.koinon.svc"}
		spec := ensuresecrets.Spec{Name: "cache-tls", Kind: ensuresecrets.KindTLS, DNSNames: names}

		It("creates a certificate for the names that its own CA signed, and logs no key", func(ctx SpecContext) {
			client := fake.NewClientset()
			_, err := ensuresecrets.Ensure(ctx, client, "koinon", []ensuresecrets.Spec{spec}, &out)
			Expect(err).NotTo(HaveOccurred())
			got, err := client.CoreV1().Secrets("koinon").Get(ctx, "cache-tls", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(got.Type).To(Equal(corev1.SecretTypeTLS))
			Expect(got.Data).To(HaveKey("tls.crt"))
			Expect(got.Data).To(HaveKey("tls.key"))
			Expect(got.Data).To(HaveKey("ca.crt"))
			Expect(got.Data).NotTo(HaveKey("ca.key"))
			pair, err := tls.X509KeyPair(got.Data["tls.crt"], got.Data["tls.key"])
			Expect(err).NotTo(HaveOccurred())
			leaf, err := x509.ParseCertificate(pair.Certificate[0])
			Expect(err).NotTo(HaveOccurred())
			pool := x509.NewCertPool()
			Expect(pool.AppendCertsFromPEM(got.Data["ca.crt"])).To(BeTrue())
			for _, n := range names {
				_, err = leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: n})
				Expect(err).NotTo(HaveOccurred())
			}
			_, err = leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "other.example.com"})
			Expect(err).To(HaveOccurred())
			Expect(out.String()).To(ContainSubstring("secret cache-tls: created"))
			Expect(out.String()).NotTo(ContainSubstring("PRIVATE KEY"))
		})

		It("keeps a valid Secret, and refuses a broken one without changing it", func(ctx SpecContext) {
			client := fake.NewClientset()
			_, err := ensuresecrets.Ensure(ctx, client, "koinon", []ensuresecrets.Spec{spec}, &out)
			Expect(err).NotTo(HaveOccurred())
			before, _ := client.CoreV1().Secrets("koinon").Get(ctx, "cache-tls", metav1.GetOptions{})
			res, err := ensuresecrets.Ensure(ctx, client, "koinon", []ensuresecrets.Spec{spec}, &out)
			Expect(err).NotTo(HaveOccurred())
			Expect(res).To(ConsistOf(ensuresecrets.Result{Name: "cache-tls"}))
			after, _ := client.CoreV1().Secrets("koinon").Get(ctx, "cache-tls", metav1.GetOptions{})
			Expect(after.Data).To(Equal(before.Data))

			// A certificate that lacks a name, or a missing CA, is refused and left alone.
			wider := spec
			wider.DNSNames = append(append([]string{}, names...), "cache.other.svc")
			_, err = ensuresecrets.Ensure(ctx, client, "koinon", []ensuresecrets.Spec{wider}, &out)
			Expect(err).To(MatchError(ContainSubstring("does not verify for cache.other.svc")))
			delete(before.Data, "ca.crt")
			Expect(client.CoreV1().Secrets("koinon").Update(ctx, before, metav1.UpdateOptions{})).Error().NotTo(HaveOccurred())
			_, err = ensuresecrets.Ensure(ctx, client, "koinon", []ensuresecrets.Spec{spec}, &out)
			Expect(err).To(MatchError(ContainSubstring("holds no certificate")))
		})

		It("needs a DNS name", func(ctx SpecContext) {
			_, err := ensuresecrets.Ensure(ctx, fake.NewClientset(), "koinon", []ensuresecrets.Spec{{Name: "t", Kind: ensuresecrets.KindTLS}}, &out)
			Expect(err).To(MatchError(ContainSubstring("at least one DNS name")))
		})
	})
})
