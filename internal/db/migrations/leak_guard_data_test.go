// SPDX-License-Identifier: Apache-2.0

package migrations_test

import (
	"database/sql"
	"io/fs"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/pressly/goose/v3"

	"github.com/stateracloud/koinon/internal/db/migrations"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

// pq scans a Postgres text[] into a Go slice.
func pq(dst *[]string) any { return pgtype.NewMap().SQLScanner(dst) }

var _ = Describe("the leak guard data migration (ADR 0084, #350)", func() {
	It("gives the servers and orgs that exist a defined default and changes nothing else", func(ctx SpecContext) {
		names, err := fs.Glob(migrations.FS, "*_leak_guard_data.sql")
		Expect(err).NotTo(HaveOccurred())
		Expect(names).To(HaveLen(1))
		version, err := strconv.ParseInt(strings.SplitN(names[0], "_", 2)[0], 10, 64)
		Expect(err).NotTo(HaveOccurred())

		old, err := sql.Open("pgx", pgtest.Run(ctx))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(old.Close)
		provider, err := goose.NewProvider(goose.DialectPostgres, old, migrations.FS)
		Expect(err).NotTo(HaveOccurred())
		_, err = provider.UpTo(ctx, version-1)
		Expect(err).NotTo(HaveOccurred())

		var org string
		Expect(old.QueryRowContext(ctx, `INSERT INTO orgs (slug, display_name, kind, deleted_at) VALUES ('before', 'Before', 'personal', now()) RETURNING id`).Scan(&org)).To(Succeed())
		_, err = old.ExecContext(ctx, `INSERT INTO mcp_servers (org_id, name, url, disabled_reason) VALUES ($1, 'tracker', 'https://mcp.example.com/mcp', 'kept')`, org)
		Expect(err).NotTo(HaveOccurred())
		_, err = old.ExecContext(ctx, `INSERT INTO org_settings (org_id, idle_timeout_seconds) VALUES ($1, 120)`, org)
		Expect(err).NotTo(HaveOccurred())
		_, err = old.ExecContext(ctx, `INSERT INTO mcp_tools (org_id, server_id, name, content_hash, tier)
			SELECT org_id, id, 'create_issue', repeat('a', 64), 'auto' FROM mcp_servers WHERE org_id = $1`, org)
		Expect(err).NotTo(HaveOccurred())

		_, err = provider.Up(ctx)
		Expect(err).NotTo(HaveOccurred())

		By("an existing server is unclassified: kind other, both legs, nothing else changed")
		var kind, reason, url string
		var untrusted, private bool
		Expect(old.QueryRowContext(ctx, `SELECT kind, exposes_untrusted_input, exposes_private_data, url, disabled_reason FROM mcp_servers WHERE org_id = $1 AND name = 'tracker'`, org).
			Scan(&kind, &untrusted, &private, &url, &reason)).To(Succeed())
		Expect(kind).To(Equal("other"))
		Expect([]bool{untrusted, private}).To(Equal([]bool{true, true}))
		Expect(url).To(Equal("https://mcp.example.com/mcp"))
		Expect(reason).To(Equal("kept"))

		By("an existing tool is undeclared: external-high, fixed, no address arguments, the server's legs, tier untouched")
		var class, derivation, tier string
		var args []string
		var resUntrusted, resPrivate sql.NullBool
		Expect(old.QueryRowContext(ctx, `SELECT sink_class, sink_derivation, address_args, result_untrusted_input, result_private_data, tier FROM mcp_tools WHERE org_id = $1`, org).
			Scan(&class, &derivation, pq(&args), &resUntrusted, &resPrivate, &tier)).To(Succeed())
		Expect(class).To(Equal("external-high"))
		Expect(derivation).To(Equal("fixed"))
		Expect(args).To(BeEmpty())
		Expect(resUntrusted.Valid || resPrivate.Valid).To(BeFalse())
		Expect(tier).To(Equal("auto"))

		By("an existing org keeps its overrides and has no threshold of its own")
		var idle int
		var threshold sql.NullInt32
		Expect(old.QueryRowContext(ctx, `SELECT idle_timeout_seconds, leak_match_threshold FROM org_settings WHERE org_id = $1`, org).Scan(&idle, &threshold)).To(Succeed())
		Expect(idle).To(Equal(120))
		Expect(threshold.Valid).To(BeFalse())

		By("the fetch-trusted list is empty")
		var hosts int
		Expect(old.QueryRowContext(ctx, `SELECT count(*) FROM org_fetch_trusted_hosts`).Scan(&hosts)).To(Succeed())
		Expect(hosts).To(BeZero())

		By("the database bounds what it stores")
		_, err = old.ExecContext(ctx, `UPDATE org_settings SET leak_match_threshold = 11 WHERE org_id = $1`, org)
		Expect(err).To(HaveOccurred())
		_, err = old.ExecContext(ctx, `UPDATE org_settings SET leak_match_threshold = 257 WHERE org_id = $1`, org)
		Expect(err).To(HaveOccurred())
		_, err = old.ExecContext(ctx, `UPDATE org_settings SET leak_match_threshold = 12 WHERE org_id = $1`, org)
		Expect(err).NotTo(HaveOccurred())
		for _, bad := range []string{"Docs.Example.com", "https://docs.example.com", "docs.example.com:443", "*", "a.*.com", "*docs.example.com", "*.*.example.com", "*.com1", "*.localhost", "localhost", "127.1", "example.0x7f", "docs.example.123", "", "-a.com", "a..com"} {
			_, err = old.ExecContext(ctx, `INSERT INTO org_fetch_trusted_hosts (org_id, host) VALUES ($1, $2)`, org, bad)
			Expect(err).To(HaveOccurred(), "host %q", bad)
		}
		_, err = old.ExecContext(ctx, `INSERT INTO org_fetch_trusted_hosts (org_id, host) VALUES ($1, 'docs.example.com')`, org)
		Expect(err).NotTo(HaveOccurred())
		for _, good := range []string{"*.example.com", "xn--bcher-kva.example", "1.example.com"} {
			_, err = old.ExecContext(ctx, `INSERT INTO org_fetch_trusted_hosts (org_id, host) VALUES ($1, $2)`, org, good)
			Expect(err).NotTo(HaveOccurred(), "host %q", good)
		}
		_, err = old.ExecContext(ctx, `INSERT INTO org_fetch_trusted_hosts (org_id, host) VALUES ($1, 'docs.example.com')`, org)
		Expect(err).To(HaveOccurred())

		By("the database bounds a tool's declaration")
		set := func(class, derivation, args string) error {
			_, err := old.ExecContext(ctx, `UPDATE mcp_tools SET sink_class = $2::mcp_sink_class, sink_derivation = $3::mcp_sink_derivation, address_args = $4::text[] WHERE org_id = $1`, org, class, derivation, args)
			return err
		}
		Expect(set("internal", "fixed", "{owner,repo}")).To(Succeed())
		Expect(set("external-high", "code_host", "{owner,repo}")).To(Succeed())
		Expect(set("external-high", "fetch", "{url}")).To(Succeed())
		Expect(set("none", "fixed", "{}")).To(Succeed())
		Expect(set("internal", "code_host", "{owner,repo}")).NotTo(Succeed(), "a derived tool's class column is the fallback, external-high")
		Expect(set("external-high", "code_host", "{owner}")).NotTo(Succeed())
		Expect(set("external-high", "fetch", "{url,extra}")).NotTo(Succeed())
		Expect(set("external-high", "fetch", "{}")).NotTo(Succeed())
		Expect(set("external-high", "fixed", `{"bad name"}`)).NotTo(Succeed())
		Expect(set("external-high", "fixed", "{a,b,c,d,e,f,g,h,i}")).NotTo(Succeed())
		Expect(set("catastrophic", "fixed", "{}")).NotTo(Succeed())

		By("the code-host scope is keyed by connection, lower case and bounded")
		var conn string
		Expect(old.QueryRowContext(ctx, `INSERT INTO connections (org_id, connector, display_name, secret_name) VALUES ($1, 'github', 'GitHub', 'conn-leak-guard-test') RETURNING id`, org).Scan(&conn)).To(Succeed())
		add := func(owner, repo, vis string) error {
			_, err := old.ExecContext(ctx, `INSERT INTO org_code_host_repos (org_id, connection_id, owner, repo, visibility) VALUES ($1, $2, $3, $4, $5::code_host_repo_visibility)`, org, conn, owner, repo, vis)
			return err
		}
		Expect(add("northwind", "platform-handbook", "private")).To(Succeed())

		By("a server is linked to no connection until an admin links it, and only to one of its org's")
		var link sql.NullString
		Expect(old.QueryRowContext(ctx, `SELECT code_host_connection_id::text FROM mcp_servers WHERE org_id = $1 AND name = 'tracker'`, org).Scan(&link)).To(Succeed())
		Expect(link.Valid).To(BeFalse())
		_, err = old.ExecContext(ctx, `UPDATE mcp_servers SET code_host_connection_id = $2 WHERE org_id = $1`, org, conn)
		Expect(err).NotTo(HaveOccurred())
		_, err = old.ExecContext(ctx, `UPDATE mcp_servers SET code_host_connection_id = uuidv7() WHERE org_id = $1`, org)
		Expect(err).To(HaveOccurred(), "a connection that is not the org's")
		var other string
		Expect(old.QueryRowContext(ctx, `INSERT INTO orgs (slug, display_name, kind, deleted_at) VALUES ('other-org', 'Other', 'personal', now()) RETURNING id`).Scan(&other)).To(Succeed())
		var theirs string
		Expect(old.QueryRowContext(ctx, `INSERT INTO connections (org_id, connector, display_name, secret_name) VALUES ($1, 'github', 'GitHub', 'conn-leak-guard-other') RETURNING id`, other).Scan(&theirs)).To(Succeed())
		_, err = old.ExecContext(ctx, `UPDATE mcp_servers SET code_host_connection_id = $2 WHERE org_id = $1`, org, theirs)
		Expect(err).To(HaveOccurred(), "another org's connection")
		_, err = old.ExecContext(ctx, `DELETE FROM org_code_host_repos WHERE connection_id = $1`, conn)
		Expect(err).NotTo(HaveOccurred())
		_, err = old.ExecContext(ctx, `DELETE FROM connections WHERE id = $1`, conn)
		Expect(err).NotTo(HaveOccurred())
		Expect(old.QueryRowContext(ctx, `SELECT code_host_connection_id::text, org_id::text FROM mcp_servers WHERE name = 'tracker'`).Scan(&link, &other)).To(Succeed())
		Expect(link.Valid).To(BeFalse(), "deleting the connection unlinks the server")
		Expect(other).To(Equal(org), "and keeps its org")
		Expect(add("northwind", "platform-handbook", "private")).NotTo(Succeed(), "the connection is gone")
		conn = ""
		Expect(old.QueryRowContext(ctx, `INSERT INTO connections (org_id, connector, display_name, secret_name) VALUES ($1, 'github', 'GitHub', 'conn-leak-guard-test2') RETURNING id`, org).Scan(&conn)).To(Succeed())
		Expect(add("northwind", "platform-handbook", "private")).To(Succeed())
		Expect(add("northwind", "platform-handbook", "public")).NotTo(Succeed(), "one reading per repository and connection")
		Expect(add("Northwind", "Other", "private")).NotTo(Succeed())
		Expect(add("northwind", "", "private")).NotTo(Succeed())
		Expect(add("northwind", "secret", "classified")).NotTo(Succeed())
		_, err = old.ExecContext(ctx, `INSERT INTO org_code_host_repos (org_id, connection_id, owner, repo, visibility) VALUES ($1, uuidv7(), 'a', 'b', 'private')`, org)
		Expect(err).To(HaveOccurred(), "a repository belongs to one of the org's connections")
	})
})
