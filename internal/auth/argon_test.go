// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("argon2 concurrency", func() {
	It("never runs more than two computations at once, hashing and verifying alike", func() {
		h, err := HashPassword("a password of some length")
		Expect(err).NotTo(HaveOccurred())
		argonPeak.Store(0)
		var wg sync.WaitGroup
		for i := range 10 {
			wg.Add(1)
			go func() {
				defer GinkgoRecover()
				defer wg.Done()
				if i%2 == 0 {
					ok, err := VerifyPassword(h, "a password of some length")
					Expect(err).NotTo(HaveOccurred())
					Expect(ok).To(BeTrue())
				} else {
					_, err := HashPassword("another password")
					Expect(err).NotTo(HaveOccurred())
				}
			}()
		}
		wg.Wait()
		Expect(argonPeak.Load()).To(BeNumerically("<=", maxArgonConcurrency))
		Expect(argonPeak.Load()).To(BeNumerically(">=", 1))
		Expect(argonInFlight.Load()).To(BeZero())
	})

	It("lets a waiting caller give up when its context ends", func() {
		for range maxArgonConcurrency {
			argonSlots <- struct{}{}
		}
		defer func() {
			for range maxArgonConcurrency {
				<-argonSlots
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_, err := HashPasswordContext(ctx, "never computed")
		Expect(err).To(MatchError(context.DeadlineExceeded))
	})
})
