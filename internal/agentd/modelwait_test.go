// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/agentd"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/testing/racescale"
)

// A run waits until the gateway serves its model (old issue 174): a new provider is left out of the
// gateway state until its key file has reached the gateway pod, and a harness started before that
// gets a 403 on its first model call.
var _ = Describe("Waiting for the gateway to serve the model", func() {
	// addModel adds a keyless provider with the env's model to the org's catalog, as koinon-api
	// would, and returns the model's ID. The reconciler has not applied it yet.
	// price gives the env's model an org price; input is the input rate as stored.
	price := func(ctx context.Context, e *env, input string) {
		GinkgoHelper()
		_, err := db.Exec(ctx, `INSERT INTO price_overrides (org_id, name, entry, valid_from, created_by)
			VALUES ($1, $2, jsonb_build_object('provider', 'openai', 'match', jsonb_build_object('glob', $3::text),
			'validFrom', '2020-01-01', 'prices', jsonb_build_object('input', $4::text, 'output', '2'),
			'tokenizer', 'bytes', 'ratio', '3.0', 'maxOutput', 10), now() - interval '1 hour', 'test')`,
			e.org, uniq("price"), model, input)
		Expect(err).NotTo(HaveOccurred())
	}
	// addUnpriced is addModel without a price.
	addUnpriced := func(ctx context.Context, e *env, provider string) pgtype.UUID {
		GinkgoHelper()
		var providerID, modelID pgtype.UUID
		Expect(db.QueryRow(ctx, `INSERT INTO providers (org_id, name, type, url) VALUES ($1, $2, 'openai', 'http://fakemodel.example.com/v1') RETURNING id`,
			e.org, provider).Scan(&providerID)).To(Succeed())
		Expect(db.QueryRow(ctx, `INSERT INTO models (org_id, provider_id, name) VALUES ($1, $2, $3) RETURNING id`,
			e.org, providerID, model).Scan(&modelID)).To(Succeed())
		return modelID
	}
	addModel := func(ctx context.Context, e *env, provider string) pgtype.UUID {
		GinkgoHelper()
		id := addUnpriced(ctx, e, provider)
		price(ctx, e, "1")
		return id
	}
	// serve records the model as served, as koinon-reconciler does once an applied state serves it.
	serve := func(ctx context.Context, e *env, modelID pgtype.UUID) {
		GinkgoHelper()
		_, err := db.Exec(ctx, `INSERT INTO gateway_served_models (org_id, model_id, provider_id)
			SELECT org_id, id, provider_id FROM models WHERE org_id = $1 AND id = $2`, e.org, modelID)
		Expect(err).NotTo(HaveOccurred())
	}
	waitEvents := func(ctx context.Context, r store.Run) func() []store.RunEvent {
		return func() []store.RunEvent { return eventOf(events(ctx, r), "run.waiting_for_model") }
	}

	It("starts the harness at once when the gateway already serves the model", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		serve(ctx, e, addModel(ctx, e, "served-upstream"))
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		Expect(waitEvents(ctx, r)()).To(BeEmpty())
	})

	It("waits, says why, and starts the harness once the reconciler records the model served", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		id := addModel(ctx, e, "new-upstream")
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a", func(c *agentd.Config) { c.ReconcileNudge = 100 * time.Millisecond })

		Eventually(waitEvents(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(HaveLen(1))
		ev := waitEvents(ctx, r)()[0]
		Expect(payload(ev)).To(HaveKeyWithValue("model", model))
		Expect(payload(ev)).To(HaveKeyWithValue("provider", "new-upstream"))
		Expect(payload(ev)).To(HaveKeyWithValue("reason", `waiting for the model provider "new-upstream" to reach the gateway`))
		Consistently(runState(ctx, r)).WithTimeout(500 * time.Millisecond).Should(Equal(store.RunStateStarting))
		Expect(e.gw.Execs()).To(BeEmpty(), "nothing runs in the sandbox, the harness least of all")
		Expect(waitEvents(ctx, r)()).To(HaveLen(1), "the wait is recorded once")

		serve(ctx, e, id)
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
	})

	It("fails with a reason naming the provider when the gateway does not serve the model in time", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		addModel(ctx, e, "stuck-upstream")
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		clock := &fakeClock{t: time.Now()}
		e.pool("agentd-a", func(c *agentd.Config) { c.Now = clock.Now; c.ModelServeTimeout = 5 * time.Minute })

		Eventually(waitEvents(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(HaveLen(1))
		clock.Advance(4 * time.Minute)
		Consistently(runState(ctx, r)).WithTimeout(300 * time.Millisecond).Should(Equal(store.RunStateStarting))
		clock.Advance(time.Minute + time.Second)
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateFailed))
		reason := getRun(ctx, r).OutcomeReason.String
		Expect(reason).To(ContainSubstring(`the gateway did not serve the model "` + model + `" within 5m0s`))
		Expect(reason).To(ContainSubstring(`its provider "stuck-upstream" has not reached the gateway`))
		Expect(reason).NotTo(ContainSubstring("uthentication"))
		Expect(e.gw.Execs()).To(BeEmpty(), "the harness never starts")
	})

	It("fails at once for a model without a price, which the gateway never serves", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		addUnpriced(ctx, e, "unpriced-upstream")
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateFailed))
		Expect(getRun(ctx, r).OutcomeReason.String).To(Equal(`the model "` + model + `" has no price, so the gateway does not serve it; ` +
			"add a price for it on the Prices page and run the task again"))
		Expect(waitEvents(ctx, r)()).To(BeEmpty())
		Expect(e.gw.Execs()).To(BeEmpty())
	})

	It("fails at once for an ollama-type model pointed at Ollama cloud, which the default list does not price at zero", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		var providerID pgtype.UUID
		Expect(db.QueryRow(ctx, `INSERT INTO providers (org_id, name, type, url) VALUES ($1, 'ollama-cloud', 'ollama', 'https://ollama.com') RETURNING id`,
			e.org).Scan(&providerID)).To(Succeed())
		_, err := db.Exec(ctx, `INSERT INTO models (org_id, provider_id, name) VALUES ($1, $2, $3)`, e.org, providerID, model)
		Expect(err).NotTo(HaveOccurred())
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateFailed))
		Expect(getRun(ctx, r).OutcomeReason.String).To(Equal(`the model "` + model + `" has no price, so the gateway does not serve it; ` +
			"add a price for it on the Prices page and run the task again"))
		Expect(waitEvents(ctx, r)()).To(BeEmpty())
		Expect(e.gw.Execs()).To(BeEmpty())
	})

	It("fails at once when the provider's key was lost, even while the gateway still serves it", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		id := addModel(ctx, e, "lost-upstream")
		serve(ctx, e, id)
		// What keys forget does (ADR 0085 decision 13); the keyless provider has no envelope to clear.
		_, err := db.Exec(ctx, `UPDATE providers SET credential_lost_at = now() WHERE org_id = $1 AND name = 'lost-upstream'`, e.org)
		Expect(err).NotTo(HaveOccurred())
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateFailed))
		Expect(getRun(ctx, r).OutcomeReason.String).To(Equal(`the API key of the model provider "lost-upstream" was lost, so the gateway does not ` +
			`serve the model "` + model + `"; enter the key again on the Providers page and run the task again`))
		Expect(waitEvents(ctx, r)()).To(BeEmpty())
		Expect(e.gw.Execs()).To(BeEmpty())
	})

	It("fails at once for a model whose stored price cannot be used", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		addUnpriced(ctx, e, "bad-price-upstream")
		price(ctx, e, "1.1234567") // seven fraction digits, from before the stricter rule
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateFailed))
		reason := getRun(ctx, r).OutcomeReason.String
		Expect(reason).To(HavePrefix(`the model "` + model + `" is not served: prices: unusable stored price: `))
		Expect(reason).To(HaveSuffix("; fix its price on the Prices page"))
		Expect(e.gw.Execs()).To(BeEmpty())
	})

	It("waits out a database error and goes on once the model is served", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		id := addModel(ctx, e, "flaky-upstream")
		// The lookup fails while the table is gone.
		_, err := db.Exec(ctx, `ALTER TABLE gateway_served_models RENAME TO gateway_served_models_away`)
		Expect(err).NotTo(HaveOccurred())
		restored := false
		restore := func() {
			if !restored {
				restored = true
				_, err := db.Exec(context.Background(), `ALTER TABLE gateway_served_models_away RENAME TO gateway_served_models`)
				Expect(err).NotTo(HaveOccurred())
			}
		}
		DeferCleanup(restore)
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateStarting))
		Consistently(runState(ctx, r)).WithTimeout(500*time.Millisecond).Should(Equal(store.RunStateStarting), "an error is no reason to fail")
		Expect(e.gw.Execs()).To(BeEmpty())

		restore()
		serve(ctx, e, id)
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
	})

	It("stops waiting when the run is canceled", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		addModel(ctx, e, "slow-upstream")
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(waitEvents(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(HaveLen(1))

		_, err := q.CancelRun(ctx, store.CancelRunParams{OrgID: e.org, ID: r.ID, OutcomeReason: "user canceled"})
		Expect(err).NotTo(HaveOccurred())
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateCanceled))
		Consistently(func() []string { return kinds(events(ctx, r)) }, 500*time.Millisecond).
			Should(Equal([]string{"run.waiting_for_model"}), "nothing is recorded after the cancel")
		Expect(getRun(ctx, r).OutcomeReason.String).To(Equal("user canceled"))
		Expect(e.gw.Execs()).To(BeEmpty())
	})

	It("does not wait for a model the org's catalog does not have", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		Expect(waitEvents(ctx, r)()).To(BeEmpty())
	})
})
