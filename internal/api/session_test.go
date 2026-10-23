// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"net/http"
	"time"

	connect "connectrpc.com/connect/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	authv1alpha1 "github.com/procoduck/koinon/internal/gen/koinon/auth/v1alpha1"
)

var _ = Describe("sessions", func() {
	var (
		e *env
		b *browser
	)

	whoAmI := func(ctx SpecContext) error {
		_, err := b.auth().WhoAmI(ctx, &authv1alpha1.WhoAmIRequest{})
		return err
	}

	BeforeEach(func(ctx SpecContext) {
		e = newEnv(ctx, envOptions{})
		b = e.browser()
		Expect(b.login(ctx, "admin", bootstrapPassword).StatusCode).To(Equal(http.StatusOK))
		Expect(whoAmI(ctx)).To(Succeed())
	})

	It("ends after the idle timeout", func(ctx SpecContext) {
		e.clock.Advance(idleTimeout + time.Second)
		Expect(codeOf(whoAmI(ctx))).To(Equal(connect.CodeUnauthenticated))
	})

	It("extends the idle deadline on use, up to the absolute lifetime", func(ctx SpecContext) {
		step := idleTimeout - time.Minute
		var elapsed time.Duration
		for elapsed+step < maxLifetime {
			e.clock.Advance(step)
			elapsed += step
			Expect(whoAmI(ctx)).To(Succeed(), "after %s", elapsed)
		}
		e.clock.Advance(maxLifetime - elapsed)
		Expect(codeOf(whoAmI(ctx))).To(Equal(connect.CodeUnauthenticated))
	})

	It("is revoked by logout, and the old token stops working", func(ctx SpecContext) {
		token := b.sessionToken()
		res := b.post(ctx, "/auth/logout", nil)
		Expect(res.StatusCode).To(Equal(http.StatusNoContent))
		Expect(res.Cookies()).To(ContainElement(SatisfyAll(
			HaveField("Name", "__Host-koinon_session"), HaveField("MaxAge", -1))))
		Expect(b.sessionToken()).To(BeEmpty())

		b.setSessionToken(token)
		Expect(codeOf(whoAmI(ctx))).To(Equal(connect.CodeUnauthenticated))
		Expect(count(ctx, e, `SELECT count(*) FROM sessions WHERE revoked_at IS NOT NULL`)).To(Equal(1))
	})

	It("replaces the session at the next login instead of reusing it", func(ctx SpecContext) {
		first := b.sessionToken()
		Expect(b.login(ctx, "admin", bootstrapPassword).StatusCode).To(Equal(http.StatusOK))
		Expect(b.sessionToken()).NotTo(Equal(first))

		replay := e.browser()
		replay.setSessionToken(first)
		_, err := replay.auth().WhoAmI(ctx, &authv1alpha1.WhoAmIRequest{})
		Expect(codeOf(err)).To(Equal(connect.CodeUnauthenticated))
	})

	It("purges ended sessions after the retention period", func(ctx SpecContext) {
		Expect(b.post(ctx, "/auth/logout", nil).StatusCode).To(Equal(http.StatusNoContent))
		Expect(e.svc.PurgeExpired(ctx, e.clock.Now().Add(-time.Hour))).To(Succeed())
		Expect(count(ctx, e, `SELECT count(*) FROM sessions`)).To(Equal(1))
		Expect(e.svc.PurgeExpired(ctx, e.clock.Now().Add(time.Second))).To(Succeed())
		Expect(count(ctx, e, `SELECT count(*) FROM sessions`)).To(Equal(0))
	})
})
