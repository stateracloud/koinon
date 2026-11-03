// SPDX-License-Identifier: Apache-2.0

package migrations_test

import (
	"database/sql"
	"io/fs"
	"strconv"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/pressly/goose/v3"

	"github.com/stateracloud/koinon/internal/db/migrations"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

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

		_, err = provider.Up(ctx)
		Expect(err).NotTo(HaveOccurred())

		By("an existing server is unclassified: kind other, all three legs, nothing else changed")
		var kind, reason, url string
		var untrusted, private, outbound bool
		Expect(old.QueryRowContext(ctx, `SELECT kind, exposes_untrusted_input, exposes_private_data, exposes_outbound, url, disabled_reason FROM mcp_servers WHERE org_id = $1 AND name = 'tracker'`, org).
			Scan(&kind, &untrusted, &private, &outbound, &url, &reason)).To(Succeed())
		Expect(kind).To(Equal("other"))
		Expect([]bool{untrusted, private, outbound}).To(Equal([]bool{true, true, true}))
		Expect(url).To(Equal("https://mcp.example.com/mcp"))
		Expect(reason).To(Equal("kept"))

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
		for _, bad := range []string{"Docs.Example.com", "https://docs.example.com", "docs.example.com:443", "*.example.com", "", "-a.com", "a..com"} {
			_, err = old.ExecContext(ctx, `INSERT INTO org_fetch_trusted_hosts (org_id, host) VALUES ($1, $2)`, org, bad)
			Expect(err).To(HaveOccurred(), "host %q", bad)
		}
		_, err = old.ExecContext(ctx, `INSERT INTO org_fetch_trusted_hosts (org_id, host) VALUES ($1, 'docs.example.com')`, org)
		Expect(err).NotTo(HaveOccurred())
		_, err = old.ExecContext(ctx, `INSERT INTO org_fetch_trusted_hosts (org_id, host) VALUES ($1, 'docs.example.com')`, org)
		Expect(err).To(HaveOccurred())
	})
})
