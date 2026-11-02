// SPDX-License-Identifier: Apache-2.0

package secretstore_test

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/secretstore/secretstoretest"
)

var _ = Describe("Key wrapping", func() {
	Describe("the fake KMS", func() {
		secretstoretest.WrapperContract(func() secretstore.Wrapper { return &secretstoretest.FakeKMS{} })
	})

	It("stores a local KEK exactly as before, so old Secrets and backups read unchanged", func(ctx SpecContext) {
		k := key()
		var local secretstore.Wrappers
		s, err := local.Wrap(ctx, "k1", k)
		Expect(err).NotTo(HaveOccurred())
		Expect(s.Backend).To(Equal(secretstore.BackendLocal))
		v, err := s.Encode()
		Expect(err).NotTo(HaveOccurred())
		Expect(v).To(Equal(secretstore.EncodeKey(k)))

		stored, active, err := secretstore.ParseKeySet(map[string][]byte{"k1": v, "active": []byte("k1")})
		Expect(err).NotTo(HaveOccurred())
		Expect(active).To(Equal("k1"))
		keys, err := local.UnwrapAll(ctx, stored)
		Expect(err).NotTo(HaveOccurred())
		Expect(keys["k1"]).To(Equal(k))
	})

	It("loads a key set that mixes local and KMS-wrapped KEKs", func(ctx SpecContext) {
		kms := &secretstoretest.FakeKMS{}
		ws, err := secretstore.NewWrappers(kms)
		Expect(err).NotTo(HaveOccurred())
		oldKey, newKey := key(), key()
		s, err := ws.Wrap(ctx, "k2", newKey)
		Expect(err).NotTo(HaveOccurred())
		Expect(s.String()).To(Equal("fakekms:koinon-test"))
		v, err := s.Encode()
		Expect(err).NotTo(HaveOccurred())
		Expect(string(v)).To(HavePrefix("{"))
		Expect(bytes.Contains(v, newKey)).To(BeFalse())

		dir := GinkgoT().TempDir()
		for name, b := range map[string][]byte{"k1": secretstore.EncodeKey(oldKey), "k2": v, "active": []byte("k2")} {
			Expect(os.WriteFile(filepath.Join(dir, name), b, 0o600)).To(Succeed())
		}
		ring, err := secretstore.LoadDir(ctx, dir, ws)
		Expect(err).NotTo(HaveOccurred())
		Expect(ring.Active()).To(Equal("k2"))
		Expect(ring.IDs()).To(ConsistOf("k1", "k2"))
		env, err := ring.Seal(bind, []byte("sk-x"))
		Expect(err).NotTo(HaveOccurred())

		// The same KEK, opened from the stored form by a keyring of the raw key.
		direct, err := secretstore.NewKeyring(map[string][]byte{"k2": newKey}, "k2")
		Expect(err).NotTo(HaveOccurred())
		Expect(direct.Open(bind, env)).To(Equal([]byte("sk-x")))

		// Without the adapter, the wrapped KEK cannot be read and nothing loads.
		_, err = secretstore.LoadDir(ctx, dir, secretstore.Wrappers{})
		Expect(err).To(MatchError(ContainSubstring("no adapter is configured for the key-wrapping key fakekms:koinon-test")))
	})

	It("binds a wrapped KEK to its ID", func(ctx SpecContext) {
		ws, _ := secretstore.NewWrappers(&secretstoretest.FakeKMS{})
		s, err := ws.Wrap(ctx, "k1", key())
		Expect(err).NotTo(HaveOccurred())
		_, err = ws.Unwrap(ctx, "k2", s)
		Expect(err).To(MatchError(secretstore.ErrOpen))
	})

	It("tells an unreachable KMS from a wrong key, and a failed reload keeps the old keys", func(ctx SpecContext) {
		kms := &secretstoretest.FakeKMS{}
		ws, _ := secretstore.NewWrappers(kms)
		s, err := ws.Wrap(ctx, "k1", key())
		Expect(err).NotTo(HaveOccurred())
		v, _ := s.Encode()
		dir := GinkgoT().TempDir()
		Expect(os.WriteFile(filepath.Join(dir, "k1"), v, 0o600)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, "active"), []byte("k1"), 0o600)).To(Succeed())
		ring, err := secretstore.LoadDir(ctx, dir, ws)
		Expect(err).NotTo(HaveOccurred())

		kms.Down.Store(true)
		Expect(ws.Probe(ctx, s)).To(MatchError(secretstore.ErrUnreachable))
		err = ring.Reload(ctx, dir, ws)
		Expect(err).To(MatchError(secretstore.ErrUnreachable))
		Expect(ring.Active()).To(Equal("k1"))
		_, err = ring.Seal(bind, []byte("x"))
		Expect(err).NotTo(HaveOccurred()) // values are sealed locally; the KMS is needed to load keys only
	})

	It("opens a key set with a decrypt-only identity; only a new KEK needs encrypt", func(ctx SpecContext) {
		kms := &secretstoretest.FakeKMS{}
		ws, _ := secretstore.NewWrappers(kms)
		s, err := ws.Wrap(ctx, "k1", key())
		Expect(err).NotTo(HaveOccurred())
		kms.DecryptOnly.Store(true)
		_, err = ws.UnwrapAll(ctx, map[string]secretstore.StoredKEK{"k1": s})
		Expect(err).NotTo(HaveOccurred())
		Expect(ws.Probe(ctx, s)).To(Succeed())
		_, err = ws.Wrap(ctx, "k2", key())
		Expect(err).To(MatchError(secretstore.ErrUnreachable))
	})

	It("never prints or logs key material", func(ctx SpecContext) {
		k := key()
		var local secretstore.Wrappers
		s, _ := local.Wrap(ctx, "k1", k)
		var logged bytes.Buffer
		slog.New(slog.NewTextHandler(&logged, nil)).Info("loaded", "kek", s)
		for _, out := range []string{fmt.Sprint(s), fmt.Sprintf("%v %+v %#v", s, s, s), logged.String()} {
			Expect(out).NotTo(ContainSubstring(string(secretstore.EncodeKey(k))))
			Expect(out).NotTo(ContainSubstring(fmt.Sprintf("%x", k)))
		}
		Expect(fmt.Sprintf("%#v", s)).To(Equal("secretstore.StoredKEK{local}"))
	})

	It("rejects stored forms it cannot read without quoting them", func() {
		for _, v := range []string{
			`{"fmt":2,"backend":"fakekms","key":"x","wrapped":"aGVsbG8="}`,
			`{"fmt":1,"backend":"local","wrapped":"aGVsbG8="}`,
			`{"fmt":1,"backend":"Bad Name","wrapped":"aGVsbG8="}`,
			`{"fmt":1,"backend":"fakekms","key":"x"}`,
			`{not json hunter2`,
		} {
			_, _, err := secretstore.ParseKeySet(map[string][]byte{"k1": []byte(v), "active": []byte("k1")})
			Expect(err).To(HaveOccurred(), v)
			Expect(err.Error()).NotTo(ContainSubstring("hunter2"))
			Expect(err.Error()).NotTo(ContainSubstring("aGVsbG8"))
		}
	})

	It("refuses a wrapping key configured twice", func() {
		_, err := secretstore.NewWrappers(&secretstoretest.FakeKMS{}, &secretstoretest.FakeKMS{})
		Expect(err).To(MatchError(ContainSubstring("configured twice")))
		_, err = secretstore.NewWrappers(nil)
		Expect(err).To(HaveOccurred())
	})

	It("fingerprints a KEK stably without revealing it", func() {
		k := bytes.Repeat([]byte("k"), secretstore.KeySize)
		fp := secretstore.Fingerprint(k)
		Expect(fp).To(MatchRegexp(`^[0-9a-f]{32}$`))
		Expect(secretstore.Fingerprint(bytes.Clone(k))).To(Equal(fp))
		Expect(secretstore.Fingerprint(key())).NotTo(Equal(fp))
		Expect(strings.Contains(fp, fmt.Sprintf("%x", k[:4]))).To(BeFalse())
	})
})
