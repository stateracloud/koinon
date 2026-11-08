// SPDX-License-Identifier: Apache-2.0

package declarative_test

import (
	"context"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/declarative"
)

var _ = Describe("Run backoff", func() {
	// run starts an Importer whose pass is fn, with a long resync so only the backoff retries.
	run := func(fn func(context.Context) (bool, error)) {
		GinkgoHelper()
		imp := &declarative.Importer{Client: fake.NewClientset(), Namespace: ns, Resync: time.Minute}
		imp.SetOrgBackoff(10 * time.Millisecond)
		imp.SetSyncFn(fn)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); imp.Run(ctx) }()
		DeferCleanup(func() { cancel(); <-done })
	}

	It("retries while a Secret waits for its org, and stops once nothing waits", func() {
		var calls, orgExists atomic.Int32
		run(func(context.Context) (bool, error) {
			calls.Add(1)
			return orgExists.Load() == 0, nil
		})
		Eventually(calls.Load).Should(BeNumerically(">=", 3))

		orgExists.Store(1)
		Eventually(func() int32 { // one more pass sees the org; then the retries stop
			a := calls.Load()
			time.Sleep(100 * time.Millisecond)
			return calls.Load() - a
		}).Should(BeZero())
		Consistently(calls.Load, "300ms", "50ms").Should(Equal(calls.Load()))
	})

	It("does not retry when no Secret waits for an org", func() {
		var calls atomic.Int32
		run(func(context.Context) (bool, error) { calls.Add(1); return false, nil })
		Eventually(calls.Load).Should(Equal(int32(1)))
		Consistently(calls.Load, "300ms", "50ms").Should(Equal(int32(1)))
	})

	It("doubles the wait between retries", func() {
		var times []time.Time
		ch := make(chan time.Time, 16)
		run(func(context.Context) (bool, error) { ch <- time.Now(); return true, nil })
		for range 5 {
			select {
			case t := <-ch:
				times = append(times, t)
			case <-time.After(5 * time.Second):
				Fail("no retry")
			}
		}
		// Gaps are about 10, 20, 40, 80 ms: the last is clearly longer than the first.
		Expect(times[4].Sub(times[3])).To(BeNumerically(">", 2*times[1].Sub(times[0])))
	})
})
