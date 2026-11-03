// SPDX-License-Identifier: Apache-2.0

package orgsettings_test

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/orgsettings"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

var _ = Describe("NormalizeHost", func() {
	DescribeTable("accepts one DNS host name and refuses anything else",
		func(raw, want string) {
			got, err := orgsettings.NormalizeHost(raw)
			if want == "" {
				var inv *orgsettings.InvalidError
				Expect(errors.As(err, &inv)).To(BeTrue(), "%q should be refused", raw)
				return
			}
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(want))
		},
		Entry("plain", "docs.example.com", "docs.example.com"),
		Entry("case and a trailing dot", " Docs.Example.COM. ", "docs.example.com"),
		Entry("a single label", "intranet", ""),
		Entry("localhost", "localhost", ""),
		Entry("a decimal IPv4 as one number", "2130706433", ""),
		Entry("a shorthand IPv4", "127.1", ""),
		Entry("a dotted IPv4 under a name", "example.0", ""),
		Entry("an all-numeric last label", "docs.example.123", ""),
		Entry("a hex last label", "docs.0x7f", ""),
		Entry("a bare 0x last label", "docs.0x", ""),
		Entry("a hex-looking label that is a name", "docs.0xample.com", "docs.0xample.com"),
		Entry("a numeric first label", "1.example.com", "1.example.com"),
		Entry("a Unicode name, stored as punycode", "Bücher.Example.com", "xn--bcher-kva.example.com"),
		Entry("a Unicode name whose label is too long", "ü"+strings.Repeat("a", 70)+".example.com", ""),
		Entry("a URL", "https://docs.example.com/a", ""),
		Entry("a port", "docs.example.com:8443", ""),
		Entry("a path", "docs.example.com/a", ""),
		Entry("userinfo", "me@docs.example.com", ""),
		Entry("a wildcard", "*.example.com", ""),
		Entry("an IPv4 address", "10.0.0.7", ""),
		Entry("an IPv6 address", "::1", ""),
		Entry("empty", "  ", ""),
		Entry("an empty label", "a..example.com", ""),
		Entry("a leading hyphen", "-a.example.com", ""),
		Entry("an underscore", "a_b.example.com", ""),
		Entry("too long", string(make([]byte, 300)), ""),
	)
})

var _ = Describe("NormalizeTrustedEntry", func() {
	DescribeTable("accepts a host or *. and a host, and nothing else",
		func(raw, want string) {
			got, err := orgsettings.NormalizeTrustedEntry(raw)
			if want == "" {
				var inv *orgsettings.InvalidError
				Expect(errors.As(err, &inv)).To(BeTrue(), "%q should be refused", raw)
				return
			}
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(want))
		},
		Entry("an exact host", "Docs.Example.com", "docs.example.com"),
		Entry("a wildcard", "*.Example.com", "*.example.com"),
		Entry("a wildcard with spaces", " *.example.com ", "*.example.com"),
		Entry("a wildcard over a multi-label domain", "*.docs.example.co.uk", "*.docs.example.co.uk"),
		Entry("a wildcard over a Unicode name", "*.Bücher.example", "*.xn--bcher-kva.example"),
		Entry("a lone star", "*", ""),
		Entry("a star dot with nothing after it", "*.", ""),
		Entry("a star in the middle", "docs.*.example.com", ""),
		Entry("a star inside a label", "*docs.example.com", ""),
		Entry("a second star", "*.*.example.com", ""),
		Entry("a star after the host", "example.com.*", ""),
		Entry("a wildcard over a TLD", "*.com", ""),
		Entry("a wildcard over a country second-level suffix", "*.co.uk", ""),
		Entry("a wildcard over a private suffix", "*.blogspot.com", ""),
		Entry("a wildcard over a single label", "*.intranet", ""),
		Entry("a wildcard over an IPv4-like name", "*.127.1", ""),
		Entry("a wildcard over an IP address", "*.10.0.0.1", ""),
		Entry("a wildcard with a port", "*.example.com:443", ""),
		Entry("a wildcard URL", "https://*.example.com", ""),
		Entry("a wildcard with a path", "*.example.com/a", ""),
	)
})

var _ = Describe("LeakGuard.TrustsHost", func() {
	It("matches *.example.com on any subdomain at any depth, but not the apex or a lookalike", func() {
		l := orgsettings.LeakGuard{TrustedHosts: []string{"*.example.com"}}
		Expect(l.TrustsHost("a.example.com")).To(BeTrue())
		Expect(l.TrustsHost("A.B.Example.com.")).To(BeTrue())
		Expect(l.TrustsHost("x.y.z.example.com")).To(BeTrue())
		Expect(l.TrustsHost("example.com")).To(BeFalse(), "the apex needs its own entry")
		Expect(l.TrustsHost("evilexample.com")).To(BeFalse())
		Expect(l.TrustsHost("a.evilexample.com")).To(BeFalse())
		Expect(l.TrustsHost("example.com.evil.net")).To(BeFalse())
		Expect(l.TrustsHost("a.example.com.evil.net")).To(BeFalse())
		Expect(l.TrustsHost("a.example.org")).To(BeFalse())
		Expect(l.TrustsHost(".example.com")).To(BeFalse())
		Expect(l.TrustsHost("")).To(BeFalse())
	})

	It("trusts the apex when it is listed as well, and keeps exact entries exact", func() {
		l := orgsettings.LeakGuard{TrustedHosts: []string{"*.example.com", "example.com", "docs.example.org"}}
		Expect(l.TrustsHost("example.com")).To(BeTrue())
		Expect(l.TrustsHost("a.example.com")).To(BeTrue())
		Expect(l.TrustsHost("docs.example.org")).To(BeTrue())
		Expect(l.TrustsHost("a.docs.example.org")).To(BeFalse(), "an exact entry does not cover subdomains")
	})

	It("matches the host exactly, not by suffix or substring", func() {
		l := orgsettings.LeakGuard{TrustedHosts: []string{"docs.example.com"}}
		Expect(l.TrustsHost("docs.example.com")).To(BeTrue())
		Expect(l.TrustsHost("DOCS.example.com.")).To(BeTrue())
		Expect(l.TrustsHost("evil.docs.example.com")).To(BeFalse())
		Expect(l.TrustsHost("example.com")).To(BeFalse())
		Expect(l.TrustsHost("docs.example.com.evil.net")).To(BeFalse())
		Expect(l.TrustsHost("notdocs.example.com")).To(BeFalse())
		Expect(l.TrustsHost("")).To(BeFalse())
		Expect(orgsettings.LeakGuard{}.TrustsHost("docs.example.com")).To(BeFalse())
	})
})

var _ = Describe("the leak-guard settings", func() {
	var (
		svc   *orgsettings.Service
		q     *store.Queries
		org   pgtype.UUID
		actor audit.Actor
	)
	BeforeEach(func(ctx SpecContext) {
		svc, q = orgsettings.NewService(pool), store.New(pool)
		var user pgtype.UUID
		org, user, _ = pgtest.NewOrg(ctx, pool)
		actor = audit.User(user)
	})

	events := func(ctx SpecContext, org pgtype.UUID, action string) []store.AuditEvent {
		GinkgoHelper()
		rows, err := q.ListAuditEvents(ctx, store.ListAuditEventsParams{OrgID: org, PageSize: 100})
		Expect(err).NotTo(HaveOccurred())
		var out []store.AuditEvent
		for _, r := range rows {
			if r.Action == action {
				out = append(out, r)
			}
		}
		return out
	}

	It("starts with the default threshold of 24 and an empty fetch-trusted list", func(ctx SpecContext) {
		l, err := svc.LeakGuard(ctx, org)
		Expect(err).NotTo(HaveOccurred())
		Expect(l.MatchThreshold).To(Equal(24))
		Expect(l.MatchThreshold).To(Equal(orgsettings.DefaultMatchThreshold))
		Expect(l.TrustedHosts).To(BeEmpty())
		Expect(l.TrustedHosts).NotTo(BeNil())
	})

	Describe("the match threshold", func() {
		It("is set, audited, kept by the latency overrides and returned to the default", func(ctx SpecContext) {
			n := 40
			Expect(svc.SetMatchThreshold(ctx, org, actor, &n)).To(Succeed())
			l, err := svc.LeakGuard(ctx, org)
			Expect(err).NotTo(HaveOccurred())
			Expect(l.MatchThreshold).To(Equal(40))

			By("the audit row carries the before and after")
			ev := events(ctx, org, "org.leak_guard.threshold.set")
			Expect(ev).To(HaveLen(1))
			var sum struct{ Before, After map[string]any }
			Expect(json.Unmarshal(ev[0].Summary, &sum)).To(Succeed())
			Expect(sum.Before).To(HaveKeyWithValue("matchThreshold", BeNumerically("==", 24)))
			Expect(sum.After).To(HaveKeyWithValue("matchThreshold", BeNumerically("==", 40)))

			By("Set of the latency overrides leaves it alone")
			idle := 5 * time.Minute
			Expect(orgsettings.Set(ctx, q, org, orgsettings.Override{IdleTimeout: &idle})).To(Succeed())
			l, err = svc.LeakGuard(ctx, org)
			Expect(err).NotTo(HaveOccurred())
			Expect(l.MatchThreshold).To(Equal(40))

			By("and the threshold leaves the latency overrides alone")
			m := 30
			Expect(svc.SetMatchThreshold(ctx, org, actor, &m)).To(Succeed())
			s, err := orgsettings.Get(ctx, q, org, orgsettings.Defaults())
			Expect(err).NotTo(HaveOccurred())
			Expect(s.IdleTimeout).To(Equal(idle))

			By("nil returns it to the default")
			Expect(svc.SetMatchThreshold(ctx, org, actor, nil)).To(Succeed())
			l, err = svc.LeakGuard(ctx, org)
			Expect(err).NotTo(HaveOccurred())
			Expect(l.MatchThreshold).To(Equal(24))
			Expect(events(ctx, org, "org.leak_guard.threshold.set")).To(HaveLen(3))
		})

		It("does not audit a change that changes nothing", func(ctx SpecContext) {
			n := 24
			Expect(svc.SetMatchThreshold(ctx, org, actor, &n)).To(Succeed())
			Expect(svc.SetMatchThreshold(ctx, org, actor, nil)).To(Succeed())
			Expect(events(ctx, org, "org.leak_guard.threshold.set")).To(BeEmpty())
		})

		DescribeTable("refuses a value the guard cannot use",
			func(ctx SpecContext, n int) {
				err := svc.SetMatchThreshold(ctx, org, actor, &n)
				var inv *orgsettings.InvalidError
				Expect(errors.As(err, &inv)).To(BeTrue())
				l, err := svc.LeakGuard(ctx, org)
				Expect(err).NotTo(HaveOccurred())
				Expect(l.MatchThreshold).To(Equal(24))
			},
			Entry("below the minimum", 11),
			Entry("zero", 0),
			Entry("negative", -1),
			Entry("above the maximum", 257),
		)
	})

	Describe("the fetch-trusted host list", func() {
		It("adds and removes hosts, sorted, audited, each change once", func(ctx SpecContext) {
			got, err := svc.AddTrustedHost(ctx, org, actor, " Docs.Example.com ")
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal("docs.example.com"))
			_, err = svc.AddTrustedHost(ctx, org, actor, "api.example.org")
			Expect(err).NotTo(HaveOccurred())

			By("adding a host that is on the list changes and audits nothing")
			_, err = svc.AddTrustedHost(ctx, org, actor, "DOCS.example.com")
			Expect(err).NotTo(HaveOccurred())
			Expect(events(ctx, org, "org.fetch_trusted_host.add")).To(HaveLen(2))

			l, err := svc.LeakGuard(ctx, org)
			Expect(err).NotTo(HaveOccurred())
			Expect(l.TrustedHosts).To(Equal([]string{"api.example.org", "docs.example.com"}))
			Expect(l.TrustsHost("docs.example.com")).To(BeTrue())

			Expect(svc.RemoveTrustedHost(ctx, org, actor, "docs.example.com")).To(Succeed())
			l, err = svc.LeakGuard(ctx, org)
			Expect(err).NotTo(HaveOccurred())
			Expect(l.TrustedHosts).To(Equal([]string{"api.example.org"}))
			Expect(l.TrustsHost("docs.example.com")).To(BeFalse())

			ev := events(ctx, org, "org.fetch_trusted_host.remove")
			Expect(ev).To(HaveLen(1))
			Expect(ev[0].TargetID).To(Equal("docs.example.com"))

			By("removing a host that is not on the list is not found, and not audited")
			err = svc.RemoveTrustedHost(ctx, org, actor, "docs.example.com")
			Expect(errors.Is(err, orgsettings.ErrNotFound)).To(BeTrue())
			Expect(events(ctx, org, "org.fetch_trusted_host.remove")).To(HaveLen(1))
		})

		It("refuses something that is not a host name", func(ctx SpecContext) {
			for _, bad := range []string{"https://docs.example.com/x", "*", "*.com", "localhost", "10.1.2.3", ""} {
				_, err := svc.AddTrustedHost(ctx, org, actor, bad)
				var inv *orgsettings.InvalidError
				Expect(errors.As(err, &inv)).To(BeTrue(), "%q", bad)
			}
			l, err := svc.LeakGuard(ctx, org)
			Expect(err).NotTo(HaveOccurred())
			Expect(l.TrustedHosts).To(BeEmpty())
		})

		It("holds a bounded number of hosts", func(ctx SpecContext) {
			for i := range orgsettings.MaxTrustedHosts {
				_, err := svc.AddTrustedHost(ctx, org, actor, "h"+itoa(i)+".example.com")
				Expect(err).NotTo(HaveOccurred())
			}
			_, err := svc.AddTrustedHost(ctx, org, actor, "one-more.example.com")
			var inv *orgsettings.InvalidError
			Expect(errors.As(err, &inv)).To(BeTrue())
			By("a host already on the full list is still accepted")
			_, err = svc.AddTrustedHost(ctx, org, actor, "h0.example.com")
			Expect(err).NotTo(HaveOccurred())
		})

		It("stores and removes a wildcard entry, and refuses a wildcard over a public suffix", func(ctx SpecContext) {
			got, err := svc.AddTrustedHost(ctx, org, actor, "*.Example.com")
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal("*.example.com"))
			_, err = svc.AddTrustedHost(ctx, org, actor, "*.co.uk")
			var inv *orgsettings.InvalidError
			Expect(errors.As(err, &inv)).To(BeTrue())
			l, err := svc.LeakGuard(ctx, org)
			Expect(err).NotTo(HaveOccurred())
			Expect(l.TrustedHosts).To(Equal([]string{"*.example.com"}))
			Expect(l.TrustsHost("a.b.example.com")).To(BeTrue())
			Expect(l.TrustsHost("example.com")).To(BeFalse())
			Expect(svc.RemoveTrustedHost(ctx, org, actor, "*.example.com")).To(Succeed())
			l, err = svc.LeakGuard(ctx, org)
			Expect(err).NotTo(HaveOccurred())
			Expect(l.TrustedHosts).To(BeEmpty())
		})

		It("never holds more than the cap when adds run in parallel", func(ctx SpecContext) {
			const room, parallel = 3, 16
			for i := range orgsettings.MaxTrustedHosts - room {
				_, err := q.AddOrgFetchTrustedHost(ctx, store.AddOrgFetchTrustedHostParams{OrgID: org, Host: "seed" + itoa(i) + ".example.com"})
				Expect(err).NotTo(HaveOccurred())
			}
			var wg sync.WaitGroup
			var ok, refused atomic.Int32
			start := make(chan struct{})
			for i := range parallel {
				wg.Add(1)
				go func() {
					defer GinkgoRecover()
					defer wg.Done()
					<-start
					_, err := svc.AddTrustedHost(ctx, org, actor, "par"+itoa(i)+".example.com")
					var inv *orgsettings.InvalidError
					switch {
					case err == nil:
						ok.Add(1)
					case errors.As(err, &inv):
						refused.Add(1)
					default:
						Expect(err).NotTo(HaveOccurred())
					}
				}()
			}
			close(start)
			wg.Wait()
			l, err := svc.LeakGuard(ctx, org)
			Expect(err).NotTo(HaveOccurred())
			Expect(l.TrustedHosts).To(HaveLen(orgsettings.MaxTrustedHosts))
			Expect(ok.Load()).To(BeEquivalentTo(room))
			Expect(refused.Load()).To(BeEquivalentTo(parallel - room))
		})

		It("is scoped to the org", func(ctx SpecContext) {
			other, otherUser, _ := pgtest.NewOrg(ctx, pool)
			_, err := svc.AddTrustedHost(ctx, org, actor, "docs.example.com")
			Expect(err).NotTo(HaveOccurred())

			l, err := svc.LeakGuard(ctx, other)
			Expect(err).NotTo(HaveOccurred())
			Expect(l.TrustedHosts).To(BeEmpty())
			err = svc.RemoveTrustedHost(ctx, other, audit.User(otherUser), "docs.example.com")
			Expect(errors.Is(err, orgsettings.ErrNotFound)).To(BeTrue())

			n := 50
			Expect(svc.SetMatchThreshold(ctx, other, audit.User(otherUser), &n)).To(Succeed())
			l, err = svc.LeakGuard(ctx, org)
			Expect(err).NotTo(HaveOccurred())
			Expect(l.MatchThreshold).To(Equal(24))
			Expect(l.TrustedHosts).To(Equal([]string{"docs.example.com"}))
			Expect(events(ctx, other, "org.fetch_trusted_host.add")).To(BeEmpty())
		})
	})
})

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}
