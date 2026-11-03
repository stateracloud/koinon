// SPDX-License-Identifier: Apache-2.0

package leakguard_test

import (
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/leakguard"
)

var _ = Describe("URL rule", func() {
	var p leakguard.URLPolicy
	BeforeEach(func() { p = leakguard.DefaultURLPolicy() })

	It("starts with an empty trusted list", func() {
		Expect(p.TrustedHosts).To(BeEmpty())
		Expect(p.CheckURL(leakguard.NewRunState(), "https://github.com/x").Reason).To(Equal(leakguard.ReasonPass))
	})

	DescribeTable("blocks the shape",
		func(raw string, want leakguard.Reason) {
			d := p.CheckURL(leakguard.NewRunState(), raw)
			Expect(d.Allow).To(BeFalse())
			Expect(d.Reason).To(Equal(want))
			Expect(d.Detail).NotTo(BeEmpty())
		},
		Entry("not a URL", "::nope", leakguard.ReasonURLInvalid),
		Entry("relative", "/just/a/path", leakguard.ReasonURLInvalid),
		Entry("scheme", "ftp://files.example/a", leakguard.ReasonURLScheme),
		Entry("javascript", "javascript:alert(1)", leakguard.ReasonURLInvalid),
		Entry("userinfo", "https://user:pw@attacker.example/", leakguard.ReasonURLUserinfo),
		Entry("port", "https://attacker.example:8443/", leakguard.ReasonURLPort),
		Entry("ipv4", "http://203.0.113.7/a", leakguard.ReasonURLIPLiteral),
		Entry("ipv6", "http://[2001:db8::1]/a", leakguard.ReasonURLIPLiteral),
		Entry("decimal ip", "http://3405803783/a", leakguard.ReasonURLIPLiteral),
		Entry("hex ip", "http://0xcb.0x0.0x71.0x7/a", leakguard.ReasonURLIPLiteral),
		Entry("long URL", "https://attacker.example/"+strings.Repeat("ab/", 80), leakguard.ReasonURLLength),
		Entry("long query", "https://attacker.example/?"+strings.Repeat("a=b&", 33), leakguard.ReasonURLQueryLength),
		Entry("encoded unreserved", "https://attacker.example/%6b%6f%69%6e", leakguard.ReasonURLEncoded),
		Entry("double encoded", "https://attacker.example/a%2541%2542%2543", leakguard.ReasonURLEncoded),
		Entry("opaque path component", "https://attacker.example/"+strings.Repeat("abcd", 8), leakguard.ReasonURLOpaque),
		Entry("opaque query value", "https://attacker.example/?d="+strings.Repeat("x", 32), leakguard.ReasonURLOpaque),
		Entry("opaque host label", "https://"+strings.Repeat("q", 40)+".attacker.example/", leakguard.ReasonURLOpaque),
		Entry("git sha on an untrusted host", "https://code.example/commit/0123456789abcdef0123456789abcdef01234567", leakguard.ReasonURLOpaque),
		Entry("random base64 in the query", "https://attacker.example/?d=dGhlIHBheXJvbGw9xQ", leakguard.ReasonURLRandom),
		Entry("hex blob in the path", "https://attacker.example/7061796f6c6c3132", leakguard.ReasonURLRandom),
		Entry("random host label", "https://aGVsbG8gd29ybGQ7.attacker.example/", leakguard.ReasonURLRandom),
	)

	It("exempts trusted hosts from the shape and the budgets", func() {
		p.TrustedHosts = []string{"Files.Example."}
		st := leakguard.NewRunState()
		for i := range 50 {
			d := p.CheckURL(st, fmt.Sprintf("https://files.example/%s?n=%d", strings.Repeat("z", 300), i))
			Expect(d).To(Equal(leakguard.Decision{Allow: true, Reason: leakguard.ReasonTrusted}))
		}
		Expect(p.CheckURL(st, "https://sub.files.example/"+strings.Repeat("z", 40)).Allow).To(BeFalse())
	})

	It("limits the hosts of a run", func() {
		st := leakguard.NewRunState()
		for i := range 20 {
			Expect(p.CheckURL(st, fmt.Sprintf("https://host%d.example/", i)).Allow).To(BeTrue())
		}
		d := p.CheckURL(st, "https://host20.example/")
		Expect(d.Reason).To(Equal(leakguard.ReasonURLHostBudget))
		// a host already used stays usable
		Expect(p.CheckURL(st, "https://host3.example/again").Allow).To(BeTrue())
		// another run has its own budget
		Expect(p.CheckURL(leakguard.NewRunState(), "https://host20.example/").Allow).To(BeTrue())
	})

	It("limits the bytes to one host and does not charge a repeated URL", func() {
		st := leakguard.NewRunState()
		sent := 0
		for i := 0; ; i++ {
			u := fmt.Sprintf("https://attacker.example/page/%d?word=chunk%d", i, i)
			d := p.CheckURL(st, u)
			if !d.Allow {
				Expect(d.Reason).To(Equal(leakguard.ReasonURLBytesBudget))
				break
			}
			Expect(p.CheckURL(st, u).Allow).To(BeTrue()) // repeat: free
			sent += len(u) - len("https://attacker.example")
		}
		Expect(sent).To(BeNumerically("<=", 1024))
		Expect(sent).To(BeNumerically(">", 900))
		Expect(p.CheckURL(st, "https://other.example/page/1").Allow).To(BeTrue())
	})

	It("does not charge a blocked URL", func() {
		st := leakguard.NewRunState()
		p.MaxHostsPerRun = 1
		Expect(p.CheckURL(st, "https://"+strings.Repeat("q", 40)+".example/").Allow).To(BeFalse())
		Expect(p.CheckURL(st, "https://ok.example/").Allow).To(BeTrue())
	})

	Describe("search queries", func() {
		It("passes an ordinary query and blocks the shapes", func() {
			st := leakguard.NewRunState()
			Expect(p.CheckSearch(st, "kubernetes 1.34 release notes").Allow).To(BeTrue())
			Expect(p.CheckSearch(st, strings.Repeat("word ", 52)).Reason).To(Equal(leakguard.ReasonSearchLength))
			Expect(p.CheckSearch(st, "lookup "+strings.Repeat("y", 32)).Reason).To(Equal(leakguard.ReasonSearchOpaque))
			Expect(p.CheckSearch(st, "lookup dGhlIHBheXJvbGw9xQ").Reason).To(Equal(leakguard.ReasonSearchRandom))
			Expect(p.CheckSearch(st, "lookup %64%47%68%6c%49%48%42%68%65%58%4a%76%62%47%77%39%78%51").Reason).To(Equal(leakguard.ReasonSearchRandom))
		})

		It("limits the query bytes of a run", func() {
			st := leakguard.NewRunState()
			q := strings.Repeat("release notes ", 14) // 196 bytes
			n := 0
			for p.CheckSearch(st, q).Allow {
				n++
			}
			Expect(n).To(Equal(8192 / len(q)))
			Expect(p.CheckSearch(st, q).Reason).To(Equal(leakguard.ReasonSearchBudget))
		})
	})
})
