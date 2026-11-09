// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

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
		keyAud = newKeyAudit(pgtest.Start(ctx), kube, ns, "koinon-kek", "koinon-saved-work")
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

	It("records the KEK Secret koinon-api creates as one keys.generated, and the start-up check then writes nothing", func(ctx SpecContext) {
		m := mark(ctx)
		keyAud.generatedEvent(ctx, "secret", "koinon-kek")
		Expect(count(ctx, "keys.generated", m)).To(Equal(1))
		keyAud.startObserving(ctx, func() {})
		Expect(count(ctx, "keys.observed", m)).To(BeZero())
		Expect(count(ctx, "keys.generated", m)).To(Equal(1))
	})

	It("records only the created KEK Secret as keys.generated, and the labelled ensure-secrets Secret as keys.observed", func(ctx SpecContext) {
		_, err := kube.CoreV1().Secrets(ns).Create(ctx, secretOf("koinon-session", "uid-1", true), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		m := mark(ctx)
		keyAud.generatedEvent(ctx, "secret", "koinon-kek")
		Expect(count(ctx, "keys.generated", m)).To(Equal(1))
		var gen string
		Expect(keyAud.pool.QueryRow(ctx, `SELECT summary::text FROM install_audit_events WHERE chain_seq > $1 AND action = 'keys.generated'`, m).Scan(&gen)).To(Succeed())
		Expect(gen).NotTo(ContainSubstring("koinon-session"))
		keyAud.startObserving(ctx, func() {})
		Expect(count(ctx, "keys.observed", m)).To(Equal(1))
		var raw string
		Expect(keyAud.pool.QueryRow(ctx, `SELECT summary::text FROM install_audit_events WHERE chain_seq > $1 AND action = 'keys.observed'`, m).Scan(&raw)).To(Succeed())
		Expect(raw).To(ContainSubstring("koinon-session"))
	})

	keksOf := func(ctx context.Context, m int64, action, section string) string {
		GinkgoHelper()
		var raw string
		Expect(keyAud.pool.QueryRow(ctx, `SELECT coalesce(summary->$3->'keks', 'null')::text FROM install_audit_events WHERE chain_seq > $1 AND action = $2`, m, action, section).Scan(&raw)).To(Succeed())
		return raw
	}

	It("credits an existing KEK Secret to keys.observed, not to the saved-work key's keys.generated, on a first start", func(ctx SpecContext) {
		_, err := kube.CoreV1().Secrets(ns).Create(ctx, secretOf("koinon-saved-work", "uid-1", false), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		m := mark(ctx)
		keyAud.generatedEvent(ctx, "secret", "koinon-saved-work")
		Expect(count(ctx, "keys.generated", m)).To(Equal(1))
		Expect(keksOf(ctx, m, "keys.generated", "after")).To(Equal("[]"))
		keyAud.startObserving(ctx, func() {})
		Expect(count(ctx, "keys.observed", m)).To(Equal(1))
		Expect(keksOf(ctx, m, "keys.observed", "after")).To(ContainSubstring("kek-20261107-aaaa"))
		Expect(keksOf(ctx, m, "keys.observed", "before")).To(Equal("[]"))
	})

	It("does the same for KEKs from a mounted --kek-dir", func(ctx SpecContext) {
		dir := GinkgoT().TempDir()
		raw := make([]byte, secretstore.KeySize)
		copy(raw, "koinon_test_not-a-real-kek-dir")
		Expect(os.WriteFile(filepath.Join(dir, "kek-20261107-dddd"), secretstore.EncodeKey(raw), 0o600)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, secretstore.ActiveFile), []byte("kek-20261107-dddd"), 0o600)).To(Succeed())
		keyAud.keySet = keyscli.DirStore{Dir: dir}
		_, err := kube.CoreV1().Secrets(ns).Create(ctx, secretOf("koinon-saved-work", "uid-1", false), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		m := mark(ctx)
		keyAud.generatedEvent(ctx, "secret", "koinon-saved-work")
		Expect(keksOf(ctx, m, "keys.generated", "after")).To(Equal("[]"))
		keyAud.startObserving(ctx, func() {})
		Expect(keksOf(ctx, m, "keys.observed", "after")).To(ContainSubstring("kek-20261107-dddd"))
		Expect(keksOf(ctx, m, "keys.observed", "before")).To(Equal("[]"))
	})

	It("writes keys.observed on a first start even with no KEK and no generated Secret", func(ctx SpecContext) {
		// The chain holds no key event yet (a new database), and this koinon-api has no key set.
		empty := newKeyAudit(keyAud.pool, nil, ns, "koinon-kek")
		Expect(empty.observe(ctx)).To(Succeed())
		Expect(count(ctx, "keys.observed", 0)).To(Equal(1))
		var raw string
		Expect(keyAud.pool.QueryRow(ctx, `SELECT summary::text FROM install_audit_events WHERE action = 'keys.observed'`).Scan(&raw)).To(Succeed())
		Expect(raw).To(And(ContainSubstring(`"keks": []`), ContainSubstring(`"generated": []`)))
		Expect(empty.observe(ctx)).To(Succeed())
		Expect(count(ctx, "keys.observed", 0)).To(Equal(1), "recorded once")
	})

	It("checks for a lost change when the keyring watch sees the key set change", func(ctx SpecContext) {
		Expect(keyAud.observe(ctx)).To(Succeed()) // the baseline
		m := mark(ctx)
		ring, err := loadKeyring(ctx, keyAud.keySet, nil)
		Expect(err).NotTo(HaveOccurred())
		wctx, cancel := context.WithCancel(ctx)
		stopped := make(chan struct{})
		go func() {
			defer GinkgoRecover()
			defer close(stopped)
			watchKeyring(wctx, ring, keyAud.keySet, 50*time.Millisecond, nil, keyAud.watchChanged)
		}()
		DeferCleanup(func() { cancel(); <-stopped })
		Consistently(func() int { return count(ctx, "keys.observed", m) }, 300*time.Millisecond).Should(BeZero(), "the key set is the recorded one")

		By("a KEK is added with no event")
		sec, err := kube.CoreV1().Secrets(ns).Get(ctx, "koinon-kek", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		raw := make([]byte, secretstore.KeySize)
		copy(raw, "koinon_test_not-a-real-kek-bbbb")
		sec.Data["kek-20261107-bbbb"] = secretstore.EncodeKey(raw)
		_, err = kube.CoreV1().Secrets(ns).Update(ctx, sec, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() int { return count(ctx, "keys.observed", m) }).Should(Equal(1))
		var actorID string
		Expect(keyAud.pool.QueryRow(ctx, `SELECT actor_id FROM install_audit_events WHERE chain_seq > $1 AND action = 'keys.observed'`, m).Scan(&actorID)).To(Succeed())
		Expect(actorID).To(Equal("koinon-api:keyring-watch"))
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

	It("leaves a lost KEK change to the start-up check when ensureGenerated creates nothing", func(ctx SpecContext) {
		Expect(keyAud.observe(ctx)).To(Succeed()) // the recorded state
		m := mark(ctx)
		By("a KEK is added with no event")
		sec, err := kube.CoreV1().Secrets(ns).Get(ctx, "koinon-kek", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		raw := make([]byte, secretstore.KeySize)
		copy(raw, "koinon_test_not-a-real-kek-cccc")
		sec.Data["kek-20261107-cccc"] = secretstore.EncodeKey(raw)
		_, err = kube.CoreV1().Secrets(ns).Update(ctx, sec, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		created, err := keyAud.ensureGenerated(ctx, "secret", "koinon-kek", func(context.Context) (bool, error) { return false, nil })
		Expect(err).NotTo(HaveOccurred())
		Expect(created).To(BeFalse())
		Expect(count(ctx, "keys.observed", m)).To(BeZero(), "the Change writes nothing when it created nothing")

		Expect(keyAud.observe(ctx)).To(Succeed())
		Expect(count(ctx, "keys.observed", m)).To(Equal(1))
		Expect(count(ctx, "keys.generated", m)).To(BeZero())
		var actorID string
		Expect(keyAud.pool.QueryRow(ctx, `SELECT actor_id FROM install_audit_events WHERE chain_seq > $1 AND action = 'keys.observed'`, m).Scan(&actorID)).To(Succeed())
		Expect(actorID).To(Equal("koinon-api:startup"))
	})

	Describe("two replicas starting together on an empty install", func() {
		It("write one keys.generated for the new KEK and no keys.observed", func(ctx SpecContext) {
			fresh := fake.NewClientset()
			store := keyscli.KubeStore{Client: fresh, Namespace: ns, Name: "koinon-kek"}
			replica := func() *keyAudit {
				a := newKeyAudit(keyAud.pool, fresh, ns, "koinon-kek", "koinon-saved-work")
				a.keySet = store
				return a
			}
			m := mark(ctx)
			var wg sync.WaitGroup
			created := make(chan bool, 2)
			for range 2 {
				wg.Add(1)
				a := replica()
				go func() {
					defer GinkgoRecover()
					defer wg.Done()
					ok, err := a.ensureGenerated(ctx, "secret", "koinon-kek", func(ctx context.Context) (bool, error) {
						return store.EnsureWith(ctx, time.Now(), nil, nil)
					})
					Expect(err).NotTo(HaveOccurred())
					created <- ok
					Expect(a.observe(ctx)).To(Succeed())
				}()
			}
			wg.Wait()
			close(created)
			n := 0
			for ok := range created {
				if ok {
					n++
				}
			}
			Expect(n).To(Equal(1), "one replica made the Secret")
			Expect(count(ctx, "keys.generated", m)).To(Equal(1))
			Expect(count(ctx, "keys.observed", m)).To(BeZero())
			var actorID string
			Expect(keyAud.pool.QueryRow(ctx, `SELECT actor_id FROM install_audit_events WHERE chain_seq > $1 AND action = 'keys.generated'`, m).Scan(&actorID)).To(Succeed())
			Expect(actorID).To(Equal("koinon-api:keys"))
		})
	})
})
