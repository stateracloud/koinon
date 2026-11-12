// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"context"
	"slices"
	"time"

	sdk "github.com/coder/acp-go-sdk"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/internal/testing/racescale"
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
		// The org's first model is its default, as catalog.CreateModel does (old issue 256).
		_, err = db.Exec(ctx, `UPDATE orgs SET default_model = $2 WHERE id = $1 AND default_model IS NULL`, e.org, name)
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
		e := newEnv(ctx, harness.OpenCode)
		serveModel(ctx, e, model, true)
		serveModel(ctx, e, "fake/agent-pick", true)
		setModel(ctx, e, "fake/agent-pick")
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		Expect(e.harness.selectedModels()).To(Equal([]string{"koinon/fake/agent-pick"}))
		_, _, _, args := e.harness.snapshot()
		Expect(args).To(ConsistOf(ContainElements("--model", "fake/agent-pick")))
	})

	It("takes the wire format from the agent's model, not the version's", func(ctx SpecContext) {
		e := newEnv(ctx, harness.OpenCode)
		serveModel(ctx, e, model, true) // the version names a chat-completions model
		serveModelAs(ctx, e, "claude-agent-pick", true, "anthropic", modelformat.Anthropic)
		setModel(ctx, e, "claude-agent-pick")
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		Expect(e.harness.selectedModels()).To(Equal([]string{"koinon-anthropic/claude-agent-pick"}))
		_, _, _, args := e.harness.snapshot()
		Expect(args).To(ConsistOf(ContainElements("--model", "claude-agent-pick", "--model-format", "anthropic")))
	})

	It("uses chat completions when the agent picks an openai-format model and the version names a Messages one", func(ctx SpecContext) {
		e := newEnv(ctx, harness.OpenCode)
		serveModelAs(ctx, e, model, true, "anthropic", modelformat.Anthropic) // the version's model
		serveModel(ctx, e, "fake/agent-pick", true)
		setModel(ctx, e, "fake/agent-pick")
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		Expect(e.harness.selectedModels()).To(Equal([]string{"koinon/fake/agent-pick"}))
		_, _, _, args := e.harness.snapshot()
		Expect(args).To(ConsistOf(ContainElements("--model", "fake/agent-pick", "--model-format", "openai")))
		Expect(args[0]).NotTo(ContainElement("anthropic"))
	})

	It("runs the version's model when the agent has no setting", func(ctx SpecContext) {
		e := newEnv(ctx, harness.OpenCode)
		serveModel(ctx, e, model, true)
		serveModel(ctx, e, "fake/agent-pick", true)
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		Expect(e.harness.selectedModels()).To(Equal([]string{"koinon/" + model}))
	})

	It("takes the org default when neither the agent nor the version names a model", func(ctx SpecContext) {
		e := newEnvWith(ctx, harness.OpenCode, func(*env) string { return `{"model":"default","instructions":"x"}` })
		serveModel(ctx, e, "fake/z-second", true)
		serveModel(ctx, e, "fake/a-first", true)
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		Expect(e.harness.selectedModels()).To(HaveLen(1))
		Expect(e.harness.selectedModels()[0]).To(HavePrefix("koinon/fake/"))
	})

	It("fails fast when the agent's model is not served in the org, and does not fall back to the version's", func(ctx SpecContext) {
		e := newEnv(ctx, harness.OpenCode)
		serveModel(ctx, e, model, true) // the version's model is fine
		setModel(ctx, e, "fake/gone")
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateFailed))
		Expect(getRun(ctx, r).OutcomeReason.String).To(Equal(
			`the agent's model "fake/gone" is not served in this org; pick another on the agent page`))
		Expect(e.gw.Execs()).To(BeEmpty(), "the harness never starts")
	})

	It("fails fast when the agent's model has no price", func(ctx SpecContext) {
		e := newEnv(ctx, harness.OpenCode)
		serveModel(ctx, e, "fake/free-ride", false)
		setModel(ctx, e, "fake/free-ride")
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateFailed))
		Expect(getRun(ctx, r).OutcomeReason.String).To(Equal(`the agent's model "fake/free-ride" has no price, so the gateway does not serve it; ` +
			"add a price for it on the Prices page or pick another model on the agent page"))
		Expect(e.gw.Execs()).To(BeEmpty())
	})

	It("runs again once the setting is cleared", func(ctx SpecContext) {
		e := newEnv(ctx, harness.OpenCode)
		serveModel(ctx, e, model, true)
		setModel(ctx, e, "fake/gone")
		first := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, first)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateFailed))
		setModel(ctx, e, "")
		second := e.enqueue(ctx, e.task(ctx, "again"))
		Eventually(runState(ctx, second)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		Expect(e.harness.selectedModels()).To(Equal([]string{"koinon/" + model}))
	})

	It("gives the same reason when the model is removed after the start check and the gateway answers 404", func(ctx SpecContext) {
		e := newEnv(ctx, harness.OpenCode)
		serveModel(ctx, e, "fake/agent-pick", true)
		setModel(ctx, e, "fake/agent-pick")
		e.harness.modelFault = func(ctx context.Context, _ string) error {
			_, err := db.Exec(ctx, `DELETE FROM models WHERE org_id = $1 AND name = 'fake/agent-pick'`, e.org)
			Expect(err).NotTo(HaveOccurred())
			return &sdk.RequestError{Code: -32603, Message: "model not found", Data: map[string]any{"statusCode": 404}}
		}
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateFailed))
		Expect(getRun(ctx, r).OutcomeReason.String).To(Equal(`the agent's model "fake/agent-pick" is not served in this org; pick another on the agent page`))
	})

	It("leaves a 404 alone when the model still checks out", func(ctx SpecContext) {
		e := newEnv(ctx, harness.OpenCode)
		serveModel(ctx, e, "fake/agent-pick", true)
		setModel(ctx, e, "fake/agent-pick")
		e.harness.modelFault = func(context.Context, string) error {
			return &sdk.RequestError{Code: -32603, Message: "not found", Data: map[string]any{"statusCode": 404}}
		}
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateFailed))
		Expect(getRun(ctx, r).OutcomeReason.String).To(ContainSubstring("session/prompt"))
	})

	// Koinon decides whether the model thinks, and the harness renders that (rc.8, ADR 0069 amendment
	// 2026-10-29): thinking is on unless the agent, or its version, turns it off, a model whose
	// catalog capability says it cannot think runs with none, and on and off are sent explicitly
	// (old issue 316).
	Describe("The agent's thinking setting", func() {
		serveClaude := func(ctx context.Context, e *env, name string) {
			GinkgoHelper()
			var providerID, modelID pgtype.UUID
			Expect(db.QueryRow(ctx, `INSERT INTO providers (org_id, name, type, url) VALUES ($1, $2, 'anthropic', 'http://fakemodel.example.com/v1') RETURNING id`,
				e.org, uniq("prov")).Scan(&providerID)).To(Succeed())
			Expect(db.QueryRow(ctx, `INSERT INTO models (org_id, provider_id, name, format) VALUES ($1, $2, $3, 'anthropic') RETURNING id`,
				e.org, providerID, name).Scan(&modelID)).To(Succeed())
			_, err := db.Exec(ctx, `INSERT INTO gateway_served_models (org_id, model_id, provider_id) VALUES ($1, $2, $3)`, e.org, modelID, providerID)
			Expect(err).NotTo(HaveOccurred())
			_, err = db.Exec(ctx, `INSERT INTO price_overrides (org_id, name, entry, valid_from, created_by)
				VALUES ($1, $2, jsonb_build_object('provider', 'anthropic', 'match', jsonb_build_object('glob', $3::text),
				'validFrom', '2020-01-01', 'prices', jsonb_build_object('input', '1', 'output', '2'),
				'tokenizer', 'bytes', 'ratio', '3.0', 'maxOutput', 10), now() - interval '1 hour', 'test')`,
				e.org, uniq("price"), name)
			Expect(err).NotTo(HaveOccurred())
		}
		setThinking := func(ctx context.Context, e *env, v string) {
			GinkgoHelper()
			_, err := q.SetAgentThinking(ctx, store.SetAgentThinkingParams{OrgID: e.org, ID: e.agent.ID, Thinking: pgtype.Text{String: v, Valid: v != ""}})
			Expect(err).NotTo(HaveOccurred())
		}
		// thinkingArg runs one task and returns the value after --thinking in the launch command.
		thinkingArg := func(ctx context.Context, e *env) string {
			GinkgoHelper()
			r := e.enqueue(ctx, e.task(ctx, "hello"))
			e.pool("agentd-a")
			Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
			_, _, _, args := e.harness.snapshot()
			Expect(args).To(HaveLen(1))
			i := slices.Index(args[0], "--thinking")
			Expect(i).To(BeNumerically(">=", 0), "the launch command always states the mode: %v", args[0])
			return args[0][i+1]
		}

		It("is on by default for a model the catalog says can think", func(ctx SpecContext) {
			e := newEnv(ctx, harness.OpenCode)
			serveClaude(ctx, e, "claude-haiku-5-5")
			setModel(ctx, e, "claude-haiku-5-5")
			Expect(thinkingArg(ctx, e)).To(Equal("adaptive-summarized"))
		})

		It("is off when the agent turns it off, and on again when it turns it on over its version's off", func(ctx SpecContext) {
			e := newEnv(ctx, harness.OpenCode)
			serveClaude(ctx, e, "claude-haiku-5-5")
			setModel(ctx, e, "claude-haiku-5-5")
			setThinking(ctx, e, "off")
			Expect(thinkingArg(ctx, e)).To(Equal("disabled"), "off is sent explicitly")
		})

		It("runs a stored off with thinking on for a model that always thinks (old issue 316)", func(ctx SpecContext) {
			e := newEnv(ctx, harness.OpenCode)
			serveClaude(ctx, e, "claude-opus-5-5")
			setModel(ctx, e, "claude-opus-5-5")
			setThinking(ctx, e, "off")
			Expect(thinkingArg(ctx, e)).To(Equal("adaptive-summarized"))
		})

		It("follows the version's value until the agent sets its own", func(ctx SpecContext) {
			e := newEnvWith(ctx, harness.OpenCode, func(*env) string {
				return `{"model":"claude-haiku-5-5","thinking":"off","instructions":"x"}`
			})
			serveClaude(ctx, e, "claude-haiku-5-5")
			Expect(thinkingArg(ctx, e)).To(Equal("disabled"))
			setThinking(ctx, e, "on")
			second := e.enqueue(ctx, e.task(ctx, "again"))
			Eventually(runState(ctx, second)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
			_, _, _, args := e.harness.snapshot()
			Expect(args).To(HaveLen(2))
			Expect(args[1][slices.Index(args[1], "--thinking")+1]).To(Equal("adaptive-summarized"))
		})

		It("is none for a model the catalog says cannot think, whatever the setting", func(ctx SpecContext) {
			e := newEnv(ctx, harness.OpenCode)
			serveModel(ctx, e, model, true)
			setThinking(ctx, e, "on")
			Expect(thinkingArg(ctx, e)).To(Equal("none"))
		})

		It("is none on chat completions even for a Claude-shaped name", func(ctx SpecContext) {
			e := newEnv(ctx, harness.OpenCode)
			serveModelAs(ctx, e, "claude-haiku-5-5", true, "anthropic", modelformat.OpenAI)
			setModel(ctx, e, "claude-haiku-5-5")
			Expect(thinkingArg(ctx, e)).To(Equal("none"))
		})
	})
})
