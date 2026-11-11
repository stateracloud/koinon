// SPDX-License-Identifier: Apache-2.0

package mcpservers_test

import (
	"encoding/json"
	"errors"

	"github.com/google/uuid"
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

var _ = Describe("the data class of a server (ADR 0084, old issue 350)", func() {
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

	It("creates a server declared disabled already disabled, in the same transaction", func(ctx SpecContext) {
		decl := mcpservers.Declared{SecretName: "s1", Version: "uid/1", Name: "wiki", URL: "https://decl.example.com/mcp", Disabled: true}
		_, err := svc.ApplyDeclared(ctx, org, decl)
		Expect(err).NotTo(HaveOccurred())
		var by string
		Expect(pool.QueryRow(ctx, `SELECT admin_disabled_by FROM mcp_servers WHERE org_id = $1 AND name = 'wiki'`, org).Scan(&by)).To(Succeed())
		Expect(by).To(Equal("secret s1"))
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

	Describe("the code-host connection link (ADR 0091 decision 3)", func() {
		// GitHub's own MCP server, which acts on github.com (connectors/github spec.site.mcpHosts).
		const copilot = "https://api.githubcopilot.com/mcp/"
		// connAt adds a connection of the connector at baseURL ("" for the connector's default host).
		connAt := func(ctx SpecContext, org pgtype.UUID, connector, baseURL string) string {
			GinkgoHelper()
			id := pgtype.UUID{Bytes: uuid.New(), Valid: true}
			config, err := json.Marshal(map[string]string{"baseUrl": baseURL})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.New(pool).CreateConnection(ctx, store.CreateConnectionParams{
				OrgID: org, ID: id, Connector: connector, DisplayName: connector + " " + baseURL, Config: config,
				SecretName: pgtype.Text{String: "conn-" + id.String(), Valid: true},
			})
			Expect(err).NotTo(HaveOccurred())
			return id.String()
		}
		// newConn is a github.com connection.
		newConn := func(ctx SpecContext, org pgtype.UUID) string {
			GinkgoHelper()
			return connAt(ctx, org, "github", "")
		}
		createAt := func(ctx SpecContext, name, u string, class *mcpservers.Declaration) mcpservers.Server {
			GinkgoHelper()
			s, err := svc.Create(ctx, org, actor, user, mcpservers.Input{Name: name, URL: u, Class: class})
			Expect(err).NotTo(HaveOccurred())
			return s
		}
		linkTo := func(conn string) mcpservers.Declaration {
			return mcpservers.Declaration{Kind: "other", Legs: mcpservers.Legs, CodeHostConnection: conn}
		}
		expectInvalid := func(err error, substr string) {
			GinkgoHelper()
			var inv *mcpservers.InvalidError
			Expect(errors.As(err, &inv)).To(BeTrue(), "%v", err)
			Expect(inv.Error()).To(ContainSubstring(substr))
		}

		It("is none for a server nobody linked, and set, read back and audited with the class", func(ctx SpecContext) {
			s := createAt(ctx, "forge", copilot, nil)
			Expect(s.CodeHostConnectionID).To(BeEmpty())
			conn := newConn(ctx, org)
			got, err := svc.SetDataClass(ctx, org, actor, s.ID, mcpservers.Declaration{Kind: "other", Legs: mcpservers.Legs, CodeHostConnection: conn})
			Expect(err).NotTo(HaveOccurred())
			Expect(got.CodeHostConnectionID).To(Equal(conn))
			again, err := svc.Get(ctx, org, s.ID)
			Expect(err).NotTo(HaveOccurred())
			Expect(again.CodeHostConnectionID).To(Equal(conn))
			ev := events(ctx, "mcp_server.data_class.set")
			Expect(ev).To(HaveLen(1))
			Expect(string(ev[0].Summary)).To(ContainSubstring(conn))

			By("a class without a connection unlinks it")
			got, err = svc.SetDataClass(ctx, org, actor, s.ID, mcpservers.Declaration{Kind: "other", Legs: mcpservers.Legs})
			Expect(err).NotTo(HaveOccurred())
			Expect(got.CodeHostConnectionID).To(BeEmpty())
		})

		It("can be given at create", func(ctx SpecContext) {
			conn := newConn(ctx, org)
			s := createAt(ctx, "forge", copilot, &mcpservers.Declaration{Kind: "other", Legs: mcpservers.Legs, CodeHostConnection: conn})
			Expect(s.CodeHostConnectionID).To(Equal(conn))
		})

		Describe("checks that the server acts on the connection's host (#117)", func() {
			DescribeTable("allows a server at a host known to act on the connection's host, at create and at update",
				func(ctx SpecContext, connector, baseURL, serverURL string) {
					conn := connAt(ctx, org, connector, baseURL)
					s := createAt(ctx, "forge", serverURL, &mcpservers.Declaration{Kind: "other", Legs: mcpservers.Legs, CodeHostConnection: conn})
					Expect(s.CodeHostConnectionID).To(Equal(conn))
					u := createAt(ctx, "forge-later", serverURL, nil)
					got, err := svc.SetDataClass(ctx, org, actor, u.ID, linkTo(conn))
					Expect(err).NotTo(HaveOccurred())
					Expect(got.CodeHostConnectionID).To(Equal(conn))
				},
				Entry("GitHub's MCP server for github.com", "github", "", copilot),
				Entry("github.com named explicitly", "github", "https://github.com", copilot),
				Entry("the github.com API host", "github", "", "https://api.github.com/mcp"),
				Entry("a GitHub Enterprise Server's own host", "github", "https://ghe.example.com", "https://ghe.example.com/mcp"),
				Entry("a GitHub Enterprise Server's own host on another port, in another case", "github", "https://ghe.example.com", "https://GHE.example.com:8443/mcp"),
				Entry("a Gitea's own host", "gitea", "https://git.example.com", "https://git.example.com/mcp"),
				Entry("a Gitea under a sub-path", "gitea", "https://git.example.com/forge", "https://git.example.com/gitea-mcp/mcp"),
			)

			DescribeTable("refuses a server at another host, at create and at update, and writes nothing",
				func(ctx SpecContext, connector, baseURL, serverURL, why string) {
					conn := connAt(ctx, org, connector, baseURL)
					_, err := svc.Create(ctx, org, actor, user, mcpservers.Input{Name: "forge", URL: serverURL, Class: &mcpservers.Declaration{Kind: "other", Legs: mcpservers.Legs, CodeHostConnection: conn}})
					expectInvalid(err, why)
					list, err := svc.List(ctx, org)
					Expect(err).NotTo(HaveOccurred())
					Expect(list).To(BeEmpty(), "the create wrote nothing")

					s := createAt(ctx, "forge", serverURL, &mcpservers.Declaration{Kind: "fetch", Legs: []string{"untrusted_input"}})
					_, err = svc.SetDataClass(ctx, org, actor, s.ID, linkTo(conn))
					expectInvalid(err, why)
					got, err := svc.Get(ctx, org, s.ID)
					Expect(err).NotTo(HaveOccurred())
					Expect(got.CodeHostConnectionID).To(BeEmpty())
					Expect(got.Kind).To(Equal("fetch"), "the update wrote nothing")
					Expect(events(ctx, "mcp_server.data_class.set")).To(BeEmpty())
				},
				Entry("GitHub's MCP server to a Gitea connection (old issue 621)", "gitea", "https://git.example.com", copilot, "does not act on the Gitea connection at https://git.example.com"),
				Entry("GitHub's MCP server to a GitHub Enterprise Server connection", "github", "https://ghe.example.com", copilot, "accepts MCP servers at ghe.example.com"),
				Entry("a Gitea MCP server to a github.com connection", "github", "", "https://git.example.com/mcp", "accepts MCP servers at github.com, api.github.com, api.githubcopilot.com"),
				Entry("another host of the same domain", "gitea", "https://git.example.com", "https://mcp.git.example.com/mcp", "an MCP server at mcp.git.example.com"),
				Entry("a host that only ends like the connection's", "github", "https://ghe.example.com", "https://notghe.example.com/mcp", "an MCP server at notghe.example.com"),
			)

			It("refuses a connection that is not a code host", func(ctx SpecContext) {
				conn := connAt(ctx, org, "slack", "")
				s := createAt(ctx, "forge", "https://slack.example.com/mcp", nil)
				_, err := svc.SetDataClass(ctx, org, actor, s.ID, linkTo(conn))
				expectInvalid(err, "is not a code-host connection")
			})

			It("refuses to keep an old link that no longer matches when the class is set again", func(ctx SpecContext) {
				conn := connAt(ctx, org, "gitea", "https://git.example.com")
				s := createAt(ctx, "forge", copilot, nil)
				// A link written before the check existed.
				var cid, sid pgtype.UUID
				Expect(cid.Scan(conn)).To(Succeed())
				Expect(sid.Scan(s.ID)).To(Succeed())
				_, err := pool.Exec(ctx, `UPDATE mcp_servers SET code_host_connection_id = $3 WHERE org_id = $1 AND id = $2`, org, sid, cid)
				Expect(err).NotTo(HaveOccurred())
				_, err = svc.SetDataClass(ctx, org, actor, s.ID, mcpservers.Declaration{Kind: "search", Legs: mcpservers.Legs, CodeHostConnection: conn})
				expectInvalid(err, "does not act on the Gitea connection")
				By("unlinking it is allowed")
				got, err := svc.SetDataClass(ctx, org, actor, s.ID, mcpservers.Declaration{Kind: "search", Legs: mcpservers.Legs})
				Expect(err).NotTo(HaveOccurred())
				Expect(got.CodeHostConnectionID).To(BeEmpty())
			})

			It("unlinks, audited, the servers a declared connection no longer serves after its host changes", func(ctx SpecContext) {
				conn := connAt(ctx, org, "gitea", "https://git.example.com")
				moved := createAt(ctx, "moved", "https://git.example.com/mcp", &mcpservers.Declaration{Kind: "fetch", Legs: []string{"private_data"}, CodeHostConnection: conn})
				kept := createAt(ctx, "kept", "https://git2.example.com/mcp", nil)
				var cid pgtype.UUID
				Expect(cid.Scan(conn)).To(Succeed())
				q := store.New(pool)
				_, err := pool.Exec(ctx, `UPDATE connections SET config = '{"baseUrl":"https://git2.example.com"}' WHERE org_id = $1 AND id = $2`, org, cid)
				Expect(err).NotTo(HaveOccurred())
				_, err = svc.SetDataClass(ctx, org, actor, kept.ID, linkTo(conn))
				Expect(err).NotTo(HaveOccurred())
				row, err := q.GetConnection(ctx, store.GetConnectionParams{OrgID: org, ID: cid})
				Expect(err).NotTo(HaveOccurred())

				_, err = audit.InTx(ctx, pool, func(q *store.Queries, b audit.Batch) error {
					return mcpservers.UnlinkMismatchedServers(ctx, q, b, audit.Secret("forge"), row)
				})
				Expect(err).NotTo(HaveOccurred())
				got, err := svc.Get(ctx, org, moved.ID)
				Expect(err).NotTo(HaveOccurred())
				Expect(got.CodeHostConnectionID).To(BeEmpty(), "git.example.com is no longer the connection's host")
				Expect(got.Kind).To(Equal("fetch"), "the rest of the class stays")
				Expect(got.DataClass).To(Equal([]string{"private_data"}))
				still, err := svc.Get(ctx, org, kept.ID)
				Expect(err).NotTo(HaveOccurred())
				Expect(still.CodeHostConnectionID).To(Equal(conn), "git2.example.com is")
				ev := events(ctx, "mcp_server.data_class.reset")
				Expect(ev).To(HaveLen(1))
				Expect(ev[0].TargetID).To(Equal(moved.ID))
				Expect(string(ev[0].Summary)).To(ContainSubstring("connection_host_changed"))
			})
		})

		It("refuses a connection of another org, one that does not exist and one that is not an ID", func(ctx SpecContext) {
			s := createAt(ctx, "forge", copilot, nil)
			other, _, _ := pgtest.NewOrg(ctx, pool)
			for _, c := range []string{newConn(ctx, other), uuid.NewString(), "not-an-id"} {
				_, err := svc.SetDataClass(ctx, org, actor, s.ID, mcpservers.Declaration{Kind: "other", Legs: mcpservers.Legs, CodeHostConnection: c})
				var inv *mcpservers.InvalidError
				Expect(errors.As(err, &inv)).To(BeTrue(), "%q: %v", c, err)
			}
			_, err := svc.Create(ctx, org, actor, user, mcpservers.Input{Name: "x", URL: "https://x.example.com/mcp", Class: &mcpservers.Declaration{Kind: "other", Legs: mcpservers.Legs, CodeHostConnection: uuid.NewString()}})
			var inv *mcpservers.InvalidError
			Expect(errors.As(err, &inv)).To(BeTrue(), "%v", err)
			got, _ := svc.Get(ctx, org, s.ID)
			Expect(got.CodeHostConnectionID).To(BeEmpty())
		})

		It("is cleared, and audited as a class reset, when the URL changes", func(ctx SpecContext) {
			conn := newConn(ctx, org)
			s := createAt(ctx, "forge", copilot, &mcpservers.Declaration{Kind: "other", Legs: mcpservers.Legs, CodeHostConnection: conn})
			same := s.URL
			got, err := svc.Update(ctx, org, actor, s.ID, mcpservers.Patch{URL: &same})
			Expect(err).NotTo(HaveOccurred())
			Expect(got.CodeHostConnectionID).To(Equal(conn), "the same URL keeps it")

			other := "https://attacker.example.com/mcp"
			got, err = svc.Update(ctx, org, actor, s.ID, mcpservers.Patch{URL: &other})
			Expect(err).NotTo(HaveOccurred())
			Expect(got.CodeHostConnectionID).To(BeEmpty())
			ev := events(ctx, "mcp_server.data_class.reset")
			Expect(ev).To(HaveLen(1), "the class was already the default, only the link changed")
			Expect(string(ev[0].Summary)).To(ContainSubstring(conn))
		})

		It("is unlinked when the connection is deleted", func(ctx SpecContext) {
			conn := newConn(ctx, org)
			s := createAt(ctx, "forge", copilot, &mcpservers.Declaration{Kind: "other", Legs: mcpservers.Legs, CodeHostConnection: conn})
			var cid pgtype.UUID
			Expect(cid.Scan(conn)).To(Succeed())
			_, err := pool.Exec(ctx, `DELETE FROM connections WHERE org_id = $1 AND id = $2`, org, cid)
			Expect(err).NotTo(HaveOccurred())
			got, err := svc.Get(ctx, org, s.ID)
			Expect(err).NotTo(HaveOccurred())
			Expect(got.CodeHostConnectionID).To(BeEmpty())
			Expect(got.Name).To(Equal("forge"), "the server is kept, and its org_id is not nulled")
		})
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
