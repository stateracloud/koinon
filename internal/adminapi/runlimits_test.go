// SPDX-License-Identifier: Apache-2.0

package adminapi_test

import (
	"net/http"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

type runLimitJSON struct {
	Value        int `json:"value"`
	DefaultValue int `json:"defaultValue"`
	Min          int `json:"min"`
	Max          int `json:"max"`
}

type runLimitsJSON struct {
	MaxConcurrentRuns         runLimitJSON `json:"maxConcurrentRuns"`
	TriggerConcurrency        runLimitJSON `json:"triggerConcurrency"`
	TriggerRatePerMinute      runLimitJSON `json:"triggerRatePerMinute"`
	SandboxCreationsPerMinute runLimitJSON `json:"sandboxCreationsPerMinute"`
}

var _ = Describe("run limits in the admin API", func() {
	var e *env
	BeforeEach(func(ctx SpecContext) { e = newEnv(ctx) })

	audits := func(ctx SpecContext) int {
		GinkgoHelper()
		var n int
		Expect(pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'org.run_limits.set' AND actor_kind = 'token'`, e.org).Scan(&n)).To(Succeed())
		return n
	}

	It("starts at the defaults with the bounds", func(ctx SpecContext) {
		code, raw := e.call(ctx, e.token, http.MethodGet, "/run-limits", nil)
		Expect(code).To(Equal(http.StatusOK), string(raw))
		Expect(decode[runLimitsJSON](raw)).To(Equal(runLimitsJSON{
			MaxConcurrentRuns:         runLimitJSON{Value: 20, DefaultValue: 20, Min: 1, Max: 10000},
			TriggerConcurrency:        runLimitJSON{Value: 4, DefaultValue: 4, Min: 1, Max: 1000},
			TriggerRatePerMinute:      runLimitJSON{Value: 60, DefaultValue: 60, Min: 1, Max: 100000},
			SandboxCreationsPerMinute: runLimitJSON{Value: 10, DefaultValue: 10, Min: 1, Max: 10000},
		}))
	})

	It("changes only the limits in the body, returns one to the default with 0 and audits each change", func(ctx SpecContext) {
		code, raw := e.call(ctx, e.token, http.MethodPatch, "/run-limits", map[string]any{"maxConcurrentRuns": 12, "sandboxCreationsPerMinute": 3})
		Expect(code).To(Equal(http.StatusOK), string(raw))
		r := decode[runLimitsJSON](raw)
		Expect(r.MaxConcurrentRuns.Value).To(Equal(12))
		Expect(r.SandboxCreationsPerMinute.Value).To(Equal(3))
		Expect(r.TriggerConcurrency.Value).To(Equal(4), "a limit left out stays")
		Expect(audits(ctx)).To(Equal(1))

		code, raw = e.call(ctx, e.token, http.MethodPatch, "/run-limits", map[string]any{"maxConcurrentRuns": 0})
		Expect(code).To(Equal(http.StatusOK), string(raw))
		r = decode[runLimitsJSON](raw)
		Expect(r.MaxConcurrentRuns.Value).To(Equal(20), "0 returns the default")
		Expect(r.SandboxCreationsPerMinute.Value).To(Equal(3), "a limit left out stays")
		Expect(audits(ctx)).To(Equal(2))

		code, raw = e.call(ctx, e.token, http.MethodGet, "/run-limits", nil)
		Expect(code).To(Equal(http.StatusOK), string(raw))
		Expect(decode[runLimitsJSON](raw).SandboxCreationsPerMinute.Value).To(Equal(3))
	})

	It("refuses a limit out of its bounds with a problem response and does not audit it", func(ctx SpecContext) {
		for _, bad := range []map[string]any{{"maxConcurrentRuns": -1}, {"triggerConcurrency": 1001}, {"maxConcurrentRuns": 5, "triggerRatePerMinute": 100001}} {
			code, raw := e.call(ctx, e.token, http.MethodPatch, "/run-limits", bad)
			Expect(code).To(Equal(http.StatusBadRequest), string(raw))
			Expect(decode[map[string]any](raw)).To(HaveKey("error"))
		}
		Expect(audits(ctx)).To(BeZero())
		code, raw := e.call(ctx, e.token, http.MethodGet, "/run-limits", nil)
		Expect(code).To(Equal(http.StatusOK), string(raw))
		Expect(decode[runLimitsJSON](raw).MaxConcurrentRuns.Value).To(Equal(20))
	})
})
