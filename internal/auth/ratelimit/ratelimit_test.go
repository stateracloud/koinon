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

	blockKey := func(k string, at time.Time) {
		for range ratelimit.FreeFails {
			l.Fail(k, at)
		}
	}

	It("keeps a key that was tracked before it became blocked, through a flood of fresh keys", func() {
		failN(2, t0)
		failN(ratelimit.FreeFails-2, t0) // tracked first, blocked now
		Expect(l.Blocked(key, t0)).To(BeNumerically(">", 0))
		for i := range 25000 {
			l.Fail(ratelimit.Key("192.0.2.6", "user:"+strconv.Itoa(i)), t0)
		}
		Expect(l.Blocked(key, t0)).To(BeNumerically(">", 0))
	})

	It("never keeps more than maxTracked unblocked keys when blocks end", func() {
		for a := range 1500 {
			addr := "10.1." + strconv.Itoa(a/250) + "." + strconv.Itoa(a%250+1)
			for u := range 8 {
				blockKey(ratelimit.Key(addr, "user:"+strconv.Itoa(u)), t0)
			}
		}
		Expect(l.Blocked(key, t0.Add(2*ratelimit.BaseBackoff))).To(BeZero())
		Expect(l.Len()).To(BeNumerically("<=", 10000))
	})

	It("blocks only the address that overflows its cap", func() {
		for u := range 8 {
			blockKey(ratelimit.Key("192.0.2.7", "user:"+strconv.Itoa(u)), t0)
		}
		Expect(l.Blocked(ratelimit.Key("192.0.2.7", ""), t0)).To(BeZero(), "within the cap")
		blockKey(ratelimit.Key("192.0.2.7", "user:overflow"), t0)
		Expect(l.Blocked(ratelimit.Key("192.0.2.7", ""), t0)).To(BeNumerically(">", 0))
		Expect(l.Blocked(ratelimit.Key("192.0.2.7", "all-users"), t0)).To(BeNumerically(">", 0))
		Expect(l.Blocked(ratelimit.Key("192.0.2.7", "abcdefgh"), t0)).To(BeNumerically(">", 0), "a prefix the address never used is refused too")
		Expect(l.Blocked(ratelimit.Key("192.0.2.8", ""), t0)).To(BeZero())
		Expect(l.Blocked(ratelimit.Key("192.0.2.8", "all-users"), t0)).To(BeZero())
		Expect(l.Blocked(ratelimit.Key("192.0.2.8", "user:admin"), t0)).To(BeZero())
	})

	It("counts an IPv6 address per /48 for the cap", func() {
		for u := range 8 {
			blockKey(ratelimit.Key("2001:db8:1:"+strconv.Itoa(u)+"::", "user:x"), t0)
		}
		blockKey(ratelimit.Key("2001:db8:1:ff::", "user:x"), t0)
		Expect(l.Blocked(ratelimit.Key("2001:db8:1:ff::", ""), t0)).To(BeNumerically(">", 0))
		Expect(l.Blocked(ratelimit.Key("2001:db8:2::", ""), t0)).To(BeZero())
	})

	It("keeps one /48 within its bound however many /64s it spreads over", func() {
		for n := range 100 {
			for u := range 20 {
				blockKey(ratelimit.Key("2001:db8:7:"+strconv.Itoa(n)+"::", "user:"+strconv.Itoa(u)), t0)
			}
		}
		Expect(l.Len()).To(BeNumerically("<=", 10), "8 blocked keys and the overflow slot")
		Expect(l.Blocked(ratelimit.Key("2001:db8:7:ffff::", "user:new"), t0)).To(BeNumerically(">", 0))
		Expect(l.Blocked(ratelimit.Key("2001:db8:8::", "user:new"), t0)).To(BeZero())
	})

	It("limits every key of a family whose parts are not addresses, however many are blocked", func() {
		for u := range 30 {
			k := ratelimit.Key("user-"+strconv.Itoa(u), "change-password")
			blockKey(k, t0)
			Expect(l.Blocked(k, t0)).To(BeNumerically(">", 0), "user %d", u)
		}
	})

	It("refuses nobody else when many addresses fill the blocked set", func() {
		for a := range 12600 {
			addr := "10." + strconv.Itoa(a/250%250) + "." + strconv.Itoa(a/62500) + "." + strconv.Itoa(a%250+1)
			for u := range 8 {
				blockKey(ratelimit.Key(addr, "user:"+strconv.Itoa(u)), t0)
			}
		}
		fresh := "198.51.100.1"
		for _, part := range []string{"", "all-users", "user:admin", "abcdefgh"} {
			Expect(l.Blocked(ratelimit.Key(fresh, part), t0)).To(BeZero(), part)
		}
		blockKey(ratelimit.Key(fresh, "user:admin"), t0)
		Expect(l.Blocked(ratelimit.Key(fresh, "user:admin"), t0)).To(BeNumerically(">", 0), "a full set drops the soonest block to make room")
	})

	It("keeps the credential name out of the key", func() {
		Expect(ratelimit.Key("192.0.2.1", "user:admin")).NotTo(ContainSubstring("admin"))
	})
})
