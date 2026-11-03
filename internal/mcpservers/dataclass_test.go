// SPDX-License-Identifier: Apache-2.0

package mcpservers_test

import (
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

var _ = Describe("Declaration", func() {
	It("puts the legs in canonical order and drops duplicates", func() {
		d, err := mcpservers.Declaration{Kind: "fetch", Legs: []string{"private_data", "untrusted_input", "private_data"}}.Normalize()
		Expect(err).NotTo(HaveOccurred())
		Expect(d.Kind).To(Equal("fetch"))
		Expect(d.Legs).To(Equal([]string{"untrusted_input", "private_data"}))
	})

	It("allows a server that carries no leg", func() {
		d, err := mcpservers.Declaration{Kind: "other"}.Normalize()
		Expect(err).NotTo(HaveOccurred())
		Expect(d.Legs).To(BeEmpty())
		Expect(d.Legs).NotTo(BeNil())
	})

	DescribeTable("refuses what it does not know",
		func(d mcpservers.Declaration) {
			_, err := d.Normalize()
			var inv *catalog.InvalidError
			Expect(errors.As(err, &inv)).To(BeTrue())
		},
		Entry("an unknown kind", mcpservers.Declaration{Kind: "browser"}),
		Entry("an empty kind", mcpservers.Declaration{}),
		Entry("an unknown leg", mcpservers.Declaration{Kind: "other", Legs: []string{"secrets"}}),
		Entry("outbound, which ADR 0091 moved to the sink class of each tool", mcpservers.Declaration{Kind: "other", Legs: []string{"outbound"}}),
		Entry("a leg in another spelling", mcpservers.Declaration{Kind: "other", Legs: []string{"private-data"}}),
	)

	It("is the cautious default: kind other, both legs", func() {
		d := mcpservers.DefaultDeclaration()
		Expect(d.Kind).To(Equal(mcpservers.KindOther))
		Expect(d.Legs).To(ConsistOf(mcpservers.LegUntrustedInput, mcpservers.LegPrivateData))
	})
})

var _ = Describe("the data class of a server (ADR 0084, #350)", func() {
	var (
		svc   *mcpservers.Service
		org   pgtype.UUID
		user  pgtype.UUID
		actor audit.Actor
	)
	BeforeEach(func(ctx SpecContext) {
		svc = mcpservers.New(pool, ring, nil).WithURLPolicy(catalog.URLPolicy{Namespace: "koinon"})
		org, user, _ = pgtest.NewOrg(ctx, pool)
		actor = audit.User(user)
	})
	events := func(ctx SpecContext, action string) []store.AuditEvent {
		GinkgoHelper()
		rows, err := store.New(pool).ListAuditEvents(ctx, store.ListAuditEventsParams{OrgID: org, PageSize: 100})
		Expect(err).NotTo(HaveOccurred())
		var out []store.AuditEvent
		for _, r := range rows {
			if r.Action == action {
				out = append(out, r)
			}
		}
		return out
	}
	create := func(ctx SpecContext, name string, class *mcpservers.Declaration) mcpservers.Server {
		GinkgoHelper()
		s, err := svc.Create(ctx, org, actor, user, mcpservers.Input{Name: name, URL: "https://mcp.example.com/mcp", Class: class})
		Expect(err).NotTo(HaveOccurred())
		return s
	}

	It("is unclassified (kind other, both legs) when the creator says nothing", func(ctx SpecContext) {
		s := create(ctx, "wiki", nil)
		Expect(s.Kind).To(Equal("other"))
		Expect(s.DataClass).To(Equal([]string{"untrusted_input", "private_data"}))
		Expect(s.Carries(mcpservers.LegPrivateData)).To(BeTrue())
	})

	It("stores what the creator declares, including no leg at all", func(ctx SpecContext) {
		f := create(ctx, "fetcher", &mcpservers.Declaration{Kind: "fetch", Legs: []string{"untrusted_input"}})
		Expect(f.Kind).To(Equal("fetch"))
		Expect(f.DataClass).To(Equal([]string{"untrusted_input"}))
		Expect(f.Carries(mcpservers.LegPrivateData)).To(BeFalse())

		none := create(ctx, "clock", &mcpservers.Declaration{Kind: "other"})
		Expect(none.DataClass).To(BeEmpty())

		got, err := svc.Get(ctx, org, f.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Kind).To(Equal("fetch"))
		Expect(got.DataClass).To(Equal([]string{"untrusted_input"}))
	})

	It("refuses an invalid declaration at create", func(ctx SpecContext) {
		_, err := svc.Create(ctx, org, actor, user, mcpservers.Input{
			Name: "bad", URL: "https://mcp.example.com/mcp", Class: &mcpservers.Declaration{Kind: "fetch", Legs: []string{"everything"}},
		})
		var inv *catalog.InvalidError
		Expect(errors.As(err, &inv)).To(BeTrue())
		list, _ := svc.List(ctx, org)
		Expect(list).To(BeEmpty())
	})

	It("changes the class, audits before and after, and leaves the rest of the server alone", func(ctx SpecContext) {
		s := create(ctx, "wiki", nil)
		got, err := svc.SetDataClass(ctx, org, actor, s.ID, mcpservers.Declaration{Kind: "search", Legs: []string{"untrusted_input"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Kind).To(Equal("search"))
		Expect(got.DataClass).To(Equal([]string{"untrusted_input"}))
		Expect(got.URL).To(Equal(s.URL))
		Expect(got.Health).To(Equal(s.Health))

		ev := events(ctx, "mcp_server.data_class.set")
		Expect(ev).To(HaveLen(1))
		Expect(ev[0].TargetID).To(Equal(s.ID))
		var sum struct{ Before, After map[string]any }
		Expect(json.Unmarshal(ev[0].Summary, &sum)).To(Succeed())
		Expect(sum.Before).To(HaveKeyWithValue("dataClass", ConsistOf("untrusted_input", "private_data")))
		Expect(sum.After).To(HaveKeyWithValue("kind", "search"))
		Expect(sum.After).To(HaveKeyWithValue("dataClass", ConsistOf("untrusted_input")))
	})

	It("is org-scoped: another org's server is not found and stays as it was", func(ctx SpecContext) {
		s := create(ctx, "wiki", nil)
		other, otherUser, _ := pgtest.NewOrg(ctx, pool)
		_, err := svc.SetDataClass(ctx, other, audit.User(otherUser), s.ID, mcpservers.Declaration{Kind: "fetch"})
		Expect(err).To(MatchError(mcpservers.ErrNotFound))
		got, err := svc.Get(ctx, org, s.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Kind).To(Equal("other"))
		Expect(got.DataClass).To(HaveLen(2))
	})

	It("does not change a server for an invalid declaration", func(ctx SpecContext) {
		s := create(ctx, "wiki", nil)
		_, err := svc.SetDataClass(ctx, org, actor, s.ID, mcpservers.Declaration{Kind: "nonsense"})
		var inv *catalog.InvalidError
		Expect(errors.As(err, &inv)).To(BeTrue())
		got, err := svc.Get(ctx, org, s.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Kind).To(Equal("other"))
		Expect(events(ctx, "mcp_server.data_class.set")).To(BeEmpty())
	})

	It("is settable on a declared server and survives the Secret being applied again with the same URL", func(ctx SpecContext) {
		decl := mcpservers.Declared{SecretName: "s1", Version: "uid/1", Name: "wiki", URL: "https://decl.example.com/mcp"}
		_, err := svc.ApplyDeclared(ctx, org, decl)
		Expect(err).NotTo(HaveOccurred())
		list, _ := svc.List(ctx, org)
		Expect(list).To(HaveLen(1))
		Expect(list[0].Source).To(Equal("secret"))
		Expect(list[0].DataClass).To(HaveLen(2), "a declared server starts unclassified")

		_, err = svc.SetDataClass(ctx, org, actor, list[0].ID, mcpservers.Declaration{Kind: "fetch", Legs: []string{"untrusted_input"}})
		Expect(err).NotTo(HaveOccurred())

		decl.Version = "uid/2"
		decl.Transport = "sse"
		changed, err := svc.ApplyDeclared(ctx, org, decl)
		Expect(err).NotTo(HaveOccurred())
		Expect(changed).To(BeTrue())
		got, err := svc.Get(ctx, org, list[0].ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Transport).To(Equal("sse"))
		Expect(got.Kind).To(Equal("fetch"))
		Expect(got.DataClass).To(Equal([]string{"untrusted_input"}))
		Expect(events(ctx, "mcp_server.data_class.reset")).To(BeEmpty())
	})

	It("resets a declared server's class to the default, and audits it, when the Secret names a new URL", func(ctx SpecContext) {
		decl := mcpservers.Declared{SecretName: "s1", Version: "uid/1", Name: "wiki", URL: "https://decl.example.com/mcp"}
		_, err := svc.ApplyDeclared(ctx, org, decl)
		Expect(err).NotTo(HaveOccurred())
		list, _ := svc.List(ctx, org)
		_, err = svc.SetDataClass(ctx, org, actor, list[0].ID, mcpservers.Declaration{Kind: "fetch", Legs: []string{"untrusted_input"}})
		Expect(err).NotTo(HaveOccurred())

		decl.Version = "uid/2"
		decl.URL = "https://decl2.example.com/mcp"
		_, err = svc.ApplyDeclared(ctx, org, decl)
		Expect(err).NotTo(HaveOccurred())
		got, err := svc.Get(ctx, org, list[0].ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(got.URL).To(Equal("https://decl2.example.com/mcp"))
		Expect(got.Kind).To(Equal("other"))
		Expect(got.DataClass).To(Equal([]string{"untrusted_input", "private_data"}))
		expectReset(events(ctx, "mcp_server.data_class.reset"), "fetch", []any{"untrusted_input"})
	})

	It("keeps a UI server's class when the URL is unchanged and resets it, audited, when the URL changes", func(ctx SpecContext) {
		s := create(ctx, "docs", &mcpservers.Declaration{Kind: "fetch", Legs: []string{"untrusted_input"}})

		same := s.URL
		tr := "sse"
		got, err := svc.Update(ctx, org, actor, s.ID, mcpservers.Patch{URL: &same, Transport: &tr})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Kind).To(Equal("fetch"))
		Expect(got.DataClass).To(Equal([]string{"untrusted_input"}))
		Expect(events(ctx, "mcp_server.data_class.reset")).To(BeEmpty())

		other := "https://github-mcp.example.com/mcp"
		got, err = svc.Update(ctx, org, actor, s.ID, mcpservers.Patch{URL: &other})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Kind).To(Equal("other"))
		Expect(got.DataClass).To(Equal([]string{"untrusted_input", "private_data"}))
		again, err := svc.Get(ctx, org, s.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(again.DataClass).To(HaveLen(2))
		expectReset(events(ctx, "mcp_server.data_class.reset"), "fetch", []any{"untrusted_input"})

		By("a second change of URL, from the default, has nothing to reset and audits nothing more")
		third := "https://third.example.com/mcp"
		_, err = svc.Update(ctx, org, actor, s.ID, mcpservers.Patch{URL: &third})
		Expect(err).NotTo(HaveOccurred())
		Expect(events(ctx, "mcp_server.data_class.reset")).To(HaveLen(1))
	})

	It("is part of the create, update and declare audit summaries", func(ctx SpecContext) {
		s := create(ctx, "fetcher", &mcpservers.Declaration{Kind: "fetch", Legs: []string{"untrusted_input"}})
		ev := events(ctx, "mcp_server.create")
		Expect(ev).To(HaveLen(1))
		Expect(string(ev[0].Summary)).To(ContainSubstring(`"kind": "fetch"`))
		u := "https://mcp2.example.com/mcp"
		_, err := svc.Update(ctx, org, actor, s.ID, mcpservers.Patch{URL: &u})
		Expect(err).NotTo(HaveOccurred())
		Expect(string(events(ctx, "mcp_server.update")[0].Summary)).To(ContainSubstring(`"kind": "fetch"`))
	})
})

func expectReset(ev []store.AuditEvent, wasKind string, wasLegs []any) {
	GinkgoHelper()
	Expect(ev).To(HaveLen(1))
	var sum struct{ Before, After map[string]any }
	Expect(json.Unmarshal(ev[0].Summary, &sum)).To(Succeed())
	Expect(sum.Before).To(HaveKeyWithValue("kind", wasKind))
	Expect(sum.Before).To(HaveKeyWithValue("dataClass", wasLegs))
	Expect(sum.After).To(HaveKeyWithValue("kind", "other"))
	Expect(sum.After).To(HaveKeyWithValue("dataClass", ConsistOf("untrusted_input", "private_data")))
	Expect(sum.After).To(HaveKeyWithValue("reason", "url_changed"))
}
