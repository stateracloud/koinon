// SPDX-License-Identifier: Apache-2.0

package adminapi_test

import (
	"net/http"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/pricing"
)

var _ = Describe("prices and budgets", func() {
	It("warns about a cache_read price whose provider format has no cache mode (ADR 0068)", func(ctx SpecContext) {
		e := newEnv(ctx)
		body := map[string]any{
			"name": "gemini", "provider": "openrouter", "match": map[string]any{"glob": "google/*"},
			"prices": map[string]any{"input": "1", "output": "2", "cacheRead": "0.1"}, "maxOutput": 4096,
		}
		st, raw := e.call(ctx, e.token, http.MethodPost, "/prices", body)
		Expect(st).To(Equal(http.StatusCreated), string(raw))
		Expect(decode[map[string]any](raw)["warnings"]).To(ConsistOf(ContainSubstring("without caching")))
	})

	It("versions org prices, shows the default list and its changes", func(ctx SpecContext) {
		e := newEnv(ctx)
		body := map[string]any{
			"name": "contract", "provider": "openrouter", "match": map[string]any{"glob": "deepseek/*"},
			"prices": map[string]any{"input": "0.5", "output": "1.5", "cacheRead": "0.05"}, "maxOutput": 4096,
			"tiers": []any{map[string]any{"contextOver": 200000, "prices": map[string]any{"input": "1"}}},
		}
		st, raw := e.call(ctx, e.token, http.MethodPost, "/prices", body)
		Expect(st).To(Equal(http.StatusCreated), string(raw))
		first := decode[map[string]any](raw)
		Expect(first["name"]).To(Equal("contract"))
		Expect(first).NotTo(HaveKey("warnings"), "deepseek caches by itself")

		// A bad price is refused with a reason.
		bad := map[string]any{"name": "x", "provider": "openrouter", "match": map[string]any{"glob": "a", "regex": "b"}, "prices": map[string]any{"input": "1", "output": "1"}}
		st, _ = e.call(ctx, e.token, http.MethodPost, "/prices", bad)
		Expect(st).To(Equal(http.StatusBadRequest))
		bad = map[string]any{"name": "x", "provider": "openrouter", "match": map[string]any{"glob": "a"}, "prices": map[string]any{"input": "1.1234567", "output": "1"}}
		st, _ = e.call(ctx, e.token, http.MethodPost, "/prices", bad)
		Expect(st).To(Equal(http.StatusBadRequest), "more than six fraction digits")

		// A new version of the same name keeps the old one.
		body["prices"] = map[string]any{"input": "0.6", "output": "1.5"}
		body["validFrom"] = "2999-01-01T00:00:00Z"
		st, _ = e.call(ctx, e.token, http.MethodPost, "/prices", map[string]any{"name": "nomax", "provider": "x", "match": map[string]any{"glob": "a"}, "prices": map[string]any{"input": "1", "output": "1"}})
		Expect(st).To(Equal(http.StatusBadRequest), "a priced entry needs maxOutput")
		st, raw = e.call(ctx, e.token, http.MethodPost, "/prices", body)
		Expect(st).To(Equal(http.StatusCreated), string(raw))
		st, raw = e.call(ctx, e.token, http.MethodGet, "/prices", nil)
		Expect(st).To(Equal(http.StatusOK))
		Expect(decode[struct{ Prices []map[string]any }](raw).Prices).To(HaveLen(2))

		st, _ = e.call(ctx, e.token, http.MethodDelete, "/prices/contract", nil)
		Expect(st).To(Equal(http.StatusNoContent))
		st, _ = e.call(ctx, e.token, http.MethodDelete, "/prices/nothing", nil)
		Expect(st).To(Equal(http.StatusNotFound))

		Expect(prices.New(pool, nil, pricing.Embedded()).SyncDefaults(ctx)).To(Succeed())
		st, raw = e.call(ctx, e.token, http.MethodGet, "/prices/defaults", nil)
		Expect(st).To(Equal(http.StatusOK))
		defs := decode[struct {
			Version string
			Entries []map[string]any
		}](raw)
		Expect(defs.Entries).NotTo(BeEmpty())
		st, raw = e.call(ctx, e.token, http.MethodGet, "/prices/defaults/changes", nil)
		Expect(st).To(Equal(http.StatusOK), string(raw))
		st, _ = e.call(ctx, e.token, http.MethodGet, "/prices/defaults/changes?from=1999-01-01.1", nil)
		Expect(st).To(Equal(http.StatusNotFound))
		st, raw = e.call(ctx, e.token, http.MethodGet, "/prices/flagged", nil)
		Expect(st).To(Equal(http.StatusOK), string(raw))
	})

	It("sets and reads the org cap and an agent's limit, and audits both", func(ctx SpecContext) {
		e := newEnv(ctx)
		st, raw := e.call(ctx, e.token, http.MethodGet, "/budget", nil)
		Expect(st).To(Equal(http.StatusOK))
		Expect(decode[map[string]any](raw)).NotTo(HaveKey("monthlyCapUsd"))
		st, raw = e.call(ctx, e.token, http.MethodPut, "/budget", map[string]any{"monthlyCapUsd": "250.5"})
		Expect(st).To(Equal(http.StatusOK), string(raw))
		Expect(decode[map[string]any](raw)["monthlyCapUsd"]).To(Equal("250.50"))
		st, _ = e.call(ctx, e.token, http.MethodPut, "/budget", map[string]any{"monthlyCapUsd": "-1"})
		Expect(st).To(Equal(http.StatusBadRequest))
		st, _ = e.call(ctx, e.token, http.MethodPut, "/budget", map[string]any{"monthlyCapUsd": "0"})
		Expect(st).To(Equal(http.StatusBadRequest))

		st, _ = e.call(ctx, e.token, http.MethodGet, "/agents/019a0000-0000-7000-8000-00000000ffff/budget", nil)
		Expect(st).To(Equal(http.StatusNotFound))
		st, _ = e.call(ctx, e.token, http.MethodPut, "/agents/019a0000-0000-7000-8000-00000000ffff/budget", map[string]any{"dailyLimitUsd": "5"})
		Expect(st).To(Equal(http.StatusNotFound))

		st, raw = e.call(ctx, e.token, http.MethodGet, "/spend", nil)
		Expect(st).To(Equal(http.StatusOK), string(raw))

		By("the spend report puts the gateway's cost and the estimated output next to Koinon's cost")
		agent, err := store.New(pool).CreateAgent(ctx, store.CreateAgentParams{OrgID: e.org, DisplayName: "a", Harness: harness.OpenCode, CreatedBy: e.user})
		Expect(err).NotTo(HaveOccurred())
		_, err = pool.Exec(ctx, `INSERT INTO spend_entries (org_id, agent_id, request_id, key_id, model,
			input_tokens, output_tokens, cost_usd, gateway_cost_usd, priced, estimated, cost_flagged, occurred_at) VALUES
			($1, $2, 'r1', 'k', 'm', 100, 10, 0.50, 0.50, true, false, false, now()),
			($1, $2, 'r2', 'k', 'm', 100, 10, 0.30, 0.20, true, false, true, now()),
			($1, $2, 'r3', 'k', 'm', 100, 32000, 0.08, NULL, true, true, true, now())`, e.org, agent.ID)
		Expect(err).NotTo(HaveOccurred())
		st, raw = e.call(ctx, e.token, http.MethodGet, "/spend", nil)
		Expect(st).To(Equal(http.StatusOK), string(raw))
		lines := decode[struct{ Models []map[string]any }](raw).Models
		Expect(lines).To(HaveLen(1))
		Expect(lines[0]).To(HaveKeyWithValue("reportedUsd", "0.80"))
		Expect(lines[0]).To(HaveKeyWithValue("estimatedUsd", "0.08"))
		Expect(lines[0]).To(HaveKeyWithValue("estimatedOutputTokens", BeNumerically("==", 32000)))
		Expect(lines[0]).To(HaveKeyWithValue("gatewayRequests", BeNumerically("==", 2)))
		Expect(lines[0]).To(HaveKeyWithValue("gatewayUsd", "0.70"))
		Expect(lines[0]).To(HaveKeyWithValue("gatewayComparedUsd", "0.80"))
		Expect(lines[0]).To(HaveKeyWithValue("flaggedRequests", BeNumerically("==", 2)))
		Expect(lines[0]).To(HaveKeyWithValue("provisionalRequests", BeNumerically("==", 0)))
		Expect(lines[0]).To(HaveKeyWithValue("reservedUsd", "0.00"))

		By("system checks are one row of their own, and are not in the models")
		Expect(decode[map[string]any](raw)["systemChecks"]).To(Equal(map[string]any{"requests": float64(0), "costUsd": "0.00"}))
		_, err = pool.Exec(ctx, `INSERT INTO spend_entries (org_id, principal, request_id, key_id, model, input_tokens, output_tokens, cost_usd, priced, occurred_at)
			VALUES ($1, 'system', 'sys-1', 'k', 'm', 100, 10, 0.02, true, now())`, e.org)
		Expect(err).NotTo(HaveOccurred())
		st, raw = e.call(ctx, e.token, http.MethodGet, "/spend", nil)
		Expect(st).To(Equal(http.StatusOK), string(raw))
		Expect(decode[map[string]any](raw)["systemChecks"]).To(Equal(map[string]any{"requests": float64(1), "costUsd": "0.02"}))
		Expect(decode[struct{ Models []map[string]any }](raw).Models).To(HaveLen(1))

		var n int
		Expect(pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'budget.org_cap.set'`, e.org).Scan(&n)).To(Succeed())
		Expect(n).To(Equal(1))
		st, _ = e.call(ctx, "", http.MethodGet, "/budget", nil)
		Expect(st).To(Equal(http.StatusUnauthorized))
	})
})
