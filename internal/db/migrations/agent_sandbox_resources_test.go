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

var _ = Describe("the agent sandbox resources migration (ADR 0083, #75)", func() {
	It("leaves every existing agent without an override and checks the shape of a new one", func(ctx SpecContext) {
		names, err := fs.Glob(migrations.FS, "*_agent_sandbox_resources.sql")
		Expect(err).NotTo(HaveOccurred())
		Expect(names).To(HaveLen(1))
		version, err := strconv.ParseInt(strings.SplitN(names[0], "_", 2)[0], 10, 64)
		Expect(err).NotTo(HaveOccurred())

		db, err := sql.Open("pgx", pgtest.Run(ctx))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(db.Close)
		provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
		Expect(err).NotTo(HaveOccurred())
		_, err = provider.UpTo(ctx, version-1)
		Expect(err).NotTo(HaveOccurred())

		var org, agent string
		Expect(db.QueryRowContext(ctx, `INSERT INTO orgs (slug, display_name, kind, deleted_at) VALUES ('sizing', 'Sizing', 'personal', now()) RETURNING id`).Scan(&org)).To(Succeed())
		Expect(db.QueryRowContext(ctx, `INSERT INTO agents (org_id, display_name, flavor) VALUES ($1, 'a', 'opencode') RETURNING id`, org).Scan(&agent)).To(Succeed())

		_, err = provider.Up(ctx)
		Expect(err).NotTo(HaveOccurred())

		By("the agent stored before the migration follows its template: all four are NULL")
		var set int
		Expect(db.QueryRowContext(ctx, `SELECT count(sandbox_cpu_request) + count(sandbox_cpu_limit) + count(sandbox_memory_request) + count(sandbox_memory_limit) FROM agents WHERE org_id = $1 AND id = $2`, org, agent).Scan(&set)).To(Succeed())
		Expect(set).To(BeZero())

		By("an override is stored on the same (org_id, id) row")
		_, err = db.ExecContext(ctx, `UPDATE agents SET sandbox_cpu_limit = '2', sandbox_memory_limit = '4Gi' WHERE org_id = $1 AND id = $2`, org, agent)
		Expect(err).NotTo(HaveOccurred())

		By("an empty or overlong value is refused")
		_, err = db.ExecContext(ctx, `UPDATE agents SET sandbox_cpu_request = '' WHERE org_id = $1 AND id = $2`, org, agent)
		Expect(err).To(MatchError(ContainSubstring("sandbox_cpu_request")))
		_, err = db.ExecContext(ctx, `UPDATE agents SET sandbox_memory_request = repeat('9', 33) WHERE org_id = $1 AND id = $2`, org, agent)
		Expect(err).To(MatchError(ContainSubstring("sandbox_memory_request")))
	})
})
