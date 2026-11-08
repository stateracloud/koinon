// SPDX-License-Identifier: Apache-2.0

package declarative_test

import (
	"context"
	"errors"
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
	type request struct {
		d    time.Duration
		fire chan time.Time
	}

	// run starts an Importer whose pass is fn. Resync is an hour, so the real ticker never fires
	// during a spec, and the test fires each requested wait itself: no wall-clock time decides
	// the sequence. A wait is fired by sending on request.fire.
	run := func(first time.Duration, fn func(context.Context) (bool, error)) (*declarative.Importer, func() request, chan request) {
		GinkgoHelper()
		requests := make(chan request, 16)
		imp := &declarative.Importer{Client: fake.NewClientset(), Namespace: ns, Resync: time.Hour}
		imp.SetOrgBackoff(first)
		imp.SetSyncFn(fn)
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
		return imp, next, requests
	}
	const first = 15 * time.Minute

	It("retries while a Secret waits for its org, and stops once nothing waits", func() {
		var calls, orgExists atomic.Int32
		_, next, requests := run(first, func(context.Context) (bool, error) {
			calls.Add(1)
			return orgExists.Load() == 0, nil
		})
		next().fire <- time.Now()
		r := next() // the second pass still saw no org
		Expect(calls.Load()).To(Equal(int32(2)))

		orgExists.Store(1)
		r.fire <- time.Now()
		Eventually(calls.Load, "10s").Should(Equal(int32(3)))
		Consistently(requests, "300ms", "50ms").ShouldNot(Receive())
	})

	It("does not retry when no Secret waits for an org", func() {
		var calls atomic.Int32
		_, _, requests := run(first, func(context.Context) (bool, error) { calls.Add(1); return false, nil })
		Eventually(calls.Load, "10s").Should(Equal(int32(1)))
		Consistently(requests, "300ms", "50ms").ShouldNot(Receive())
	})

	It("requests a doubling wait, none once it reaches the resync interval, and starts over on a Secret event", func() {
		imp, next, requests := run(first, func(context.Context) (bool, error) { return true, nil })
		r := next()
		Expect(r.d).To(Equal(15 * time.Minute))
		r.fire <- time.Now()
		r = next()
		Expect(r.d).To(Equal(30 * time.Minute))

		By("the backoff has reached the resync interval: the ticker covers it, so no retry is requested")
		r.fire <- time.Now()
		Consistently(requests, "300ms", "50ms").ShouldNot(Receive())

		By("a Secret event starts the backoff over, though the backoff was at the cap")
		_, err := imp.Client.CoreV1().Secrets(ns).Create(context.Background(), &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: "again", Namespace: ns,
				Labels: map[string]string{declarative.LabelSecretType: declarative.TypeModelProvider},
			},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(next().d).To(Equal(first))
	})

	It("retries on the backoff after a pass that ended in an error, and starts over after a clean one", func() {
		var fail atomic.Bool
		fail.Store(true)
		_, next, requests := run(first, func(context.Context) (bool, error) {
			if fail.Load() {
				return false, errors.New("transient")
			}
			return false, nil
		})
		r := next()
		Expect(r.d).To(Equal(first))
		fail.Store(false)
		r.fire <- time.Now()
		Consistently(requests, "300ms", "50ms").ShouldNot(Receive())
	})
})
