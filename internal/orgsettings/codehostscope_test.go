// SPDX-License-Identifier: Apache-2.0

package orgsettings_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/orgsettings"
	"github.com/stateracloud/koinon/internal/sinkclass"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

var _ = Describe("the org's code-host scope (ADR 0091, koinon-private#350)", func() {
	var (
		svc   *orgsettings.Service
		q     *store.Queries
		org   pgtype.UUID
		actor audit.Actor
		conn  string
	)
	newConn := func(ctx SpecContext, org pgtype.UUID) string {
		GinkgoHelper()
		id := pgtype.UUID{Bytes: uuid.New(), Valid: true}
		_, err := q.CreateConnection(ctx, store.CreateConnectionParams{
			OrgID: org, ID: id, Connector: "github", DisplayName: "gh", Config: []byte(`{}`),
			SecretName: pgtype.Text{String: "conn-" + id.String(), Valid: true},
		})
		Expect(err).NotTo(HaveOccurred())
		return id.String()
	}
	BeforeEach(func(ctx SpecContext) {
		svc, q = orgsettings.NewService(pool), store.New(pool)
		var user pgtype.UUID
		org, user, _ = pgtest.NewOrg(ctx, pool)
		actor = audit.User(user)
		conn = newConn(ctx, org)
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
	now := time.Now()

	It("is empty for a new org, and then no code-host write is internal", func(ctx SpecContext) {
		l, err := svc.LeakGuard(ctx, org)
		Expect(err).NotTo(HaveOccurred())
		Expect(l.CodeHostRepos).To(BeEmpty())
		Expect(l.CodeHostRepos).NotTo(BeNil())
		Expect(l.Scope().Internal(conn, "northwind", "platform-handbook", now)).To(BeFalse())
		Expect(l.ScopeSummary(now).Repositories).To(BeZero())
		Expect(l.ScopeSummary(now).OldestCheckedAt).To(BeNil())
	})

	It("replaces a connection's repositories and lower-cases the names", func(ctx SpecContext) {
		Expect(svc.ReplaceCodeHostRepos(ctx, org, actor, conn, []orgsettings.CodeHostRepo{
			{Owner: "Northwind", Name: "Platform-Handbook", Visibility: "private"},
			{Owner: "northwind", Name: "website", Visibility: "public"},
			{Owner: "northwind", Name: "old", Visibility: "unknown"},
		}, now)).To(Succeed())
		l, err := svc.LeakGuard(ctx, org)
		Expect(err).NotTo(HaveOccurred())
		Expect(l.CodeHostRepos).To(HaveLen(3))
		Expect(l.Scope().Internal(conn, "northwind", "platform-handbook", now)).To(BeTrue())
		Expect(l.Scope().Internal(conn, "northwind", "website", now)).To(BeFalse())
		Expect(l.Scope().Internal(conn, "northwind", "old", now)).To(BeFalse(), "unknown visibility is not internal")

		By("a second report is the whole list: a repository it leaves out leaves the scope, a changed one is updated")
		Expect(svc.ReplaceCodeHostRepos(ctx, org, actor, conn, []orgsettings.CodeHostRepo{
			{Owner: "northwind", Name: "platform-handbook", Visibility: "public"},
		}, now)).To(Succeed())
		l, err = svc.LeakGuard(ctx, org)
		Expect(err).NotTo(HaveOccurred())
		Expect(l.CodeHostRepos).To(HaveLen(1))
		Expect(l.Scope().Internal(conn, "northwind", "platform-handbook", now)).To(BeFalse(), "it turned public")

		By("an empty report empties the connection's part of the scope")
		Expect(svc.ReplaceCodeHostRepos(ctx, org, actor, conn, nil, now)).To(Succeed())
		l, _ = svc.LeakGuard(ctx, org)
		Expect(l.CodeHostRepos).To(BeEmpty())
	})

	It("keeps the connections apart", func(ctx SpecContext) {
		second := newConn(ctx, org)
		Expect(svc.ReplaceCodeHostRepos(ctx, org, actor, conn, []orgsettings.CodeHostRepo{{Owner: "a", Name: "one", Visibility: "private"}}, now)).To(Succeed())
		Expect(svc.ReplaceCodeHostRepos(ctx, org, actor, second, []orgsettings.CodeHostRepo{{Owner: "a", Name: "two", Visibility: "private"}}, now)).To(Succeed())
		Expect(svc.ReplaceCodeHostRepos(ctx, org, actor, conn, nil, now)).To(Succeed())
		l, _ := svc.LeakGuard(ctx, org)
		Expect(l.CodeHostRepos).To(HaveLen(1))
		Expect(l.CodeHostRepos[0].Name).To(Equal("two"))
	})

	It("treats a reading older than the window as unknown", func(ctx SpecContext) {
		Expect(svc.ReplaceCodeHostRepos(ctx, org, actor, conn, []orgsettings.CodeHostRepo{{Owner: "a", Name: "b", Visibility: "private"}}, now.Add(-2*time.Hour))).To(Succeed())
		l, _ := svc.LeakGuard(ctx, org)
		Expect(l.Scope().Internal(conn, "a", "b", now)).To(BeFalse())
		s := l.ScopeSummary(now)
		Expect(s.Private).To(Equal(1))
		Expect(s.Stale).To(Equal(1))
		Expect(s.OldestCheckedAt).NotTo(BeNil())

		Expect(svc.ReplaceCodeHostRepos(ctx, org, actor, conn, []orgsettings.CodeHostRepo{{Owner: "a", Name: "b", Visibility: "private"}}, now)).To(Succeed())
		l, _ = svc.LeakGuard(ctx, org)
		Expect(l.Scope().Internal(conn, "a", "b", now)).To(BeTrue())
		Expect(l.ScopeSummary(now).Stale).To(BeZero())
	})

	It("summarises by visibility", func(ctx SpecContext) {
		Expect(svc.ReplaceCodeHostRepos(ctx, org, actor, conn, []orgsettings.CodeHostRepo{
			{Owner: "a", Name: "p1", Visibility: "private"},
			{Owner: "a", Name: "p2", Visibility: "private"},
			{Owner: "a", Name: "i", Visibility: "internal"},
			{Owner: "a", Name: "pub", Visibility: "public"},
			{Owner: "a", Name: "u", Visibility: "unknown"},
		}, now)).To(Succeed())
		l, _ := svc.LeakGuard(ctx, org)
		s := l.ScopeSummary(now)
		Expect([]int{s.Repositories, s.Private, s.Internal, s.Public, s.Unknown}).To(Equal([]int{5, 2, 1, 1, 1}))
	})

	It("audits a replacement with counts, not the list", func(ctx SpecContext) {
		Expect(svc.ReplaceCodeHostRepos(ctx, org, actor, conn, []orgsettings.CodeHostRepo{
			{Owner: "a", Name: "p", Visibility: "private"}, {Owner: "a", Name: "pub", Visibility: "public"},
		}, now)).To(Succeed())
		ev := events(ctx, org, "org.code_host_scope.replace")
		Expect(ev).To(HaveLen(1))
		Expect(ev[0].TargetID).To(Equal(conn))
		var sum struct{ Before, After map[string]any }
		Expect(json.Unmarshal(ev[0].Summary, &sum)).To(Succeed())
		Expect(sum.Before).To(HaveKeyWithValue("repositories", BeNumerically("==", 0)))
		Expect(sum.After).To(HaveKeyWithValue("repositories", BeNumerically("==", 2)))
		Expect(sum.After).To(HaveKeyWithValue("public", BeNumerically("==", 1)))
		Expect(string(ev[0].Summary)).NotTo(ContainSubstring(`"p"`))
	})

	It("audits the system's refresh only on a change, and a person's sync always", func(ctx SpecContext) {
		repos := []orgsettings.CodeHostRepo{{Owner: "a", Name: "p", Visibility: "private"}, {Owner: "a", Name: "pub", Visibility: "public"}}
		sys := audit.System("koinon-test:spec")
		Expect(svc.ReplaceCodeHostRepos(ctx, org, sys, conn, repos, now.Add(-3*time.Minute))).To(Succeed())
		Expect(events(ctx, org, "org.code_host_scope.replace")).To(HaveLen(1), "the first reading is a change")

		Expect(svc.ReplaceCodeHostRepos(ctx, org, sys, conn, repos, now.Add(-2*time.Minute))).To(Succeed())
		Expect(events(ctx, org, "org.code_host_scope.replace")).To(HaveLen(1), "the same list, read again, is not audited")
		l, _ := svc.LeakGuard(ctx, org)
		Expect(l.ScopeSummary(now).OldestCheckedAt).To(HaveValue(BeTemporally("~", now.Add(-2*time.Minute), time.Millisecond)), "but its freshness is stored")

		Expect(svc.ReplaceCodeHostRepos(ctx, org, actor, conn, repos, now.Add(-time.Minute))).To(Succeed())
		Expect(events(ctx, org, "org.code_host_scope.replace")).To(HaveLen(2), "a person's sync is always audited")

		changed := []orgsettings.CodeHostRepo{{Owner: "a", Name: "p", Visibility: "public"}, {Owner: "a", Name: "pub", Visibility: "public"}}
		Expect(svc.ReplaceCodeHostRepos(ctx, org, sys, conn, changed, now)).To(Succeed())
		Expect(events(ctx, org, "org.code_host_scope.replace")).To(HaveLen(3), "a visibility that changed is")
		Expect(svc.ReplaceCodeHostRepos(ctx, org, sys, conn, changed[:1], now)).To(Succeed())
		Expect(events(ctx, org, "org.code_host_scope.replace")).To(HaveLen(4), "and so is a repository that left")
	})

	It("keeps the newer reading when an older one arrives after it", func(ctx SpecContext) {
		newer := []orgsettings.CodeHostRepo{{Owner: "a", Name: "b", Visibility: "public"}}
		older := []orgsettings.CodeHostRepo{{Owner: "a", Name: "b", Visibility: "private"}, {Owner: "a", Name: "c", Visibility: "private"}}
		Expect(svc.ReplaceCodeHostRepos(ctx, org, actor, conn, newer, now)).To(Succeed())
		Expect(svc.ReplaceCodeHostRepos(ctx, org, actor, conn, older, now.Add(-time.Minute))).To(Succeed(), "dropped, not an error")
		l, _ := svc.LeakGuard(ctx, org)
		Expect(l.CodeHostRepos).To(HaveLen(1))
		Expect(l.Scope().Internal(conn, "a", "b", now)).To(BeFalse(), "the older private reading must not win over the newer public one")
		Expect(events(ctx, org, "org.code_host_scope.replace")).To(HaveLen(1), "a dropped reading is not audited")
	})

	It("ends with the newest reading whatever order concurrent replacements finish in", func(ctx SpecContext) {
		const n = 12
		errs := make(chan error, n)
		for i := range n {
			go func() {
				defer GinkgoRecover()
				// Reading i lists i+1 repositories, read i seconds after the first.
				var repos []orgsettings.CodeHostRepo
				for j := range i + 1 {
					repos = append(repos, orgsettings.CodeHostRepo{Owner: "a", Name: fmt.Sprintf("r%02d", j), Visibility: "private"})
				}
				errs <- svc.ReplaceCodeHostRepos(ctx, org, actor, conn, repos, now.Add(time.Duration(i-n)*time.Second))
			}()
		}
		for range n {
			Expect(<-errs).To(Succeed())
		}
		l, _ := svc.LeakGuard(ctx, org)
		Expect(l.CodeHostRepos).To(HaveLen(n), "the newest reading lists n repositories")
		for _, r := range l.CodeHostRepos {
			Expect(r.CheckedAt).To(BeTemporally("~", now.Add(-time.Second), time.Millisecond))
		}
	})

	DescribeTable("refuses what it cannot store, and changes nothing",
		func(ctx SpecContext, repos []orgsettings.CodeHostRepo) {
			Expect(svc.ReplaceCodeHostRepos(ctx, org, actor, conn, []orgsettings.CodeHostRepo{{Owner: "keep", Name: "me", Visibility: "private"}}, now)).To(Succeed())
			err := svc.ReplaceCodeHostRepos(ctx, org, actor, conn, repos, now)
			var inv *orgsettings.InvalidError
			Expect(errors.As(err, &inv)).To(BeTrue(), "%v", err)
			l, _ := svc.LeakGuard(ctx, org)
			Expect(l.CodeHostRepos).To(HaveLen(1))
			Expect(l.CodeHostRepos[0].Owner).To(Equal("keep"))
		},
		Entry("an unknown visibility word", []orgsettings.CodeHostRepo{{Owner: "a", Name: "b", Visibility: "secret"}}),
		Entry("an empty visibility", []orgsettings.CodeHostRepo{{Owner: "a", Name: "b"}}),
		Entry("an empty owner", []orgsettings.CodeHostRepo{{Name: "b", Visibility: "private"}}),
		Entry("a slash in the name", []orgsettings.CodeHostRepo{{Owner: "a", Name: "b/c", Visibility: "private"}}),
		Entry("a non-ASCII name that lower-cases to an ASCII one (U+212A)", []orgsettings.CodeHostRepo{{Owner: "\u212Aorg", Name: "b", Visibility: "private"}}),
		Entry("the same repository twice, in another case", []orgsettings.CodeHostRepo{{Owner: "a", Name: "b", Visibility: "private"}, {Owner: "A", Name: "B", Visibility: "private"}}),
	)

	It("trusts an internationalised host through the real list, in either spelling", func(ctx SpecContext) {
		stored, err := svc.AddTrustedHost(ctx, org, actor, "*.m\u00fcnchen.example.com")
		Expect(err).NotTo(HaveOccurred())
		Expect(stored).To(Equal("*.xn--mnchen-3ya.example.com"))
		_, err = svc.AddTrustedHost(ctx, org, actor, "b\u00fccher.example.org")
		Expect(err).NotTo(HaveOccurred())
		l, _ := svc.LeakGuard(ctx, org)
		fetch := sinkclass.Declaration{Class: sinkclass.ExternalHigh, Derivation: sinkclass.Fetch, AddressArgs: []string{"url"}}
		get := func(u string) sinkclass.Class {
			return sinkclass.Classify(fetch, map[string]string{"url": u}, conn, l.Scope(), l, now)
		}
		Expect(get("https://shop.m\u00fcnchen.example.com/")).To(Equal(sinkclass.Internal))
		Expect(get("https://shop.xn--mnchen-3ya.example.com/")).To(Equal(sinkclass.Internal))
		Expect(get("https://m\u00fcnchen.example.com/")).To(Equal(sinkclass.ExternalLow), "the wildcard does not cover its apex")
		Expect(get("https://B\u00dcCHER.example.org/")).To(Equal(sinkclass.Internal))
		Expect(get("https://xn--bcher-kva.example.org/")).To(Equal(sinkclass.Internal))
		Expect(get("https://b\u00fccher.example.net/")).To(Equal(sinkclass.ExternalLow))
	})

	It("refuses a reading dated in the future, and keeps the old one", func(ctx SpecContext) {
		Expect(svc.ReplaceCodeHostRepos(ctx, org, actor, conn, []orgsettings.CodeHostRepo{{Owner: "a", Name: "b", Visibility: "private"}}, now)).To(Succeed())
		err := svc.ReplaceCodeHostRepos(ctx, org, actor, conn, []orgsettings.CodeHostRepo{{Owner: "a", Name: "c", Visibility: "private"}}, time.Now().Add(24*time.Hour))
		var inv *orgsettings.InvalidError
		Expect(errors.As(err, &inv)).To(BeTrue(), "%v", err)
		Expect(svc.ReplaceCodeHostRepos(ctx, org, actor, conn, nil, time.Now().Add(sinkclass.MaxClockSkew/2))).To(Succeed(), "a small skew is tolerated")
	})

	It("looks a repository up in the linked connection only", func(ctx SpecContext) {
		second := newConn(ctx, org)
		Expect(svc.ReplaceCodeHostRepos(ctx, org, actor, conn, []orgsettings.CodeHostRepo{{Owner: "northwind", Name: "platform-handbook", Visibility: "private"}}, now)).To(Succeed())
		Expect(svc.ReplaceCodeHostRepos(ctx, org, actor, second, []orgsettings.CodeHostRepo{{Owner: "northwind", Name: "platform-handbook", Visibility: "public"}}, now)).To(Succeed())
		l, _ := svc.LeakGuard(ctx, org)
		Expect(l.Scope().Internal(conn, "northwind", "platform-handbook", now)).To(BeTrue())
		Expect(l.Scope().Internal(second, "northwind", "platform-handbook", now)).To(BeFalse())
		Expect(l.Scope().Internal("", "northwind", "platform-handbook", now)).To(BeFalse())
	})

	It("does not find another org's connection, or one that does not exist", func(ctx SpecContext) {
		other, _, _ := pgtest.NewOrg(ctx, pool)
		theirs := newConn(ctx, other)
		err := svc.ReplaceCodeHostRepos(ctx, org, actor, theirs, []orgsettings.CodeHostRepo{{Owner: "a", Name: "b", Visibility: "private"}}, now)
		Expect(err).To(MatchError(orgsettings.ErrNotFound))
		Expect(svc.ReplaceCodeHostRepos(ctx, org, actor, uuid.NewString(), nil, now)).To(MatchError(orgsettings.ErrNotFound))
		Expect(svc.ReplaceCodeHostRepos(ctx, org, actor, "not-a-uuid", nil, now)).To(MatchError(orgsettings.ErrNotFound))
		l, _ := svc.LeakGuard(ctx, other)
		Expect(l.CodeHostRepos).To(BeEmpty())
	})

	It("is read per org", func(ctx SpecContext) {
		other, otherUser, _ := pgtest.NewOrg(ctx, pool)
		theirs := newConn(ctx, other)
		Expect(svc.ReplaceCodeHostRepos(ctx, other, audit.User(otherUser), theirs, []orgsettings.CodeHostRepo{{Owner: "n", Name: "x", Visibility: "private"}}, now)).To(Succeed())
		l, _ := svc.LeakGuard(ctx, org)
		Expect(l.CodeHostRepos).To(BeEmpty())
	})

	It("feeds the derivation: an org's internal write, a public one, and a trusted wildcard fetch host", func(ctx SpecContext) {
		Expect(svc.ReplaceCodeHostRepos(ctx, org, actor, conn, []orgsettings.CodeHostRepo{
			{Owner: "northwind", Name: "platform-handbook", Visibility: "private"},
			{Owner: "northwind", Name: "website", Visibility: "public"},
		}, now)).To(Succeed())
		_, err := svc.AddTrustedHost(ctx, org, actor, "*.docs.example.com")
		Expect(err).NotTo(HaveOccurred())
		l, err := svc.LeakGuard(ctx, org)
		Expect(err).NotTo(HaveOccurred())

		code := sinkclass.Declaration{Class: sinkclass.ExternalHigh, Derivation: sinkclass.CodeHost, AddressArgs: []string{"owner", "repo"}}
		classify := func(owner, repo string) sinkclass.Class {
			return sinkclass.Classify(code, map[string]string{"owner": owner, "repo": repo}, conn, l.Scope(), l, now)
		}
		Expect(classify("northwind", "platform-handbook")).To(Equal(sinkclass.Internal))
		Expect(classify("northwind", "website")).To(Equal(sinkclass.ExternalHigh))
		Expect(classify("northwind", "not-listed")).To(Equal(sinkclass.ExternalHigh))

		fetch := sinkclass.Declaration{Class: sinkclass.ExternalHigh, Derivation: sinkclass.Fetch, AddressArgs: []string{"url"}}
		get := func(u string) sinkclass.Class {
			return sinkclass.Classify(fetch, map[string]string{"url": u}, conn, l.Scope(), l, now)
		}
		Expect(get("https://a.b.docs.example.com/x?q=1")).To(Equal(sinkclass.Internal), "a wildcard covers any depth")
		Expect(get("https://docs.example.com/")).To(Equal(sinkclass.ExternalLow), "but not the apex")
		Expect(get("https://evil.example.org/")).To(Equal(sinkclass.ExternalLow))
		Expect(get("https://x.docs.example.com\\@evil.example.org/")).To(Equal(sinkclass.ExternalHigh), "a backslash in the authority does not parse (a browser would read it as a slash)")
		Expect(get("https://evil.example.org\\.docs.example.com/")).To(Equal(sinkclass.ExternalHigh))
		Expect(get("https://evil.example.org%2f.docs.example.com/")).To(Equal(sinkclass.ExternalHigh), "a percent-encoded separator in the host does not parse")
		Expect(get("https://docs.example.com%2eevil.example.org/")).To(Equal(sinkclass.ExternalHigh))
		Expect(get("https://A.DOCS.Example.COM./")).To(Equal(sinkclass.Internal), "case and a trailing dot name the same host")
	})
})
