// SPDX-License-Identifier: Apache-2.0

package router_test

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/checkpoint"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/secretstore/secretstoretest"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
	"github.com/stateracloud/koinon/internal/tracedest"
	"github.com/stateracloud/koinon/internal/tracing/router"
	"github.com/stateracloud/koinon/internal/tracing/scrub"
)

// The Postgres side of the router: attribution, the ledger query, the secret set and the health
// writes, against a real database (skipped without Docker).
var _ = Describe("Postgres", Ordered, func() {
	var (
		pool  *pgxpool.Pool
		q     *store.Queries
		org   pgtype.UUID
		user  pgtype.UUID
		slug  string
		run   pgtype.UUID
		agent pgtype.UUID
		task  pgtype.UUID
	)

	BeforeAll(func(ctx SpecContext) {
		pool = pgtest.Start(ctx) // skips without Docker (testenv)
		q = store.New(pool)
		org, user, slug = pgtest.NewOrg(ctx, pool)

		Expect(pool.QueryRow(ctx, `INSERT INTO agents (org_id, display_name, harness, created_by) VALUES ($1, 'Router agent', 'opencode', $2) RETURNING id`, org, user).Scan(&agent)).To(Succeed())
		version, err := q.CreateAgentVersion(ctx, store.CreateAgentVersionParams{OrgID: org, AgentID: agent, Config: []byte(`{"instructions":""}`), CreatedBy: user})
		Expect(err).NotTo(HaveOccurred())
		_, err = q.ActivateAgent(ctx, store.ActivateAgentParams{OrgID: org, ID: agent, VersionID: version.ID, ActivatedBy: user})
		Expect(err).NotTo(HaveOccurred())
		tk, err := q.CreateTask(ctx, store.CreateTaskParams{OrgID: org, Title: "t", AgentID: agent, CreatedByKind: store.TaskCreatorKindUser, CreatedByUserID: user})
		Expect(err).NotTo(HaveOccurred())
		task = tk.ID
		r, err := q.CreateRun(ctx, store.CreateRunParams{OrgID: org, AgentID: agent, TaskID: task, MaxWallClockSeconds: 1800})
		Expect(err).NotTo(HaveOccurred())
		run = r.ID
		DeferCleanup(func(ctx context.Context) {
			_, err := pool.Exec(ctx, `UPDATE runs SET state = 'canceled', outcome_reason = 'spec ended', finished_at = now(), ready_at = NULL WHERE org_id = $1 AND state NOT IN ('succeeded', 'failed', 'canceled')`, org)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	It("attributes a span only to a traced run, in its own org, with its own trace ID", func(ctx SpecContext) {
		s := router.NewPGStore(pool)
		var trace [16]byte
		trace[0] = 0x51

		_, err := s.Attribute(ctx, org, uid("0192f5a0-0000-7000-8000-0000000000ff"), trace)
		Expect(err).To(MatchError(router.ErrUnknownRun))

		_, err = s.Attribute(ctx, org, run, trace)
		Expect(err).To(MatchError(router.ErrUntraced), "traced is NULL until agentd decides")

		_, err = q.DecideRunTraced(ctx, store.DecideRunTracedParams{OrgID: org, ID: run, Traced: true})
		Expect(err).NotTo(HaveOccurred())
		_, err = s.Attribute(ctx, org, run, trace)
		Expect(err).To(MatchError(router.ErrUnknownTrace))

		row, err := q.InsertRunTurn(ctx, store.InsertRunTurnParams{OrgID: org, RunID: run, TraceID: trace[:]})
		Expect(err).NotTo(HaveOccurred())
		turn, err := s.Attribute(ctx, org, run, trace)
		Expect(err).NotTo(HaveOccurred())
		Expect(turn.Seq).To(Equal(row.Seq))
		Expect(turn.EndedAt).To(BeNil())

		other, _, _ := pgtest.NewOrg(ctx, pool)
		_, err = s.Attribute(ctx, other, run, trace)
		Expect(err).To(MatchError(router.ErrUnknownRun), "another org's ID does not reach this run")

		_, err = q.EndRunTurn(ctx, store.EndRunTurnParams{OrgID: org, ID: row.ID, EndReason: pgtype.Text{String: "end_turn", Valid: true}})
		Expect(err).NotTo(HaveOccurred())
		turn, err = s.Attribute(ctx, org, run, trace)
		Expect(err).NotTo(HaveOccurred())
		Expect(turn.EndedAt).NotTo(BeNil())
	})

	It("reads a run's ledger rows in a window, with the provider's type and the cost the ledger billed", func(ctx SpecContext) {
		var provider, model pgtype.UUID
		Expect(pool.QueryRow(ctx, `INSERT INTO providers (org_id, name, type, url) VALUES ($1, 'prov', 'openrouter', 'https://openrouter.test/api/v1') RETURNING id`, org).Scan(&provider)).To(Succeed())
		Expect(pool.QueryRow(ctx, `INSERT INTO models (org_id, provider_id, name, input_rate, output_rate) VALUES ($1, $2, 'sonnet', '1', '1') RETURNING id`, org, provider).Scan(&model)).To(Succeed())

		base := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
		insert := func(req, modelName string, at time.Time, cost string, priced, provisional bool) {
			_, err := pool.Exec(ctx, `INSERT INTO spend_entries (org_id, agent_id, run_id, task_id, request_id, key_id, model, input_tokens, output_tokens,
				cache_read_tokens, cache_write_tokens, reasoning_tokens, cost_usd, gateway_cost_usd, priced, provisional, occurred_at)
				VALUES ($1, $2, $3, $4, $5, 'key', $6, 100, 20, 60, 10, 5, $7::numeric, $7::numeric, $8, $9, $10)`,
				org, agent, run, task, req, modelName, cost, priced, provisional, at)
			Expect(err).NotTo(HaveOccurred())
		}
		insert("r1", slug+"/sonnet", base.Add(time.Second), "0.012345678901", true, false)
		insert("r2", "sonnet", base.Add(2*time.Second), "0.5", true, true)
		insert("r3", "unknown-model", base.Add(3*time.Second), "0", false, false)
		insert("r-outside", "sonnet", base.Add(time.Hour), "9", true, false)

		calls, err := router.NewPGStore(pool).ModelCalls(ctx, org, run, 1, base, base.Add(10*time.Second))
		Expect(err).NotTo(HaveOccurred())
		Expect(calls).To(HaveLen(3))
		Expect(calls[0].RequestID).To(Equal("r1"))
		Expect(calls[0].Provider).To(Equal("openrouter"), "the model is found with or without the org slug")
		Expect(*calls[0].Cost).To(BeNumerically("~", 0.012345678901, 1e-12))
		Expect(calls[0].Input).To(Equal(int64(100)))
		Expect(calls[0].CacheRead).To(Equal(int64(60)))
		Expect(calls[0].Reasoning).To(Equal(int64(5)))
		Expect(calls[1].Provisional).To(BeTrue())
		Expect(calls[2].Cost).To(BeNil(), "unpriced: no cost")
		Expect(calls[2].Provider).To(BeEmpty())
		Expect(calls[2].AgentID).To(Equal(agent.String()))
		Expect(calls[0].ModelID).NotTo(BeEmpty())
		Expect(calls[2].ModelID).To(BeEmpty())

		By("a row on the boundary of two turns counts once, in the later turn")
		turn2, err := q.InsertRunTurn(ctx, store.InsertRunTurnParams{OrgID: org, RunID: run, TraceID: []byte("0123456789abcdeX")})
		Expect(err).NotTo(HaveOccurred())
		_, err = pool.Exec(ctx, `UPDATE run_turns SET started_at = $3 WHERE org_id = $1 AND id = $2`, org, turn2.ID, base.Add(2500*time.Millisecond))
		Expect(err).NotTo(HaveOccurred())
		first, err := router.NewPGStore(pool).ModelCalls(ctx, org, run, 1, base, base.Add(10*time.Second))
		Expect(err).NotTo(HaveOccurred())
		second, err := router.NewPGStore(pool).ModelCalls(ctx, org, run, 2, base.Add(2500*time.Millisecond), base.Add(10*time.Second))
		Expect(err).NotTo(HaveOccurred())
		Expect(first).To(HaveLen(2))
		Expect(second).To(HaveLen(1))
		Expect(second[0].RequestID).To(Equal("r3"))
	})

	It("builds the org's secret set from its stored credentials, and no other org's", func(ctx SpecContext) {
		ring := secretstoretest.Keyring()
		var provider pgtype.UUID
		Expect(pool.QueryRow(ctx, `SELECT id FROM providers WHERE org_id = $1`, org).Scan(&provider)).To(Succeed())
		const providerKey = "koinon_test_not-a-real-token-provider"
		env, err := ring.Seal(secretstoreBinding(org, provider), []byte(providerKey))
		Expect(err).NotTo(HaveOccurred())
		raw, _ := json.Marshal(env)
		_, err = pool.Exec(ctx, `UPDATE providers SET api_key = $3 WHERE org_id = $1 AND id = $2`, org, provider, raw)
		Expect(err).NotTo(HaveOccurred())

		set, err := router.NewPGSecrets(pool, ring, nil).Secrets(ctx, org)
		Expect(err).NotTo(HaveOccurred())
		sc := scrub.New(scrub.Config{Level: scrub.LevelFull, Secrets: set})
		Expect(sc.Text("key=" + providerKey)).To(Equal("key=[redacted:secret]"))

		other, _, _ := pgtest.NewOrg(ctx, pool)
		set, err = router.NewPGSecrets(pool, ring, nil).Secrets(ctx, other)
		Expect(err).NotTo(HaveOccurred())
		sc = scrub.New(scrub.Config{Level: scrub.LevelFull, Secrets: set})
		Expect(sc.Text("key=" + providerKey)).To(Equal("key=" + providerKey))
	})

	It("scrubs every repository epoch's home repository password, and fingerprints a new epoch", func(ctx SpecContext) {
		ring := secretstoretest.Keyring()
		pw := checkpoint.Passwords{Q: q, Keys: ring, Begin: pool.Begin}
		// An org of its own: the other specs seal this org's credentials with another keyring.
		org, _, _ := pgtest.NewOrg(ctx, pool)
		var agent pgtype.UUID
		Expect(pool.QueryRow(ctx, `INSERT INTO agents (org_id, display_name, harness) VALUES ($1, 'Epoch agent', 'opencode') RETURNING id`, org).Scan(&agent)).To(Succeed())
		e0, err := pw.Current(ctx, org, agent)
		Expect(err).NotTo(HaveOccurred())
		secrets := router.NewPGSecrets(pool, ring, nil)
		before, err := secrets.Version(ctx, org)
		Expect(err).NotTo(HaveOccurred())
		e1, ok, err := pw.NewEpoch(ctx, org, agent, checkpoint.ReasonKeyLost, time.Hour)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		after, err := secrets.Version(ctx, org)
		Expect(err).NotTo(HaveOccurred())
		Expect(after).NotTo(Equal(before), "a new epoch's password rebuilds the set")

		set, err := secrets.Secrets(ctx, org)
		Expect(err).NotTo(HaveOccurred())
		sc := scrub.New(scrub.Config{Level: scrub.LevelFull, Secrets: set})
		Expect(sc.Text("pw="+e0.Password)).To(Equal("pw=[redacted:secret]"), "the retired epoch's")
		Expect(sc.Text("pw="+e1.Password)).To(Equal("pw=[redacted:secret]"), "the current epoch's")
	})

	It("records a destination's health, the platform's too", func(ctx SpecContext) {
		h := router.NewPGHealth(pool)
		var dest pgtype.UUID
		Expect(pool.QueryRow(ctx, `INSERT INTO trace_destinations (org_id, name, backend, endpoint_url, credentials, secret_fields)
			VALUES ($1, 'd', 'generic', 'https://x.test/v1/traces', '{}'::jsonb, ARRAY['X-Test']) RETURNING id`, org).Scan(&dest)).To(Succeed())
		route := tracedest.Route{ID: dest.String()}
		Expect(h.Success(ctx, org, route)).To(Succeed())
		Expect(h.Failure(ctx, org, route, "the backend answered HTTP 503 (the backend failed)", 7)).To(Succeed())
		Expect(h.Failure(ctx, org, route, "again", 3)).To(Succeed())
		var okAt pgtype.Timestamptz
		var errMsg pgtype.Text
		var dropped int64
		Expect(pool.QueryRow(ctx, `SELECT last_success_at, last_error, spans_dropped FROM trace_destinations WHERE org_id = $1 AND id = $2`, org, dest).Scan(&okAt, &errMsg, &dropped)).To(Succeed())
		Expect(okAt.Valid).To(BeTrue())
		Expect(errMsg.String).To(Equal("again"))
		Expect(dropped).To(Equal(int64(10)))
	})
})

func secretstoreBinding(org, id pgtype.UUID) secretstore.Binding {
	return catalog.KeyBinding(org, id)
}
