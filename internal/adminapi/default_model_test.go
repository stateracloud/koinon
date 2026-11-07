// SPDX-License-Identifier: Apache-2.0

package adminapi_test

import (
	"net/http"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("the org's default model in the admin API (old issue 256)", func() {
	var e *env
	BeforeEach(func(ctx SpecContext) { e = newEnv(ctx) })

	It("has no model before the org has one, and the answer carries no `model` field", func(ctx SpecContext) {
		st, raw := e.call(ctx, e.token, http.MethodGet, "/default-model", nil)
		Expect(st).To(Equal(http.StatusOK), string(raw))
		Expect(decode[map[string]any](raw)).NotTo(HaveKey("model"))
	})

	It("sets, changes and clears the default, and audits every change with the old and new value", func(ctx SpecContext) {
		serveModel(ctx, e, "vendor/a-model", true)
		serveModel(ctx, e, "vendor/b-model", true)

		st, raw := e.call(ctx, e.token, http.MethodGet, "/default-model", nil)
		Expect(st).To(Equal(http.StatusOK), string(raw))
		Expect(decode[map[string]any](raw)).To(HaveKeyWithValue("model", "vendor/a-model"), "the org's first model")

		st, raw = e.call(ctx, e.token, http.MethodPut, "/default-model", map[string]any{"model": "vendor/b-model"})
		Expect(st).To(Equal(http.StatusOK), string(raw))
		Expect(decode[map[string]any](raw)).To(HaveKeyWithValue("model", "vendor/b-model"))
		st, raw = e.call(ctx, e.token, http.MethodGet, "/default-model", nil)
		Expect(st).To(Equal(http.StatusOK), string(raw))
		Expect(decode[map[string]any](raw)).To(HaveKeyWithValue("model", "vendor/b-model"))

		By("the same value again writes no second event")
		st, raw = e.call(ctx, e.token, http.MethodPut, "/default-model", map[string]any{"model": "vendor/b-model"})
		Expect(st).To(Equal(http.StatusOK), string(raw))

		By("clearing by leaving `model` out")
		st, raw = e.call(ctx, e.token, http.MethodPut, "/default-model", map[string]any{})
		Expect(st).To(Equal(http.StatusOK), string(raw))
		Expect(decode[map[string]any](raw)).NotTo(HaveKey("model"))
		st, raw = e.call(ctx, e.token, http.MethodGet, "/default-model", nil)
		Expect(st).To(Equal(http.StatusOK), string(raw))
		Expect(decode[map[string]any](raw)).NotTo(HaveKey("model"))

		var n int
		Expect(pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'org.default_model.set'
			AND actor_kind = 'token' AND summary->'before'->>'model' = 'vendor/a-model' AND summary->'after'->>'model' = 'vendor/b-model'`, e.org).Scan(&n)).To(Succeed())
		Expect(n).To(Equal(1))
		Expect(pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'org.default_model.set'
			AND actor_kind = 'token' AND summary->'before'->>'model' = 'vendor/b-model' AND summary->'after'->>'model' = ''`, e.org).Scan(&n)).To(Succeed())
		Expect(n).To(Equal(1))
		Expect(pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'org.default_model.set'`, e.org).Scan(&n)).To(Succeed())
		Expect(n).To(Equal(2), "the unchanged PUT wrote nothing")
	})

	It("refuses a model the org does not serve and one it has no price for, and keeps the default", func(ctx SpecContext) {
		serveModel(ctx, e, "vendor/a-model", true)
		serveModel(ctx, e, "vendor/unpriced", false)
		// A model the org does not serve is not found (another org's model looks the same); a
		// served model without a price is a bad request.
		for bad, want := range map[string]int{"vendor/ghost": http.StatusNotFound, "vendor/unpriced": http.StatusBadRequest} {
			st, raw := e.call(ctx, e.token, http.MethodPut, "/default-model", map[string]any{"model": bad})
			Expect(st).To(Equal(want), bad+": "+string(raw))
		}
		st, raw := e.call(ctx, e.token, http.MethodGet, "/default-model", nil)
		Expect(st).To(Equal(http.StatusOK), string(raw))
		Expect(decode[map[string]any](raw)).To(HaveKeyWithValue("model", "vendor/a-model"))
	})
})
