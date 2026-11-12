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

	blockKey := func(k string, at time.Time) {
		for range ratelimit.FreeFails {
			l.Fail(k, at)
		}
	}
	ipv6 := func(group, n int) string { return "2001:db8:" + strconv.Itoa(group) + ":" + strconv.Itoa(n) + "::" }
	ipv4 := func(n int) string {
		return "10." + strconv.Itoa(n/62500) + "." + strconv.Itoa(n/250%250) + "." + strconv.Itoa(n%250+1)
	}

	It("keeps its table within the bound", func() {
		for i := range 200000 {
			l.Fail(ratelimit.Key(ipv4(i), "user:x"), t0)
		}
		Expect(l.Len()).To(BeNumerically("<=", 110000))
	})

	It("resets nothing of a key, an address or a group when fresh keys flood the table", func() {
		blockKey(key, t0)
		for n := range 20 { // the address of key: 5 + 20 failures, 5 short of its budget
			l.Fail(ratelimit.Key("192.0.2.1", "user:"+strconv.Itoa(n)), t0)
		}
		for n := range 100 { // a group: 100 of its 120 failures, over many /64s
			l.Fail(ratelimit.Key(ipv6(1, n), "user:x"), t0)
		}
		want := l.Blocked(key, t0)
		for i := range 10000 {
			l.Fail(ratelimit.Key(ipv4(i), "user:x"), t0)
		}
		Expect(l.Blocked(key, t0)).To(Equal(want), "the key keeps its block")
		for n := range 5 {
			l.Fail(ratelimit.Key("192.0.2.1", "more:"+strconv.Itoa(n)), t0)
		}
		Expect(l.Blocked(ratelimit.Key("192.0.2.1", "new"), t0)).To(BeNumerically(">", 0), "the address kept its count")
		for n := 100; n < 120; n++ {
			l.Fail(ratelimit.Key(ipv6(1, n), "user:x"), t0)
		}
		Expect(l.Blocked(ratelimit.Key(ipv6(1, 999), "new"), t0)).To(BeNumerically(">", 0), "the group kept its count")
	})

	It("refuses a /48 that spreads a few failures over many /64s, and no other group", func() {
		for n := range 40 {
			for range ratelimit.FreeFails - 1 {
				l.Fail(ratelimit.Key(ipv6(2, n), "user:x"), t0)
			}
		}
		Expect(l.Blocked(ratelimit.Key(ipv6(2, 4000), "user:new"), t0)).To(BeNumerically(">", 0))
		Expect(l.Blocked(ratelimit.Key(ipv6(3, 0), "user:new"), t0)).To(BeZero())
		Expect(l.Blocked(ratelimit.Key("203.0.113.9", "abcdefgh"), t0)).To(BeZero())
	})

	It("refuses an address that spreads failures over many credentials, and no other address", func() {
		for n := range ratelimit.AddressFreeFails {
			l.Fail(ratelimit.Key("192.0.2.2", "user:"+strconv.Itoa(n)), t0)
		}
		Expect(l.Blocked(ratelimit.Key("192.0.2.2", "abcdefgh"), t0)).To(BeNumerically(">", 0))
		Expect(l.Blocked(ratelimit.Key("192.0.2.3", "abcdefgh"), t0)).To(BeZero())
	})

	// escalation is a client that waits out each block and fails once more; the wait of its level
	// doubles to MaxBackoff, and a quiet FailWindow starts it afresh.
	escalation := func(fresh func(n int) string, budget int) {
		n := 0
		next := func() string { n++; return fresh(n) }
		probe := fresh(0)
		for range budget - 1 {
			l.Fail(next(), t0)
		}
		now := t0
		var waits []time.Duration
		for range 13 {
			l.Fail(next(), now)
			w := l.Blocked(probe, now)
			waits = append(waits, w)
			now = now.Add(w + time.Millisecond)
		}
		Expect(waits[0]).To(Equal(ratelimit.BaseBackoff))
		Expect(waits[1]).To(Equal(2 * ratelimit.BaseBackoff))
		Expect(waits[len(waits)-1]).To(Equal(ratelimit.MaxBackoff))
		now = now.Add(ratelimit.MaxBackoff + ratelimit.FailWindow + time.Second)
		l.Fail(next(), now)
		Expect(l.Blocked(probe, now)).To(BeZero(), "a quiet window starts the level afresh")
	}

	It("escalates the block of a /48 past its budget", func() {
		escalation(func(n int) string { return ratelimit.Key(ipv6(4, n), "user:"+strconv.Itoa(n)) }, ratelimit.GroupFreeFails)
	})

	It("escalates the block of an IPv4 address past its budget", func() {
		escalation(func(n int) string { return ratelimit.Key("203.0.113.5", "part:"+strconv.Itoa(n)) }, ratelimit.AddressFreeFails)
	})

	It("limits each user of a family whose parts are not addresses", func() {
		for u := range 30 {
			k := ratelimit.Key("user-"+strconv.Itoa(u), "change-password")
			blockKey(k, t0)
			Expect(l.Blocked(k, t0)).To(BeNumerically(">", 0), "user %d", u)
		}
	})

	It("keeps the doubling of a key whose block ended when fresh keys flood the table later", func() {
		blockKey(key, t0)
		t1 := t0.Add(5 * time.Second) // the block ended; its quiet window is open
		for i := range 200000 {
			l.Fail(ratelimit.Key(ipv4(i), "user:x"), t1)
		}
		Expect(l.Len()).To(BeNumerically("<=", 110000))
		l.Fail(key, t1)
		Expect(l.Blocked(key, t1)).To(Equal(2*ratelimit.BaseBackoff), "the sixth failure doubles the block")
	})

	It("refuses nobody when the table is full of live entries, and tracks sources again once they expire", func() {
		blockKey(key, t0)
		want := l.Blocked(key, t0)
		for i := range 70000 { // two entries each: past the bound
			l.Fail(ratelimit.Key(ipv4(i), "user:x"), t0)
		}
		fresh := "198.51.100.1"
		for _, part := range []string{"", "user:admin", "abcdefgh"} {
			Expect(l.Blocked(ratelimit.Key(fresh, part), t0)).To(BeZero(), part)
		}
		Expect(l.Blocked(key, t0)).To(Equal(want), "state that exists is kept")
		later := t0.Add(2 * ratelimit.FailWindow)
		blockKey(ratelimit.Key(fresh, "user:admin"), later)
		Expect(l.Blocked(ratelimit.Key(fresh, "user:admin"), later)).To(BeNumerically(">", 0))
	})

	It("checks the own level of a key alone with BlockedKey", func() {
		for n := range ratelimit.AddressFreeFails {
			l.Fail(ratelimit.Key("192.0.2.2", "user:"+strconv.Itoa(n)), t0)
		}
		other := ratelimit.Key("192.0.2.2", "abcdefgh")
		Expect(l.Blocked(other, t0)).To(BeNumerically(">", 0))
		Expect(l.BlockedKey(other, t0)).To(BeZero())
	})

	It("caps the block of a key at the given longest wait", func() {
		for range 40 {
			l.FailCapped(key, 3, time.Minute, t0)
		}
		Expect(l.Blocked(key, t0)).To(Equal(time.Minute))
	})

	It("keeps the credential name out of the key", func() {
		Expect(ratelimit.Key("192.0.2.1", "user:admin")).NotTo(ContainSubstring("admin"))
	})
})
