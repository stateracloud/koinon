// SPDX-License-Identifier: Apache-2.0

package secretstore_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/secretstore/secretstoretest"
	"github.com/stateracloud/koinon/internal/secretstore/wrapping"
)

var _ = Describe("Key wrapping", func() {
	Describe("the fake KMS", func() {
		secretstoretest.WrapperContract(
			func() secretstore.Wrapper { return &secretstoretest.FakeKMS{} },
			func() secretstore.Wrapper { f := &secretstoretest.FakeKMS{}; f.Hang.Store(true); return f },
		)
		Describe("the install binding (old issue 615)", func() {
			secretstoretest.InstallBindingContract(func() secretstore.Wrapper { return &secretstoretest.FakeKMS{} })
		})
	})

	Describe("the local KEK and the install binding (old issue 615)", func() {
		secretstoretest.LocalBindingContract()
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
		ws, err := keyAdmin(kms)
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
		ws, _ := keyAdmin(&secretstoretest.FakeKMS{})
		s, err := ws.Wrap(ctx, "k1", key())
		Expect(err).NotTo(HaveOccurred())
		_, err = ws.Unwrap(ctx, "k2", s)
		Expect(err).To(MatchError(secretstore.ErrOpen))
	})

	It("tells an unreachable KMS from a wrong key, and a failed reload keeps the old keys", func(ctx SpecContext) {
		kms := &secretstoretest.FakeKMS{}
		ws, _ := keyAdmin(kms)
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
		ws, _ := keyAdmin(kms)
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
			`{"fmt":2,"backend":"fakekms","key":"x","wrapped":"aGVsbG8=","install":"NOT-A-UUID-hunter2"}`,
			`{"fmt":1,"backend":"fakekms","key":"x","wrapped":"aGVsbG8=","install":"` + secretstoretest.InstallA + `"}`,
			`{"fmt":3,"backend":"fakekms","key":"x","wrapped":"aGVsbG8=","install":"` + secretstoretest.InstallA + `"}`,
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

	It("writes a bound KEK in a form a binary from before old issue 615 refuses, and an unbound one it reads", func(ctx SpecContext) {
		kms := &secretstoretest.FakeKMS{}
		ws, err := keyAdmin(kms)
		Expect(err).NotTo(HaveOccurred())
		s, err := ws.Wrap(ctx, "k1", key())
		Expect(err).NotTo(HaveOccurred())
		v, err := s.Encode()
		Expect(err).NotTo(HaveOccurred())
		Expect(decodeBefore615(v)).To(MatchError(ContainSubstring("unsupported format 2")), "an old binary fails closed")

		legacy, err := kms.Wrap(ctx, key(), secretstoretest.LegacyKEKAAD("k1"))
		Expect(err).NotTo(HaveOccurred())
		v, err = secretstore.StoredKEK{Backend: kms.Backend(), KeyID: kms.KeyID(), Wrapped: legacy}.Encode()
		Expect(err).NotTo(HaveOccurred())
		Expect(decodeBefore615(v)).To(Succeed(), "an unbound KEK keeps the form older binaries read")
	})

	It("refuses a wrapping key configured twice", func() {
		_, err := secretstore.NewWrappers(secretstoretest.BoundOnlyUnset, &secretstoretest.FakeKMS{}, &secretstoretest.FakeKMS{})
		Expect(err).To(MatchError(ContainSubstring("configured twice")))
		_, err = secretstore.NewWrappers(secretstoretest.BoundOnlyUnset, nil)
		Expect(err).To(HaveOccurred())
	})

	It("refuses to build wrappers without a source for the bound-only marker (old issue 615): there is no mode that accepts format 1 unasked", func() {
		for _, w := range []secretstore.Wrapper{secretstore.Local{}, &secretstoretest.FakeKMS{}} {
			_, err := secretstore.NewWrappers(nil, w)
			Expect(err).To(MatchError(secretstore.ErrNoBoundOnlySource), w.Backend())
			_, err = secretstore.NewKeyAdminWrappers(nil, w)
			Expect(err).To(MatchError(secretstore.ErrNoBoundOnlySource), w.Backend())
		}
	})

	It("refuses a KMS KEK stored unbound when the process has no database to read the marker from (fail closed)", func(ctx SpecContext) {
		kms := &secretstoretest.FakeKMS{}
		noDB := errors.New("koinon_test no database")
		ws, err := secretstore.NewWrappers(secretstore.UnreadableBoundOnly(noDB), kms)
		Expect(err).NotTo(HaveOccurred())
		wrapped, err := kms.Wrap(ctx, key(), secretstoretest.LegacyKEKAAD("k1"))
		Expect(err).NotTo(HaveOccurred())
		_, err = ws.Unwrap(ctx, "k1", secretstore.StoredKEK{Backend: kms.Backend(), KeyID: kms.KeyID(), Wrapped: wrapped})
		Expect(err).To(MatchError(noDB))
	})

	It("lets a service wrap with the default local KEK, as before key wrapping", func(ctx SpecContext) {
		ws, err := wrapping.New(ctx, wrapping.Config{}, wrapping.Service, secretstoretest.BoundOnlyUnset)
		Expect(err).NotTo(HaveOccurred())
		Expect(ws.Primary().Backend()).To(Equal(secretstore.BackendLocal))
		s, err := ws.Wrap(ctx, "k1", key())
		Expect(err).NotTo(HaveOccurred())
		Expect(s.Backend).To(Equal(secretstore.BackendLocal))
	})

	It("never lets a service wrap with an opted-in KMS, and never calls it to try", func(ctx SpecContext) {
		kms := &secretstoretest.FakeKMS{}
		kms.DecryptOnly.Store(true)
		admin, _ := keyAdmin(&secretstoretest.FakeKMS{})
		s, err := admin.Wrap(ctx, "k1", key())
		Expect(err).NotTo(HaveOccurred())

		service, err := secretstore.NewWrappers(secretstoretest.BoundOnlyUnset, kms)
		Expect(err).NotTo(HaveOccurred())
		service = service.WithInstallID(secretstore.StaticInstallID(secretstoretest.InstallA))
		_, err = service.Wrap(ctx, "k2", key())
		Expect(err).To(MatchError(secretstore.ErrKeyAdminWraps))
		Expect(err.Error()).To(ContainSubstring("new KEKs are wrapped by the key-admin Job"))
		Expect(kms.Wraps()).To(BeZero())
		_, err = service.Unwrap(ctx, "k1", s)
		Expect(err).NotTo(HaveOccurred(), "a decrypt-only service still unwraps")
	})

	It("bounds each adapter call, whatever the caller's context", func(ctx SpecContext) {
		kms := &secretstoretest.FakeKMS{}
		admin, _ := keyAdmin(kms)
		s, err := admin.Wrap(ctx, "k1", key())
		Expect(err).NotTo(HaveOccurred())
		kms.Hang.Store(true)
		ws := admin.WithCallTimeout(100 * time.Millisecond)
		start := time.Now()
		_, err = ws.Unwrap(context.Background(), "k1", s) // a watch loop's long-lived context
		Expect(err).To(MatchError(secretstore.ErrUnreachable))
		Expect(ws.Probe(context.Background(), s)).To(MatchError(secretstore.ErrUnreachable))
		Expect(time.Since(start)).To(BeNumerically("<", 2*time.Second))
	})

	It("reports a Wrap that ran out of time as unreachable", func(ctx SpecContext) {
		admin, _ := keyAdmin(bareHang{&secretstoretest.FakeKMS{}})
		_, err := admin.WithCallTimeout(100*time.Millisecond).Wrap(context.Background(), "k1", key())
		Expect(err).To(MatchError(secretstore.ErrUnreachable))
		Expect(err).To(MatchError(context.DeadlineExceeded))
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

// decodeBefore615 is the check that the releases from old issue 416 to old issue 615 make on a wrapped KEK's Secret
// value (secretstore.decodeStored as it was), kept here to show what such a binary does with the
// forms this one writes.
func decodeBefore615(v []byte) error {
	var j struct {
		Format  int    `json:"fmt"`
		Backend string `json:"backend"`
		KeyID   string `json:"key"`
		Wrapped []byte `json:"wrapped"`
	}
	if err := json.Unmarshal(v, &j); err != nil {
		return errors.New("a wrapped key that is not valid JSON")
	}
	if j.Format != 1 || j.Backend == secretstore.BackendLocal || len(j.Wrapped) == 0 {
		return fmt.Errorf("a wrapped key of unsupported format %d, backend %q", j.Format, j.Backend)
	}
	return nil
}

// keyAdmin is the key-admin Job's wrappers over w, with an install ID.
func keyAdmin(w secretstore.Wrapper, more ...secretstore.Wrapper) (secretstore.Wrappers, error) {
	ws, err := secretstore.NewKeyAdminWrappers(secretstoretest.BoundOnlyUnset, w, more...)
	return ws.WithInstallID(secretstore.StaticInstallID(secretstoretest.InstallA)), err
}

// bareHang is an adapter whose Wrap blocks until its context ends and returns only ctx.Err(), not
// ErrUnreachable: Wrappers must still report it as unreachable.
type bareHang struct{ *secretstoretest.FakeKMS }

func (bareHang) Wrap(ctx context.Context, _, _ []byte) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
