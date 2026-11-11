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

// tasksMigrationVersion finds the tasks migration by name, so no test hard-codes its number.
func tasksMigrationVersion() int64 {
	GinkgoHelper()
	names, err := fs.Glob(migrations.FS, "*_tasks.sql")
	Expect(err).NotTo(HaveOccurred())
	Expect(names).To(HaveLen(1))
	v, err := strconv.ParseInt(strings.SplitN(names[0], "_", 2)[0], 10, 64)
	Expect(err).NotTo(HaveOccurred())
	return v
}

var _ = Describe("the tasks migration", func() {
	It("moves tickets with their timelines, runs, approvals, spend and saved work to tasks", func(ctx SpecContext) {
		version := tasksMigrationVersion()
		old, err := sql.Open("pgx", pgtest.Run(ctx))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(old.Close)
		provider, err := goose.NewProvider(goose.DialectPostgres, old, migrations.FS)
		Expect(err).NotTo(HaveOccurred())
		_, err = provider.UpTo(ctx, version-1)
		Expect(err).NotTo(HaveOccurred())

		exec := func(query string, args ...any) {
			GinkgoHelper()
			_, err := old.ExecContext(ctx, query, args...)
			Expect(err).NotTo(HaveOccurred(), query)
		}
		scan := func(query string, args ...any) string {
			GinkgoHelper()
			var s string
			Expect(old.QueryRowContext(ctx, query, args...).Scan(&s)).To(Succeed(), query)
			return s
		}

		By("an org with an active agent")
		// (A deleted org: the manual-owner rule is checked for live orgs only.)
		org := scan(`INSERT INTO orgs (slug, display_name, kind, deleted_at) VALUES ('before', 'Before', 'personal', now()) RETURNING id`)
		user := scan(`INSERT INTO users (oidc_issuer, oidc_subject, personal_org_id) VALUES ('https://idp.test', 'sub-before', $1) RETURNING id`, org)
		agent := scan(`INSERT INTO agents (org_id, display_name, harness) VALUES ($1, 'helper', 'opencode') RETURNING id`, org)
		agentVersion := scan(`INSERT INTO agent_versions (org_id, agent_id, version, config) VALUES ($1, $2, 1, '{}') RETURNING id`, org, agent)
		exec(`UPDATE agents SET status = 'active', active_version_id = $2, activated_at = now() WHERE id = $1`, agent, agentVersion)

		By("a ticket in each board column, and one whose creator is gone")
		ticket := map[string]string{}
		for _, col := range []string{"todo", "in_progress", "waiting_on_you", "done"} {
			done := "NULL"
			if col == "done" {
				done = "'2026-10-01T10:00:00Z'"
			}
			ticket[col] = scan(`INSERT INTO tickets (org_id, title, status, agent_id, created_by, done_at) VALUES ($1, $2, $3, $4, $5, `+done+`) RETURNING id`,
				org, "ticket "+col, col, agent, user)
		}
		orphan := scan(`INSERT INTO tickets (org_id, title, agent_id) VALUES ($1, 'orphan', $2) RETURNING id`, org, agent)
		exec(`UPDATE tickets SET harness_session_id = 'ses_1', harness_session_cwd = '/sandbox/work', agent_summary = 'where it stands', agent_summary_at = now() WHERE id = $1`, ticket["in_progress"])

		By("timelines, with column moves")
		event := func(t string, seq int, kind, actor, payload string) {
			GinkgoHelper()
			userID := "NULL"
			if actor == "user" {
				userID = "'" + user + "'"
			}
			exec(`INSERT INTO ticket_events (org_id, ticket_id, seq, kind, actor_kind, actor_user_id, payload) VALUES ($1, $2, $3, $4, $5, `+userID+`, $6)`,
				org, t, seq, kind, actor, payload)
			exec(`UPDATE tickets SET last_event_seq = $2 WHERE id = $1`, t, seq)
		}
		event(ticket["done"], 1, "ticket.created", "user", `{"title": "ticket done"}`)
		event(ticket["done"], 2, "message", "user", `{"text": "please fix"}`)
		event(ticket["done"], 3, "status.changed", "user", `{"from": "todo", "to": "in_progress"}`)
		event(ticket["done"], 4, "status.changed", "user", `{"from": "in_progress", "to": "waiting_on_you"}`)
		event(ticket["done"], 5, "status.changed", "user", `{"from": "waiting_on_you", "to": "done"}`)
		event(ticket["done"], 6, "status.changed", "system", `{"from": "something", "to": "else"}`)

		By("a parked run with an approval, spend, saved work and an incident, on the Waiting on you ticket")
		wt := ticket["waiting_on_you"]
		run := scan(`INSERT INTO runs (org_id, agent_id, agent_version_id, ticket_id, ready_at, max_tokens) VALUES ($1, $2, $3, $4, now(), 1000) RETURNING id`, org, agent, agentVersion, wt)
		exec(`UPDATE runs SET state = 'starting', ready_at = NULL, owner_id = 'agentd-0', lease_expires_at = now() + interval '1 hour' WHERE id = $1`, run)
		exec(`UPDATE runs SET state = 'running' WHERE id = $1`, run)
		exec(`UPDATE runs SET state = 'parked', park_reason = 'approval', parked_at = now(), owner_id = NULL, lease_expires_at = NULL WHERE id = $1`, run)
		argsJSON := `{"title":"fix"}`
		approval := scan(`INSERT INTO approval_requests (org_id, ticket_id, run_id, agent_id, connector, tool, target, args_json, args_sha256, decision, expires_at)
			VALUES ($1, $2, $3, $4, 'github', 'open_pull_request', 'github:acme/widgets', $5, encode(sha256(convert_to($5, 'UTF8')), 'hex'), 'ask', now() + interval '7 days') RETURNING id`,
			org, wt, run, agent, argsJSON)
		spend := scan(`INSERT INTO spend_entries (org_id, agent_id, run_id, ticket_id, request_id, key_id, model, input_tokens, output_tokens, cost_usd, priced, occurred_at)
			VALUES ($1, $2, $3, $4, 'req-1', 'key-1', 'm', 10, 20, 0.5, true, now()) RETURNING id`, org, agent, run, wt)
		saved := scan(`INSERT INTO saved_work (org_id, ticket_id, run_id, agent_id, repo, base_sha, branch, manifest, size_bytes, envelope, expires_at)
			VALUES ($1, $2, $3, $4, 'acme/widgets', repeat('a', 40), 'fix', '{}', 10, '{}', now() + interval '1 day') RETURNING id`, org, wt, run, agent)
		incident := scan(`INSERT INTO sandbox_incidents (org_id, agent_id, run_id, ticket_id, processes, outcome, work, saved_work_id)
			VALUES ($1, $2, $3, $4, '[]', 'recreate', 'saved', $5) RETURNING id`, org, agent, run, wt, saved)
		event(wt, 1, "run.parked", "system", `{"reason": "approval"}`)

		_, err = provider.Up(ctx)
		Expect(err).NotTo(HaveOccurred())

		By("every ticket is a task with the same id, in the state of its column")
		type taskRow struct {
			state, kind string
			createdBy   sql.NullString
			finished    bool
		}
		task := func(id string) taskRow {
			GinkgoHelper()
			var r taskRow
			Expect(old.QueryRowContext(ctx, `SELECT state::text, created_by_kind::text, created_by_user_id::text, finished_at IS NOT NULL FROM tasks WHERE id = $1`, id).
				Scan(&r.state, &r.kind, &r.createdBy, &r.finished)).To(Succeed())
			return r
		}
		for col, want := range map[string]string{"todo": "submitted", "in_progress": "working", "waiting_on_you": "input_required", "done": "completed"} {
			got := task(ticket[col])
			Expect(got.state).To(Equal(want), col)
			Expect(got.kind).To(Equal("user"), col)
			Expect(got.createdBy.String).To(Equal(user), col)
			Expect(got.finished).To(Equal(col == "done"), col)
		}
		o := task(orphan)
		Expect(o.state).To(Equal("submitted"))
		Expect(o.kind).To(Equal("user"))
		Expect(o.createdBy.Valid).To(BeFalse())

		var title, session, summary string
		Expect(old.QueryRowContext(ctx, `SELECT title, harness_session_id, agent_summary FROM tasks WHERE id = $1`, ticket["in_progress"]).Scan(&title, &session, &summary)).To(Succeed())
		Expect([]string{title, session, summary}).To(Equal([]string{"ticket in_progress", "ses_1", "where it stands"}))
		Expect(scan(`SELECT coalesce(to_regclass('tickets')::text, '')`)).To(BeEmpty(), "the tickets table is gone")

		By("timelines keep their order, with column names turned into states")
		rows, err := old.QueryContext(ctx, `SELECT seq, kind, payload::text FROM task_events WHERE task_id = $1 ORDER BY seq`, ticket["done"])
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = rows.Close() }()
		type ev struct {
			seq           int64
			kind, payload string
		}
		var events []ev
		for rows.Next() {
			var e ev
			Expect(rows.Scan(&e.seq, &e.kind, &e.payload)).To(Succeed())
			events = append(events, e)
		}
		Expect(rows.Err()).NotTo(HaveOccurred())
		Expect(events).To(HaveLen(6))
		Expect(events[0].kind).To(Equal("task.created"))
		Expect(events[1].kind).To(Equal("message"))
		Expect(events[1].payload).To(MatchJSON(`{"text": "please fix"}`))
		Expect(events[2].payload).To(MatchJSON(`{"from": "submitted", "to": "working"}`))
		Expect(events[3].payload).To(MatchJSON(`{"from": "working", "to": "input_required"}`))
		Expect(events[4].payload).To(MatchJSON(`{"from": "input_required", "to": "completed"}`))
		Expect(events[5].payload).To(MatchJSON(`{"from": "something", "to": "else"}`), "a payload that names no column is left alone")
		Expect(scan(`SELECT last_event_seq::text FROM tasks WHERE id = $1`, ticket["done"])).To(Equal("6"))

		By("runs, approvals, spend, saved work and incidents point at the task")
		for _, q := range []struct{ table, id string }{
			{"runs", run}, {"approval_requests", approval}, {"spend_entries", spend}, {"saved_work", saved}, {"sandbox_incidents", incident},
		} {
			Expect(scan(`SELECT task_id::text FROM `+q.table+` WHERE id = $1`, q.id)).To(Equal(wt), q.table)
		}
		Expect(scan(`SELECT state::text || ' ' || park_reason::text FROM runs WHERE id = $1`, run)).To(Equal("parked approval"))
		Expect(scan(`SELECT task_id::text FROM task_events WHERE kind = 'run.parked'`)).To(Equal(wt))

		By("nothing is named after tickets any more, and the claim query still works")
		Expect(scan(`SELECT count(*)::text FROM pg_constraint WHERE conname LIKE '%ticket%'`)).To(Equal("0"))
		Expect(scan(`SELECT count(*)::text FROM pg_indexes WHERE indexname LIKE '%ticket%'`)).To(Equal("0"))
		Expect(scan(`SELECT count(*)::text FROM pg_trigger WHERE tgname LIKE '%ticket%'`)).To(Equal("0"))
		Expect(scan(`SELECT run_claimable($1, $2)::text`, org, run)).To(Equal("false"), "a parked run waits for its decision, and the function reads task_id")

		By("the new rules: a task has an end-state finish, one creator and an owner")
		_, err = old.ExecContext(ctx, `UPDATE tasks SET state = 'canceled' WHERE id = $1`, ticket["todo"])
		Expect(err).To(MatchError(ContainSubstring("tasks_finished_at")))
		_, err = old.ExecContext(ctx, `INSERT INTO tasks (org_id, title, agent_id, created_by_kind) VALUES ($1, 'x', $2, 'agent')`, org, agent)
		Expect(err).To(MatchError(ContainSubstring("tasks_created_by_actor")))
		_, err = old.ExecContext(ctx, `INSERT INTO tasks (org_id, title, agent_id, created_by_kind, created_by_user_id) VALUES ($1, 'x', $2, 'trigger', $3)`, org, agent, user)
		Expect(err).To(MatchError(ContainSubstring("tasks_created_by_actor")))
		_, err = old.ExecContext(ctx, `INSERT INTO tasks (org_id, title, created_by_kind) VALUES ($1, 'x', 'user')`, org)
		Expect(err).To(MatchError(ContainSubstring("tasks_owned_by_agent")))
		for _, ins := range []struct{ query, args string }{
			{`INSERT INTO tasks (org_id, title, agent_id, created_by_kind, created_by_agent_id) VALUES ($1, 'by agent', $2, 'agent', $2)`, "agent"},
			{`INSERT INTO tasks (org_id, title, agent_id, created_by_kind, created_by_trigger_id) VALUES ($1, 'by trigger', $2, 'trigger', uuidv7())`, "trigger"},
		} {
			_, err = old.ExecContext(ctx, ins.query, org, agent)
			Expect(err).NotTo(HaveOccurred(), ins.args)
		}
	})
})
