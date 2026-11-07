// SPDX-License-Identifier: Apache-2.0

package kekhealth_test

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/kekhealth"
	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/secretstore/secretstoretest"
)

func TestKEKHealth(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "KEK health Suite")
}

// memDB is the two queries the recorder uses, over a map: the first fingerprint of an ID stays.
type memDB struct {
	store.Querier
	rows map[string]store.KekFingerprint
}

func (m *memDB) RecordKEKFingerprint(_ context.Context, a store.RecordKEKFingerprintParams) (store.KekFingerprint, error) {
	r, ok := m.rows[a.KekID]
	if !ok {
		r = store.KekFingerprint{KekID: a.KekID, Fingerprint: a.Fingerprint}
	}
	r.Backend, r.KeyID = a.Backend, a.KeyID
	if a.ActivatedAt.Valid {
		r.ActivatedAt = a.ActivatedAt
	}
	m.rows[a.KekID] = r
	return r, nil
}

func (m *memDB) ListKEKFingerprints(context.Context) ([]store.KekFingerprint, error) {
	var out []store.KekFingerprint
	for _, r := range m.rows {
		out = append(out, r)
	}
	return out, nil
}

var _ = Describe("the KEK monitor", func() {
	var (
		logs *bytes.Buffer
		log  *slog.Logger
		m    *kekhealth.Metrics
		db   *memDB
	)
	key := func(b byte) []byte { return bytes.Repeat([]byte{b}, secretstore.KeySize) }
	gauge := func(backend string) float64 {
		return testutil.ToFloat64(m.ActiveReachable.WithLabelValues(backend))
	}
	loadedLocal := func(k []byte) secretstore.Loaded {
		return secretstore.NewLoaded(
			map[string]secretstore.StoredKEK{"kek-1": {Backend: secretstore.BackendLocal, Wrapped: k}},
			"kek-1", map[string][]byte{"kek-1": k}, nil)
	}

	BeforeEach(func() {
		logs = &bytes.Buffer{}
		log = slog.New(slog.NewTextHandler(logs, nil))
		m = kekhealth.NewMetrics(nil)
		db = &memDB{rows: map[string]store.KekFingerprint{}}
	})

	It("a local-only install reports 1 and stays quiet", func(ctx SpecContext) {
		mon := &kekhealth.Monitor{Metrics: m, Recorder: &kekhealth.Recorder{Q: db}, Log: log}
		mon.Observe(ctx, loadedLocal(key(1)))
		mon.Observe(ctx, loadedLocal(key(1)))
		Expect(gauge("local")).To(Equal(1.0))
		Expect(logs.String()).To(BeEmpty())
		Expect(db.rows).To(HaveKey("kek-1"))
		Expect(db.rows["kek-1"].Fingerprint).To(Equal(secretstore.Fingerprint(key(1))))
		Expect(testutil.ToFloat64(m.Unbound)).To(Equal(0.0), "a local KEK is never counted as unbound")
	})

	It("counts the KMS KEKs wrapped before the install binding on every load, and warns while there are any (old issue 615)", func(ctx SpecContext) {
		kms := &secretstoretest.FakeKMS{}
		ws, err := secretstore.NewWrappers(secretstoretest.BoundOnlyUnset, kms)
		Expect(err).NotTo(HaveOccurred())
		mon := &kekhealth.Monitor{Wrappers: ws, Metrics: m, Log: log}
		kmsKEK := func(install string) secretstore.StoredKEK {
			return secretstore.StoredKEK{Backend: secretstoretest.FakeKMSBackend, KeyID: kms.KeyID(), Wrapped: []byte("x"), InstallID: install}
		}
		load := func(stored map[string]secretstore.StoredKEK) secretstore.Loaded {
			return secretstore.Loaded{Stored: stored, Active: "kek-2"}
		}

		By("two KMS KEKs of format 1 and a local one")
		mon.Observe(ctx, load(map[string]secretstore.StoredKEK{
			"kek-0": {Backend: secretstore.BackendLocal, Wrapped: key(1)},
			"kek-1": kmsKEK(""),
			"kek-2": kmsKEK(""),
		}))
		Expect(testutil.ToFloat64(m.Unbound)).To(Equal(2.0))
		Expect(logs.String()).To(And(ContainSubstring("not bound to the install ID"), ContainSubstring("unbound=2"),
			ContainSubstring("--rewrap-unbound")))

		By("one bound after the key-admin Job ran")
		mon.Observe(ctx, load(map[string]secretstore.StoredKEK{"kek-1": kmsKEK(""), "kek-2": kmsKEK(secretstoretest.InstallA)}))
		Expect(testutil.ToFloat64(m.Unbound)).To(Equal(1.0), "set on every load")

		By("all bound")
		logs.Reset()
		mon.Observe(ctx, load(map[string]secretstore.StoredKEK{"kek-1": kmsKEK(secretstoretest.InstallA), "kek-2": kmsKEK(secretstoretest.InstallA)}))
		Expect(testutil.ToFloat64(m.Unbound)).To(Equal(0.0))
		Expect(logs.String()).To(BeEmpty())

		By("an unbound KEK coming back (an old KEK Secret restored) warns again at once")
		mon.Observe(ctx, load(map[string]secretstore.StoredKEK{"kek-1": kmsKEK(""), "kek-2": kmsKEK(secretstoretest.InstallA)}))
		Expect(testutil.ToFloat64(m.Unbound)).To(Equal(1.0))
		Expect(logs.String()).To(ContainSubstring("unbound=1"))
	})

	It("does not advise a rewrap when the bound-only policy refused the unbound KEKs (old issue 615)", func(ctx SpecContext) {
		kms := &secretstoretest.FakeKMS{}
		ws, err := secretstore.NewWrappers(secretstore.StaticBoundOnly(secretstoretest.BoundOnlySet), kms)
		Expect(err).NotTo(HaveOccurred())
		mon := &kekhealth.Monitor{Wrappers: ws, Metrics: m, Log: log}
		stored := map[string]secretstore.StoredKEK{
			"kek-1": {Backend: secretstoretest.FakeKMSBackend, KeyID: kms.KeyID(), Wrapped: []byte("x")},
		}
		_, loadErr := ws.UnwrapAll(ctx, stored)
		Expect(loadErr).To(MatchError(secretstore.ErrBoundOnly))
		mon.Observe(ctx, secretstore.Loaded{Stored: stored, Active: "kek-1", Err: loadErr})
		Expect(testutil.ToFloat64(m.Unbound)).To(Equal(1.0))
		Expect(logs.String()).To(And(ContainSubstring("refused by the bound-only policy"),
			ContainSubstring("restore a KEK Secret taken after the KEKs were bound"),
			ContainSubstring("rerun the key-admin Job"), Not(ContainSubstring("bind them with the key-admin Job"))))
	})

	It("follows the wrapping key of the active KEK and keeps one series", func(ctx SpecContext) {
		kms := &secretstoretest.FakeKMS{}
		ws, err := secretstore.NewWrappers(secretstoretest.BoundOnlyUnset, kms)
		Expect(err).NotTo(HaveOccurred())
		mon := &kekhealth.Monitor{Wrappers: ws, Metrics: m, Log: log}
		wrapped := secretstore.StoredKEK{Backend: secretstoretest.FakeKMSBackend, KeyID: kms.KeyID(), Wrapped: []byte("x")}
		l := secretstore.Loaded{Stored: map[string]secretstore.StoredKEK{"kek-1": wrapped}, Active: "kek-1"}

		mon.Observe(ctx, l)
		Expect(gauge(secretstoretest.FakeKMSBackend)).To(Equal(1.0))

		kms.Down.Store(true)
		mon.Observe(ctx, l)
		Expect(gauge(secretstoretest.FakeKMSBackend)).To(Equal(0.0))
		Expect(logs.String()).To(ContainSubstring("unreachable"))

		By("a failed load of another KEK does not blame the active one")
		kms.Down.Store(false)
		l.Err = secretstore.ErrUnreachable
		mon.Observe(ctx, l)
		Expect(gauge(secretstoretest.FakeKMSBackend)).To(Equal(1.0))

		By("a local active KEK replaces the series")
		mon.Observe(ctx, loadedLocal(key(1)))
		Expect(testutil.CollectAndCount(m.ActiveReachable)).To(Equal(1))
		Expect(gauge("local")).To(Equal(1.0))
		Expect(logs.String()).To(ContainSubstring("a KMS is configured"))
	})

	It("warns when a KEK differs from the fingerprint the database recorded, and never replaces it", func(ctx SpecContext) {
		mon := &kekhealth.Monitor{Metrics: m, Recorder: &kekhealth.Recorder{Q: db}, Log: log}
		mon.Observe(ctx, loadedLocal(key(1)))
		mon.Observe(ctx, loadedLocal(key(2)))
		Expect(logs.String()).To(And(ContainSubstring("differs from the one the database recorded"), ContainSubstring("kek=kek-1")))
		Expect(db.rows["kek-1"].Fingerprint).To(Equal(secretstore.Fingerprint(key(1))))
	})

	It("is told by the directory load and reload, also when the reload cannot unwrap", func(ctx SpecContext) {
		kms := &secretstoretest.FakeKMS{}
		admin, err := secretstore.NewKeyAdminWrappers(secretstoretest.BoundOnlyUnset, kms)
		Expect(err).NotTo(HaveOccurred())
		install := secretstore.StaticInstallID(secretstoretest.InstallA)
		w, err := admin.WithInstallID(install).Wrap(ctx, "kek-1", key(3))
		Expect(err).NotTo(HaveOccurred())
		enc, err := w.Encode()
		Expect(err).NotTo(HaveOccurred())
		dir := GinkgoT().TempDir()
		Expect(os.WriteFile(filepath.Join(dir, "kek-1"), enc, 0o600)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, "active"), []byte("kek-1"), 0o600)).To(Succeed())

		service, err := secretstore.NewWrappers(secretstoretest.BoundOnlyUnset, kms)
		Expect(err).NotTo(HaveOccurred())
		service = service.WithInstallID(install)
		mon := &kekhealth.Monitor{Wrappers: service, Metrics: m, Recorder: &kekhealth.Recorder{Q: db}, Log: log}
		ring, err := secretstore.LoadDir(ctx, dir, service, mon.Observer())
		Expect(err).NotTo(HaveOccurred())
		Expect(gauge(secretstoretest.FakeKMSBackend)).To(Equal(1.0))
		Expect(db.rows["kek-1"].Backend).To(Equal(secretstoretest.FakeKMSBackend))

		kms.Down.Store(true)
		Expect(ring.Reload(ctx, dir, service, mon.Observer())).To(MatchError(secretstore.ErrUnreachable))
		Expect(gauge(secretstoretest.FakeKMSBackend)).To(Equal(0.0))
	})

	It("registers its gauges once", func() {
		reg := prometheus.NewRegistry()
		rm := kekhealth.NewMetrics(reg)
		Expect(func() { kekhealth.NewMetrics(reg) }).To(Panic(), "a second registration collides")
		rm.ActiveReachable.WithLabelValues("local").Set(1)
		mfs, err := reg.Gather()
		Expect(err).NotTo(HaveOccurred())
		var names []string
		for _, mf := range mfs {
			names = append(names, mf.GetName())
		}
		Expect(names).To(ConsistOf("koinon_kek_active_reachable", "koinon_kek_unbound"))

		By("a registry made later gets the same collectors")
		late := prometheus.NewRegistry()
		late.MustRegister(kekhealth.NewMetrics(nil).Collectors()...)
		got, err := late.Gather()
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(HaveLen(1), "the unlabelled gauge is gathered; a vector without series is not")
		Expect(got[0].GetName()).To(Equal("koinon_kek_unbound"))
	})

	It("warns about an unreachable key once, not on every retry", func(ctx SpecContext) {
		kms := &secretstoretest.FakeKMS{}
		ws, err := secretstore.NewWrappers(secretstoretest.BoundOnlyUnset, kms)
		Expect(err).NotTo(HaveOccurred())
		mon := &kekhealth.Monitor{Wrappers: ws, Metrics: m, Log: log}
		wrapped := secretstore.StoredKEK{Backend: secretstoretest.FakeKMSBackend, KeyID: kms.KeyID(), Wrapped: []byte("x")}
		l := secretstore.Loaded{Stored: map[string]secretstore.StoredKEK{"kek-1": wrapped}, Active: "kek-1"}
		kms.Down.Store(true)
		for range 3 {
			mon.Observe(ctx, l)
		}
		Expect(strings.Count(logs.String(), "the key-wrapping key of the active")).To(Equal(1))
	})
})
