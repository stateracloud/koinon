// SPDX-License-Identifier: Apache-2.0

package secretstoretest

import (
	"bytes"

	. "github.com/onsi/ginkgo/v2" //nolint:revive // a Ginkgo helper
	. "github.com/onsi/gomega"    //nolint:revive // a Ginkgo helper

	"github.com/stateracloud/koinon/internal/secretstore"
)

// WrapperContract declares the specs every secretstore.Wrapper that really wraps (a KMS adapter)
// passes: call it inside a Describe of the adapter's suite, with a function that returns a wrapper
// over a reachable test key (an OpenBao dev container, LocalStack KMS). The test KEK is obviously
// fake.
func WrapperContract(newWrapper func() secretstore.Wrapper) {
	kek := bytes.Repeat([]byte("k"), secretstore.KeySize)
	aad := []byte("koinon-test-aad")

	It("names a fixed backend and its key", func() {
		w := newWrapper()
		Expect(w.Backend()).To(MatchRegexp(`^[a-z][a-z0-9]{0,15}$`))
		Expect(w.Backend()).NotTo(Equal(secretstore.BackendLocal))
		Expect(w.KeyID()).NotTo(BeEmpty())
	})

	It("round-trips a KEK and does not store it in the clear", func(ctx SpecContext) {
		w := newWrapper()
		wrapped, err := w.Wrap(ctx, kek, aad)
		Expect(err).NotTo(HaveOccurred())
		Expect(bytes.Contains(wrapped, kek)).To(BeFalse())
		got, err := w.Unwrap(ctx, wrapped, aad)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(kek))
	})

	It("refuses other associated data and modified ciphertext", func(ctx SpecContext) {
		w := newWrapper()
		wrapped, err := w.Wrap(ctx, kek, aad)
		Expect(err).NotTo(HaveOccurred())
		_, err = w.Unwrap(ctx, wrapped, []byte("koinon-test-other"))
		Expect(err).To(MatchError(secretstore.ErrOpen))
		bad := bytes.Clone(wrapped)
		bad[len(bad)-1] ^= 1
		_, err = w.Unwrap(ctx, bad, aad)
		Expect(err).To(HaveOccurred())
		Expect(err).NotTo(MatchError(secretstore.ErrUnreachable))
	})

	It("is reachable", func(ctx SpecContext) {
		Expect(newWrapper().Probe(ctx)).To(Succeed())
	})
}
