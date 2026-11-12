// SPDX-License-Identifier: Apache-2.0

package ratelimit_test

import (
	"net/http/httptest"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/auth/ratelimit"
)

var _ = Describe("Limiter", func() {
	var (
		l   *ratelimit.Limiter
		t0  time.Time
		key string
	)

	BeforeEach(func() {
		l = ratelimit.New()
		t0 = time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
		key = ratelimit.Key("192.0.2.1", "user:admin")
	})

	failN := func(n int, at time.Time) {
		for range n {
			l.Fail(key, at)
		}
	}

	It("lets FreeFails-1 failures through and blocks at the FreeFails-th", func() {
		failN(ratelimit.FreeFails-1, t0)
		Expect(l.Blocked(key, t0)).To(BeZero())
		l.Fail(key, t0)
		Expect(l.Blocked(key, t0)).To(Equal(ratelimit.BaseBackoff))
	})

	It("doubles the block with each further failure, up to MaxBackoff", func() {
		failN(ratelimit.FreeFails+2, t0)
		Expect(l.Blocked(key, t0)).To(Equal(4 * ratelimit.BaseBackoff))
		failN(40, t0)
		Expect(l.Blocked(key, t0)).To(Equal(ratelimit.MaxBackoff))
	})

	It("keeps doubling for a client that waits out each block", func() {
		now := t0
		failN(ratelimit.FreeFails, now)
		for i := 1; i <= 8; i++ {
			now = now.Add(l.Blocked(key, now) + time.Millisecond)
			l.Fail(key, now)
			Expect(l.Blocked(key, now)).To(Equal(ratelimit.BaseBackoff<<i), "failure %d after its block", i)
		}
	})

	It("starts afresh after a quiet window that follows the block", func() {
		failN(ratelimit.FreeFails, t0)
		later := t0.Add(ratelimit.BaseBackoff + ratelimit.FailWindow + time.Second)
		l.Fail(key, later)
		Expect(l.Blocked(key, later)).To(BeZero())
	})

	It("keeps slots apart: another address or credential is not blocked", func() {
		failN(ratelimit.FreeFails, t0)
		Expect(l.Blocked(ratelimit.Key("192.0.2.2", "user:admin"), t0)).To(BeZero())
		Expect(l.Blocked(ratelimit.Key("192.0.2.1", "user:other"), t0)).To(BeZero())
		Expect(l.Blocked(ratelimit.Key("192.0.2.1", ""), t0)).To(BeZero())
	})

	It("forgets a key on success", func() {
		failN(ratelimit.FreeFails, t0)
		l.Succeed(key)
		Expect(l.Blocked(key, t0)).To(BeZero())
	})

	It("counts refusals until they are taken, across a fresh start", func() {
		l.Refuse(key)
		Expect(l.TakeRefused(key)).To(BeZero(), "an untracked key counts nothing")
		failN(ratelimit.FreeFails, t0)
		l.Refuse(key)
		l.Refuse(key)
		l.Fail(key, t0.Add(ratelimit.BaseBackoff+ratelimit.FailWindow+time.Second))
		Expect(l.TakeRefused(key)).To(Equal(2))
		Expect(l.TakeRefused(key)).To(BeZero())
	})

	It("rounds Retry-After up to whole seconds", func() {
		Expect(ratelimit.RetryAfter(time.Second)).To(Equal("1"))
		Expect(ratelimit.RetryAfter(1500 * time.Millisecond)).To(Equal("2"))
		Expect(ratelimit.RetryAfter(time.Millisecond)).To(Equal("1"))
	})

	It("takes the peer address without its port", func() {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = "192.0.2.7:4242"
		Expect(ratelimit.ClientAddr(r)).To(Equal("192.0.2.7"))
		r.RemoteAddr = "[2001:db8::1]:4242"
		Expect(ratelimit.ClientAddr(r)).To(Equal("2001:db8::1"))
	})
})
