// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"bytes"
	"context"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

var _ = Describe("koinon-api's audit signing key", func() {
	const (
		ns     = "koinon"
		secret = "koinon-audit-signing"
		origin = "koinon.dev/install/11111111-1111-1111-1111-111111111111/audit"
	)
	var (
		kube *fake.Clientset
		src  *kubeSigningKeys
	)

	generatedCount := func(ctx context.Context) (n int, summaries string) {
		GinkgoHelper()
		keyAud := src.audits
		Expect(keyAud.pool.QueryRow(ctx, `SELECT count(*), coalesce(string_agg(summary::text, ' '), '') FROM install_audit_events WHERE action = 'keys.generated' AND target_id = $1`, secret).Scan(&n, &summaries)).To(Succeed())
		return n, summaries
	}

	BeforeEach(func(ctx SpecContext) {
		kube = fake.NewClientset()
		src = &kubeSigningKeys{
			kube: kube, namespace: ns, name: secret,
			audits: newKeyAudit(pgtest.Start(ctx), kube, ns, "", "koinon-kek", secret),
		}
	})

	It("makes the Secret on the first use, records keys.generated once, and returns the same key afterwards", func(ctx SpecContext) {
		first, err := src.SigningKey(ctx, origin)
		Expect(err).NotTo(HaveOccurred())
		sec, err := kube.CoreV1().Secrets(ns).Get(ctx, secret, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(sec.Data).To(HaveKey(auditSigningSeedKey))
		Expect(sec.Data[auditSigningSeedKey]).To(HaveLen(audit.SeedSize))
		Expect(sec.Labels).To(HaveKeyWithValue("koinon.dev/managed-by", "koinon-api"))

		n, summaries := generatedCount(ctx)
		Expect(n).To(Equal(1))
		Expect(summaries).NotTo(ContainSubstring(string(sec.Data[auditSigningSeedKey])), "the seed never reaches the audit log")

		second, err := src.SigningKey(ctx, origin)
		Expect(err).NotTo(HaveOccurred())
		Expect(second.Public().String()).To(Equal(first.Public().String()))
		n, _ = generatedCount(ctx)
		Expect(n).To(Equal(1), "a Secret that exists is read, not recorded again")
	})

	It("never overwrites a Secret that exists", func(ctx SpecContext) {
		seed := bytes.Repeat([]byte{7}, audit.SeedSize)
		_, err := kube.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secret, Namespace: ns},
			Data:       map[string][]byte{auditSigningSeedKey: seed},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		k, err := src.SigningKey(ctx, origin)
		Expect(err).NotTo(HaveOccurred())
		want, err := audit.NewSigningKey(origin, seed)
		Expect(err).NotTo(HaveOccurred())
		Expect(k.Public().String()).To(Equal(want.Public().String()))
		for _, a := range kube.Actions() {
			Expect(a.GetVerb()).NotTo(BeElementOf("update", "patch", "delete"), "koinon-api only gets and creates the signing Secret")
		}
	})

	It("makes a new key when the Secret was deleted (rotation)", func(ctx SpecContext) {
		first, err := src.SigningKey(ctx, origin)
		Expect(err).NotTo(HaveOccurred())
		Expect(kube.CoreV1().Secrets(ns).Delete(ctx, secret, metav1.DeleteOptions{})).To(Succeed())
		second, err := src.SigningKey(ctx, origin)
		Expect(err).NotTo(HaveOccurred())
		Expect(second.Public().ID()).NotTo(Equal(first.Public().ID()))
		n, _ := generatedCount(ctx)
		Expect(n).To(Equal(2), "each new key is a keys.generated event")
	})

	It("refuses a Secret whose seed has the wrong size, without leaking it", func(ctx SpecContext) {
		_, err := kube.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secret, Namespace: ns},
			Data:       map[string][]byte{auditSigningSeedKey: []byte("koinon_test_not-a-real-token")},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		_, err = src.SigningKey(ctx, origin)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).NotTo(ContainSubstring("koinon_test_not-a-real-token"))
	})

	Describe("reading a checkpoint off the cluster", func() {
		It("checks the note against the public keys, from a key line or a key file", func() {
			k, err := audit.NewSigningKey(origin, bytes.Repeat([]byte{9}, audit.SeedSize))
			Expect(err).NotTo(HaveOccurred())
			hash := bytes.Repeat([]byte{5}, 32)
			raw, err := k.Sign(audit.Note{Origin: origin, ChainSeq: 12, Hash: hash})
			Expect(err).NotTo(HaveOccurred())
			dir := GinkgoT().TempDir()
			noteFile := filepath.Join(dir, "checkpoint.note")
			Expect(os.WriteFile(noteFile, raw, 0o600)).To(Succeed())
			keyFile := filepath.Join(dir, "keys.txt")
			Expect(os.WriteFile(keyFile, []byte("# the key kept off the cluster\n"+k.Public().String()+"\n"), 0o600)).To(Succeed())

			var out bytes.Buffer
			for _, arg := range []string{k.Public().String(), keyFile} {
				n, err := readCheckpoint(noteFile, []string{arg}, &out)
				Expect(err).NotTo(HaveOccurred())
				Expect(n.ChainSeq).To(Equal(int64(12)))
			}
			Expect(out.String()).To(ContainSubstring("ok "))

			_, err = readCheckpoint(noteFile, nil, &out)
			Expect(err).To(HaveOccurred(), "a note proves nothing without a key")
			other, err := audit.NewSigningKey(origin, bytes.Repeat([]byte{10}, audit.SeedSize))
			Expect(err).NotTo(HaveOccurred())
			_, err = readCheckpoint(noteFile, []string{other.Public().String()}, &out)
			Expect(err).To(MatchError(audit.ErrNoteUnknownKey))
		})
	})
})
