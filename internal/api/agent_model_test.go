// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"context"

	connect "connectrpc.com/connect/v2"
	guuid "github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	agentv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/agent/v1alpha1"
)

// serveModel adds a model to the org's catalog on a new provider, with an org price unless
// unpriced, so the org serves it.
func serveModel(ctx context.Context, e *env, org, name string, priced bool) (provider string) {
	GinkgoHelper()
	var providerID pgtype.UUID
	Expect(e.pool.QueryRow(ctx, `INSERT INTO providers (org_id, name, type, url) VALUES ($1, $2, 'openai', 'http://fakemodel.example.com/v1') RETURNING id`,
		org, "prov-"+guuid.NewString()).Scan(&providerID)).To(Succeed())
	_, err := e.pool.Exec(ctx, `INSERT INTO models (org_id, provider_id, name) VALUES ($1, $2, $3)`, org, providerID, name)
	Expect(err).NotTo(HaveOccurred())
	if priced {
		_, err = e.pool.Exec(ctx, `INSERT INTO price_overrides (org_id, name, entry, valid_from, created_by)
			VALUES ($1, $2, jsonb_build_object('provider', 'openai', 'match', jsonb_build_object('glob', $3::text),
			'validFrom', '2020-01-01', 'prices', jsonb_build_object('input', '1', 'output', '2'),
			'tokenizer', 'bytes', 'ratio', '3.0', 'maxOutput', 10), now() - interval '1 hour', 'test')`,
			org, "price-"+guuid.NewString(), name)
		Expect(err).NotTo(HaveOccurred())
	}
	return providerID.String()
}

// The per-agent model setting (rc.6): a mutable agent setting, audited, validated against the served
// catalog, with the precedence agent, version, org default.
var _ = Describe("the agent's model setting", func() {
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
	get := func(ctx SpecContext) *agentv1alpha1.GetAgentModelResponse {
		GinkgoHelper()
		res, err := owner.agents.GetAgentModel(ctx, &agentv1alpha1.GetAgentModelRequest{OrgId: org, AgentId: agent.GetId()})
		Expect(err).NotTo(HaveOccurred())
		return res
	}
	set := func(ctx SpecContext, c *client, model string) (*agentv1alpha1.SetAgentModelResponse, error) {
		return c.agents.SetAgentModel(ctx, &agentv1alpha1.SetAgentModelRequest{OrgId: org, AgentId: agent.GetId(), Model: model})
	}

	It("resolves agent setting, then the version's model, then the org default, and says which", func(ctx SpecContext) {
		Expect(get(ctx).GetSource()).To(Equal(agentv1alpha1.ModelSource_MODEL_SOURCE_NONE), "the org serves nothing yet")

		serveModel(ctx, e, org, "vendor/b-model", true)
		serveModel(ctx, e, org, "vendor/a-model", true)
		serveModel(ctx, e, org, "vendor/unpriced", false)

		// The template's model is in the version; with none served it is the org's first priced model.
		_, err := e.pool.Exec(ctx, `UPDATE agent_versions SET config = jsonb_set(config, '{model}', '"default"') WHERE agent_id = $1`, agent.GetId())
		Expect(err).NotTo(HaveOccurred())
		v := get(ctx)
		Expect(v.GetSource()).To(Equal(agentv1alpha1.ModelSource_MODEL_SOURCE_ORG_DEFAULT))
		Expect(v.GetAvailable()).To(HaveLen(2), "an unpriced model is not offered")
		orgDefault := v.GetEffective()

		_, err = e.pool.Exec(ctx, `UPDATE agent_versions SET config = jsonb_set(config, '{model}', '"vendor/b-model"') WHERE agent_id = $1`, agent.GetId())
		Expect(err).NotTo(HaveOccurred())
		v = get(ctx)
		Expect(v.GetSource()).To(Equal(agentv1alpha1.ModelSource_MODEL_SOURCE_VERSION))
		Expect(v.GetEffective()).To(Equal("vendor/b-model"))
		Expect(orgDefault).NotTo(BeEmpty())

		res, err := set(ctx, owner, "vendor/a-model")
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetModel().GetSource()).To(Equal(agentv1alpha1.ModelSource_MODEL_SOURCE_AGENT))
		Expect(res.GetModel().GetEffective()).To(Equal("vendor/a-model"))
		Expect(res.GetModel().GetVersionModel()).To(Equal("vendor/b-model"))
		got, err := owner.agents.GetAgent(ctx, &agentv1alpha1.GetAgentRequest{OrgId: org, AgentId: agent.GetId()})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.GetAgent().GetModelSetting()).To(Equal("vendor/a-model"))
		Expect(got.GetAgent().GetLatestVersion()).To(Equal(agent.GetLatestVersion()), "a setting is not a new version")

		res, err = set(ctx, owner, "default")
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetModel().GetSource()).To(Equal(agentv1alpha1.ModelSource_MODEL_SOURCE_VERSION))
		Expect(res.GetModel().GetSetting()).To(BeEmpty())
	})

	It("audits every change with the old and the new value, and a repeat is no change", func(ctx SpecContext) {
		serveModel(ctx, e, org, "vendor/a-model", true)
		serveModel(ctx, e, org, "vendor/b-model", true)
		for _, m := range []string{"vendor/a-model", "vendor/a-model", "vendor/b-model", "default"} {
			_, err := set(ctx, owner, m)
			Expect(err).NotTo(HaveOccurred())
		}
		rows, err := e.pool.Query(ctx, `SELECT summary->'before'->>'model', summary->'after'->>'model' FROM audit_events
			WHERE org_id = $1 AND action = 'agent.model.set' AND target_id = $2 ORDER BY id`, org, agent.GetId())
		Expect(err).NotTo(HaveOccurred())
		defer rows.Close()
		var changes [][2]string
		for rows.Next() {
			var before, after string
			Expect(rows.Scan(&before, &after)).To(Succeed())
			changes = append(changes, [2]string{before, after})
		}
		Expect(changes).To(Equal([][2]string{
			{"default", "vendor/a-model"}, {"vendor/a-model", "vendor/b-model"}, {"vendor/b-model", "default"},
		}))
	})

	It("refuses a model the org does not serve or has no price for, and keeps the setting", func(ctx SpecContext) {
		serveModel(ctx, e, org, "vendor/unpriced", false)
		for _, m := range []string{"vendor/ghost", "vendor/unpriced"} {
			_, err := set(ctx, owner, m)
			Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument), m)
		}
		Expect(get(ctx).GetSetting()).To(BeEmpty())
		Expect(count(ctx, e, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'agent.model.set'`, org)).To(BeZero())
	})

	It("reports a set model that is no longer served as a problem", func(ctx SpecContext) {
		serveModel(ctx, e, org, "vendor/a-model", true)
		_, err := set(ctx, owner, "vendor/a-model")
		Expect(err).NotTo(HaveOccurred())
		_, err = e.pool.Exec(ctx, `DELETE FROM models WHERE org_id = $1 AND name = 'vendor/a-model'`, org)
		Expect(err).NotTo(HaveOccurred())
		v := get(ctx)
		Expect(v.GetEffective()).To(Equal("vendor/a-model"), "there is no silent fallback")
		Expect(v.GetProblem()).To(Equal(`the agent's model "vendor/a-model" is not served in this org; pick another on the agent page`))
	})

	It("is for admins and owners, and an agent of another org is not found", func(ctx SpecContext) {
		member := e.userIn(ctx, org, store.OrgRoleMember)
		_, err := set(ctx, member, "default")
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
		_, err = member.agents.GetAgentModel(ctx, &agentv1alpha1.GetAgentModelRequest{OrgId: org, AgentId: agent.GetId()})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
		_, err = owner.agents.SetAgentModel(ctx, &agentv1alpha1.SetAgentModelRequest{OrgId: org, AgentId: "019a0000-0000-7000-8000-00000000ffff", Model: "default"})
		Expect(codeOf(err)).To(Equal(connect.CodeNotFound))
	})
})
