// SPDX-License-Identifier: Apache-2.0

package migrations_test

import (
	"database/sql"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/pressly/goose/v3"

	"github.com/stateracloud/koinon/internal/db/migrations"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

// The actor kind `external` (ADR 0130, #1408): a person without a Koinon account can only write
// messages, and each of their events names their external_actors row.
var _ = Describe("the external actors migrations (#1408)", func() {
	var (
		db           *sql.DB
		org, task    string
		user, person string
	)
	scan := func(ctx SpecContext, query string, args ...any) string {
		GinkgoHelper()
		var s string
		Expect(db.QueryRowContext(ctx, query, args...).Scan(&s)).To(Succeed(), query)
		return s
	}
	insertPrompt := func(ctx SpecContext, kind, actor string, byUser, byExternal any, prompt bool) error {
		_, err := db.ExecContext(ctx, `INSERT INTO task_events (org_id, task_id, seq, kind, actor_kind, actor_user_id, actor_external_id, payload, prompt)
			VALUES ($1, $2, (SELECT coalesce(max(seq), 0) + 1 FROM task_events WHERE task_id = $2), $3, $4, $5, $6, '{"text":"hi","source":"slack"}', $7)`,
			org, task, kind, actor, byUser, byExternal, prompt)
		return err
	}
	insert := func(ctx SpecContext, kind, actor string, byUser, byExternal any) error {
		return insertPrompt(ctx, kind, actor, byUser, byExternal, kind == "message")
	}

	BeforeEach(func(ctx SpecContext) {
		var err error
		db, err = sql.Open("pgx", pgtest.Run(ctx))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(db.Close)
		provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
		Expect(err).NotTo(HaveOccurred())
		_, err = provider.Up(ctx)
		Expect(err).NotTo(HaveOccurred())

		org = scan(ctx, `INSERT INTO orgs (slug, display_name, kind, deleted_at) VALUES ('external-actors', 'External', 'personal', now()) RETURNING id`)
		agent := scan(ctx, `INSERT INTO agents (org_id, display_name, harness) VALUES ($1, 'a', 'opencode') RETURNING id`, org)
		user = scan(ctx, `INSERT INTO users (oidc_issuer, oidc_subject, personal_org_id) VALUES ('https://idp.test', 'external-actors', $1) RETURNING id`, org)
		task = scan(ctx, `INSERT INTO tasks (org_id, title, agent_id, created_by_kind, created_by_agent_id) VALUES ($1, 'x', $2, 'agent', $2) RETURNING id`, org, agent)
		person = scan(ctx, `INSERT INTO external_actors (org_id, platform, team, platform_user_id) VALUES ($1, 'slack', 'T0TEST', 'U0TEST') RETURNING id`, org)
	})

	It("records a message by an external actor", func(ctx SpecContext) {
		Expect(insert(ctx, "message", "external", nil, person)).To(Succeed())
	})

	It("refuses every other kind by an external actor: no approval, answer or state change", func(ctx SpecContext) {
		for _, kind := range []string{"approval.approved", "question.answered", "status.changed", "task.created"} {
			Expect(insert(ctx, kind, "external", nil, person)).To(MatchError(ContainSubstring("task_events_external_message_check")), kind)
		}
	})

	It("needs the external actor's row for an external event, and refuses it for any other actor", func(ctx SpecContext) {
		Expect(insert(ctx, "message", "external", nil, nil)).To(MatchError(ContainSubstring("task_events_actor_external_check")))
		Expect(insert(ctx, "message", "user", user, person)).To(MatchError(ContainSubstring("task_events_actor_external_check")))
		Expect(insert(ctx, "message", "system", nil, person)).To(MatchError(ContainSubstring("task_events_actor_external_check")))
	})

	It("refuses a message by an external actor that is not part of the run's prompt", func(ctx SpecContext) {
		Expect(insertPrompt(ctx, "message", "external", nil, person, false)).To(MatchError(ContainSubstring("task_events_external_prompt_check")))
	})

	It("refuses an external actor of another org", func(ctx SpecContext) {
		other := scan(ctx, `INSERT INTO orgs (slug, display_name, kind, deleted_at) VALUES ('external-other', 'Other', 'personal', now()) RETURNING id`)
		stranger := scan(ctx, `INSERT INTO external_actors (org_id, platform, team, platform_user_id) VALUES ($1, 'slack', 'T0TEST', 'U0TEST') RETURNING id`, other)
		Expect(insert(ctx, "message", "external", nil, stranger)).To(MatchError(ContainSubstring("task_events_actor_external_fkey")))
	})

	It("keeps one row for each person of an org, and only known platforms", func(ctx SpecContext) {
		_, err := db.ExecContext(ctx, `INSERT INTO external_actors (org_id, platform, team, platform_user_id) VALUES ($1, 'slack', 'T0TEST', 'U0TEST')`, org)
		Expect(err).To(MatchError(ContainSubstring("duplicate key")))
		_, err = db.ExecContext(ctx, `INSERT INTO external_actors (org_id, platform, team, platform_user_id) VALUES ($1, 'irc', 'T0TEST', 'U0TEST')`, org)
		Expect(err).To(MatchError(ContainSubstring("external_actors_platform_check")))
		_, err = db.ExecContext(ctx, `INSERT INTO external_actors (org_id, platform, team, platform_user_id) VALUES ($1, 'teams', 'T0TEST', '')`, org)
		Expect(err).To(MatchError(ContainSubstring("external_actors_platform_user_id_check")))
	})
})
