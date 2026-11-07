// SPDX-License-Identifier: Apache-2.0

package keyscli_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/keyscli"
	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/secretstore/secretstoretest"
)

// memMarker is the bound-only marker in memory: set once, never cleared, as installid.Marker.
type memMarker struct {
	mu   sync.Mutex
	b    secretstore.BoundOnly
	fail error
}

func (m *memMarker) BoundOnly(context.Context) (secretstore.BoundOnly, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.b, nil
}

func (m *memMarker) SetBoundOnly(_ context.Context, how string) (secretstore.BoundOnly, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return secretstore.BoundOnly{}, m.fail
	}
	if !m.b.Set {
		m.b = secretstore.BoundOnly{Set: true, At: time.Date(2026, 11, 3, 13, 0, 0, 0, time.UTC), How: how}
	}
	return m.b, nil
}

func (m *memMarker) source() secretstore.BoundOnlySource { return m.BoundOnly }

var _ = Describe("the key-admin Job", func() {
	const (
		ns     = "koinon-test"
		secret = "koinon-kek"
	)
	var (
		kube   *fake.Clientset
		kms    *secretstoretest.FakeKMS
		admin  secretstore.Wrappers
		out    *bytes.Buffer
		marker *memMarker
		clock  = func() time.Time { return time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC) }
	)
	job := func(create bool, add string, hasValues func(context.Context) (bool, error)) keyscli.KeyAdmin {
		return keyscli.KeyAdmin{
			Store:  keyscli.KubeStore{Client: kube, Namespace: ns, Name: secret, Wrappers: admin},
			Create: create, AddID: add, HasValues: hasValues, Out: out, Now: clock, Marker: marker,
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
		marker = &memMarker{}
		admin, err = secretstore.NewKeyAdminWrappers(marker.source(), kms)
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
		Expect(ks.Stored[ks.Active].InstallID).To(Equal(secretstoretest.InstallA), "bound to the install (koinon-private#615)")
		Expect(marker.b.Set).To(BeTrue(), "a fresh install is bound-only from its first KEK")
		Expect(marker.b.How).To(Equal(secretstore.BoundOnlyFirstInstall))
		Expect(out.String()).To(ContainSubstring("The bound-only marker of this install is set at 2026-11-03T13:00:00Z, when the first KEK of this install was created"))
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
		Expect(marker.b.Set).To(BeFalse(), "not a first install")
	})

	It("creates no KEK when the marker cannot be set, and sets it on the next run", func(ctx SpecContext) {
		marker.fail = errors.New("koinon_test database down")
		Expect(job(true, "", none).Run(ctx)).To(MatchError(marker.fail))
		list, err := kube.CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(list.Items).To(BeEmpty())
		Expect(kms.Wraps()).To(BeZero())

		marker.fail = nil
		Expect(job(true, "", none).Run(ctx)).To(Succeed())
		Expect(marker.b.Set).To(BeTrue())
		Expect(load(ctx).Keys).To(HaveLen(1))
	})

	It("needs the marker to create or bind KEKs", func(ctx SpecContext) {
		a := job(true, "", none)
		a.Marker = nil
		Expect(a.Run(ctx)).To(MatchError(ContainSubstring("bound-only marker")))
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
		Expect(marker.b.Set).To(BeFalse(), "an existing install is not a first install")

		out.Reset()
		wraps := kms.Wraps()
		Expect(job(true, "kek-20261103-bbbb", none).Run(ctx)).To(Succeed())
		Expect(out.String()).To(ContainSubstring("already in the key set"))
		Expect(kms.Wraps()).To(Equal(wraps))

		By("an ID a local KEK has is never reused")
		Expect(job(false, "kek-20260101-aaaa", none).Run(ctx)).To(MatchError(ContainSubstring("IDs are never reused")))
		Expect(job(false, "not a valid id", none).Run(ctx)).To(MatchError(ContainSubstring("not a valid KEK ID")))
	})

	Describe("recovering from a lost active KEK (activateIfNoActive, issue #255)", func() {
		const (
			lost     = "kek-20260101-aaaa"
			survivor = "kek-20260201-cccc"
			fresh    = "kek-20261103-bbbb"
		)
		fakeKey := func(b byte) []byte { return bytes.Repeat([]byte{b}, secretstore.KeySize) } // obviously fake
		// seed makes the KEK Secret as the operator leaves it: the lost KEK's entry removed, `active` not.
		seed := func(ctx context.Context, active string, kept ...string) {
			GinkgoHelper()
			data := map[string][]byte{}
			if active != "" {
				data[secretstore.ActiveFile] = []byte(active)
			}
			for _, id := range kept {
				data[id] = secretstore.EncodeKey(fakeKey('s'))
			}
			_, err := kube.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: secret, Namespace: ns}, Data: data,
			}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		sealed := map[string]bool{} // KEK IDs that database envelopes still carry
		recover := func(add string) keyscli.KeyAdmin {
			a := job(true, add, none)
			a.ActivateIfNoActive = true
			a.UsedID = func(_ context.Context, id string) (bool, error) { return sealed[id], nil }
			return a
		}

		It("adds the KEK and makes it active when the active entry names a KEK the Secret does not hold", func(ctx SpecContext) {
			seed(ctx, lost)
			By("nothing loads, and the Job without the flag cannot add either")
			_, err := keyscli.KubeStore{Client: kube, Namespace: ns, Name: secret, Wrappers: admin}.Load(ctx)
			Expect(err).To(MatchError(ContainSubstring("is not among the keys")))
			Expect(job(true, fresh, none).Run(ctx)).To(MatchError(ContainSubstring("is not among the keys")))

			By("the recovery adds the KEK, wrapped by the KMS, and activates it in one write")
			Expect(recover(fresh).Run(ctx)).To(Succeed())
			Expect(out.String()).To(ContainSubstring("made it active (the Secret named " + lost + ", which it does not hold)"))
			ks := load(ctx)
			Expect(ks.Active).To(Equal(fresh))
			Expect(ks.Keys).To(HaveLen(1))
			Expect(ks.Stored[fresh].Backend).To(Equal(secretstoretest.FakeKMSBackend))
			Expect(ks.Stored[fresh].InstallID).To(Equal(secretstoretest.InstallA))
			Expect(ks.ActivatedAt).To(Equal(clock()))

			By("a rerun changes nothing")
			wraps := kms.Wraps()
			out.Reset()
			Expect(recover(fresh).Run(ctx)).To(Succeed())
			Expect(out.String()).To(ContainSubstring("nothing to recover"))
			Expect(kms.Wraps()).To(Equal(wraps))
			Expect(load(ctx).Active).To(Equal(fresh))
		})

		It("keeps the KEKs the Secret still holds, and also recovers a Secret with no active entry", func(ctx SpecContext) {
			seed(ctx, "", survivor)
			Expect(recover(fresh).Run(ctx)).To(Succeed())
			ks := load(ctx)
			Expect(ks.Active).To(Equal(fresh))
			Expect(ks.Keys).To(HaveKey(survivor))
			Expect(ks.Keys[survivor]).To(Equal(fakeKey('s')), "a surviving KEK is not changed")
		})

		It("never replaces a working active KEK", func(ctx SpecContext) {
			seed(ctx, survivor, survivor)
			Expect(recover(fresh).Run(ctx)).To(Succeed())
			Expect(out.String()).To(ContainSubstring("The active KEK " + survivor + " is in the key set; nothing to recover."))
			ks := load(ctx)
			Expect(ks.Active).To(Equal(survivor))
			Expect(ks.Stored).To(HaveKey(fresh), "the KEK is added, inactive, as without the flag")
		})

		It("refuses the ID of the lost active KEK and leaves the Secret unchanged", func(ctx SpecContext) {
			seed(ctx, lost, survivor)
			Expect(recover(lost).Run(ctx)).To(MatchError(ContainSubstring("is the lost active KEK")))
			sec, err := kube.CoreV1().Secrets(ns).Get(ctx, secret, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(sec.Data).To(HaveLen(2))
			Expect(string(sec.Data[secretstore.ActiveFile])).To(Equal(lost))
			Expect(sec.Data).NotTo(HaveKey(lost))
			Expect(kms.Wraps()).To(BeZero(), "nothing was wrapped")
		})

		It("refuses the ID of a removed KEK that database envelopes still carry, and changes nothing", func(ctx SpecContext) {
			const removed = "kek-20251201-eeee"
			seed(ctx, lost, survivor)
			sealed[removed] = true
			Expect(recover(removed).Run(ctx)).To(MatchError(ContainSubstring("IDs are never reused")))
			sec, err := kube.CoreV1().Secrets(ns).Get(ctx, secret, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(sec.Data).To(HaveLen(2))
			Expect(string(sec.Data[secretstore.ActiveFile])).To(Equal(lost))
			Expect(sec.Data).NotTo(HaveKey(removed))
			Expect(kms.Wraps()).To(BeZero(), "nothing was wrapped")
			Expect(recover(fresh).Run(ctx)).To(Succeed(), "an unused ID still recovers")
		})

		It("stops when the usage check fails, and needs the check at all", func(ctx SpecContext) {
			seed(ctx, lost, survivor)
			a := recover(fresh)
			a.UsedID = func(context.Context, string) (bool, error) { return false, errors.New("database down") }
			Expect(a.Run(ctx)).To(MatchError(ContainSubstring("database down")))
			a.UsedID = nil
			Expect(a.Run(ctx)).To(MatchError(ContainSubstring("check that the new KEK ID is unused")))
			Expect(kms.Wraps()).To(BeZero(), "nothing was wrapped")
		})

		It("refuses a recovery without an ID before it creates or changes anything", func(ctx SpecContext) {
			By("no Secret: the Job does not create one")
			Expect(recover("").Run(ctx)).To(MatchError(ContainSubstring("needs the ID of the KEK to add")))
			list, err := kube.CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(list.Items).To(BeEmpty())
			Expect(kms.Wraps()).To(BeZero(), "nothing was wrapped")
			Expect(marker.b.Set).To(BeFalse(), "the bound-only marker is not set")

			By("a Secret: it is left as it was")
			seed(ctx, lost, survivor)
			Expect(recover("").Run(ctx)).To(MatchError(ContainSubstring("needs the ID of the KEK to add")))
			sec, err := kube.CoreV1().Secrets(ns).Get(ctx, secret, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(sec.Data).To(HaveLen(2))
			Expect(string(sec.Data[secretstore.ActiveFile])).To(Equal(lost))
			Expect(kms.Wraps()).To(BeZero())
		})

		It("returns a missing Secret unchanged, without the hint about removing entries", func(ctx SpecContext) {
			a := recover(fresh)
			a.Create = false
			err := a.Run(ctx)
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the API error is still reachable")
			Expect(err.Error()).NotTo(ContainSubstring("remove the entries"))
			Expect(kms.Wraps()).To(BeZero())
		})

		It("needs the ID of the KEK to add, refuses an ID in use, and stops while a held KEK does not unwrap", func(ctx SpecContext) {
			seed(ctx, lost, survivor)
			Expect(recover(survivor).Run(ctx)).To(MatchError(ContainSubstring("IDs are never reused")))

			sec, err := kube.CoreV1().Secrets(ns).Get(ctx, secret, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			dead := secretstore.StoredKEK{Backend: secretstoretest.FakeKMSBackend, KeyID: "koinon-test", Wrapped: []byte("not wrapped by anything")}
			raw, err := dead.Encode()
			Expect(err).NotTo(HaveOccurred())
			sec.Data["kek-20260301-dddd"] = raw
			_, err = kube.CoreV1().Secrets(ns).Update(ctx, sec, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(recover(fresh).Run(ctx)).To(MatchError(ContainSubstring("remove the entries of the KEKs that cannot be unwrapped")))
			Expect(kms.Wraps()).To(BeZero(), "nothing was wrapped")
		})
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
		unbound, err := secretstore.NewKeyAdminWrappers(secretstoretest.BoundOnlyUnset, kms)
		Expect(err).NotTo(HaveOccurred())
		a := job(true, "", none)
		a.Store.Wrappers = unbound
		Expect(a.Run(ctx)).To(MatchError(secretstore.ErrNoInstallID))
		Expect(kms.Wraps()).To(BeZero())
		list, err := kube.CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(list.Items).To(BeEmpty())
	})

	Describe("binding the KEKs wrapped before koinon-private#615 (rewrapUnbound)", func() {
		var (
			old      *secretstoretest.FakeKMS
			local    []byte
			legacies map[string][]byte
		)
		// legacy is a KEK as a release before koinon-private#615 wrapped it: format 1, bound to its ID only.
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
			admin, err = secretstore.NewKeyAdminWrappers(marker.source(), kms, old)
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
				ContainSubstring("a rollback below koinon-private#615 no longer works"),
				ContainSubstring("kek-20261001-olde stay unbound, so the bound-only marker is not set"),
			))
			Expect(marker.b.Set).To(BeFalse(), "an unbound KEK remains")
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

			Expect(marker.b.Set).To(BeFalse())

			By("the bound KEK no longer opens next to another install's database")
			other, err := secretstore.NewWrappers(secretstoretest.BoundOnlyUnset, kms, old)
			Expect(err).NotTo(HaveOccurred())
			other = other.WithInstallID(secretstore.StaticInstallID(secretstoretest.InstallB))
			_, err = keyscli.KubeStore{Client: kube, Namespace: ns, Name: secret, Wrappers: other}.Load(ctx)
			Expect(err).To(MatchError(secretstore.ErrOpen))
		})

		It("sets the bound-only marker when it binds the last unbound KEK, and then an old backup does not open", func(ctx SpecContext) {
			backup, err := kube.CoreV1().Secrets(ns).Get(ctx, secret, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			backup = backup.DeepCopy()
			// The KEK of the old wrapping key is rotated out (keys retire), as the Job told the operator.
			s := backup.DeepCopy()
			delete(s.Data, "kek-20261001-olde")
			_, err = kube.CoreV1().Secrets(ns).Update(ctx, s, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())

			a := job(false, "", none)
			a.RewrapUnbound = true
			Expect(a.Run(ctx)).To(Succeed())
			Expect(marker.b.Set).To(BeTrue())
			Expect(marker.b.How).To(Equal(secretstore.BoundOnlyRewrap))
			Expect(out.String()).To(And(ContainSubstring("Every KEK is bound. The bound-only marker of this install is set"),
				ContainSubstring("a rollback below koinon-private#615 no longer works"), ContainSubstring("rotate the KEKs as well")))
			Expect(load(ctx).Keys["kek-20261001-prim"]).To(Equal(legacies["kek-20261001-prim"]))

			By("a rerun keeps it and wraps nothing")
			out.Reset()
			wraps := kms.Wraps()
			Expect(a.Run(ctx)).To(Succeed())
			Expect(kms.Wraps()).To(Equal(wraps))
			Expect(out.String()).To(ContainSubstring("The bound-only marker of this install is set"))

			By("the KEK Secret backup from before, which holds the same KEKs unbound, no longer opens")
			backup.ResourceVersion = ""
			Expect(kube.CoreV1().Secrets(ns).Delete(ctx, secret, metav1.DeleteOptions{})).To(Succeed())
			_, err = kube.CoreV1().Secrets(ns).Create(ctx, backup, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			service, err := secretstore.NewWrappers(marker.source(), kms, old)
			Expect(err).NotTo(HaveOccurred())
			service = service.WithInstallID(secretstore.StaticInstallID(secretstoretest.InstallA))
			_, err = keyscli.KubeStore{Client: kube, Namespace: ns, Name: secret, Wrappers: service}.Load(ctx)
			Expect(err).To(And(MatchError(secretstore.ErrBoundOnly), MatchError(secretstore.ErrOpen)))
			Expect(a.Run(ctx)).To(MatchError(secretstore.ErrBoundOnly), "the Job cannot bind it again either: rotate")
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
