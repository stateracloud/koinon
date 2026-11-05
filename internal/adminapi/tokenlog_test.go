// SPDX-License-Identifier: Apache-2.0

package adminapi_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"

	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/adminapi"
	"github.com/stateracloud/koinon/internal/tokens"
	"github.com/stateracloud/koinon/pricing"
)

// A token check that fails for a reason other than a bad token (here: the database is down) is
// logged as an error class. The raw token and the Authorization header never reach the log (#152).
var _ = Describe("token check failure logging", func() {
	It("logs a class, not the error, and never the token", func(ctx SpecContext) {
		// Nothing listens on this port; the pool connects lazily, so Authenticate fails on its
		// first query with a connection error that is not a tokens.AuthError.
		dead, err := pgxpool.New(ctx, "postgres://koinon_test_user:koinon_test_not-a-real-pw@127.0.0.1:1/koinon?sslmode=disable&connect_timeout=1")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(dead.Close)

		var logs bytes.Buffer
		log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
		srv := adminapi.New(dead, nil, tokens.New(dead, nil), log, pricing.Embedded())
		mux := http.NewServeMux()
		srv.Mount(mux)

		const secret = "koinon_abcd1234_koinon_test_not-a-real-secret-0123456789abcdefghi"
		req := httptest.NewRequest(http.MethodGet, adminapi.BasePath+"/tokens", nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		Expect(rec.Code).To(Equal(http.StatusInternalServerError))
		Expect(logs.String()).To(ContainSubstring("admin API token check failed"))
		Expect(logs.String()).To(ContainSubstring("error_class="))
		Expect(logs.String()).NotTo(ContainSubstring("secret-0123456789"))
		Expect(logs.String()).NotTo(ContainSubstring("Bearer"))
		Expect(rec.Body.String()).NotTo(ContainSubstring("secret-0123456789"))
	}, SpecTimeout(30e9))
})
