// SPDX-License-Identifier: Apache-2.0

package adminapi_test

import (
	"context"
	"net/http"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("GET /admin/v1/restore/status", func() {
	It("serves the report to a token rooted at a platform admin only, checked on every request (ADR 0110 decision 7)", func(ctx SpecContext) {
		e := newEnv(ctx)
		e.api.WithRestoreStatus(func(context.Context) (any, error) {
			return map[string]any{"binding": map[string]any{"state": "copy"}, "release": map[string]any{"pending": true}}, nil
		})

		st, raw := e.call(ctx, e.token, http.MethodGet, "/restore/status", nil)
		Expect(st).To(Equal(http.StatusForbidden), string(raw))

		_, err := pool.Exec(ctx, `INSERT INTO platform_admins (user_id) VALUES ($1)`, e.user)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx SpecContext) {
			_, _ = pool.Exec(ctx, `DELETE FROM platform_admins WHERE user_id = $1`, e.user)
		})
		st, raw = e.call(ctx, e.token, http.MethodGet, "/restore/status", nil)
		Expect(st).To(Equal(http.StatusOK), string(raw))
		Expect(decode[map[string]any](raw)).To(HaveKeyWithValue("release", HaveKeyWithValue("pending", true)))

		_, err = pool.Exec(ctx, `DELETE FROM platform_admins WHERE user_id = $1`, e.user)
		Expect(err).NotTo(HaveOccurred())
		st, _ = e.call(ctx, e.token, http.MethodGet, "/restore/status", nil)
		Expect(st).To(Equal(http.StatusForbidden))
	})
})
