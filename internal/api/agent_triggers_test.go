// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	connect "connectrpc.com/connect/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	agentv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/agent/v1alpha1"
	templatev1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/template/v1alpha1"
	"github.com/stateracloud/koinon/internal/triggers"
	"github.com/stateracloud/koinon/templates"
)

// Triggers on the agent version (plan item TR1, ADR 0066).
var _ = Describe("agent triggers", func() {
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
	dm := &templatev1alpha1.Trigger{
		Id: "chat", Type: templatev1alpha1.TriggerType_TRIGGER_TYPE_DIRECT_MESSAGE, Instruction: "Answer the person.",
	}
	set := func(ctx SpecContext, c *client, a *agentv1alpha1.Agent, ts ...*templatev1alpha1.Trigger) (*agentv1alpha1.Agent, error) {
		res, err := c.agents.SetAgentTriggers(ctx, &agentv1alpha1.SetAgentTriggersRequest{
			OrgId: org, AgentId: a.GetId(), BaseVersion: a.GetLatestVersion(), Triggers: ts,
		})
		return res.GetAgent(), err
	}
	active := func(ctx SpecContext) []triggers.Active {
		got, err := triggers.ForOrg(ctx, store.New(e.pool), mustUUID(org))
		Expect(err).NotTo(HaveOccurred())
		return got
	}

	BeforeEach(func(ctx SpecContext) {
		e = newEnv(ctx, envOptions{})
		org, owner = e.newTeamOrg(ctx)
		agent = owner.activeAgent(ctx, org)
	})

	It("takes effect only after a new version is activated, and both steps are audited", func(ctx SpecContext) {
		Expect(active(ctx)).To(BeEmpty())

		proposed, err := set(ctx, owner, agent, daily, dm)
		Expect(err).NotTo(HaveOccurred())
		Expect(proposed.GetLatestVersion()).To(Equal(agent.GetLatestVersion() + 1))
		Expect(proposed.GetActiveVersion()).To(Equal(agent.GetActiveVersion()))
		Expect(proposed.GetProposedTriggers()).To(HaveLen(2))
		Expect(proposed.GetApprovedTriggers()).To(BeEmpty())
		Expect(proposed.GetStatus()).To(Equal(agentv1alpha1.AgentStatus_AGENT_STATUS_ACTIVE), "the agent keeps running its active version")
		Expect(active(ctx)).To(BeEmpty(), "a proposed version's triggers have no effect")
		Expect(count(ctx, e, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'agent.triggers.propose' AND target_id = $2`,
			org, agent.GetId())).To(Equal(1))

		res, err := owner.agents.ActivateAgent(ctx, &agentv1alpha1.ActivateAgentRequest{
			OrgId: org, AgentId: agent.GetId(), Version: proposed.GetLatestVersion(),
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetAgent().GetApprovedTriggers()).To(HaveLen(2))
		got := active(ctx)
		Expect(got).To(HaveLen(2))
		Expect(got[0].AgentID).To(Equal(mustUUID(agent.GetId())))
		Expect(got[0].Version).To(Equal(proposed.GetLatestVersion()))
		Expect(count(ctx, e, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'agent.activate' AND target_id = $2 AND summary->'after'->'triggers' IS NOT NULL`,
			org, agent.GetId())).To(Equal(2), "the first activation and this one")

		sched, err := triggers.All(ctx, store.New(e.pool), templates.TriggerSchedule)
		Expect(err).NotTo(HaveOccurred())
		Expect(sched).To(HaveLen(1))
		Expect(sched[0].Filter.Cron).To(Equal("0 9 * * 1-5"))
		Expect(sched[0].Filter.Timezone).To(Equal("Europe/Tallinn"))

		_, err = owner.agents.ActivateAgent(ctx, &agentv1alpha1.ActivateAgentRequest{
			OrgId: org, AgentId: agent.GetId(), Version: proposed.GetLatestVersion(),
		})
		Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition), "the version is already active")
	})

	It("keeps the old triggers in effect while a change is proposed, and drops removed ones on activation", func(ctx SpecContext) {
		p1, err := set(ctx, owner, agent, daily)
		Expect(err).NotTo(HaveOccurred())
		_, err = owner.agents.ActivateAgent(ctx, &agentv1alpha1.ActivateAgentRequest{OrgId: org, AgentId: agent.GetId(), Version: p1.GetLatestVersion()})
		Expect(err).NotTo(HaveOccurred())

		p2, err := set(ctx, owner, p1)
		Expect(err).NotTo(HaveOccurred())
		Expect(active(ctx)).To(HaveLen(1), "removal is only proposed")
		_, err = owner.agents.ActivateAgent(ctx, &agentv1alpha1.ActivateAgentRequest{OrgId: org, AgentId: agent.GetId(), Version: p2.GetLatestVersion()})
		Expect(err).NotTo(HaveOccurred())
		Expect(active(ctx)).To(BeEmpty())
	})

	It("lists nothing for an archived agent", func(ctx SpecContext) {
		p, err := set(ctx, owner, agent, dm)
		Expect(err).NotTo(HaveOccurred())
		_, err = owner.agents.ActivateAgent(ctx, &agentv1alpha1.ActivateAgentRequest{OrgId: org, AgentId: agent.GetId(), Version: p.GetLatestVersion()})
		Expect(err).NotTo(HaveOccurred())
		Expect(active(ctx)).To(HaveLen(1))
		_, err = owner.agents.ArchiveAgent(ctx, &agentv1alpha1.ArchiveAgentRequest{OrgId: org, AgentId: agent.GetId()})
		Expect(err).NotTo(HaveOccurred())
		Expect(active(ctx)).To(BeEmpty())
		_, err = set(ctx, owner, p)
		Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition))
	})

	It("is for admins and owners, refuses a stale base version", func(ctx SpecContext) {
		member := e.userIn(ctx, org, store.OrgRoleMember)
		_, err := set(ctx, member, agent, dm)
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))

		_, err = set(ctx, owner, agent, dm)
		Expect(err).NotTo(HaveOccurred())
		_, err = set(ctx, owner, agent, dm) // still names the old version
		Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition))
	})

	DescribeTable("refuses an invalid trigger without adding a version",
		func(ctx SpecContext, bad *templatev1alpha1.Trigger) {
			_, err := set(ctx, owner, agent, bad)
			Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
			Expect(count(ctx, e, `SELECT count(*) FROM agent_versions WHERE agent_id = $1`, agent.GetId())).To(Equal(1))
		},
		Entry("bad cron", &templatev1alpha1.Trigger{Id: "a", Type: templatev1alpha1.TriggerType_TRIGGER_TYPE_SCHEDULE, Cron: "every day", Timezone: "UTC", Instruction: "x"}),
		Entry("bad timezone", &templatev1alpha1.Trigger{Id: "a", Type: templatev1alpha1.TriggerType_TRIGGER_TYPE_SCHEDULE, Cron: "0 9 * * *", Timezone: "Mars/Olympus", Instruction: "x"}),
		Entry("schedule without a timezone", &templatev1alpha1.Trigger{Id: "a", Type: templatev1alpha1.TriggerType_TRIGGER_TYPE_SCHEDULE, Cron: "0 9 * * *", Instruction: "x"}),
		Entry("a filter on a direct message", &templatev1alpha1.Trigger{Id: "a", Type: templatev1alpha1.TriggerType_TRIGGER_TYPE_DIRECT_MESSAGE, Cron: "0 9 * * *", Instruction: "x"}),
		Entry("a reserved type", &templatev1alpha1.Trigger{Id: "a", Type: templatev1alpha1.TriggerType_TRIGGER_TYPE_CODE_HOST_EVENT, Instruction: "x"}),
		Entry("no instruction", &templatev1alpha1.Trigger{Id: "a", Type: templatev1alpha1.TriggerType_TRIGGER_TYPE_DIRECT_MESSAGE}),
		Entry("unspecified type", &templatev1alpha1.Trigger{Id: "a", Instruction: "x"}),
	)
})
