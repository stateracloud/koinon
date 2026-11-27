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

var _ = Describe("the typed task events migration (#735)", func() {
	It("marks as prompt exactly the rows that the old prompt query read, old and new, and keeps their payload as version 0", func(ctx SpecContext) {
		names, err := fs.Glob(migrations.FS, "*_task_events_typed.sql")
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
		org := scan(`INSERT INTO orgs (slug, display_name, kind, deleted_at) VALUES ('typed-events', 'Typed', 'personal', now()) RETURNING id`)
		agent := scan(`INSERT INTO agents (org_id, display_name, harness) VALUES ($1, 'a', 'opencode') RETURNING id`, org)
		user := scan(`INSERT INTO users (oidc_issuer, oidc_subject, personal_org_id) VALUES ('https://idp.test', 'typed-events', $1) RETURNING id`, org)
		task := scan(`INSERT INTO tasks (org_id, title, agent_id, created_by_kind, created_by_agent_id) VALUES ($1, 'x', $2, 'agent', $2) RETURNING id`, org, agent)
		rows := []struct {
			kind, actor, payload string
			prompt               bool
		}{
			{"task.created", "user", `{"title":"x"}`, false},
			{"message", "user", `{"text":"hello"}`, true},
			{"message", "system", `{"text":"run the report","source":"trigger","fire":"scheduled"}`, true},
			{"message", "system", `{"text":"go on","source":"continuation","continuation":1,"of":3}`, true},
			{"message", "system", `{"text":"a note without a source"}`, false},
			{"message", "agent", `{"text":"done"}`, false},
			{"status.changed", "user", `{"from":"working","to":"canceled","source":"x"}`, false},
		}
		for i, r := range rows {
			var byUser, byAgent any
			switch r.actor {
			case "user":
				byUser = user
			case "agent":
				byAgent = agent
			}
			_, err = db.ExecContext(ctx, `INSERT INTO task_events (org_id, task_id, seq, kind, actor_kind, actor_user_id, actor_agent_id, payload) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
				org, task, i+1, r.kind, r.actor, byUser, byAgent, r.payload)
			Expect(err).NotTo(HaveOccurred())
		}

		_, err = provider.Up(ctx)
		Expect(err).NotTo(HaveOccurred())

		for i, r := range rows {
			var prompt bool
			var v int
			Expect(db.QueryRowContext(ctx, `SELECT prompt, payload_version FROM task_events WHERE task_id = $1 AND seq = $2`, task, i+1).Scan(&prompt, &v)).To(Succeed())
			Expect(prompt).To(Equal(r.prompt), "row %d (%s by %s)", i+1, r.kind, r.actor)
			Expect(v).To(BeZero())
		}

		By("the trigger of the old rule is gone: an insert without the mark is not a prompt (#1393)")
		Expect(scan(`SELECT count(*)::text FROM pg_trigger WHERE tgrelid = 'task_events'::regclass AND tgname = 'task_events_prompt_by_old_rule'`)).To(Equal("0"))
		Expect(scan(`SELECT count(*)::text FROM pg_proc WHERE proname = 'task_events_prompt_by_old_rule'`)).To(Equal("0"))
		_, err = db.ExecContext(ctx, `INSERT INTO task_events (org_id, task_id, seq, kind, actor_kind, actor_user_id, payload) VALUES ($1, $2, 100, 'message', 'user', $3, '{"text":"hello"}')`, org, task, user)
		Expect(err).NotTo(HaveOccurred())
		Expect(scan(`SELECT prompt::text || '|' || payload_version FROM task_events WHERE task_id = $1 AND seq = 100`, task)).To(Equal("false|0"))

		By("a new binary's own mark is kept")
		_, err = db.ExecContext(ctx, `INSERT INTO task_events (org_id, task_id, seq, kind, actor_kind, actor_user_id, payload, payload_version, prompt) VALUES ($1, $2, 99, 'message', 'user', $3, '{}', 1, true)`, org, task, user)
		Expect(err).NotTo(HaveOccurred())
		Expect(scan(`SELECT prompt::text || '|' || payload_version FROM task_events WHERE task_id = $1 AND seq = 99`, task)).To(Equal("true|1"))
		_, err = db.ExecContext(ctx, `UPDATE task_events SET payload_version = -1 WHERE task_id = $1 AND seq = 99`, task)
		Expect(err).To(HaveOccurred(), "a version is never negative")
	})
})
