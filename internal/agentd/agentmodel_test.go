// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"context"
	"time"

	sdk "github.com/coder/acp-go-sdk"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/modelformat"
)

// The model of a run: the agent's own setting, then the version's model, then the org default
// (rc.6). A set model that is not served or priced fails the run at once.
var _ = Describe("The agent's model setting", func() {
	// serveModel adds a keyless provider with a model of the given name to the org, records it as
	// served as koinon-reconciler would, and prices it unless unpriced.
	serveModelAs := func(ctx context.Context, e *env, name string, priced bool, providerType string, format modelformat.Format) {
		GinkgoHelper()
		var providerID, modelID pgtype.UUID
		Expect(db.QueryRow(ctx, `INSERT INTO providers (org_id, name, type, url) VALUES ($1, $2, $3, 'http://fakemodel.example.com/v1') RETURNING id`,
			e.org, uniq("prov"), providerType).Scan(&providerID)).To(Succeed())
		Expect(db.QueryRow(ctx, `INSERT INTO models (org_id, provider_id, name, format) VALUES ($1, $2, $3, $4) RETURNING id`,
			e.org, providerID, name, string(format)).Scan(&modelID)).To(Succeed())
		_, err := db.Exec(ctx, `INSERT INTO gateway_served_models (org_id, model_id, provider_id) VALUES ($1, $2, $3)`, e.org, modelID, providerID)
		Expect(err).NotTo(HaveOccurred())
		if !priced {
			return
		}
		_, err = db.Exec(ctx, `INSERT INTO price_overrides (org_id, name, entry, valid_from, created_by)
			VALUES ($1, $2, jsonb_build_object('provider', $4::text, 'match', jsonb_build_object('glob', $3::text),
			'validFrom', '2020-01-01', 'prices', jsonb_build_object('input', '1', 'output', '2'),
			'tokenizer', 'bytes', 'ratio', '3.0', 'maxOutput', 10), now() - interval '1 hour', 'test')`,
			e.org, uniq("price"), name, providerType)
		Expect(err).NotTo(HaveOccurred())
	}
	serveModel := func(ctx context.Context, e *env, name string, priced bool) {
		GinkgoHelper()
		serveModelAs(ctx, e, name, priced, "openai", modelformat.OpenAI)
	}
	setModel := func(ctx context.Context, e *env, name string) {
		GinkgoHelper()
		_, err := q.SetAgentModel(ctx, store.SetAgentModelParams{OrgID: e.org, ID: e.agent.ID, Model: pgtype.Text{String: name, Valid: name != ""}})
		Expect(err).NotTo(HaveOccurred())
	}

	It("runs the agent's model rather than the version's", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		serveModel(ctx, e, model, true)
		serveModel(ctx, e, "fake/agent-pick", true)
		setModel(ctx, e, "fake/agent-pick")
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateSucceeded))
		Expect(e.harness.selectedModels()).To(Equal([]string{"koinon/fake/agent-pick"}))
		_, _, _, args := e.harness.snapshot()
		Expect(args).To(ConsistOf(ContainElements("--model", "fake/agent-pick")))
	})

	It("takes the wire format from the agent's model, not the version's", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		serveModel(ctx, e, model, true) // the version names a chat-completions model
		serveModelAs(ctx, e, "claude-agent-pick", true, "anthropic", modelformat.Anthropic)
		setModel(ctx, e, "claude-agent-pick")
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateSucceeded))
		Expect(e.harness.selectedModels()).To(Equal([]string{"koinon-anthropic/claude-agent-pick"}))
		_, _, _, args := e.harness.snapshot()
		Expect(args).To(ConsistOf(ContainElements("--model", "claude-agent-pick", "--model-format", "anthropic")))
	})

	It("uses chat completions when the agent picks an openai-format model and the version names a Messages one", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		serveModelAs(ctx, e, model, true, "anthropic", modelformat.Anthropic) // the version's model
		serveModel(ctx, e, "fake/agent-pick", true)
		setModel(ctx, e, "fake/agent-pick")
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateSucceeded))
		Expect(e.harness.selectedModels()).To(Equal([]string{"koinon/fake/agent-pick"}))
		_, _, _, args := e.harness.snapshot()
		Expect(args).To(ConsistOf(ContainElements("--model", "fake/agent-pick", "--model-format", "openai")))
		Expect(args[0]).NotTo(ContainElement("anthropic"))
	})

	It("runs the version's model when the agent has no setting", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		serveModel(ctx, e, model, true)
		serveModel(ctx, e, "fake/agent-pick", true)
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateSucceeded))
		Expect(e.harness.selectedModels()).To(Equal([]string{"koinon/" + model}))
	})

	It("takes the org default when neither the agent nor the version names a model", func(ctx SpecContext) {
		e := newEnvWith(ctx, store.AgentFlavorOpencode, func(*env) string { return `{"model":"default","instructions":"x"}` })
		serveModel(ctx, e, "fake/z-second", true)
		serveModel(ctx, e, "fake/a-first", true)
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateSucceeded))
		Expect(e.harness.selectedModels()).To(HaveLen(1))
		Expect(e.harness.selectedModels()[0]).To(HavePrefix("koinon/fake/"))
	})

	It("fails fast when the agent's model is not served in the org, and does not fall back to the version's", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		serveModel(ctx, e, model, true) // the version's model is fine
		setModel(ctx, e, "fake/gone")
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateFailed))
		Expect(getRun(ctx, r).OutcomeReason.String).To(Equal(
			`the agent's model "fake/gone" is not served in this org; pick another on the agent page`))
		Expect(e.gw.Execs()).To(BeEmpty(), "the harness never starts")
	})

	It("fails fast when the agent's model has no price", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		serveModel(ctx, e, "fake/free-ride", false)
		setModel(ctx, e, "fake/free-ride")
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateFailed))
		Expect(getRun(ctx, r).OutcomeReason.String).To(Equal(`the agent's model "fake/free-ride" has no price, so the gateway does not serve it; ` +
			"add a price for it on the Prices page or pick another model on the agent page"))
		Expect(e.gw.Execs()).To(BeEmpty())
	})

	It("runs again once the setting is cleared", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		serveModel(ctx, e, model, true)
		setModel(ctx, e, "fake/gone")
		first := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, first)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateFailed))
		setModel(ctx, e, "")
		second := e.enqueue(ctx, e.task(ctx, "again"))
		Eventually(runState(ctx, second)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateSucceeded))
		Expect(e.harness.selectedModels()).To(Equal([]string{"koinon/" + model}))
	})

	It("gives the same reason when the model is removed after the start check and the gateway answers 404", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		serveModel(ctx, e, "fake/agent-pick", true)
		setModel(ctx, e, "fake/agent-pick")
		e.harness.modelFault = func(ctx context.Context, _ string) error {
			_, err := db.Exec(ctx, `DELETE FROM models WHERE org_id = $1 AND name = 'fake/agent-pick'`, e.org)
			Expect(err).NotTo(HaveOccurred())
			return &sdk.RequestError{Code: -32603, Message: "model not found", Data: map[string]any{"statusCode": 404}}
		}
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateFailed))
		Expect(getRun(ctx, r).OutcomeReason.String).To(Equal(`the agent's model "fake/agent-pick" is not served in this org; pick another on the agent page`))
	})

	It("leaves a 404 alone when the model still checks out", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		serveModel(ctx, e, "fake/agent-pick", true)
		setModel(ctx, e, "fake/agent-pick")
		e.harness.modelFault = func(context.Context, string) error {
			return &sdk.RequestError{Code: -32603, Message: "not found", Data: map[string]any{"statusCode": 404}}
		}
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateFailed))
		Expect(getRun(ctx, r).OutcomeReason.String).To(ContainSubstring("session/prompt"))
	})
})
