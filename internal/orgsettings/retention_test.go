// SPDX-License-Identifier: Apache-2.0

package orgsettings_test

import (
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/orgsettings"
	"github.com/stateracloud/koinon/internal/runoutcome"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

var _ = Describe("Run content retention", func() {
	var (
		t0    = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
		clock time.Time
		svc   *orgsettings.Service
	)
	BeforeEach(func() {
		clock = t0
		svc = orgsettings.NewService(pool)
	})
	now := func() time.Time { return clock }

	// finishedRun inserts a run that ended `age` before t0, with n events carrying content. The purge
	// only ever updates run_events, so traces and the ledger have no query to be touched by.
	finishedRun := func(ctx SpecContext, org pgtype.UUID, age time.Duration, n int) pgtype.UUID {
		GinkgoHelper()
		var agent, version, user, task, run pgtype.UUID
		Expect(pool.QueryRow(ctx, `SELECT id FROM users LIMIT 1`).Scan(&user)).To(Succeed())
		Expect(pool.QueryRow(ctx, `INSERT INTO agents (org_id, display_name, harness) VALUES ($1, 'helper', 'opencode') RETURNING id`, org).Scan(&agent)).To(Succeed())
		Expect(pool.QueryRow(ctx, `INSERT INTO agent_versions (org_id, agent_id, version, config) VALUES ($1, $2, 1, '{}') RETURNING id`, org, agent).Scan(&version)).To(Succeed())
		_, err := pool.Exec(ctx, `UPDATE agents SET status = 'active', active_version_id = $2, activated_at = now() WHERE id = $1`, agent, version)
		Expect(err).NotTo(HaveOccurred())
		Expect(pool.QueryRow(ctx, `INSERT INTO tasks (org_id, title, agent_id, created_by_kind, created_by_user_id) VALUES ($1, 't', $2, 'user', $3) RETURNING id`, org, agent, user).Scan(&task)).To(Succeed())
		Expect(pool.QueryRow(ctx, `INSERT INTO runs (org_id, agent_id, agent_version_id, task_id, ready_at, max_tokens) VALUES ($1, $2, $3, $4, now(), 1000) RETURNING id`, org, agent, version, task).Scan(&run)).To(Succeed())
		for i := 1; i <= n; i++ {
			_, err := pool.Exec(ctx, `INSERT INTO run_events (org_id, run_id, seq, epoch, kind, payload) VALUES ($1, $2, $3, 0, 'acp.agent_message_chunk', '{"text":"secret words"}')`, org, run, i)
			Expect(err).NotTo(HaveOccurred())
		}
		if age >= 0 {
			_, err = pool.Exec(ctx, `UPDATE runs SET state = 'canceled', ready_at = NULL, finished_at = $2 WHERE id = $1`, run, t0.Add(-age))
			Expect(err).NotTo(HaveOccurred())
		}
		return run
	}
	purged := func(ctx SpecContext, run pgtype.UUID) (blank, total int) {
		GinkgoHelper()
		Expect(pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE payload = '{}' AND content_purged_at IS NOT NULL), count(*) FROM run_events WHERE run_id = $1`, run).Scan(&blank, &total)).To(Succeed())
		return blank, total
	}

	It("defaults to 30 days and lets an org admin change it, audited", func(ctx SpecContext) {
		org, _, _ := pgtest.NewOrg(ctx, pool)
		days, err := svc.RunContentRetentionDays(ctx, org)
		Expect(err).NotTo(HaveOccurred())
		Expect(days).To(Equal(30))

		seven := 7
		Expect(svc.SetRunContentRetention(ctx, org, audit.System("koinon-test:spec"), &seven)).To(Succeed())
		days, _ = svc.RunContentRetentionDays(ctx, org)
		Expect(days).To(Equal(7))

		Expect(svc.SetRunContentRetention(ctx, org, audit.System("koinon-test:spec"), nil)).To(Succeed())
		days, _ = svc.RunContentRetentionDays(ctx, org)
		Expect(days).To(Equal(30))

		var changes int
		Expect(pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'org.run_content_retention.set'`, org).Scan(&changes)).To(Succeed())
		Expect(changes).To(Equal(2))
	})

	It("rejects a retention outside 1 to 365 days", func(ctx SpecContext) {
		org, _, _ := pgtest.NewOrg(ctx, pool)
		for _, d := range []int{-1, 366} {
			Expect(svc.SetRunContentRetention(ctx, org, audit.System("koinon-test:spec"), &d)).To(MatchError(ContainSubstring("days")))
		}
	})

	It("blanks content older than the retention, keeps newer content and other orgs", func(ctx SpecContext) {
		a, _, _ := pgtest.NewOrg(ctx, pool)
		b, _, _ := pgtest.NewOrg(ctx, pool)
		old := finishedRun(ctx, a, 31*24*time.Hour, 3)
		fresh := finishedRun(ctx, a, 29*24*time.Hour, 3)
		other := finishedRun(ctx, b, 31*24*time.Hour, 2)
		ten := 10
		Expect(svc.SetRunContentRetention(ctx, b, audit.System("koinon-test:spec"), &ten)).To(Succeed())

		p := orgsettings.NewPurger(pool, nil, now)
		n, err := p.PurgeOrg(ctx, a)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(BeEquivalentTo(3))

		blank, total := purged(ctx, old)
		Expect(blank).To(Equal(3))
		Expect(total).To(Equal(3), "the event rows stay: the run keeps its skeleton")
		blank, _ = purged(ctx, fresh)
		Expect(blank).To(Equal(0))
		blank, _ = purged(ctx, other)
		Expect(blank).To(Equal(0), "org b was not purged by org a's pass")

		// Org b's own, shorter retention applies on the sweep over every org.
		n, err = p.PurgeAll(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(BeNumerically(">=", 2))
		blank, _ = purged(ctx, other)
		Expect(blank).To(Equal(2))
		blank, _ = purged(ctx, fresh)
		Expect(blank).To(Equal(0))

		// The clock moving on makes the fresh run old.
		clock = t0.Add(2 * 24 * time.Hour)
		_, err = p.PurgeOrg(ctx, a)
		Expect(err).NotTo(HaveOccurred())
		blank, _ = purged(ctx, fresh)
		Expect(blank).To(Equal(3))
	})

	It("is idempotent and batched", func(ctx SpecContext) {
		org, _, _ := pgtest.NewOrg(ctx, pool)
		run := finishedRun(ctx, org, 40*24*time.Hour, 7)
		p := orgsettings.NewPurger(pool, nil, now).WithBatchSize(3)

		n, err := p.PurgeOrg(ctx, org)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(BeEquivalentTo(7), "three batches of 3, 3 and 1")
		n, err = p.PurgeOrg(ctx, org)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(BeZero())
		blank, total := purged(ctx, run)
		Expect(blank).To(Equal(7))
		Expect(total).To(Equal(7))
	})

	It("blanks only content kinds and marks the run purged", func(ctx SpecContext) {
		org, _, _ := pgtest.NewOrg(ctx, pool)
		run := finishedRun(ctx, org, 40*24*time.Hour, 2)
		_, err := pool.Exec(ctx, `INSERT INTO run_events (org_id, run_id, seq, epoch, kind, payload) VALUES ($1, $2, 3, 0, 'run.wrap_up_notice', '{"spent_usd":1.5}')`, org, run)
		Expect(err).NotTo(HaveOccurred())
		n, err := orgsettings.NewPurger(pool, nil, now).PurgeOrg(ctx, org)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(BeEquivalentTo(2))
		var payload string
		Expect(pool.QueryRow(ctx, `SELECT payload::text FROM run_events WHERE run_id = $1 AND kind = 'run.wrap_up_notice'`, run).Scan(&payload)).To(Succeed())
		Expect(payload).To(ContainSubstring("spent_usd"))
		var marked bool
		Expect(pool.QueryRow(ctx, `SELECT content_purged_at IS NOT NULL FROM runs WHERE id = $1`, run).Scan(&marked)).To(Succeed())
		Expect(marked).To(BeTrue())
	})

	It("lists only content kinds the event class table knows", func() {
		known := runoutcome.KnownEventKinds()
		for _, k := range orgsettings.ContentEventKinds {
			Expect(known).To(HaveKey(k))
		}
		for k := range orgsettings.StrippedContentKeys {
			Expect(known).To(HaveKey(k))
		}
	})

	It("removes the content of prompts, permission answers and refusals and keeps their state", func(ctx SpecContext) {
		org, _, _ := pgtest.NewOrg(ctx, pool)
		run := finishedRun(ctx, org, 40*24*time.Hour, 1)
		for i, ev := range []struct{ kind, payload string }{
			{"run.prompt", `{"text":"secret prompt","throughSeq":7,"resumed":true}`},
			{"acp.permission", `{"toolCall":{"title":"rm -rf secret"},"decision":{"kind":"allow","tier":"vm"}}`},
			{"run.refusal", `{"refusals":["secret call"],"count":2,"cwd":"/work"}`},
		} {
			_, err := pool.Exec(ctx, `INSERT INTO run_events (org_id, run_id, seq, epoch, kind, payload) VALUES ($1, $2, $3, 0, $4, $5::jsonb)`, org, run, 10+i, ev.kind, ev.payload)
			Expect(err).NotTo(HaveOccurred())
		}
		_, err := orgsettings.NewPurger(pool, nil, now).PurgeOrg(ctx, org)
		Expect(err).NotTo(HaveOccurred())
		get := func(kind string) string {
			var p string
			Expect(pool.QueryRow(ctx, `SELECT payload::text FROM run_events WHERE run_id = $1 AND kind = $2`, run, kind).Scan(&p)).To(Succeed())
			return p
		}
		Expect(get("run.prompt")).To(And(Not(ContainSubstring("secret")), ContainSubstring(`"throughSeq": 7`), ContainSubstring(`"resumed": true`)))
		Expect(get("acp.permission")).To(And(Not(ContainSubstring("secret")), Not(ContainSubstring("toolCall")), ContainSubstring(`"decision"`), ContainSubstring(`"tier": "vm"`)))
		Expect(get("run.refusal")).To(And(Not(ContainSubstring("secret")), ContainSubstring(`"count": 2`)))
		var marked bool
		Expect(pool.QueryRow(ctx, `SELECT content_purged_at IS NOT NULL FROM runs WHERE id = $1`, run).Scan(&marked)).To(Succeed())
		Expect(marked).To(BeTrue())
	})

	It("leaves the trace rows and the spend ledger unchanged", func(ctx SpecContext) {
		org, _, _ := pgtest.NewOrg(ctx, pool)
		run := finishedRun(ctx, org, 40*24*time.Hour, 2)
		_, err := pool.Exec(ctx, `INSERT INTO run_turns (org_id, run_id, seq, trace_id, end_reason) VALUES ($1, $2, 1, '\x0102030405060708090a0b0c0d0e0f10'::bytea, 'end_turn')`, org, run)
		Expect(err).NotTo(HaveOccurred())
		_, err = pool.Exec(ctx, `INSERT INTO spend_entries (org_id, agent_id, request_id, key_id, model, input_tokens, output_tokens, cost_usd, priced, occurred_at)
			SELECT org_id, agent_id, 'req-retention-1', 'key-1', 'm', 10, 20, 0.5, true, $2 FROM runs WHERE id = $1`, run, t0.Add(-40*24*time.Hour))
		Expect(err).NotTo(HaveOccurred())
		snapshot := func() (string, string) {
			var turns, spend string
			Expect(pool.QueryRow(ctx, `SELECT coalesce(jsonb_agg(to_jsonb(t) ORDER BY id)::text, '') FROM run_turns t WHERE org_id = $1`, org).Scan(&turns)).To(Succeed())
			Expect(pool.QueryRow(ctx, `SELECT coalesce(jsonb_agg(to_jsonb(s) ORDER BY id)::text, '') FROM spend_entries s WHERE org_id = $1`, org).Scan(&spend)).To(Succeed())
			return turns, spend
		}
		turnsBefore, spendBefore := snapshot()
		Expect(turnsBefore).NotTo(BeEmpty())
		Expect(spendBefore).NotTo(BeEmpty())

		n, err := orgsettings.NewPurger(pool, nil, now).PurgeOrg(ctx, org)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(BeEquivalentTo(2))

		turnsAfter, spendAfter := snapshot()
		Expect(turnsAfter).To(Equal(turnsBefore))
		Expect(spendAfter).To(Equal(spendBefore))
	})

	It("leaves a run that has not finished alone", func(ctx SpecContext) {
		org, _, _ := pgtest.NewOrg(ctx, pool)
		// A negative age leaves the run queued: it never finished, so its content is in use.
		run := finishedRun(ctx, org, -1, 2)
		n, err := orgsettings.NewPurger(pool, nil, now).PurgeOrg(ctx, org)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(BeZero())
		blank, total := purged(ctx, run)
		Expect(blank).To(BeZero())
		Expect(total).To(Equal(2))
	})
})
