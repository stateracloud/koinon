// SPDX-License-Identifier: Apache-2.0

package orgsettings_test

import (
	"encoding/json"
	"errors"
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
		Entry("single label", "intranet", "intranet"),
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

var _ = Describe("LeakGuard.TrustsHost", func() {
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
			for _, bad := range []string{"https://docs.example.com/x", "*.example.com", "10.1.2.3", ""} {
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
