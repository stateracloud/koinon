// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"encoding/json"
	"time"

	connect "connectrpc.com/connect/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/durable"
	agentv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/agent/v1alpha1"
	templatev1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/template/v1alpha1"
	"github.com/stateracloud/koinon/internal/services"
)

// "Run now" for schedule triggers (rc.6, ADR 0066). The environment has no DBOS runtime, which is
// the case this suite covers: the fire is the services' own, not the scheduler's workflow.
var _ = Describe("running a schedule trigger now", func() {
	var (
		e     *env
		org   string
		owner *client
		agent *agentv1alpha1.Agent
	)
	daily := &templatev1alpha1.Trigger{
		Id: "daily-check", Type: templatev1alpha1.TriggerType_TRIGGER_TYPE_SCHEDULE,
		Cron: "0 9 * * 1-5", Timezone: "Europe/Tallinn", Instruction: "Check yesterday's costs.",
	}
	dm := &templatev1alpha1.Trigger{Id: "chat", Type: templatev1alpha1.TriggerType_TRIGGER_TYPE_DIRECT_MESSAGE, Instruction: "Answer the person."}
	run := func(ctx SpecContext, c *client, id string) (*agentv1alpha1.RunAgentTriggerResponse, error) {
		return c.agents.RunAgentTrigger(ctx, &agentv1alpha1.RunAgentTriggerRequest{OrgId: org, AgentId: agent.GetId(), TriggerId: id})
	}
	activate := func(ctx SpecContext, ts ...*templatev1alpha1.Trigger) {
		GinkgoHelper()
		p, err := owner.agents.SetAgentTriggers(ctx, &agentv1alpha1.SetAgentTriggersRequest{
			OrgId: org, AgentId: agent.GetId(), BaseVersion: agent.GetLatestVersion(), Triggers: ts,
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = owner.agents.ActivateAgent(ctx, &agentv1alpha1.ActivateAgentRequest{
			OrgId: org, AgentId: agent.GetId(), Version: p.GetAgent().GetLatestVersion(),
		})
		Expect(err).NotTo(HaveOccurred())
	}
	BeforeEach(func(ctx SpecContext) {
		e = newEnv(ctx, envOptions{})
		org, owner = e.newTeamOrg(ctx)
		agent = owner.activeAgent(ctx, org)
	})

	It("creates the task a scheduled fire would, marked as a manual fire, and audits it", func(ctx SpecContext) {
		activate(ctx, daily, dm)
		res, err := run(ctx, owner, "daily-check")
		Expect(err).NotTo(HaveOccurred())

		var creatorKind, title string
		var origin, state string
		Expect(e.pool.QueryRow(ctx, `SELECT t.created_by_kind::text, t.title, r.origin::text, r.state::text
			FROM tasks t JOIN runs r ON r.org_id = t.org_id AND r.task_id = t.id WHERE t.id = $1`, res.GetTaskId()).
			Scan(&creatorKind, &title, &origin, &state)).To(Succeed())
		Expect(creatorKind).To(Equal("trigger"))
		Expect(title).To(HavePrefix("Run now: daily-check, "))
		Expect(origin).To(Equal("scheduled"), "queued exactly as a scheduled run is")
		Expect(state).To(Equal("queued"))

		var msg, created []byte
		Expect(e.pool.QueryRow(ctx, `SELECT payload FROM task_events WHERE task_id = $1 AND kind = 'message'`, res.GetTaskId()).Scan(&msg)).To(Succeed())
		Expect(e.pool.QueryRow(ctx, `SELECT payload FROM task_events WHERE task_id = $1 AND kind = 'task.created'`, res.GetTaskId()).Scan(&created)).To(Succeed())
		var m, c map[string]any
		Expect(json.Unmarshal(msg, &m)).To(Succeed())
		Expect(json.Unmarshal(created, &c)).To(Succeed())
		Expect(m).To(HaveKeyWithValue("text", "Check yesterday's costs."))
		Expect(m).To(HaveKeyWithValue("fire", "manual"))
		Expect(c).To(HaveKeyWithValue("fire", "manual"))
		Expect(c).To(HaveKey("firedBy"))

		Expect(count(ctx, e, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'agent.trigger.run_now' AND target_id = $2
			AND summary->'after'->>'trigger' = 'daily-check' AND summary->'after'->>'taskId' = $3`, org, agent.GetId(), res.GetTaskId())).To(Equal(1))
	})

	It("fires again for each request, a second apart or more", func(ctx SpecContext) {
		activate(ctx, daily)
		first, err := run(ctx, owner, "daily-check")
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() string {
			res, err := run(ctx, owner, "daily-check")
			Expect(err).NotTo(HaveOccurred())
			return res.GetTaskId()
		}).WithPolling(1100 * time.Millisecond).WithTimeout(5 * time.Second).ShouldNot(Equal(first.GetTaskId()))
	})

	It("refuses what the scheduler would not fire", func(ctx SpecContext) {
		_, err := run(ctx, owner, "daily-check")
		Expect(codeOf(err)).To(Equal(connect.CodeNotFound), "no such trigger")

		activate(ctx, daily, dm)
		_, err = run(ctx, owner, "chat")
		Expect(codeOf(err)).To(Equal(connect.CodeNotFound), "only schedule triggers run now")
		_, err = run(ctx, owner, "")
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))

		// Proposed but not active: no effect, as for the scheduler.
		_, err = owner.agents.SetAgentTriggers(ctx, &agentv1alpha1.SetAgentTriggersRequest{
			OrgId: org, AgentId: agent.GetId(), BaseVersion: agent.GetLatestVersion() + 1,
			Triggers: []*templatev1alpha1.Trigger{daily, dm, {
				Id: "proposed", Type: templatev1alpha1.TriggerType_TRIGGER_TYPE_SCHEDULE, Cron: "0 1 * * *", Timezone: "UTC", Instruction: "x",
			}},
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = run(ctx, owner, "proposed")
		Expect(codeOf(err)).To(Equal(connect.CodeNotFound))

		Expect(count(ctx, e, `SELECT count(*) FROM tasks WHERE org_id = $1`, org)).To(BeZero())
	})

	It("respects the agent's status and health", func(ctx SpecContext) {
		activate(ctx, daily)
		_, err := e.pool.Exec(ctx, `UPDATE agents SET unhealthy_at = now(), unhealthy_reason = 'stuck three times' WHERE id = $1`, agent.GetId())
		Expect(err).NotTo(HaveOccurred())
		_, err = run(ctx, owner, "daily-check")
		Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition))
		Expect(err.Error()).To(ContainSubstring("stuck three times"))
		Expect(count(ctx, e, `SELECT count(*) FROM tasks WHERE org_id = $1`, org)).To(BeZero(), "nothing is left behind")

		_, err = e.pool.Exec(ctx, `UPDATE agents SET unhealthy_at = NULL, unhealthy_reason = NULL WHERE id = $1`, agent.GetId())
		Expect(err).NotTo(HaveOccurred())
		_, err = owner.agents.ArchiveAgent(ctx, &agentv1alpha1.ArchiveAgentRequest{OrgId: org, AgentId: agent.GetId()})
		Expect(err).NotTo(HaveOccurred())
		_, err = run(ctx, owner, "daily-check")
		Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition))
	})

	It("is for admins and owners", func(ctx SpecContext) {
		activate(ctx, daily)
		member := e.userIn(ctx, org, store.OrgRoleMember)
		_, err := run(ctx, member, "daily-check")
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
		admin := e.userIn(ctx, org, store.OrgRoleAdmin)
		_, err = run(ctx, admin, "daily-check")
		Expect(err).NotTo(HaveOccurred())
	})

	It("goes through the org's partition of the org queue when durable is on, and the same second is the same fire", func(ctx SpecContext) {
		activate(ctx, daily)
		dsn := e.pool.Config().ConnString()
		Expect(durable.Migrate(ctx, dsn, nil)).To(Succeed())
		rt := durable.New(durable.Config{DatabaseURL: dsn, ApplicationVersion: "v1", ExecutorID: "koinon-api-run-now-test"})
		work, err := durable.NewOrgWork(rt, durable.OrgQueueConfig{PollingInterval: 50 * time.Millisecond})
		Expect(err).NotTo(HaveOccurred())
		queue, err := services.RegisterRunNow(rt, work)
		Expect(err).NotTo(HaveOccurred())
		Expect(rt.Launch(ctx)).To(Succeed())
		DeferCleanup(func() { _ = rt.Shutdown(5 * time.Second) })
		c := owner.on(newEnvOn(ctx, e, envOptions{runNow: queue}))

		res, err := run(ctx, c, "daily-check")
		Expect(err).NotTo(HaveOccurred())
		Expect(count(ctx, e, `SELECT count(*) FROM dbos.workflow_status WHERE queue_name = $1 AND name = $2 AND queue_partition_key = $3`,
			durable.OrgQueueName, services.RunNowWorkflow, org)).To(Equal(1), "on the org's partition, where trigger_concurrency and trigger_rate_per_minute apply")
		var creator, fire string
		Expect(e.pool.QueryRow(ctx, `SELECT t.created_by_kind::text, m.payload->>'fire' FROM tasks t
			JOIN task_events m ON m.org_id = t.org_id AND m.task_id = t.id AND m.kind = 'message' WHERE t.id = $1`, res.GetTaskId()).Scan(&creator, &fire)).To(Succeed())
		Expect([]string{creator, fire}).To(Equal([]string{"trigger", "manual"}))
		Expect(count(ctx, e, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'agent.trigger.run_now'`, org)).To(Equal(1))

		By("errors keep their code through the workflow")
		_, err = run(ctx, c, "nothing")
		Expect(codeOf(err)).To(Equal(connect.CodeNotFound))
		_, err = e.pool.Exec(ctx, `UPDATE agents SET unhealthy_at = now(), unhealthy_reason = 'stuck' WHERE id = $1`, agent.GetId())
		Expect(err).NotTo(HaveOccurred())
		_, err = run(ctx, c, "daily-check")
		Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition))
		Expect(err.Error()).To(ContainSubstring("stuck"))
	}, SpecTimeout(2*time.Minute))
})
