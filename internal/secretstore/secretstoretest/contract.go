// SPDX-License-Identifier: Apache-2.0

package secretstoretest

import (
	"bytes"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/secretstore"
)

// WrapperContract declares the specs every secretstore.Wrapper that really wraps (a KMS adapter)
// passes: call it inside a Describe of the adapter's suite, with a function that returns a wrapper
// over a reachable test key (an OpenBao dev container, LocalStack KMS). The test KEK is obviously
// fake.
func WrapperContract(newWrapper func() secretstore.Wrapper) {
	kek := bytes.Repeat([]byte("k"), secretstore.KeySize)
	aad := []byte("koinon-test-aad")
	expect := gomega.Expect

	ginkgo.It("names a fixed backend and its key", func() {
		w := newWrapper()
		expect(w.Backend()).To(gomega.MatchRegexp(`^[a-z][a-z0-9]{0,15}$`))
		expect(w.Backend()).NotTo(gomega.Equal(secretstore.BackendLocal))
		expect(w.KeyID()).NotTo(gomega.BeEmpty())
	})

	ginkgo.It("round-trips a KEK and does not store it in the clear", func(ctx ginkgo.SpecContext) {
		w := newWrapper()
		wrapped, err := w.Wrap(ctx, kek, aad)
		expect(err).NotTo(gomega.HaveOccurred())
		expect(bytes.Contains(wrapped, kek)).To(gomega.BeFalse())
		got, err := w.Unwrap(ctx, wrapped, aad)
		expect(err).NotTo(gomega.HaveOccurred())
		expect(got).To(gomega.Equal(kek))
	})

	ginkgo.It("refuses other associated data and modified ciphertext", func(ctx ginkgo.SpecContext) {
		w := newWrapper()
		wrapped, err := w.Wrap(ctx, kek, aad)
		expect(err).NotTo(gomega.HaveOccurred())
		_, err = w.Unwrap(ctx, wrapped, []byte("koinon-test-other"))
		expect(err).To(gomega.MatchError(secretstore.ErrOpen))
		bad := bytes.Clone(wrapped)
		bad[len(bad)-1] ^= 1
		_, err = w.Unwrap(ctx, bad, aad)
		expect(err).To(gomega.HaveOccurred())
		expect(err).NotTo(gomega.MatchError(secretstore.ErrUnreachable))
	})

	ginkgo.It("is reachable", func(ctx ginkgo.SpecContext) {
		expect(newWrapper().Probe(ctx)).To(gomega.Succeed())
	})
}
