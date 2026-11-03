// SPDX-License-Identifier: Apache-2.0

package kekhealth_test

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
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
	})

	It("follows the wrapping key of the active KEK and keeps one series", func(ctx SpecContext) {
		kms := &secretstoretest.FakeKMS{}
		ws, err := secretstore.NewWrappers(kms)
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

		By("a load that already failed as unreachable is 0 without a second probe")
		kms.Down.Store(false)
		l.Err = secretstore.ErrUnreachable
		mon.Observe(ctx, l)
		Expect(gauge(secretstoretest.FakeKMSBackend)).To(Equal(0.0))

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
		admin, err := secretstore.NewKeyAdminWrappers(kms)
		Expect(err).NotTo(HaveOccurred())
		w, err := admin.Wrap(ctx, "kek-1", key(3))
		Expect(err).NotTo(HaveOccurred())
		enc, err := w.Encode()
		Expect(err).NotTo(HaveOccurred())
		dir := GinkgoT().TempDir()
		Expect(os.WriteFile(filepath.Join(dir, "kek-1"), enc, 0o600)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, "active"), []byte("kek-1"), 0o600)).To(Succeed())

		service, err := secretstore.NewWrappers(kms)
		Expect(err).NotTo(HaveOccurred())
		mon := &kekhealth.Monitor{Wrappers: service, Metrics: m, Recorder: &kekhealth.Recorder{Q: db}, Log: log}
		ring, err := secretstore.LoadDir(ctx, dir, service, mon.Observer())
		Expect(err).NotTo(HaveOccurred())
		Expect(gauge(secretstoretest.FakeKMSBackend)).To(Equal(1.0))
		Expect(db.rows["kek-1"].Backend).To(Equal(secretstoretest.FakeKMSBackend))

		kms.Down.Store(true)
		Expect(ring.Reload(ctx, dir, service, mon.Observer())).To(MatchError(secretstore.ErrUnreachable))
		Expect(gauge(secretstoretest.FakeKMSBackend)).To(Equal(0.0))
	})

	It("registers its gauge once", func() {
		reg := prometheus.NewRegistry()
		kekhealth.NewMetrics(reg)
		mfs, err := reg.Gather()
		Expect(err).NotTo(HaveOccurred())
		Expect(mfs).To(BeEmpty(), "a vec without series has no samples")
	})
})
