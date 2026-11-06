// SPDX-License-Identifier: Apache-2.0

package adminapi_test

import (
	"net/http"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

type runContentRetentionJSON struct {
	Days        int `json:"days"`
	DefaultDays int `json:"defaultDays"`
	MinDays     int `json:"minDays"`
	MaxDays     int `json:"maxDays"`
}

var _ = Describe("run content retention in the admin API", func() {
	var e *env
	BeforeEach(func(ctx SpecContext) { e = newEnv(ctx) })

	auditCount := func(ctx SpecContext) int {
		GinkgoHelper()
		var n int
		Expect(pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'org.run_content_retention.set' AND actor_kind = 'token'`, e.org).Scan(&n)).To(Succeed())
		return n
	}

	It("starts at 30 days with the bounds", func(ctx SpecContext) {
		code, raw := e.call(ctx, e.token, http.MethodGet, "/run-content-retention", nil)
		Expect(code).To(Equal(http.StatusOK), string(raw))
		r := decode[runContentRetentionJSON](raw)
		Expect(r).To(Equal(runContentRetentionJSON{Days: 30, DefaultDays: 30, MinDays: 1, MaxDays: 365}))
	})

	It("sets the period and returns it to the default with 0 or an omitted value, auditing each change", func(ctx SpecContext) {
		code, raw := e.call(ctx, e.token, http.MethodPut, "/run-content-retention", map[string]any{"days": 7})
		Expect(code).To(Equal(http.StatusOK), string(raw))
		Expect(decode[runContentRetentionJSON](raw).Days).To(Equal(7))
		code, raw = e.call(ctx, e.token, http.MethodGet, "/run-content-retention", nil)
		Expect(code).To(Equal(http.StatusOK), string(raw))
		Expect(decode[runContentRetentionJSON](raw).Days).To(Equal(7))

		code, raw = e.call(ctx, e.token, http.MethodPut, "/run-content-retention", map[string]any{})
		Expect(code).To(Equal(http.StatusOK), string(raw))
		Expect(decode[runContentRetentionJSON](raw).Days).To(Equal(30), "an omitted value returns the default")
		Expect(auditCount(ctx)).To(Equal(2))

		code, raw = e.call(ctx, e.token, http.MethodPut, "/run-content-retention", map[string]any{"days": 90})
		Expect(code).To(Equal(http.StatusOK), string(raw))
		code, raw = e.call(ctx, e.token, http.MethodPut, "/run-content-retention", map[string]any{"days": 0})
		Expect(code).To(Equal(http.StatusOK), string(raw))
		Expect(decode[runContentRetentionJSON](raw).Days).To(Equal(30), "0 returns the default, as on the UI API")
	})

	It("refuses a value out of range with a problem response and does not audit it", func(ctx SpecContext) {
		for _, bad := range []int{-1, 366} {
			code, raw := e.call(ctx, e.token, http.MethodPut, "/run-content-retention", map[string]any{"days": bad})
			Expect(code).To(Equal(http.StatusBadRequest), string(raw))
			Expect(decode[map[string]any](raw)).To(HaveKey("error"))
		}
		Expect(auditCount(ctx)).To(BeZero())
		code, raw := e.call(ctx, e.token, http.MethodGet, "/run-content-retention", nil)
		Expect(code).To(Equal(http.StatusOK), string(raw))
		Expect(decode[runContentRetentionJSON](raw).Days).To(Equal(30))
	})
})
