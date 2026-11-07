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
	"github.com/stateracloud/koinon/internal/sinkclass"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

var _ = Describe("the declaration of a tool (ADR 0091, old issue 350)", func() {
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
	summary := func(e store.AuditEvent) (before, after map[string]any) {
		GinkgoHelper()
		var sum struct{ Before, After map[string]any }
		Expect(json.Unmarshal(e.Summary, &sum)).To(Succeed())
		return sum.Before, sum.After
	}
	// serverWithTools creates a server and syncs the given tools; it returns the server.
	serverWithTools := func(ctx SpecContext, name string, tools ...string) mcpservers.Server {
		GinkgoHelper()
		s, err := svc.Create(ctx, org, actor, user, mcpservers.Input{Name: name, URL: "https://mcp.example.com/mcp"})
		Expect(err).NotTo(HaveOccurred())
		var listed []mcpservers.ListedTool
		for _, t := range tools {
			listed = append(listed, mcpservers.ListedTool{Name: t, Description: "Tool " + t, InputSchema: json.RawMessage(`{"type":"object"}`)})
		}
		_, err = svc.SyncTools(ctx, org, uuidOf(s.ID), listed)
		Expect(err).NotTo(HaveOccurred())
		return s
	}
	tools := func(ctx SpecContext, s mcpservers.Server) map[string]mcpservers.Tool {
		GinkgoHelper()
		ts, err := svc.Tools(ctx, org, s.ID)
		Expect(err).NotTo(HaveOccurred())
		out := map[string]mcpservers.Tool{}
		for _, t := range ts {
			out[t.Name] = t
		}
		return out
	}
	// setDecl declares a tool against the content hash it has now, as a console that just read it would.
	setDecl := func(ctx SpecContext, org pgtype.UUID, actor audit.Actor, serverID, tool string, d sinkclass.Declaration) (mcpservers.Tool, error) {
		GinkgoHelper()
		hash := "no-such-tool"
		ts, err := svc.Tools(ctx, org, serverID)
		if err == nil {
			for _, t := range ts {
				if t.Name == tool {
					hash = t.ContentHash
				}
			}
		}
		return svc.SetToolDeclaration(ctx, org, actor, serverID, tool, hash, d)
	}
	codeHost := sinkclass.Declaration{Class: sinkclass.ExternalHigh, Derivation: sinkclass.CodeHost, AddressArgs: []string{"owner", "repo"}}

	It("refuses a declaration made against a tool that changed since it was read, and changes nothing", func(ctx SpecContext) {
		s := serverWithTools(ctx, "forge", "create_issue")
		reviewed := tools(ctx, s)["create_issue"].ContentHash

		By("the server republishes the tool between the review and the save")
		_, err := svc.SyncTools(ctx, org, uuidOf(s.ID), []mcpservers.ListedTool{{Name: "create_issue", Description: "Now it also emails everyone", InputSchema: json.RawMessage(`{"type":"object"}`)}})
		Expect(err).NotTo(HaveOccurred())
		Expect(tools(ctx, s)["create_issue"].ContentHash).NotTo(Equal(reviewed))

		_, err = svc.SetToolDeclaration(ctx, org, actor, s.ID, "create_issue", reviewed, sinkclass.Declaration{Class: sinkclass.None, Derivation: sinkclass.Fixed})
		Expect(err).To(MatchError(mcpservers.ErrToolChanged))
		Expect(tools(ctx, s)["create_issue"].Sink.IsDefault()).To(BeTrue(), "the new text is still external-high")
		Expect(events(ctx, "mcp_tool.declaration.set")).To(BeEmpty())

		By("the hash of the new text is accepted")
		_, err = setDecl(ctx, org, actor, s.ID, "create_issue", sinkclass.Declaration{Class: sinkclass.None, Derivation: sinkclass.Fixed})
		Expect(err).NotTo(HaveOccurred())
	})

	It("refuses a declaration without the reviewed content hash", func(ctx SpecContext) {
		s := serverWithTools(ctx, "forge", "create_issue")
		_, err := svc.SetToolDeclaration(ctx, org, actor, s.ID, "create_issue", "", sinkclass.Declaration{Class: sinkclass.None, Derivation: sinkclass.Fixed})
		var inv *mcpservers.InvalidError
		Expect(errors.As(err, &inv)).To(BeTrue(), "%v", err)
	})

	It("is undeclared when a tool first appears: external-high, fixed, the server's legs", func(ctx SpecContext) {
		s := serverWithTools(ctx, "forge", "create_issue")
		Expect(tools(ctx, s)["create_issue"].Sink.IsDefault()).To(BeTrue())
		Expect(tools(ctx, s)["create_issue"].Sink.Class).To(Equal(sinkclass.ExternalHigh))
	})

	It("stores a fixed declaration, a derived one and the result legs, and audits before and after", func(ctx SpecContext) {
		s := serverWithTools(ctx, "forge", "create_issue", "list_issues")
		yes, no := true, false

		got, err := setDecl(ctx, org, actor, s.ID, "list_issues",
			sinkclass.Declaration{Class: sinkclass.None, Derivation: sinkclass.Fixed, ResultUntrustedInput: &yes, ResultPrivateData: &no})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Sink.Class).To(Equal(sinkclass.None))
		Expect(*got.Sink.ResultUntrustedInput).To(BeTrue())
		Expect(*got.Sink.ResultPrivateData).To(BeFalse())

		got, err = setDecl(ctx, org, actor, s.ID, "create_issue", codeHost)
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Sink.Derivation).To(Equal(sinkclass.CodeHost))
		Expect(got.Sink.AddressArgs).To(Equal([]string{"owner", "repo"}))
		Expect(got.Sink.ResultUntrustedInput).To(BeNil())

		again := tools(ctx, s)
		Expect(again["create_issue"].Sink.Equal(got.Sink)).To(BeTrue())
		Expect(again["list_issues"].Sink.Class).To(Equal(sinkclass.None), "another tool is untouched by the first call")

		ev := events(ctx, "mcp_tool.declaration.set")
		Expect(ev).To(HaveLen(2))
		var created store.AuditEvent
		for _, e := range ev {
			if e.TargetID == s.ID+"/create_issue" {
				created = e
			}
		}
		before, after := summary(created)
		Expect(before).To(HaveKeyWithValue("sinkClass", "external-high"))
		Expect(before).To(HaveKeyWithValue("derivation", "fixed"))
		Expect(after).To(HaveKeyWithValue("derivation", "code_host"))
		Expect(after).To(HaveKeyWithValue("addressArgs", []any{"owner", "repo"}))
		Expect(after).To(HaveKeyWithValue("server", "forge"))
	})

	It("refuses an invalid declaration, leaves the tool and audits nothing", func(ctx SpecContext) {
		s := serverWithTools(ctx, "forge", "create_issue")
		for _, d := range []sinkclass.Declaration{
			{Class: "everywhere", Derivation: sinkclass.Fixed},
			{Derivation: sinkclass.CodeHost, AddressArgs: []string{"repo"}},
			{Class: sinkclass.None, Derivation: sinkclass.Fixed, AddressArgs: []string{"a b"}},
		} {
			_, err := setDecl(ctx, org, actor, s.ID, "create_issue", d)
			var inv *catalog.InvalidError
			Expect(errors.As(err, &inv)).To(BeTrue(), "%+v: %v", d, err)
		}
		Expect(tools(ctx, s)["create_issue"].Sink.IsDefault()).To(BeTrue())
		Expect(events(ctx, "mcp_tool.declaration.set")).To(BeEmpty())
	})

	It("is org-scoped, and a tool that does not exist is not found", func(ctx SpecContext) {
		s := serverWithTools(ctx, "forge", "create_issue")
		other, otherUser, _ := pgtest.NewOrg(ctx, pool)
		_, err := setDecl(ctx, other, audit.User(otherUser), s.ID, "create_issue", codeHost)
		Expect(err).To(MatchError(mcpservers.ErrNotFound))
		_, err = setDecl(ctx, org, actor, s.ID, "no_such_tool", codeHost)
		Expect(err).To(MatchError(mcpservers.ErrNotFound))
		Expect(tools(ctx, s)["create_issue"].Sink.IsDefault()).To(BeTrue())
	})

	Describe("when the tool's content hash changes (the ADR 0057 rule)", func() {
		It("goes back to the default, audited, while the tool and its neighbours keep what they have", func(ctx SpecContext) {
			s := serverWithTools(ctx, "forge", "create_issue", "list_issues")
			_, err := setDecl(ctx, org, actor, s.ID, "create_issue", codeHost)
			Expect(err).NotTo(HaveOccurred())
			_, err = setDecl(ctx, org, actor, s.ID, "list_issues", sinkclass.Declaration{Class: sinkclass.None, Derivation: sinkclass.Fixed})
			Expect(err).NotTo(HaveOccurred())

			By("a sync with the same text changes nothing")
			schema := json.RawMessage(`{"type":"object"}`)
			_, err = svc.SyncTools(ctx, org, uuidOf(s.ID), []mcpservers.ListedTool{
				{Name: "create_issue", Description: "Tool create_issue", InputSchema: schema},
				{Name: "list_issues", Description: "Tool list_issues", InputSchema: schema},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(tools(ctx, s)["create_issue"].Sink.Derivation).To(Equal(sinkclass.CodeHost))
			Expect(events(ctx, "mcp_tool.declaration.reset")).To(BeEmpty())

			By("a new description for one tool resets that tool's declaration")
			res, err := svc.SyncTools(ctx, org, uuidOf(s.ID), []mcpservers.ListedTool{
				{Name: "create_issue", Description: "Tool create_issue, now also posts to a webhook", InputSchema: schema},
				{Name: "list_issues", Description: "Tool list_issues", InputSchema: schema},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Changed).To(ConsistOf("create_issue"))
			got := tools(ctx, s)
			Expect(got["create_issue"].Sink.IsDefault()).To(BeTrue())
			Expect(got["create_issue"].Sink.Derivation).To(Equal(sinkclass.Fixed))
			Expect(got["create_issue"].Sink.AddressArgs).To(BeEmpty())
			Expect(got["list_issues"].Sink.Class).To(Equal(sinkclass.None))

			ev := events(ctx, "mcp_tool.declaration.reset")
			Expect(ev).To(HaveLen(1))
			Expect(ev[0].TargetID).To(Equal(s.ID + "/create_issue"))
			before, after := summary(ev[0])
			Expect(before).To(HaveKeyWithValue("derivation", "code_host"))
			Expect(before).To(HaveKeyWithValue("addressArgs", []any{"owner", "repo"}))
			Expect(after).To(HaveKeyWithValue("sinkClass", "external-high"))
			Expect(after).To(HaveKeyWithValue("derivation", "fixed"))
			Expect(after).To(HaveKeyWithValue("reason", "content_changed"))
		})

		It("resets a new input schema too, and result legs", func(ctx SpecContext) {
			s := serverWithTools(ctx, "forge", "fetch_page")
			yes := true
			_, err := setDecl(ctx, org, actor, s.ID, "fetch_page", sinkclass.Declaration{
				Class: sinkclass.ExternalHigh, Derivation: sinkclass.Fetch, AddressArgs: []string{"url"}, ResultUntrustedInput: &yes,
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = svc.SyncTools(ctx, org, uuidOf(s.ID), []mcpservers.ListedTool{
				{Name: "fetch_page", Description: "Tool fetch_page", InputSchema: json.RawMessage(`{"type":"object","properties":{"url":{"type":"string"}}}`)},
			})
			Expect(err).NotTo(HaveOccurred())
			got := tools(ctx, s)["fetch_page"]
			Expect(got.Sink.IsDefault()).To(BeTrue())
			Expect(got.Sink.ResultUntrustedInput).To(BeNil())
		})

		It("audits nothing for a tool that was undeclared", func(ctx SpecContext) {
			s := serverWithTools(ctx, "forge", "create_issue")
			_, err := svc.SyncTools(ctx, org, uuidOf(s.ID), []mcpservers.ListedTool{
				{Name: "create_issue", Description: "changed", InputSchema: json.RawMessage(`{"type":"object"}`)},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(events(ctx, "mcp_tool.declaration.reset")).To(BeEmpty())
		})
	})

	Describe("when the server's URL changes (the old issue 350 rule)", func() {
		declare := func(ctx SpecContext, s mcpservers.Server) {
			GinkgoHelper()
			_, err := setDecl(ctx, org, actor, s.ID, "create_issue", codeHost)
			Expect(err).NotTo(HaveOccurred())
			_, err = setDecl(ctx, org, actor, s.ID, "list_issues", sinkclass.Declaration{Class: sinkclass.None, Derivation: sinkclass.Fixed})
			Expect(err).NotTo(HaveOccurred())
		}

		It("resets every declared tool of a UI server, audited, and leaves the other servers alone", func(ctx SpecContext) {
			s := serverWithTools(ctx, "forge", "create_issue", "list_issues", "untouched")
			peer := serverWithTools(ctx, "peer", "create_issue")
			_, err := setDecl(ctx, org, actor, peer.ID, "create_issue", codeHost)
			Expect(err).NotTo(HaveOccurred())
			declare(ctx, s)

			same := s.URL
			tr := "sse"
			_, err = svc.Update(ctx, org, actor, s.ID, mcpservers.Patch{URL: &same, Transport: &tr})
			Expect(err).NotTo(HaveOccurred())
			Expect(tools(ctx, s)["create_issue"].Sink.Derivation).To(Equal(sinkclass.CodeHost), "the same URL keeps the declaration")
			Expect(events(ctx, "mcp_tool.declaration.reset")).To(BeEmpty())

			other := "https://other-forge.example.com/mcp"
			_, err = svc.Update(ctx, org, actor, s.ID, mcpservers.Patch{URL: &other})
			Expect(err).NotTo(HaveOccurred())
			for name, t := range tools(ctx, s) {
				Expect(t.Sink.IsDefault()).To(BeTrue(), name)
			}
			Expect(tools(ctx, peer)["create_issue"].Sink.Derivation).To(Equal(sinkclass.CodeHost))

			ev := events(ctx, "mcp_tool.declaration.reset")
			Expect(ev).To(HaveLen(2), "only the two declared tools; the third was the default already")
			targets := []string{ev[0].TargetID, ev[1].TargetID}
			Expect(targets).To(ConsistOf(s.ID+"/create_issue", s.ID+"/list_issues"))
			for _, e := range ev {
				before, after := summary(e)
				Expect(before).To(HaveKey("derivation"))
				Expect(before).To(HaveKeyWithValue("server", "forge"))
				Expect(after).To(HaveKeyWithValue("reason", "url_changed"))
				Expect(after).To(HaveKeyWithValue("sinkClass", "external-high"))
				Expect(after).To(HaveKeyWithValue("derivation", "fixed"))
			}

			By("a second change from the default has nothing more to reset")
			third := "https://third.example.com/mcp"
			_, err = svc.Update(ctx, org, actor, s.ID, mcpservers.Patch{URL: &third})
			Expect(err).NotTo(HaveOccurred())
			Expect(events(ctx, "mcp_tool.declaration.reset")).To(HaveLen(2))
		})

		It("resets the tools of a declared server when the Secret names a new URL", func(ctx SpecContext) {
			decl := mcpservers.Declared{SecretName: "s1", Version: "uid/1", Name: "forge", URL: "https://decl.example.com/mcp"}
			_, err := svc.ApplyDeclared(ctx, org, decl)
			Expect(err).NotTo(HaveOccurred())
			list, _ := svc.List(ctx, org)
			Expect(list).To(HaveLen(1))
			_, err = svc.SyncTools(ctx, org, uuidOf(list[0].ID), []mcpservers.ListedTool{
				{Name: "create_issue", Description: "x", InputSchema: json.RawMessage(`{"type":"object"}`)},
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = setDecl(ctx, org, actor, list[0].ID, "create_issue", codeHost)
			Expect(err).NotTo(HaveOccurred())

			decl.Version, decl.Transport = "uid/2", "sse"
			_, err = svc.ApplyDeclared(ctx, org, decl)
			Expect(err).NotTo(HaveOccurred())
			Expect(tools(ctx, list[0])["create_issue"].Sink.Derivation).To(Equal(sinkclass.CodeHost), "a new version at the same URL keeps it")

			decl.Version, decl.URL = "uid/3", "https://decl2.example.com/mcp"
			_, err = svc.ApplyDeclared(ctx, org, decl)
			Expect(err).NotTo(HaveOccurred())
			Expect(tools(ctx, list[0])["create_issue"].Sink.IsDefault()).To(BeTrue())
			ev := events(ctx, "mcp_tool.declaration.reset")
			Expect(ev).To(HaveLen(1))
			_, after := summary(ev[0])
			Expect(after).To(HaveKeyWithValue("reason", "url_changed"))
		})
	})
})
