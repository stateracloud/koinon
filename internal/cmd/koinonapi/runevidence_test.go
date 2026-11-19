// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/secretstore/wrapping"
)

var _ = Describe("the run-evidence export's key source (#966)", func() {
	const ns = "koinon"
	var kube *fake.Clientset
	var raw []byte

	BeforeEach(func(ctx SpecContext) {
		raw = make([]byte, secretstore.KeySize)
		copy(raw, "koinon_test_not-a-real-kek-aaaa")
		kube = fake.NewClientset()
		_, err := kube.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "koinon-kek", Namespace: ns},
			Data:       map[string][]byte{"kek-a": secretstore.EncodeKey(raw), "active": []byte("kek-a")},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
	})

	It("opens a value sealed with the Secret's KEK, read through the Kubernetes API", func(ctx SpecContext) {
		ring, err := loadKEKs(ctx, kekSource{secret: "koinon-kek", namespace: ns, client: kube}, secretstore.Wrappers{})
		Expect(err).NotTo(HaveOccurred())
		b := secretstore.Binding{OrgID: "o", Table: "t", RowID: "r", Column: "c"}
		env, err := ring.Seal(b, []byte("koinon_test_not-a-real-token"))
		Expect(err).NotTo(HaveOccurred())
		own, err := secretstore.NewKeyring(map[string][]byte{"kek-a": raw}, "kek-a")
		Expect(err).NotTo(HaveOccurred())
		got, err := own.Open(b, env)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(got)).To(Equal("koinon_test_not-a-real-token"))
	})

	It("only reads the Secret", func(ctx SpecContext) {
		kube.ClearActions()
		_, err := loadKEKs(ctx, kekSource{secret: "koinon-kek", namespace: ns, client: kube}, secretstore.Wrappers{})
		Expect(err).NotTo(HaveOccurred())
		for _, a := range kube.Actions() {
			Expect(a.GetVerb()).To(Equal("get"))
		}
	})

	It("fails when the Secret is missing or the namespace is not set", func(ctx SpecContext) {
		_, err := loadKEKs(ctx, kekSource{secret: "absent", namespace: ns, client: kube}, secretstore.Wrappers{})
		Expect(err).To(MatchError(ContainSubstring("read the KEK Secret")))
		_, err = loadKEKs(ctx, kekSource{secret: "koinon-kek", client: kube}, secretstore.Wrappers{})
		Expect(err).To(MatchError(ContainSubstring("--namespace")))
	})

	It("refuses both sources, and no source without --no-org-secrets", func(ctx SpecContext) {
		_, err := exportSecrets(ctx, nil, pgtype.UUID{}, kekSource{dir: "/k", secret: "koinon-kek"}, wrapping.Config{}, false)
		Expect(err).To(MatchError(ContainSubstring("not both")))
		_, err = exportSecrets(ctx, nil, pgtype.UUID{}, kekSource{}, wrapping.Config{}, false)
		Expect(err).To(MatchError(ContainSubstring("--kek-secret")))
		s, err := exportSecrets(ctx, nil, pgtype.UUID{}, kekSource{}, wrapping.Config{}, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(s).NotTo(BeNil())
	})

	It("has --kek-secret, --namespace and --kubeconfig on export", func() {
		export, _, err := newRunEvidenceCommand().Find([]string{"export"})
		Expect(err).NotTo(HaveOccurred())
		for _, f := range []string{"kek-secret", "namespace", "kubeconfig"} {
			Expect(export.Flags().Lookup(f)).NotTo(BeNil(), f)
		}
	})
})
