// SPDX-License-Identifier: Apache-2.0

package ratelimit_test

import (
	"net/http/httptest"
	"strconv"
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
		Expect(ratelimit.ClientAddr(r)).To(Equal("2001:db8::"))
	})

	It("shares a key between two addresses of one IPv6 /64 and not between IPv4 hosts", func() {
		a, b := httptest.NewRequest("GET", "/", nil), httptest.NewRequest("GET", "/", nil)
		a.RemoteAddr, b.RemoteAddr = "[2001:db8:1:2::1]:1", "[2001:db8:1:2:ffff::9]:2"
		Expect(ratelimit.ClientAddr(a)).To(Equal(ratelimit.ClientAddr(b)))
		b.RemoteAddr = "[2001:db8:1:3::1]:2"
		Expect(ratelimit.ClientAddr(a)).NotTo(Equal(ratelimit.ClientAddr(b)))
		a.RemoteAddr, b.RemoteAddr = "192.0.2.1:1", "192.0.2.2:1"
		Expect(ratelimit.ClientAddr(a)).NotTo(Equal(ratelimit.ClientAddr(b)))
	})

	It("never tracks more keys than its cap, and drops the least recently used", func() {
		l.Fail("first", t0)
		for i := range 25000 {
			l.Fail(ratelimit.Key("192.0.2.1", "user:"+strconv.Itoa(i)), t0)
		}
		Expect(l.Len()).To(BeNumerically("<=", 10000))
		Expect(l.TakeRefused("first")).To(BeZero())
	})

	It("keeps a blocked key blocked while fresh keys flood the limiter", func() {
		failN(ratelimit.FreeFails+3, t0)
		want := l.Blocked(key, t0)
		Expect(want).To(BeNumerically(">", ratelimit.BaseBackoff))
		for i := range 25000 {
			l.Fail(ratelimit.Key("192.0.2.2", "user:"+strconv.Itoa(i)), t0)
		}
		Expect(l.Blocked(key, t0)).To(Equal(want))
		failN(1, t0)
		Expect(l.Blocked(key, t0)).To(Equal(2 * want))
	})

	It("refuses a new key when the blocked set is full, and recovers when blocks end", func() {
		for i := range 10000 {
			k := ratelimit.Key("192.0.2.3", "user:"+strconv.Itoa(i))
			for range ratelimit.FreeFails {
				l.Fail(k, t0)
			}
		}
		fresh := ratelimit.Key("192.0.2.4", "user:new")
		Expect(l.Blocked(fresh, t0)).To(BeNumerically(">", 0))
		l.Fail(fresh, t0)
		Expect(l.Len()).To(Equal(10000), "an untracked key is not added")
		Expect(l.Blocked(key, t0)).To(BeNumerically(">", 0), "and so are all keys that were never seen")
		Expect(l.Blocked(fresh, t0.Add(ratelimit.BaseBackoff+time.Millisecond))).To(BeZero())
	})

	It("keeps the credential name out of the key", func() {
		Expect(ratelimit.Key("192.0.2.1", "user:admin")).NotTo(ContainSubstring("admin"))
	})
})
