// SPDX-License-Identifier: Apache-2.0

package secretstoretest

import (
	"bytes"
	"encoding/binary"
	"encoding/json"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/secretstore"
)

// Obviously fake install IDs.
const (
	InstallA = "00000000-0000-4000-8000-00000000000a"
	InstallB = "00000000-0000-4000-8000-00000000000b"
)

// LegacyKEKAAD is the associated data of a KEK in stored format 1 (#416), written out here apart
// from secretstore, so that a change to it there fails the contract: KEKs wrapped with it are in
// Secrets and backups and must keep unwrapping.
func LegacyKEKAAD(id string) []byte {
	var out []byte
	for _, f := range []string{"koinon/secretstore/v1", "kek", id, "", "", "", ""} {
		out = binary.BigEndian.AppendUint32(out, uint32(len(f))) //nolint:gosec // short strings
		out = append(out, f...)
	}
	return out
}

// InstallBindingContract declares the specs of the install binding (#615) that every KMS adapter
// passes through secretstore.Wrappers: call it inside a Describe of the adapter's suite with a
// function that returns a wrapper over a reachable test key that may encrypt. [LocalBindingContract]
// is the local KEK's side.
func InstallBindingContract(newWrapper func() secretstore.Wrapper) {
	const id = "kek-20261103-test"
	kek := bytes.Repeat([]byte("k"), secretstore.KeySize) // obviously fake
	expect := gomega.Expect

	wrappers := func(w secretstore.Wrapper, install string) secretstore.Wrappers {
		ginkgo.GinkgoHelper()
		ws, err := secretstore.NewKeyAdminWrappers(w)
		expect(err).NotTo(gomega.HaveOccurred())
		if install != "" {
			ws = ws.WithInstallID(secretstore.StaticInstallID(install))
		}
		return ws
	}
	// stored is s as a process reads it back from the KEK Secret.
	stored := func(s secretstore.StoredKEK) secretstore.StoredKEK {
		ginkgo.GinkgoHelper()
		v, err := s.Encode()
		expect(err).NotTo(gomega.HaveOccurred())
		keys, _, err := secretstore.ParseKeySet(map[string][]byte{id: v, secretstore.ActiveFile: []byte(id)})
		expect(err).NotTo(gomega.HaveOccurred())
		return keys[id]
	}
	format := func(s secretstore.StoredKEK) map[string]any {
		ginkgo.GinkgoHelper()
		v, err := s.Encode()
		expect(err).NotTo(gomega.HaveOccurred())
		var j map[string]any
		expect(json.Unmarshal(v, &j)).To(gomega.Succeed())
		return j
	}

	var w secretstore.Wrapper
	ginkgo.BeforeEach(func() { w = newWrapper() })

	ginkgo.It("binds every new KEK to the install (format 2) and unwraps it with that install ID", func(ctx ginkgo.SpecContext) {
		s, err := wrappers(w, InstallA).Wrap(ctx, id, kek)
		expect(err).NotTo(gomega.HaveOccurred())
		expect(s.Bound()).To(gomega.BeTrue())
		expect(s.InstallID).To(gomega.Equal(InstallA))
		expect(format(s)).To(gomega.And(gomega.HaveKeyWithValue("fmt", 2.0), gomega.HaveKeyWithValue("install", InstallA)))
		got, err := wrappers(w, InstallA).Unwrap(ctx, id, stored(s))
		expect(err).NotTo(gomega.HaveOccurred())
		expect(got).To(gomega.Equal(kek))
		_, err = wrappers(w, InstallA).Unwrap(ctx, "kek-20261103-other", stored(s))
		expect(err).To(gomega.MatchError(secretstore.ErrOpen), "still bound to its KEK ID")
	})

	ginkgo.It("refuses a KEK of another install as not opening, not as unreachable", func(ctx ginkgo.SpecContext) {
		s, err := wrappers(w, InstallA).Wrap(ctx, id, kek)
		expect(err).NotTo(gomega.HaveOccurred())
		_, err = wrappers(w, InstallB).Unwrap(ctx, id, stored(s))
		expect(err).To(gomega.MatchError(secretstore.ErrOpen))
		expect(err).NotTo(gomega.MatchError(secretstore.ErrUnreachable))
		expect(err.Error()).To(gomega.And(gomega.ContainSubstring(InstallA), gomega.ContainSubstring(InstallB)))

		ginkgo.By("the install ID recorded with the KEK is not trusted: the process's own one decides")
		forged := stored(s)
		forged.InstallID = InstallB
		_, err = wrappers(w, InstallB).Unwrap(ctx, id, stored(forged))
		expect(err).To(gomega.MatchError(secretstore.ErrOpen))
	})

	ginkgo.It("refuses a bound KEK relabelled as unbound, and an unbound one relabelled as bound", func(ctx ginkgo.SpecContext) {
		s, err := wrappers(w, InstallA).Wrap(ctx, id, kek)
		expect(err).NotTo(gomega.HaveOccurred())
		unbound := stored(s)
		unbound.InstallID = ""
		for _, install := range []string{"", InstallA} {
			_, err = wrappers(w, install).Unwrap(ctx, id, stored(unbound))
			expect(err).To(gomega.MatchError(secretstore.ErrOpen), install)
		}

		legacy, err := w.Wrap(ctx, kek, LegacyKEKAAD(id))
		expect(err).NotTo(gomega.HaveOccurred())
		bound := secretstore.StoredKEK{Backend: w.Backend(), KeyID: w.KeyID(), Wrapped: legacy, InstallID: InstallA}
		_, err = wrappers(w, InstallA).Unwrap(ctx, id, stored(bound))
		expect(err).To(gomega.MatchError(secretstore.ErrOpen))
	})

	ginkgo.It("unwraps a KEK wrapped before the binding (format 1) as before, with or without an install ID", func(ctx ginkgo.SpecContext) {
		legacy, err := w.Wrap(ctx, kek, LegacyKEKAAD(id))
		expect(err).NotTo(gomega.HaveOccurred())
		s := stored(secretstore.StoredKEK{Backend: w.Backend(), KeyID: w.KeyID(), Wrapped: legacy})
		expect(s.Bound()).To(gomega.BeFalse())
		expect(format(s)).To(gomega.And(gomega.HaveKeyWithValue("fmt", 1.0), gomega.Not(gomega.HaveKey("install"))))
		for _, install := range []string{"", InstallA, InstallB} {
			got, err := wrappers(w, install).Unwrap(ctx, id, s)
			expect(err).NotTo(gomega.HaveOccurred(), install)
			expect(got).To(gomega.Equal(kek))
		}
		_, err = wrappers(w, InstallA).Unwrap(ctx, "kek-20261103-other", s)
		expect(err).To(gomega.MatchError(secretstore.ErrOpen), "still bound to its KEK ID")
	})

	ginkgo.It("wraps nothing without the install ID, and says so when a bound KEK is opened without it", func(ctx ginkgo.SpecContext) {
		_, err := wrappers(w, "").Wrap(ctx, id, kek)
		expect(err).To(gomega.MatchError(secretstore.ErrNoInstallID))
		s, err := wrappers(w, InstallA).Wrap(ctx, id, kek)
		expect(err).NotTo(gomega.HaveOccurred())
		_, err = wrappers(w, "").Unwrap(ctx, id, stored(s))
		expect(err).To(gomega.MatchError(secretstore.ErrNoInstallID))
		expect(err).NotTo(gomega.MatchError(secretstore.ErrOpen))
	})
}

// LocalBindingContract declares what the install binding (#615) means for the local KEK: nothing.
// A local KEK is stored as the key itself, exactly as before, and reads whatever the install ID.
func LocalBindingContract() {
	const id = "kek-20261103-test"
	kek := bytes.Repeat([]byte("k"), secretstore.KeySize) // obviously fake
	expect := gomega.Expect

	ginkgo.It("stores a local KEK as before, unbound, and reads it whatever the install ID", func(ctx ginkgo.SpecContext) {
		var local secretstore.Wrappers
		s, err := local.WithInstallID(secretstore.StaticInstallID(InstallA)).Wrap(ctx, id, kek)
		expect(err).NotTo(gomega.HaveOccurred())
		expect(s.Bound()).To(gomega.BeFalse(), "a local KEK cannot be bound")
		v, err := s.Encode()
		expect(err).NotTo(gomega.HaveOccurred())
		expect(v).To(gomega.Equal(secretstore.EncodeKey(kek)))
		for _, install := range []string{"", InstallA, InstallB} {
			ws := local
			if install != "" {
				ws = ws.WithInstallID(secretstore.StaticInstallID(install))
			}
			keys, _, err := secretstore.ParseKeySet(map[string][]byte{id: v, secretstore.ActiveFile: []byte(id)})
			expect(err).NotTo(gomega.HaveOccurred())
			got, err := ws.Unwrap(ctx, id, keys[id])
			expect(err).NotTo(gomega.HaveOccurred(), install)
			expect(got).To(gomega.Equal(kek))
		}
	})
}
