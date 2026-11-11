// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	connect "connectrpc.com/connect/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	agentv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/agent/v1alpha1"
)

// The per-agent sandbox resource override (#75, ADR 0083): inside the platform bounds, refused with
// the bounds in the message outside them, audited, and not a new version.
var _ = Describe("the agent's sandbox resource override", func() {
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
	set := func(ctx SpecContext, c *client, r *agentv1alpha1.SandboxResources) (*agentv1alpha1.SetAgentSandboxResourcesResponse, error) {
		return c.agents.SetAgentSandboxResources(ctx, &agentv1alpha1.SetAgentSandboxResourcesRequest{OrgId: org, AgentId: agent.GetId(), Resources: r})
	}
	get := func(ctx SpecContext) *agentv1alpha1.SandboxResourcesState {
		GinkgoHelper()
		res, err := owner.agents.GetAgentSandboxResources(ctx, &agentv1alpha1.GetAgentSandboxResourcesRequest{OrgId: org, AgentId: agent.GetId()})
		Expect(err).NotTo(HaveOccurred())
		return res.GetState()
	}

	It("starts without an override and reports the platform bounds", func(ctx SpecContext) {
		s := get(ctx)
		Expect(s.GetOverride().GetMemoryLimit()).To(BeEmpty())
		Expect(s.GetBounds().GetMemoryLimit()).To(HaveField("Max", "8Gi"))
		Expect(s.GetBounds().GetCpuLimit()).To(HaveField("Max", "4"))
		Expect(s.GetMemoryRequestEqualsLimit()).To(BeTrue())
		Expect(s.GetEffective().GetMemoryLimit()).NotTo(BeEmpty(), "the platform default or the template's value")
	})

	It("stores an override inside the bounds, applies it over the template, and is not a new version", func(ctx SpecContext) {
		_, err := e.pool.Exec(ctx, `UPDATE agent_versions SET config = jsonb_set(config, '{resources}', '{"cpu":{"limit":"1"},"memory":{"limit":"1Gi"}}') WHERE agent_id = $1`, agent.GetId())
		Expect(err).NotTo(HaveOccurred())
		Expect(get(ctx).GetEffective()).To(And(HaveField("CpuLimit", "1"), HaveField("MemoryLimit", "1Gi")))

		res, err := set(ctx, owner, &agentv1alpha1.SandboxResources{CpuLimit: "3", MemoryLimit: "4Gi"})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetState().GetOverride()).To(And(HaveField("CpuLimit", "3"), HaveField("MemoryLimit", "4Gi")))
		Expect(res.GetState().GetTemplate()).To(And(HaveField("CpuLimit", "1"), HaveField("MemoryLimit", "1Gi")))
		Expect(res.GetState().GetEffective()).To(And(HaveField("CpuLimit", "3"), HaveField("MemoryLimit", "4Gi")),
			"the override wins over the template")

		got, err := owner.agents.GetAgent(ctx, &agentv1alpha1.GetAgentRequest{OrgId: org, AgentId: agent.GetId()})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.GetAgent().GetLatestVersion()).To(Equal(agent.GetLatestVersion()), "a setting is not a new version")
		Expect(got.GetAgent().GetSandboxResources().GetMemoryLimit()).To(Equal("4Gi"))

		res, err = set(ctx, owner, &agentv1alpha1.SandboxResources{})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetState().GetOverride().GetMemoryLimit()).To(BeEmpty())
		Expect(res.GetState().GetEffective().GetMemoryLimit()).To(Equal("1Gi"), "clearing the override restores the template's value")
	})

	DescribeTable("refuses a value outside the platform bounds, with the bounds in the error",
		func(ctx SpecContext, r *agentv1alpha1.SandboxResources, want string) {
			_, err := set(ctx, owner, r)
			Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
			Expect(err).To(MatchError(ContainSubstring(want)))
			Expect(get(ctx).GetOverride()).To(And(HaveField("MemoryLimit", ""), HaveField("CpuLimit", "")), "nothing is stored")
			Expect(count(ctx, e, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'agent.sandbox_resources.set'`, org)).To(BeZero())
		},
		Entry("memory above the maximum", &agentv1alpha1.SandboxResources{MemoryLimit: "16Gi"}, "memory.limit 16Gi is above the maximum: the platform allows 512Mi to 8Gi"),
		Entry("memory below the minimum", &agentv1alpha1.SandboxResources{MemoryLimit: "128Mi"}, "memory.limit 128Mi is below the minimum: the platform allows 512Mi to 8Gi"),
		Entry("cpu above the maximum", &agentv1alpha1.SandboxResources{CpuLimit: "64"}, "cpu.limit 64 is above the maximum: the platform allows at most 4"),
		Entry("a malformed quantity", &agentv1alpha1.SandboxResources{CpuLimit: "lots"}, `cpu.limit "lots"`),
		Entry("a request above the limit", &agentv1alpha1.SandboxResources{CpuRequest: "2", CpuLimit: "1"}, "cpu.request 2 is greater than cpu.limit 1"),
	)

	It("audits every change with the old and the new override, and a repeat is no change", func(ctx SpecContext) {
		for _, r := range []*agentv1alpha1.SandboxResources{{MemoryLimit: "4Gi"}, {MemoryLimit: "4Gi"}, {}} {
			_, err := set(ctx, owner, r)
			Expect(err).NotTo(HaveOccurred())
		}
		rows, err := e.pool.Query(ctx, `SELECT summary->'before'->>'memory_limit', summary->'after'->>'memory_limit' FROM audit_events
			WHERE org_id = $1 AND action = 'agent.sandbox_resources.set' AND target_id = $2 ORDER BY id`, org, agent.GetId())
		Expect(err).NotTo(HaveOccurred())
		defer rows.Close()
		var changes [][2]string
		for rows.Next() {
			var before, after string
			Expect(rows.Scan(&before, &after)).To(Succeed())
			changes = append(changes, [2]string{before, after})
		}
		Expect(changes).To(Equal([][2]string{{"", "4Gi"}, {"4Gi", ""}}))
	})

	It("is for admins and owners, and an agent of another org is not found", func(ctx SpecContext) {
		member := e.userIn(ctx, org, store.OrgRoleMember)
		_, err := set(ctx, member, &agentv1alpha1.SandboxResources{MemoryLimit: "2Gi"})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
		_, err = owner.agents.SetAgentSandboxResources(ctx, &agentv1alpha1.SetAgentSandboxResourcesRequest{OrgId: org, AgentId: "019a0000-0000-7000-8000-00000000ffff", Resources: &agentv1alpha1.SandboxResources{MemoryLimit: "2Gi"}})
		Expect(codeOf(err)).To(Equal(connect.CodeNotFound))
	})
})
