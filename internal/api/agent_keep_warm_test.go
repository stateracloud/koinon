// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	connect "connectrpc.com/connect/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	agentv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/agent/v1alpha1"
)

// The agent's keep-warm request (#100, ADR 0049 amendment 2026-11-01). The reconciler's grant under
// the operator's cap is specified in internal/reconciler; here the grant column is set by hand, as
// the reconciler would.
var _ = Describe("the agent's keep-warm request", func() {
	var (
		e     *env
		org   string
		owner *client
		agent *agentv1alpha1.Agent
	)
	BeforeEach(func(ctx SpecContext) {
		e = newEnv(ctx, envOptions{})
		org, owner = e.newTeamOrg(ctx)
		agent = owner.activeAgent(ctx, org)
	})
	set := func(ctx SpecContext, c *client, v bool) (*agentv1alpha1.SetAgentKeepWarmResponse, error) {
		return c.agents.SetAgentKeepWarm(ctx, &agentv1alpha1.SetAgentKeepWarmRequest{OrgId: org, AgentId: agent.GetId(), KeepWarm: v})
	}

	It("is off by default and an admin turns it on and off without a new version", func(ctx SpecContext) {
		Expect(agent.GetKeepWarm()).To(BeFalse())
		res, err := set(ctx, owner, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetAgent().GetKeepWarm()).To(BeTrue())
		Expect(res.GetAgent().GetLatestVersion()).To(Equal(agent.GetLatestVersion()), "a setting is not a new version")
		got, err := owner.agents.GetAgent(ctx, &agentv1alpha1.GetAgentRequest{OrgId: org, AgentId: agent.GetId()})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.GetAgent().GetKeepWarm()).To(BeTrue())
		res, err = set(ctx, owner, false)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetAgent().GetKeepWarm()).To(BeFalse())
	})

	It("is refused for a member, and changes nothing", func(ctx SpecContext) {
		member := e.userIn(ctx, org, store.OrgRoleMember)
		_, err := set(ctx, member, true)
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
		Expect(count(ctx, e, `SELECT count(*) FROM agents WHERE org_id = $1 AND keep_warm`, org)).To(BeZero())
		Expect(count(ctx, e, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'agent.keep_warm.set'`, org)).To(BeZero())
	})

	It("says whether the operator's cap granted the request", func(ctx SpecContext) {
		res, err := set(ctx, owner, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetAgent().GetKeepWarmGranted()).To(BeFalse(), "a new request waits for the reconciler's pass")

		By("the reconciler grants it")
		_, err = e.pool.Exec(ctx, `UPDATE agents SET keep_warm_granted = true WHERE org_id = $1 AND id = $2`, org, agent.GetId())
		Expect(err).NotTo(HaveOccurred())
		got, err := owner.agents.GetAgent(ctx, &agentv1alpha1.GetAgentRequest{OrgId: org, AgentId: agent.GetId()})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.GetAgent()).To(And(HaveField("KeepWarm", true), HaveField("KeepWarmGranted", true)))

		By("withdrawing the request withdraws the grant")
		res, err = set(ctx, owner, false)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetAgent()).To(And(HaveField("KeepWarm", false), HaveField("KeepWarmGranted", false)))
		Expect(count(ctx, e, `SELECT count(*) FROM agents WHERE org_id = $1 AND keep_warm_granted`, org)).To(BeZero())
	})

	It("audits every change with the old and the new value, and a repeat is no change", func(ctx SpecContext) {
		for _, v := range []bool{true, true, false} {
			_, err := set(ctx, owner, v)
			Expect(err).NotTo(HaveOccurred())
		}
		rows, err := e.pool.Query(ctx, `SELECT summary->'before'->>'keepWarm', summary->'after'->>'keepWarm' FROM audit_events
			WHERE org_id = $1 AND action = 'agent.keep_warm.set' AND target_id = $2 ORDER BY id`, org, agent.GetId())
		Expect(err).NotTo(HaveOccurred())
		defer rows.Close()
		var changes [][2]string
		for rows.Next() {
			var before, after string
			Expect(rows.Scan(&before, &after)).To(Succeed())
			changes = append(changes, [2]string{before, after})
		}
		Expect(changes).To(Equal([][2]string{{"false", "true"}, {"true", "false"}}))
	})

	It("refuses an archived agent and does not find another org's agent", func(ctx SpecContext) {
		_, err := owner.agents.SetAgentKeepWarm(ctx, &agentv1alpha1.SetAgentKeepWarmRequest{OrgId: org, AgentId: "019a0000-0000-7000-8000-00000000ffff", KeepWarm: true})
		Expect(codeOf(err)).To(Equal(connect.CodeNotFound))
		_, err = owner.agents.ArchiveAgent(ctx, &agentv1alpha1.ArchiveAgentRequest{OrgId: org, AgentId: agent.GetId()})
		Expect(err).NotTo(HaveOccurred())
		_, err = set(ctx, owner, true)
		Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition))
	})
})
