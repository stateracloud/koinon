// SPDX-License-Identifier: Apache-2.0

package migrations_test

import (
	"context"
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

// notifyTargetsVersion finds the migration by name, so no test hard-codes its number.
func notifyTargetsVersion() int64 {
	GinkgoHelper()
	names, err := fs.Glob(migrations.FS, "*_notify_targets.sql")
	Expect(err).NotTo(HaveOccurred())
	Expect(names).To(HaveLen(1))
	v, err := strconv.ParseInt(strings.SplitN(names[0], "_", 2)[0], 10, 64)
	Expect(err).NotTo(HaveOccurred())
	return v
}

var _ = Describe("the notify targets migration", func() {
	var (
		old                 *sql.DB
		provider            *goose.Provider
		org, user, agent    string
		task, run           string
		scan                func(query string, args ...any) string
		exec                func(query string, args ...any)
		insertQuestion      func(sha, state string, openedAt string) string
		migrateToNotifyStep func(ctx context.Context)
	)

	BeforeEach(func(ctx SpecContext) {
		var err error
		old, err = sql.Open("pgx", pgtest.Run(ctx))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(old.Close)
		provider, err = goose.NewProvider(goose.DialectPostgres, old, migrations.FS)
		Expect(err).NotTo(HaveOccurred())
		version := notifyTargetsVersion()
		_, err = provider.UpTo(ctx, version-1)
		Expect(err).NotTo(HaveOccurred())
		migrateToNotifyStep = func(ctx context.Context) {
			GinkgoHelper()
			_, err := provider.UpTo(ctx, version)
			Expect(err).NotTo(HaveOccurred())
		}

		exec = func(query string, args ...any) {
			GinkgoHelper()
			_, err := old.ExecContext(context.Background(), query, args...)
			Expect(err).NotTo(HaveOccurred(), query)
		}
		scan = func(query string, args ...any) string {
			GinkgoHelper()
			var s sql.NullString
			Expect(old.QueryRowContext(context.Background(), query, args...).Scan(&s)).To(Succeed(), query)
			return s.String
		}

		// (A deleted org: the manual-owner rule is checked for live orgs only.)
		org = scan(`INSERT INTO orgs (slug, display_name, kind, deleted_at) VALUES ('notify', 'Notify', 'personal', now()) RETURNING id`)
		user = scan(`INSERT INTO users (oidc_issuer, oidc_subject, personal_org_id) VALUES ('https://idp.test', 'sub-notify', $1) RETURNING id`, org)
		agent = scan(`INSERT INTO agents (org_id, display_name, harness) VALUES ($1, 'helper', 'opencode') RETURNING id`, org)
		agentVersion := scan(`INSERT INTO agent_versions (org_id, agent_id, version, config) VALUES ($1, $2, 1, '{}') RETURNING id`, org, agent)
		exec(`UPDATE agents SET status = 'active', active_version_id = $2, activated_at = now() WHERE id = $1`, agent, agentVersion)
		task = scan(`INSERT INTO tasks (org_id, title, agent_id, created_by_kind, created_by_user_id) VALUES ($1, 't', $2, 'user', $3) RETURNING id`, org, agent, user)
		run = scan(`INSERT INTO runs (org_id, agent_id, agent_version_id, task_id, ready_at, max_tokens) VALUES ($1, $2, $3, $4, now(), 1000) RETURNING id`, org, agent, agentVersion, task)
		insertQuestion = func(sha, state, openedAt string) string {
			GinkgoHelper()
			return scan(`INSERT INTO questions (org_id, task_id, run_id, agent_id, asked_to, question, default_answer, state, call_sha256, expires_at, opened_at)
				VALUES ($1, $2, $3, $4, $5, 'q', 'd', $6, $7, now() + interval '1 day', `+openedAt+`) RETURNING id`,
				org, task, run, agent, user, state, strings.Repeat(sha, 64))
		}
	})

	It("moves each question's addressee to a recipient that has waited since it opened", func(ctx SpecContext) {
		open := insertQuestion("a", "open", "now() - interval '3 hours'")
		asked := insertQuestion("b", "asked", "NULL")
		migrateToNotifyStep(ctx)

		for _, id := range []string{open, asked} {
			Expect(scan(`SELECT target_kind::text FROM questions WHERE id = $1`, id)).To(Equal("user"))
			Expect(scan(`SELECT target_user_id::text FROM questions WHERE id = $1`, id)).To(Equal(user))
			Expect(scan(`SELECT count(*)::text FROM question_recipients WHERE question_id = $1 AND user_id = $2`, id, user)).To(Equal("1"))
		}
		Expect(scan(`SELECT (qr.waiting_since = qn.opened_at)::text FROM question_recipients qr JOIN questions qn ON qn.org_id = qr.org_id AND qn.id = qr.question_id WHERE qn.id = $1`, open)).To(Equal("true"))
		Expect(scan(`SELECT (waiting_since IS NULL)::text FROM question_recipients WHERE question_id = $1`, asked)).To(Equal("true"))
		Expect(scan(`SELECT is_nullable FROM information_schema.columns WHERE table_name = 'questions' AND column_name = 'asked_to'`)).To(Equal("YES"))
	})

	It("translates an old pod's insert", func(ctx SpecContext) {
		migrateToNotifyStep(ctx)
		// The column list before 00036: asked_to and no target_*.
		id := insertQuestion("c", "asked", "NULL")
		Expect(scan(`SELECT target_kind::text FROM questions WHERE id = $1`, id)).To(Equal("user"))
		Expect(scan(`SELECT target_user_id::text FROM questions WHERE id = $1`, id)).To(Equal(user))
		Expect(scan(`SELECT count(*)::text FROM question_recipients WHERE question_id = $1 AND user_id = $2 AND removed_at IS NULL`, id, user)).To(Equal("1"))

		// The new style: a target and asked_to, then AddQuestionRecipients, ends with one active row.
		id = scan(`INSERT INTO questions (org_id, task_id, run_id, agent_id, asked_to, target_kind, target_user_id, question, default_answer, call_sha256, expires_at)
			VALUES ($1, $2, $3, $4, $5, 'user', $5, 'q', 'd', $6, now() + interval '1 day') RETURNING id`, org, task, run, agent, user, strings.Repeat("d", 64))
		exec(`INSERT INTO question_recipients (org_id, question_id, user_id) SELECT $1, $2, unnest($3::uuid[]) ON CONFLICT DO NOTHING`, org, id, "{"+user+"}")
		Expect(scan(`SELECT count(*)::text FROM question_recipients WHERE question_id = $1 AND removed_at IS NULL`, id)).To(Equal("1"))
	})

	It("refuses a task notify target of the wrong shape", func(ctx SpecContext) {
		migrateToNotifyStep(ctx)
		bad := func(set string) {
			GinkgoHelper()
			_, err := old.ExecContext(ctx, `UPDATE tasks SET `+set+` WHERE id = $1`, task)
			Expect(err).To(HaveOccurred(), set)
			Expect(err.Error()).To(ContainSubstring("tasks_notify_target"), set)
		}
		bad(`notify_kind = 'role', notify_role = 'viewer'`)
		bad(`notify_kind = 'user'`)
		bad(`notify_kind = 'role', notify_role = 'admin', notify_user_id = '` + user + `'`)
		// A channel is accepted by the database; the API refuses it until v0.2 adds the foreign key.
		exec(`UPDATE tasks SET notify_kind = 'channel', notify_channel_id = '00000000-0000-7000-8000-00000000c001' WHERE id = $1`, task)
	})
})
