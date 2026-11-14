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

var _ = Describe("the one queued run per task migration (#678)", func() {
	It("repairs duplicate queued runs, with their events, before it adds the index", func(ctx SpecContext) {
		names, err := fs.Glob(migrations.FS, "*_one_queued_run_per_task.sql")
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

		scan := func(query string, args ...any) string {
			GinkgoHelper()
			var s string
			Expect(db.QueryRowContext(ctx, query, args...).Scan(&s)).To(Succeed(), query)
			return s
		}
		org := scan(`INSERT INTO orgs (slug, display_name, kind, deleted_at) VALUES ('dup-queued', 'Dup', 'personal', now()) RETURNING id`)
		agent := scan(`INSERT INTO agents (org_id, display_name, harness) VALUES ($1, 'a', 'opencode') RETURNING id`, org)
		version1 := scan(`INSERT INTO agent_versions (org_id, agent_id, version, config) VALUES ($1, $2, 1, '{}') RETURNING id`, org, agent)
		_, err = db.ExecContext(ctx, `UPDATE agents SET status = 'active', active_version_id = $2, activated_at = now() WHERE id = $1`, agent, version1)
		Expect(err).NotTo(HaveOccurred())
		task := scan(`INSERT INTO tasks (org_id, title, agent_id, created_by_kind, created_by_agent_id) VALUES ($1, 'x', $2, 'agent', $2) RETURNING id`, org, agent)
		var runs []string
		for range 3 {
			runs = append(runs, scan(`INSERT INTO runs (org_id, agent_id, agent_version_id, task_id, ready_at, max_tokens) VALUES ($1, $2, $3, $4, now(), 1000) RETURNING id`, org, agent, version1, task))
		}
		_, err = db.ExecContext(ctx, `UPDATE runs SET failure_class = 'sandbox_unavailable' WHERE id = $1`, runs[2])
		Expect(err).NotTo(HaveOccurred())

		_, err = provider.Up(ctx)
		Expect(err).NotTo(HaveOccurred())

		count := func(query string, args ...any) int {
			GinkgoHelper()
			var n int
			Expect(db.QueryRowContext(ctx, query, args...).Scan(&n)).To(Succeed(), query)
			return n
		}
		Expect(count(`SELECT count(*) FROM runs WHERE task_id = $1 AND state = 'queued'`, task)).To(Equal(1))
		Expect(scan(`SELECT id FROM runs WHERE task_id = $1 AND state = 'queued'`, task)).To(Equal(runs[0]), "the oldest stays")
		Expect(count(`SELECT count(*) FROM runs WHERE task_id = $1 AND state = 'canceled' AND finished_at IS NOT NULL`, task)).To(Equal(2))
		Expect(count(`SELECT count(*) FROM run_events WHERE kind = 'run.canceled' AND run_id = ANY($1)`, []string{runs[1], runs[2]})).To(Equal(2))
		Expect(count(`SELECT count(*) FROM task_events WHERE kind = 'run.canceled' AND task_id = $1`, task)).To(Equal(2))
		for _, id := range runs[1:] {
			Expect(scan(`SELECT outcome_reason FROM runs WHERE id = $1`, id)).To(Equal("superseded by a queued run of the task"))
			Expect(scan(`SELECT payload->>'reason' || '|' || (payload->>'message') FROM run_events WHERE run_id = $1 AND kind = 'run.canceled'`, id)).
				To(Equal("requeue_superseded|superseded by a queued run of the task"))
		}
		Expect(scan(`SELECT payload->>'failureClass' FROM run_events WHERE run_id = $1 AND kind = 'run.canceled'`, runs[2])).To(Equal("sandbox_unavailable"))
		Expect(count(`SELECT count(*) FROM run_events WHERE run_id = $1 AND kind = 'run.canceled' AND NOT payload ? 'failureClass'`, runs[1])).To(Equal(1))
		Expect(count(`SELECT count(DISTINCT seq) FROM task_events WHERE task_id = $1`, task)).To(Equal(2))

		By("the index now refuses a second queued run")
		_, err = db.ExecContext(ctx, `INSERT INTO runs (org_id, agent_id, agent_version_id, task_id, ready_at, max_tokens) VALUES ($1, $2, $3, $4, now(), 1000)`, org, agent, version1, task)
		Expect(err).To(HaveOccurred())
	})
})
