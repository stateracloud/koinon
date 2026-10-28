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
)

// A run waits until the gateway serves its model (issue #174): a new provider is left out of the
// gateway state until its key file has reached the gateway pod, and a harness started before that
// gets a 403 on its first model call.
var _ = Describe("Waiting for the gateway to serve the model", func() {
	// addModel adds a keyless provider with the env's model to the org's catalog, as koinon-api
	// would, and returns the model's ID. The reconciler has not applied it yet.
	addModel := func(ctx context.Context, e *env, provider string) pgtype.UUID {
		GinkgoHelper()
		var providerID, modelID pgtype.UUID
		Expect(db.QueryRow(ctx, `INSERT INTO providers (org_id, name, type, url) VALUES ($1, $2, 'openai', 'http://fakemodel.example.com/v1') RETURNING id`,
			e.org, provider).Scan(&providerID)).To(Succeed())
		Expect(db.QueryRow(ctx, `INSERT INTO models (org_id, provider_id, name) VALUES ($1, $2, $3) RETURNING id`,
			e.org, providerID, model).Scan(&modelID)).To(Succeed())
		return modelID
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
		Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateSucceeded))
		Expect(waitEvents(ctx, r)()).To(BeEmpty())
	})

	It("waits, says why, and starts the harness once the reconciler records the model served", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		id := addModel(ctx, e, "new-upstream")
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a", func(c *agentd.Config) { c.ReconcileNudge = 100 * time.Millisecond })

		Eventually(waitEvents(ctx, r)).WithTimeout(10 * time.Second).Should(HaveLen(1))
		ev := waitEvents(ctx, r)()[0]
		Expect(payload(ev)).To(HaveKeyWithValue("model", model))
		Expect(payload(ev)).To(HaveKeyWithValue("provider", "new-upstream"))
		Expect(payload(ev)).To(HaveKeyWithValue("reason", `waiting for the model provider "new-upstream" to reach the gateway`))
		Consistently(runState(ctx, r)).WithTimeout(500 * time.Millisecond).Should(Equal(store.RunStateStarting))
		Expect(e.gw.Execs()).To(BeEmpty(), "nothing runs in the sandbox, the harness least of all")
		Expect(waitEvents(ctx, r)()).To(HaveLen(1), "the wait is recorded once")

		serve(ctx, e, id)
		Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateSucceeded))
	})

	It("fails with a reason naming the provider when the gateway does not serve the model in time", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		addModel(ctx, e, "stuck-upstream")
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		clock := &fakeClock{t: time.Now()}
		e.pool("agentd-a", func(c *agentd.Config) { c.Now = clock.Now; c.ModelServeTimeout = 5 * time.Minute })

		Eventually(waitEvents(ctx, r)).WithTimeout(10 * time.Second).Should(HaveLen(1))
		clock.Advance(4 * time.Minute)
		Consistently(runState(ctx, r)).WithTimeout(300 * time.Millisecond).Should(Equal(store.RunStateStarting))
		clock.Advance(time.Minute + time.Second)
		Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateFailed))
		reason := getRun(ctx, r).OutcomeReason.String
		Expect(reason).To(ContainSubstring(`the gateway did not serve the model "` + model + `" within 5m0s`))
		Expect(reason).To(ContainSubstring(`its provider "stuck-upstream" has not reached the gateway`))
		Expect(reason).NotTo(ContainSubstring("uthentication"))
		Expect(e.gw.Execs()).To(BeEmpty(), "the harness never starts")
	})

	It("does not wait for a model the org's catalog does not have", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateSucceeded))
		Expect(waitEvents(ctx, r)()).To(BeEmpty())
	})
})
