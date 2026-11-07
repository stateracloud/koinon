// SPDX-License-Identifier: Apache-2.0

package secretstoretest

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"sync/atomic"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/secretstore"
)

// Obviously fake install IDs.
const (
	InstallA = "00000000-0000-4000-8000-00000000000a"
	InstallB = "00000000-0000-4000-8000-00000000000b"
)

// BoundOnlySet is a set bound-only marker for tests (old issue 615).
var BoundOnlySet = secretstore.BoundOnly{Set: true, At: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC), How: secretstore.BoundOnlyRewrap}

// BoundOnlyUnset is the bound-only marker source of an install that still accepts KEKs stored
// unbound (format 1): tests pass it, or another explicit source, to the wrappers' constructors.
var BoundOnlyUnset = secretstore.StaticBoundOnly(secretstore.BoundOnly{})

// LegacyKEKAAD is the associated data of a KEK in stored format 1 (old issue 416), written out here apart
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

// InstallBindingContract declares the specs of the install binding (old issue 615) that every KMS adapter
// passes through secretstore.Wrappers: call it inside a Describe of the adapter's suite with a
// function that returns a wrapper over a reachable test key that may encrypt. [LocalBindingContract]
// is the local KEK's side.
func InstallBindingContract(newWrapper func() secretstore.Wrapper) {
	const id = "kek-20261103-test"
	kek := bytes.Repeat([]byte("k"), secretstore.KeySize) // obviously fake
	expect := gomega.Expect

	wrappersWith := func(w secretstore.Wrapper, install string, marker secretstore.BoundOnlySource) secretstore.Wrappers {
		ginkgo.GinkgoHelper()
		ws, err := secretstore.NewKeyAdminWrappers(marker, w)
		expect(err).NotTo(gomega.HaveOccurred())
		if install != "" {
			ws = ws.WithInstallID(secretstore.StaticInstallID(install))
		}
		return ws
	}
	wrappers := func(w secretstore.Wrapper, install string) secretstore.Wrappers {
		ginkgo.GinkgoHelper()
		return wrappersWith(w, install, BoundOnlyUnset)
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

	ginkgo.Describe("the bound-only marker", func() {
		var legacy secretstore.StoredKEK
		ginkgo.BeforeEach(func(ctx ginkgo.SpecContext) {
			v, err := w.Wrap(ctx, kek, LegacyKEKAAD(id))
			expect(err).NotTo(gomega.HaveOccurred())
			legacy = stored(secretstore.StoredKEK{Backend: w.Backend(), KeyID: w.KeyID(), Wrapped: v})
		})
		withMarker := func(src secretstore.BoundOnlySource) secretstore.Wrappers {
			ginkgo.GinkgoHelper()
			return wrappersWith(w, InstallA, src)
		}

		ginkgo.It("still opens a KEK stored unbound (format 1) while it is not set", func(ctx ginkgo.SpecContext) {
			got, err := withMarker(secretstore.StaticBoundOnly(secretstore.BoundOnly{})).Unwrap(ctx, id, legacy)
			expect(err).NotTo(gomega.HaveOccurred())
			expect(got).To(gomega.Equal(kek))
		})

		ginkgo.It("refuses a KEK stored unbound once it is set, as not opening, and says why", func(ctx ginkgo.SpecContext) {
			_, err := withMarker(secretstore.StaticBoundOnly(BoundOnlySet)).Unwrap(ctx, id, legacy)
			expect(err).To(gomega.And(gomega.MatchError(secretstore.ErrBoundOnly), gomega.MatchError(secretstore.ErrOpen)))
			expect(err).NotTo(gomega.MatchError(secretstore.ErrUnreachable))
			expect(err.Error()).To(gomega.And(gomega.ContainSubstring("2026-11-03T12:00:00Z"), gomega.ContainSubstring("--rewrap-unbound"),
				gomega.ContainSubstring(id), gomega.ContainSubstring("delete the KEK Secret and rerun the key-admin Job")))

			ginkgo.By("a bound KEK still opens")
			s, err := wrappers(w, InstallA).Wrap(ctx, id, kek)
			expect(err).NotTo(gomega.HaveOccurred())
			got, err := withMarker(secretstore.StaticBoundOnly(BoundOnlySet)).Unwrap(ctx, id, stored(s))
			expect(err).NotTo(gomega.HaveOccurred())
			expect(got).To(gomega.Equal(kek))
		})

		ginkgo.It("is read on every unwrap of an unbound KEK, so a process that opened it before the marker was set refuses it after", func(ctx ginkgo.SpecContext) {
			var set atomic.Bool
			src := func(context.Context) (secretstore.BoundOnly, error) {
				if set.Load() {
					return BoundOnlySet, nil
				}
				return secretstore.BoundOnly{}, nil
			}
			ws := withMarker(src)
			_, err := ws.Unwrap(ctx, id, legacy)
			expect(err).NotTo(gomega.HaveOccurred())
			set.Store(true)
			_, err = ws.Unwrap(ctx, id, legacy)
			expect(err).To(gomega.MatchError(secretstore.ErrBoundOnly))
		})

		ginkgo.It("bounds the marker read, so a hung database fails the unwrap instead of blocking it", func(ctx ginkgo.SpecContext) {
			hang := func(ctx context.Context) (secretstore.BoundOnly, error) {
				<-ctx.Done()
				return secretstore.BoundOnly{}, ctx.Err()
			}
			done := make(chan error, 1)
			go func() {
				_, err := withMarker(hang).WithCallTimeout(50*time.Millisecond).Unwrap(ctx, id, legacy)
				done <- err
			}()
			var err error
			gomega.Eventually(done).WithContext(ctx).WithTimeout(5*time.Second).Should(gomega.Receive(&err), "the read must end at the call timeout, not hang")
			expect(err).To(gomega.MatchError(context.DeadlineExceeded))
			expect(err).NotTo(gomega.MatchError(secretstore.ErrUnreachable))
		})

		ginkgo.It("refuses a KEK stored unbound when the marker cannot be read (fail closed)", func(ctx ginkgo.SpecContext) {
			down := errors.New("koinon_test database down")
			_, err := withMarker(func(context.Context) (secretstore.BoundOnly, error) { return secretstore.BoundOnly{}, down }).Unwrap(ctx, id, legacy)
			expect(err).To(gomega.MatchError(down))
			expect(err).NotTo(gomega.MatchError(secretstore.ErrUnreachable))
		})
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

// LocalBindingContract declares what the install binding (old issue 615) means for the local KEK: nothing.
// A local KEK is stored as the key itself, exactly as before, and reads whatever the install ID.
func LocalBindingContract() {
	const id = "kek-20261103-test"
	kek := bytes.Repeat([]byte("k"), secretstore.KeySize) // obviously fake
	expect := gomega.Expect

	ginkgo.It("stores a local KEK as before, unbound, and reads it whatever the install ID", func(ctx ginkgo.SpecContext) {
		// The bound-only marker is about KMS KEKs: a local one opens with it set.
		local, err := secretstore.NewWrappers(secretstore.StaticBoundOnly(BoundOnlySet), secretstore.Local{})
		expect(err).NotTo(gomega.HaveOccurred())
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
