// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
)

// ADR 0068: a model with a cache_read price whose provider format has no cache mode runs without
// caching, and the run says so.
var _ = Describe("Runs without prompt caching", func() {
	setup := func(ctx context.Context, e *env, providerType string) {
		GinkgoHelper()
		var providerID, modelID pgtype.UUID
		Expect(db.QueryRow(ctx, `INSERT INTO providers (org_id, name, type, url) VALUES ($1, $2, $3, 'http://fakemodel.example.com/v1') RETURNING id`,
			e.org, uniq("cache-upstream"), providerType).Scan(&providerID)).To(Succeed())
		Expect(db.QueryRow(ctx, `INSERT INTO models (org_id, provider_id, name) VALUES ($1, $2, $3) RETURNING id`,
			e.org, providerID, model).Scan(&modelID)).To(Succeed())
		_, err := db.Exec(ctx, `INSERT INTO price_overrides (org_id, name, entry, valid_from, created_by)
			VALUES ($1, $2, jsonb_build_object('provider', $4::text, 'match', jsonb_build_object('glob', $3::text),
			'validFrom', '2020-01-01', 'prices', jsonb_build_object('input', '1', 'output', '2', 'cacheRead', '0.1'),
			'tokenizer', 'bytes', 'ratio', '3.0', 'maxOutput', 10), now() - interval '1 hour', 'test')`,
			e.org, uniq("price"), model, providerType)
		Expect(err).NotTo(HaveOccurred())
		_, err = db.Exec(ctx, `INSERT INTO gateway_served_models (org_id, model_id, provider_id) VALUES ($1, $2, $3)`, e.org, modelID, providerID)
		Expect(err).NotTo(HaveOccurred())
	}
	uncached := func(ctx context.Context, r store.Run) []store.RunEvent {
		return eventOf(events(ctx, r), "run.uncached")
	}

	It("records run.uncached with the model and the reason for an unmapped provider format", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		setup(ctx, e, "mystery")
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateSucceeded))
		got := uncached(ctx, r)
		Expect(got).To(HaveLen(1))
		Expect(string(got[0].Payload)).To(And(ContainSubstring(model), ContainSubstring("provider format")))
	})

	It("records nothing for a provider that caches by itself", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		setup(ctx, e, "openai")
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateSucceeded))
		Expect(uncached(ctx, r)).To(BeEmpty())
	})
})
