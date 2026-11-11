// SPDX-License-Identifier: Apache-2.0

package secretstoretest

import (
	"bytes"
	"context"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/secretstore"
)

// WrapperContract declares the specs every secretstore.Wrapper that really wraps (a KMS adapter)
// passes: call it inside a Describe of the adapter's suite, with a function that returns a wrapper
// over a reachable test key (an OpenBao dev container, an AWS KMS stand-in that does the dry run,
// or LocalStack with SkipReachability, which does not). newHanging returns the
// adapter pointed at a backend that accepts connections and never answers (a listener that never
// replies); nil skips that spec. The test KEK is obviously fake.
//
// An adapter whose backend cannot pass every spec (LocalStack ignores DryRun, so its probe is never
// healthy) runs the rest with SkipReachability and keeps a backend that can pass it as the
// reachability target in another run of the contract.
func WrapperContract(newWrapper, newHanging func() secretstore.Wrapper, opts ...WrapperContractOption) {
	var o wrapperContractOptions
	for _, opt := range opts {
		opt(&o)
	}
	kek := bytes.Repeat([]byte("k"), secretstore.KeySize)
	aad := []byte("koinon-test-aad")
	expect := gomega.Expect

	ginkgo.It("gives up by the context's deadline when the backend never answers", func(ctx ginkgo.SpecContext) {
		if newHanging == nil {
			ginkgo.Skip("no hanging backend for this adapter")
		}
		w := newHanging()
		for name, call := range map[string]func(context.Context) error{
			"unwrap": func(c context.Context) error { _, err := w.Unwrap(c, []byte("koinon-test"), aad); return err },
			"probe":  w.Probe,
		} {
			c, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
			start := time.Now()
			err := call(c)
			cancel()
			expect(err).To(gomega.MatchError(secretstore.ErrUnreachable), name)
			expect(time.Since(start)).To(gomega.BeNumerically("<", 2*time.Second), name)
		}
	})

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
		if o.skipReachability != "" {
			ginkgo.Skip("reachability skipped: " + o.skipReachability)
		}
		expect(newWrapper().Probe(ctx)).To(gomega.Succeed())
	})
}

type wrapperContractOptions struct{ skipReachability string }

// WrapperContractOption changes what WrapperContract declares.
type WrapperContractOption func(*wrapperContractOptions)

// SkipReachability skips only the "is reachable" spec, for a backend whose probe cannot succeed.
// The reason is required and shows in the spec's skip message; every other spec still runs.
func SkipReachability(reason string) WrapperContractOption {
	if reason == "" {
		panic("secretstoretest.SkipReachability: a reason is required")
	}
	return func(o *wrapperContractOptions) { o.skipReachability = reason }
}
