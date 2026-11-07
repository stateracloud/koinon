// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/keyscli"
	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

var _ = Describe("koinon-api's key events", func() {
	const ns = "koinon"
	var (
		keyAud *keyAudit
		kube   *fake.Clientset
	)
	secretOf := func(name, uid string, labelled bool) *corev1.Secret {
		s := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID(uid)},
			Data:       map[string][]byte{"value": []byte("koinon_test_not-a-real-secret")},
		}
		if labelled {
			s.Labels = map[string]string{keyscli.GeneratedLabel: "true"}
		}
		return s
	}
	count := func(ctx context.Context, action string, mark int64) int {
		GinkgoHelper()
		var n int
		Expect(keyAud.pool.QueryRow(ctx, `SELECT count(*) FROM install_audit_events WHERE chain_seq > $1 AND action = $2`, mark, action).Scan(&n)).To(Succeed())
		return n
	}
	mark := func(ctx context.Context) int64 {
		GinkgoHelper()
		var seq int64
		Expect(keyAud.pool.QueryRow(ctx, `SELECT coalesce(max(chain_seq), 0) FROM install_audit_events`).Scan(&seq)).To(Succeed())
		return seq
	}

	BeforeEach(func(ctx SpecContext) {
		kube = fake.NewClientset()
		keyAud = newKeyAudit(pgtest.Start(ctx), kube, ns, "koinon-saved-work")
		raw := make([]byte, secretstore.KeySize)
		copy(raw, "koinon_test_not-a-real-kek")
		_, err := kube.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "koinon-kek", Namespace: ns},
			Data:       map[string][]byte{"kek-20261107-aaaa": secretstore.EncodeKey(raw), secretstore.ActiveFile: []byte("kek-20261107-aaaa")},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		keyAud.keySet = keyscli.KubeStore{Client: kube, Namespace: ns, Name: "koinon-kek"}
	})

	It("records a generated key as keys.generated under koinon-api:keys", func(ctx SpecContext) {
		_, err := kube.CoreV1().Secrets(ns).Create(ctx, secretOf("koinon-saved-work", "uid-1", false), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		m := mark(ctx)
		keyAud.generatedEvent(ctx, "secret", "koinon-saved-work")
		var actorKind, actorID, raw string
		Expect(keyAud.pool.QueryRow(ctx, `SELECT actor_kind, actor_id, summary::text FROM install_audit_events WHERE chain_seq > $1 AND action = 'keys.generated'`, m).
			Scan(&actorKind, &actorID, &raw)).To(Succeed())
		Expect(actorKind).To(Equal("system"))
		Expect(actorID).To(Equal("koinon-api:keys"))
		Expect(raw).To(ContainSubstring("koinon-saved-work"))
		Expect(raw).NotTo(ContainSubstring("koinon_test_not-a-real-secret"), "a Secret is read by metadata only")
	})

	It("observes once, again when a generated Secret is re-created, and never records a value", func(ctx SpecContext) {
		_, err := kube.CoreV1().Secrets(ns).Create(ctx, secretOf("koinon-session", "uid-1", true), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(keyAud.observe(ctx)).To(Succeed()) // baseline
		m := mark(ctx)
		Expect(keyAud.observe(ctx)).To(Succeed())
		Expect(count(ctx, "keys.observed", m)).To(BeZero(), "unchanged")

		Expect(kube.CoreV1().Secrets(ns).Delete(ctx, "koinon-session", metav1.DeleteOptions{})).To(Succeed())
		_, err = kube.CoreV1().Secrets(ns).Create(ctx, secretOf("koinon-session", "uid-2", true), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(keyAud.observe(ctx)).To(Succeed())
		Expect(count(ctx, "keys.observed", m)).To(Equal(1))
		var raw string
		Expect(keyAud.pool.QueryRow(ctx, `SELECT summary::text FROM install_audit_events WHERE chain_seq > $1 AND action = 'keys.observed'`, m).Scan(&raw)).To(Succeed())
		Expect(raw).To(And(ContainSubstring("uid-2"), Not(ContainSubstring("koinon_test_not-a-real"))))
	})
})
