// SPDX-License-Identifier: Apache-2.0

package checkpointscli_test

import (
	"bytes"
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/checkpointscli"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

var _ = Describe("koinon-api checkpoints resync", func() {
	var (
		out  *bytes.Buffer
		tool *checkpointscli.Tool
	)
	agentIn := func(ctx context.Context, org pgtype.UUID, repository bool) pgtype.UUID {
		GinkgoHelper()
		var a pgtype.UUID
		Expect(pool.QueryRow(ctx, `INSERT INTO agents (org_id, display_name, harness) VALUES ($1, $2, 'opencode') RETURNING id`, org, pgtest.Uniq("a")).Scan(&a)).To(Succeed())
		if repository {
			_, err := pool.Exec(ctx, `INSERT INTO home_repositories (org_id, agent_id, prefix, password_envelope) VALUES ($1, $2, $3, '{}')`,
				org, a, "orgs/"+org.String()+"/homes/"+a.String()+"/")
			Expect(err).NotTo(HaveOccurred())
		}
		return a
	}
	requestOf := func(ctx context.Context, agent pgtype.UUID) (id pgtype.UUID) {
		GinkgoHelper()
		Expect(pool.QueryRow(ctx, `SELECT request_id FROM home_checkpoint_resyncs WHERE agent_id = $1`, agent).Scan(&id)).To(Succeed())
		return id
	}
	// agentd plays koinon-agentd: it records the outcome of each pending request.
	agentd := func(ctx context.Context, finish func(agent pgtype.UUID) string) {
		defer GinkgoRecover()
		for ctx.Err() == nil {
			rows, err := pool.Query(ctx, `SELECT agent_id FROM home_checkpoint_resyncs WHERE finished_at IS NULL AND failed_at IS NULL`)
			if err != nil {
				return
			}
			agents, err := pgx.CollectRows(rows, pgx.RowTo[pgtype.UUID])
			if err != nil {
				return
			}
			for _, a := range agents {
				switch finish(a) {
				case "failed":
					_, _ = pool.Exec(ctx, `UPDATE home_checkpoint_resyncs SET failed_at = now(), attempts = 5, error = 'repository epoch 0 is not in the bucket' WHERE agent_id = $1`, a)
				case "missing":
					_, _ = pool.Exec(ctx, `UPDATE home_checkpoint_resyncs SET finished_at = now(), attempts = 1, snapshots = 3, added = 1, missing = 2, skipped = 1 WHERE agent_id = $1`, a)
				case "taken":
					_, _ = pool.Exec(ctx, `UPDATE home_checkpoint_resyncs SET finished_at = now(), attempts = 1, snapshots = 1, added = 0, missing = 0, skipped = 0, taken = 2 WHERE agent_id = $1`, a)
				case "clean":
					_, _ = pool.Exec(ctx, `UPDATE home_checkpoint_resyncs SET finished_at = now(), attempts = 1, snapshots = 4, added = 2, missing = 0, skipped = 0 WHERE agent_id = $1`, a)
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	BeforeEach(func(ctx SpecContext) {
		pgtest.TruncateOrgs(ctx, pool)
		out = &bytes.Buffer{}
		tool = &checkpointscli.Tool{Pool: pool, Out: out, Poll: 20 * time.Millisecond}
	})

	It("wants exactly one of --all and --agent, valid agent IDs that have a repository, and a database", func(ctx SpecContext) {
		org, _, _ := pgtest.NewOrg(ctx, pool)
		bare := agentIn(ctx, org, false)
		with := agentIn(ctx, org, true)
		Expect(tool.Resync(ctx, false, nil, false, time.Minute)).To(MatchError(ContainSubstring("--agent")))
		Expect(tool.Resync(ctx, true, []string{with.String()}, false, time.Minute)).To(MatchError(ContainSubstring("--all")))
		Expect(tool.Resync(ctx, false, []string{"nope"}, false, time.Minute)).To(MatchError(ContainSubstring(`"nope" is not an agent ID`)))
		Expect(tool.Resync(ctx, false, []string{with.String(), bare.String()}, false, time.Minute)).To(MatchError(ContainSubstring(bare.String())))
		Expect((&checkpointscli.Tool{Out: out}).Resync(ctx, true, nil, false, time.Minute)).To(MatchError(ContainSubstring("--database-url")))
		Expect(tool.Resync(ctx, true, nil, true, 0)).To(MatchError(ContainSubstring("--timeout must be above 0")))
		Expect(tool.Resync(ctx, true, nil, true, -time.Second)).To(MatchError(ContainSubstring("--timeout must be above 0")))
		var n int
		Expect(pool.QueryRow(ctx, `SELECT count(*) FROM home_checkpoint_resyncs`).Scan(&n)).To(Succeed())
		Expect(n).To(BeZero(), "nothing is requested when one named agent has no repository")
	})

	It("has nothing to resync when no agent has a repository", func(ctx SpecContext) {
		org, _, _ := pgtest.NewOrg(ctx, pool)
		agentIn(ctx, org, false)
		Expect(tool.Resync(ctx, true, nil, true, time.Minute)).To(Succeed())
		Expect(out.String()).To(ContainSubstring("nothing to resync"))
	})

	It("requests a resync for every agent with a repository and audits each in its org, by the cli actor", func(ctx SpecContext) {
		org, _, _ := pgtest.NewOrg(ctx, pool)
		other, _, _ := pgtest.NewOrg(ctx, pool)
		a, b := agentIn(ctx, org, true), agentIn(ctx, other, true)
		agentIn(ctx, org, false)
		Expect(tool.Resync(ctx, true, nil, false, time.Minute)).To(Succeed())
		Expect(out.String()).To(ContainSubstring("for 2 agent(s)"))
		Expect(out.String()).To(ContainSubstring(`koinon_agentd_home_checkpoint_resyncs{state="pending"}`))
		for _, x := range []struct{ org, agent pgtype.UUID }{{org, a}, {other, b}} {
			var actor, after string
			Expect(pool.QueryRow(ctx, `SELECT actor_kind || ':' || actor_id, summary::text FROM audit_events
				WHERE org_id = $1 AND action = $2 AND target_kind = 'agent' AND target_id = $3`,
				x.org, checkpointscli.RequestedAction, x.agent.String()).Scan(&actor, &after)).To(Succeed())
			Expect(actor).To(Equal("cli:checkpoints resync"))
			Expect(after).To(ContainSubstring(requestOf(ctx, x.agent).String()))
		}

		By("a second request replaces the first")
		first := requestOf(ctx, a)
		Expect(tool.Resync(ctx, false, []string{a.String()}, false, time.Minute)).To(Succeed())
		Expect(requestOf(ctx, a)).NotTo(Equal(first))
	})

	It("waits for koinon-agentd, reports progress and a summary naming the agents to look at", func(ctx SpecContext) {
		org, _, _ := pgtest.NewOrg(ctx, pool)
		clean, missing := agentIn(ctx, org, true), agentIn(ctx, org, true)
		run, stop := context.WithCancel(ctx)
		defer stop()
		go agentd(run, func(a pgtype.UUID) string {
			if a == missing {
				return "missing"
			}
			return "clean"
		})
		Expect(tool.Resync(ctx, true, nil, true, time.Minute)).To(Succeed())
		Expect(out.String()).To(ContainSubstring("2 of 2 agent(s) resynced or given up."))
		Expect(out.String()).To(ContainSubstring("Resynced 2 of 2 agent(s): 7 snapshot(s) in the bucket, 3 checkpoint(s) added, 2 marked failed because their snapshot is gone, 1 snapshot(s) left out."))
		Expect(out.String()).To(ContainSubstring("agent " + missing.String()))
		Expect(out.String()).NotTo(ContainSubstring("agent " + clean.String()))
	})

	It("names the agents whose repository prefixes koinon-agentd found taken (#262)", func(ctx SpecContext) {
		org, _, _ := pgtest.NewOrg(ctx, pool)
		clean, taken := agentIn(ctx, org, true), agentIn(ctx, org, true)
		run, stop := context.WithCancel(ctx)
		defer stop()
		go agentd(run, func(a pgtype.UUID) string {
			if a == taken {
				return "taken"
			}
			return "clean"
		})
		Expect(tool.Resync(ctx, true, nil, true, time.Minute)).To(Succeed())
		Expect(out.String()).To(ContainSubstring("2 repository prefix(es) hold a repository this database has no password for"))
		Expect(out.String()).To(ContainSubstring("home.repository_prefix_taken"))
		Expect(out.String()).To(ContainSubstring("agent " + taken.String() + " (org " + org.String() + "): 2 repository prefix(es) taken"))
		Expect(out.String()).NotTo(ContainSubstring("agent " + clean.String()))
	})

	It("fails when a resync was given up or replaced, naming the agent and its error", func(ctx SpecContext) {
		org, _, _ := pgtest.NewOrg(ctx, pool)
		failed, replaced := agentIn(ctx, org, true), agentIn(ctx, org, true)
		run, stop := context.WithCancel(ctx)
		defer stop()
		go agentd(run, func(a pgtype.UUID) string {
			if a == failed {
				return "failed"
			}
			// Another operator's newer request: this command's one is not the agent's any more.
			_, _ = pool.Exec(run, `UPDATE home_checkpoint_resyncs SET request_id = gen_random_uuid(), finished_at = now(),
				snapshots = 0, added = 0, missing = 0, skipped = 0 WHERE agent_id = $1`, a)
			return ""
		})
		err := tool.Resync(ctx, true, nil, true, time.Minute)
		Expect(err).To(MatchError(ContainSubstring("1 resync(s) given up and 1 replaced")))
		Expect(out.String()).To(ContainSubstring("agent " + failed.String()))
		Expect(out.String()).To(ContainSubstring("given up after 5 attempt(s), its checkpoints are as they were: repository epoch 0 is not in the bucket"))
		Expect(out.String()).To(ContainSubstring("agent " + replaced.String() + " (org " + org.String() + "): a newer request replaced this one"))
	})

	It("says it was interrupted, not timed out, when the command is stopped", func(ctx SpecContext) {
		org, _, _ := pgtest.NewOrg(ctx, pool)
		agentIn(ctx, org, true)
		run, stop := context.WithCancel(ctx)
		defer stop()
		time.AfterFunc(200*time.Millisecond, stop)
		err := tool.Resync(run, true, nil, true, time.Minute)
		Expect(err).To(MatchError(ContainSubstring("stopped waiting (context canceled) with 1 agent(s) not resynced yet")))
		Expect(err).NotTo(MatchError(ContainSubstring("timed out")))
	})

	It("fails at the timeout and leaves the requests for koinon-agentd", func(ctx SpecContext) {
		org, _, _ := pgtest.NewOrg(ctx, pool)
		a := agentIn(ctx, org, true)
		Expect(tool.Resync(ctx, true, nil, true, 100*time.Millisecond)).To(MatchError(ContainSubstring("timed out after 100ms with 1 agent(s) not resynced yet")))
		var pending bool
		Expect(pool.QueryRow(ctx, `SELECT finished_at IS NULL AND failed_at IS NULL FROM home_checkpoint_resyncs WHERE agent_id = $1`, a).Scan(&pending)).To(Succeed())
		Expect(pending).To(BeTrue())
	})
})
