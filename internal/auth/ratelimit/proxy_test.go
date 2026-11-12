// SPDX-License-Identifier: Apache-2.0

package ratelimit_test

import (
	"net/http"
	"net/http/httptest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/auth/ratelimit"
)

var _ = Describe("TrustedProxies", func() {
	// via sends a request from peer with the given header lines through the middleware and returns
	// the address ClientAddr sees.
	via := func(trusted []string, peer string, hdr ...string) string {
		t, err := ratelimit.ParseTrustedProxies(trusted)
		Expect(err).NotTo(HaveOccurred())
		// the Forwarded header is the configured one when the request carries only that
		if len(hdr) > 0 && hdr[0] == "Forwarded" {
			Expect(t.UseHeader("Forwarded")).To(Succeed())
		}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = peer
		for i := 0; i+1 < len(hdr); i += 2 {
			r.Header.Add(hdr[i], hdr[i+1])
		}
		var got string
		t.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = ratelimit.ClientAddr(r) })).
			ServeHTTP(httptest.NewRecorder(), r)
		return got
	}
	const xff = "X-Forwarded-For"

	It("ignores the headers when no proxy is listed", func() {
		Expect(via(nil, "10.0.0.1:1234", xff, "203.0.113.7")).To(Equal("10.0.0.1"))
		Expect(via([]string{" ", ""}, "10.0.0.1:1234", "Forwarded", "for=203.0.113.7")).To(Equal("10.0.0.1"))
	})
	It("ignores the headers from an untrusted peer", func() {
		Expect(via([]string{"10.0.0.0/8"}, "198.51.100.5:1", xff, "203.0.113.7")).To(Equal("198.51.100.5"))
	})
	It("takes the client from a trusted peer", func() {
		Expect(via([]string{"10.0.0.0/8"}, "10.1.2.3:1", xff, "203.0.113.7")).To(Equal("203.0.113.7"))
		Expect(via([]string{"10.0.0.0/8"}, "10.1.2.3:1", "Forwarded", `for=203.0.113.7;proto=https`)).To(Equal("203.0.113.7"))
		Expect(via([]string{"10.1.2.3"}, "10.1.2.3:1", xff, "203.0.113.7")).To(Equal("203.0.113.7"))
	})
	It("uses the peer when a trusted peer sends no header", func() {
		Expect(via([]string{"10.0.0.0/8"}, "10.1.2.3:1")).To(Equal("10.1.2.3"))
	})
	It("walks right to left and stops at the first untrusted hop", func() {
		trusted := []string{"10.0.0.0/8", "192.168.0.0/16"}
		// 6.6.6.6 is the client's forgery; 203.0.113.7 is what the first proxy saw.
		Expect(via(trusted, "10.0.0.1:1", xff, "6.6.6.6, 203.0.113.7, 192.168.1.1, 10.0.0.9")).To(Equal("203.0.113.7"))
		Expect(via(trusted, "10.0.0.1:1", xff, "6.6.6.6", xff, "203.0.113.7, 10.0.0.9")).To(Equal("203.0.113.7"))
		Expect(via(trusted, "10.0.0.1:1", "Forwarded", `for=6.6.6.6, for=203.0.113.7, for=10.0.0.9`)).To(Equal("203.0.113.7"))
	})
	It("returns the leftmost hop when every hop is trusted", func() {
		Expect(via([]string{"10.0.0.0/8"}, "10.0.0.1:1", xff, "10.0.0.7, 10.0.0.8")).To(Equal("10.0.0.7"))
	})
	It("stops at a malformed hop with the hop to its right", func() {
		trusted := []string{"10.0.0.0/8"}
		Expect(via(trusted, "10.0.0.1:1", xff, "203.0.113.7, garbage, 10.0.0.9")).To(Equal("10.0.0.9"))
		Expect(via(trusted, "10.0.0.1:1", xff, "203.0.113.7, 10.0.0.9, ")).To(Equal("10.0.0.1"))
		Expect(via(trusted, "10.0.0.1:1", xff, "unknown")).To(Equal("10.0.0.1"))
		Expect(via(trusted, "10.0.0.1:1", "Forwarded", `for=_hidden`)).To(Equal("10.0.0.1"))
		Expect(via(trusted, "10.0.0.1:1", "Forwarded", `by=10.0.0.2`)).To(Equal("10.0.0.1"))
		Expect(via(trusted, "10.0.0.1:1", xff, "999.1.1.1")).To(Equal("10.0.0.1"))
	})
	It("reads ports and brackets in hops", func() {
		Expect(via([]string{"10.0.0.0/8"}, "10.0.0.1:1", xff, "203.0.113.7:5555")).To(Equal("203.0.113.7"))
		Expect(via([]string{"10.0.0.0/8"}, "10.0.0.1:1", "Forwarded", `for="203.0.113.7:4711"`)).To(Equal("203.0.113.7"))
		Expect(via([]string{"10.0.0.0/8"}, "10.0.0.1:1", "Forwarded", `for="[2001:db8:1:2:3:4:5:6]:4711"`)).To(Equal("2001:db8:1:2::"))
	})
	It("handles IPv6 peers, hops and networks", func() {
		trusted := []string{"fd00::/8"}
		Expect(via(trusted, "[fd00::1]:1", xff, "2001:db8:1:2:aaaa::1")).To(Equal("2001:db8:1:2::"))
		Expect(via(trusted, "[2001:db8::1]:1", xff, "203.0.113.7")).To(Equal("2001:db8::"))
		Expect(via(trusted, "[fd00::1]:1", xff, "203.0.113.7, fd00::5")).To(Equal("203.0.113.7"))
		// an IPv4 peer arriving as ::ffff:a.b.c.d matches an IPv4 network
		Expect(via([]string{"10.0.0.0/8"}, "[::ffff:10.0.0.1]:1", xff, "203.0.113.7")).To(Equal("203.0.113.7"))
	})
	It("reads only the configured header", func() {
		// default X-Forwarded-For: a client-written Forwarded is ignored
		Expect(via([]string{"10.0.0.0/8"}, "10.0.0.1:1", xff, "203.0.113.7", "Forwarded", "for=6.6.6.6")).To(Equal("203.0.113.7"))
		t, _ := ratelimit.ParseTrustedProxies([]string{"10.0.0.0/8"})
		only := httptest.NewRequest(http.MethodGet, "/", nil)
		only.RemoteAddr = "10.0.0.1:1"
		only.Header.Set("Forwarded", "for=6.6.6.6")
		Expect(t.Resolve(only)).To(Equal("10.0.0.1")) // default mode: a trusted peer's Forwarded alone is ignored
		Expect(t.UseHeader("forwarded")).To(Succeed())
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "10.0.0.1:1"
		r.Header.Set(xff, "6.6.6.6")
		r.Header.Set("Forwarded", "for=203.0.113.7")
		Expect(t.Resolve(r)).To(Equal("203.0.113.7"))
		Expect(t.UseHeader("x-real-ip")).NotTo(Succeed())
	})
	It("reads several Forwarded lines, any case and bracketed IPv6 without a port", func() {
		Expect(via([]string{"10.0.0.0/8"}, "10.0.0.1:1", "Forwarded", "For=6.6.6.6", "Forwarded", `FOR="[2001:db8:1:2::1]", for=10.0.0.9`)).To(Equal("2001:db8:1:2::"))
	})
	It("reads mapped and zoned hops", func() {
		Expect(via([]string{"10.0.0.0/8"}, "10.0.0.1:1", xff, "::ffff:203.0.113.7")).To(Equal("203.0.113.7"))
		Expect(via([]string{"fd00::/8"}, "[fd00::1]:1", xff, "2001:db8::7%eth0")).To(Equal("2001:db8::"))
	})
	It("bounds the hops it reads", func() {
		h := "6.6.6.6"
		for range 40 {
			h += ", 10.0.0.2"
		}
		Expect(via([]string{"10.0.0.0/8"}, "10.0.0.1:1", xff, h)).To(Equal("10.0.0.2"))
	})
	It("refuses a bad list", func() {
		for _, bad := range []string{"10.0.0.0/33", "nonsense", "10.0.0.0/8/9", "::ffff:10.0.0.0/64"} {
			_, err := ratelimit.ParseTrustedProxies([]string{bad})
			Expect(err).To(HaveOccurred(), bad)
		}
	})
})
