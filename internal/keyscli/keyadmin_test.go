// SPDX-License-Identifier: Apache-2.0

package keyscli_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/keyscli"
	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/secretstore/secretstoretest"
)

var _ = Describe("the key-admin Job", func() {
	const (
		ns     = "koinon-test"
		secret = "koinon-kek"
	)
	var (
		kube  *fake.Clientset
		kms   *secretstoretest.FakeKMS
		admin secretstore.Wrappers
		out   *bytes.Buffer
		clock = func() time.Time { return time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC) }
	)
	job := func(create bool, add string, hasValues func(context.Context) (bool, error)) keyscli.KeyAdmin {
		return keyscli.KeyAdmin{
			Store:  keyscli.KubeStore{Client: kube, Namespace: ns, Name: secret, Wrappers: admin},
			Create: create, AddID: add, HasValues: hasValues, Out: out, Now: clock,
		}
	}
	load := func(ctx context.Context) keyscli.KeySet {
		GinkgoHelper()
		ks, err := keyscli.KubeStore{Client: kube, Namespace: ns, Name: secret, Wrappers: admin}.Load(ctx)
		Expect(err).NotTo(HaveOccurred())
		return ks
	}
	none := func(context.Context) (bool, error) { return false, nil }

	BeforeEach(func() {
		kube = fake.NewClientset()
		kms = &secretstoretest.FakeKMS{}
		var err error
		admin, err = secretstore.NewKeyAdminWrappers(kms)
		Expect(err).NotTo(HaveOccurred())
		admin = admin.WithInstallID(secretstore.StaticInstallID(secretstoretest.InstallA))
		out = &bytes.Buffer{}
	})

	It("creates the first KEK, wrapped and active, and changes nothing when it runs again", func(ctx SpecContext) {
		Expect(job(true, "", none).Run(ctx)).To(Succeed())
		Expect(out.String()).To(ContainSubstring("Created the KEK Secret koinon-kek"))
		ks := load(ctx)
		Expect(ks.Keys).To(HaveLen(1))
		Expect(ks.Stored[ks.Active].Backend).To(Equal(secretstoretest.FakeKMSBackend))
		Expect(ks.Stored[ks.Active].InstallID).To(Equal(secretstoretest.InstallA), "bound to the install (#615)")
		wraps := kms.Wraps()

		out.Reset()
		Expect(job(true, "", none).Run(ctx)).To(Succeed())
		Expect(out.String()).NotTo(ContainSubstring("Created"))
		Expect(kms.Wraps()).To(Equal(wraps), "a rerun wraps nothing")
		Expect(load(ctx).Active).To(Equal(ks.Active))
	})

	It("does not create a Secret while the database holds values, nor without Create", func(ctx SpecContext) {
		err := job(true, "", func(context.Context) (bool, error) { return true, nil }).Run(ctx)
		Expect(err).To(MatchError(keyscli.ErrKEKMissing))
		Expect(job(false, "", none).Run(ctx)).To(Succeed())
		list, err := kube.CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(list.Items).To(BeEmpty())
		Expect(kms.Wraps()).To(BeZero())
	})

	It("adds the asked-for KEK once, inactive, next to local ones", func(ctx SpecContext) {
		// An install that had local KEKs and opts in to the KMS.
		local := bytes.Repeat([]byte("l"), secretstore.KeySize) // obviously fake
		_, err := kube.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secret, Namespace: ns},
			Data: map[string][]byte{
				secretstore.ActiveFile: []byte("kek-20260101-aaaa"),
				"kek-20260101-aaaa":    []byte(base64.StdEncoding.EncodeToString(local)),
			},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		Expect(job(true, "kek-20261103-bbbb", none).Run(ctx)).To(Succeed())
		Expect(out.String()).To(ContainSubstring("Added KEK kek-20261103-bbbb, wrapped by fakekms:koinon-test"))
		ks := load(ctx)
		Expect(ks.Active).To(Equal("kek-20260101-aaaa"), "the Job never activates")
		Expect(ks.Keys["kek-20260101-aaaa"]).To(Equal(local))
		Expect(ks.Stored["kek-20261103-bbbb"].Backend).To(Equal(secretstoretest.FakeKMSBackend))
		Expect(ks.Stored["kek-20260101-aaaa"].Backend).To(Equal(secretstore.BackendLocal))

		out.Reset()
		wraps := kms.Wraps()
		Expect(job(true, "kek-20261103-bbbb", none).Run(ctx)).To(Succeed())
		Expect(out.String()).To(ContainSubstring("already in the key set"))
		Expect(kms.Wraps()).To(Equal(wraps))

		By("an ID a local KEK has is never reused")
		Expect(job(false, "kek-20260101-aaaa", none).Run(ctx)).To(MatchError(ContainSubstring("IDs are never reused")))
		Expect(job(false, "not a valid id", none).Run(ctx)).To(MatchError(ContainSubstring("not a valid KEK ID")))
	})

	It("reports a KEK that is already there even while the KMS is down, and fails to add one", func(ctx SpecContext) {
		Expect(job(true, "kek-20261103-bbbb", none).Run(ctx)).To(Succeed())
		kms.Down.Store(true)
		Expect(job(true, "kek-20261103-bbbb", none).Run(ctx)).To(Succeed())
		Expect(job(true, "kek-20261103-cccc", none).Run(ctx)).To(MatchError(secretstore.ErrUnreachable))
	})

	It("refuses to run without a KMS", func(ctx SpecContext) {
		a := job(true, "", none)
		a.Store.Wrappers = secretstore.Wrappers{}
		Expect(a.Run(ctx)).To(MatchError(keyscli.ErrLocalKeyAdmin))
		list, err := kube.CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(list.Items).To(BeEmpty())
	})

	It("never wraps a KEK without the install ID", func(ctx SpecContext) {
		unbound, err := secretstore.NewKeyAdminWrappers(kms)
		Expect(err).NotTo(HaveOccurred())
		a := job(true, "", none)
		a.Store.Wrappers = unbound
		Expect(a.Run(ctx)).To(MatchError(secretstore.ErrNoInstallID))
		Expect(kms.Wraps()).To(BeZero())
		list, err := kube.CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(list.Items).To(BeEmpty())
	})

	Describe("binding the KEKs wrapped before #615 (rewrapUnbound)", func() {
		var (
			old      *secretstoretest.FakeKMS
			local    []byte
			legacies map[string][]byte
		)
		// legacy is a KEK as a release before #615 wrapped it: format 1, bound to its ID only.
		legacy := func(ctx SpecContext, w secretstore.Wrapper, id string, kek []byte) []byte {
			GinkgoHelper()
			b, err := w.Wrap(ctx, kek, secretstoretest.LegacyKEKAAD(id))
			Expect(err).NotTo(HaveOccurred())
			v, err := secretstore.StoredKEK{Backend: w.Backend(), KeyID: w.KeyID(), Wrapped: b}.Encode()
			Expect(err).NotTo(HaveOccurred())
			return v
		}
		BeforeEach(func(ctx SpecContext) {
			old = &secretstoretest.FakeKMS{Name: "koinon-test-old"}
			var err error
			// The Job reads KEKs of an older wrapping key too, as configured for the move.
			admin, err = secretstore.NewKeyAdminWrappers(kms, old)
			Expect(err).NotTo(HaveOccurred())
			admin = admin.WithInstallID(secretstore.StaticInstallID(secretstoretest.InstallA))
			local = bytes.Repeat([]byte("l"), secretstore.KeySize) // obviously fake
			legacies = map[string][]byte{
				"kek-20261001-prim": bytes.Repeat([]byte("p"), secretstore.KeySize),
				"kek-20261001-olde": bytes.Repeat([]byte("o"), secretstore.KeySize),
			}
			_, err = kube.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: secret, Namespace: ns},
				Data: map[string][]byte{
					secretstore.ActiveFile: []byte("kek-20261001-prim"),
					"kek-20260101-aaaa":    []byte(base64.StdEncoding.EncodeToString(local)),
					"kek-20261001-prim":    legacy(ctx, kms, "kek-20261001-prim", legacies["kek-20261001-prim"]),
					"kek-20261001-olde":    legacy(ctx, old, "kek-20261001-olde", legacies["kek-20261001-olde"]),
				},
			}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		})

		It("wraps the primary key's unbound KEKs again, bound, keeps each KEK, and wraps nothing on a rerun", func(ctx SpecContext) {
			before := kms.Wraps()
			a := job(false, "", none)
			a.RewrapUnbound = true
			Expect(a.Run(ctx)).To(Succeed())
			Expect(out.String()).To(And(
				ContainSubstring("Bound kek-20261001-prim to the install ID"),
				ContainSubstring("KEK kek-20261001-olde is wrapped by fakekms:koinon-test-old, not by fakekms:koinon-test, and stays unbound"),
			))
			Expect(kms.Wraps() - before).To(BeEquivalentTo(1))
			Expect(old.Wraps()).To(BeEquivalentTo(1), "only the fixture wrapped with the older key")

			ks := load(ctx)
			Expect(ks.Active).To(Equal("kek-20261001-prim"), "nothing is activated")
			Expect(ks.Stored["kek-20261001-prim"].InstallID).To(Equal(secretstoretest.InstallA))
			Expect(ks.Stored["kek-20261001-olde"].Bound()).To(BeFalse())
			Expect(ks.Stored["kek-20260101-aaaa"].Backend).To(Equal(secretstore.BackendLocal))
			for id, k := range legacies {
				Expect(ks.Keys[id]).To(Equal(k), "KEK %s is the same key", id)
			}
			Expect(ks.Keys["kek-20260101-aaaa"]).To(Equal(local))

			By("a rerun finds nothing to bind and wraps nothing")
			out.Reset()
			wraps := kms.Wraps()
			Expect(a.Run(ctx)).To(Succeed())
			Expect(out.String()).To(ContainSubstring("No unbound KEK of fakekms:koinon-test to bind"))
			Expect(kms.Wraps()).To(Equal(wraps))

			By("the bound KEK no longer opens next to another install's database")
			other, err := secretstore.NewWrappers(kms, old)
			Expect(err).NotTo(HaveOccurred())
			other = other.WithInstallID(secretstore.StaticInstallID(secretstoretest.InstallB))
			_, err = keyscli.KubeStore{Client: kube, Namespace: ns, Name: secret, Wrappers: other}.Load(ctx)
			Expect(err).To(MatchError(secretstore.ErrOpen))
		})

		It("leaves the KEKs as they are without rewrapUnbound, and changes nothing while a KEK does not open", func(ctx SpecContext) {
			before := kms.Wraps()
			Expect(job(false, "", none).Run(ctx)).To(Succeed())
			Expect(load(ctx).Stored["kek-20261001-prim"].Bound()).To(BeFalse())

			old.Down.Store(true)
			a := job(false, "", none)
			a.RewrapUnbound = true
			Expect(a.Run(ctx)).To(MatchError(secretstore.ErrUnreachable))
			Expect(kms.Wraps()).To(Equal(before))
			old.Down.Store(false)
			Expect(load(ctx).Stored["kek-20261001-prim"].Bound()).To(BeFalse())
		})
	})
})
