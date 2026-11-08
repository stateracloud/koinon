// SPDX-License-Identifier: Apache-2.0

package declarative_test

import (
	"context"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/declarative"
)

var _ = Describe("Run backoff", func() {
	// run starts an Importer whose pass is fn, with a long resync so only the backoff retries.
	run := func(fn func(context.Context) (bool, error)) *declarative.Importer {
		GinkgoHelper()
		imp := &declarative.Importer{Client: fake.NewClientset(), Namespace: ns, Resync: time.Minute}
		imp.SetOrgBackoff(10 * time.Millisecond)
		imp.SetSyncFn(fn)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); imp.Run(ctx) }()
		DeferCleanup(func() { cancel(); <-done })
		return imp
	}

	It("retries while a Secret waits for its org, and stops once nothing waits", func() {
		var calls, orgExists atomic.Int32
		run(func(context.Context) (bool, error) {
			calls.Add(1)
			return orgExists.Load() == 0, nil
		})
		Eventually(calls.Load, "10s").Should(BeNumerically(">=", 3))

		orgExists.Store(1)
		Eventually(func() int32 { // one more pass sees the org; then the retries stop
			a := calls.Load()
			time.Sleep(100 * time.Millisecond)
			return calls.Load() - a
		}, "10s").Should(BeZero())
		Consistently(calls.Load, "300ms", "50ms").Should(Equal(calls.Load()))
	})

	It("does not retry when no Secret waits for an org", func() {
		var calls atomic.Int32
		run(func(context.Context) (bool, error) { calls.Add(1); return false, nil })
		Eventually(calls.Load, "10s").Should(Equal(int32(1)))
		Consistently(calls.Load, "300ms", "50ms").Should(Equal(int32(1)))
	})

	It("requests a doubling wait capped at the resync interval, and starts over after a pass that does not wait", func() {
		type request struct {
			d    time.Duration
			fire chan time.Time
		}
		requests := make(chan request, 16)
		var waiting atomic.Bool
		waiting.Store(true)
		imp := &declarative.Importer{Client: fake.NewClientset(), Namespace: ns, Resync: 50 * time.Millisecond}
		imp.SetOrgBackoff(10 * time.Millisecond)
		imp.SetSyncFn(func(context.Context) (bool, error) { return waiting.Load(), nil })
		// The test fires each wait itself, so no wall-clock time decides the sequence.
		imp.SetAfter(func(d time.Duration) <-chan time.Time {
			fire := make(chan time.Time, 1)
			requests <- request{d, fire}
			return fire
		})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); imp.Run(ctx) }()
		DeferCleanup(func() { cancel(); <-done })

		next := func() request {
			GinkgoHelper()
			var r request
			Eventually(requests, "10s").Should(Receive(&r))
			return r
		}
		for _, want := range []time.Duration{10, 20, 40, 50} {
			r := next()
			Expect(r.d).To(Equal(want * time.Millisecond))
			r.fire <- time.Now()
		}

		By("the wait stays at the resync interval, and a pass that waits for nothing requests no retry")
		r := next()
		Expect(r.d).To(Equal(50 * time.Millisecond))
		waiting.Store(false)
		r.fire <- time.Now()
		Consistently(requests, "300ms", "50ms").ShouldNot(Receive())

		By("the next Secret event starts the backoff over")
		waiting.Store(true)
		_, err := imp.Client.CoreV1().Secrets(ns).Create(context.Background(), &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: "again", Namespace: ns,
				Labels: map[string]string{declarative.LabelSecretType: declarative.TypeModelProvider},
			},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(next().d).To(Equal(10 * time.Millisecond))
	})
})
