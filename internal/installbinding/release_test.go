// SPDX-License-Identifier: Apache-2.0

package installbinding_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/installbinding"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

var _ = Describe("the release step (ADR 0110 decision 1)", func() {
	prov := installbinding.Removal{Kind: "model-provider", Org: "default", Secret: "src-provider"}
	conn := installbinding.Removal{Kind: "connection", Org: "default", Secret: "src-conn"}

	BeforeEach(func(ctx SpecContext) {
		for _, stmt := range []string{`DELETE FROM install_binding`, `DELETE FROM sessions`, `DELETE FROM restore_accepted_removals`} {
			_, err := pool.Exec(ctx, stmt)
			Expect(err).NotTo(HaveOccurred())
		}
	})

	seedCopy := func(ctx SpecContext) {
		GinkgoHelper()
		_, user, _ := pgtest.NewOrg(ctx, pool)
		held := newBinding("hash-copy", true)
		_, err := held.Claim(ctx, installbinding.Targets{})
		Expect(err).NotTo(HaveOccurred())
		_, err = pool.Exec(ctx, `INSERT INTO sessions (token_hash, user_id, auth_method, created_at, last_seen_at, idle_expires_at, expires_at, install_mark)
			VALUES (sha256('s'), $1, 'password', now(), now(), now() + interval '1 hour', now() + interval '2 hours', 'hash-source')`, user)
		Expect(err).NotTo(HaveOccurred())
	}
	count := func(ctx SpecContext, q string, args ...any) int {
		GinkgoHelper()
		var n int
		Expect(pool.QueryRow(ctx, q, args...).Scan(&n)).To(Succeed())
		return n
	}

	It("is pending while held_since is set and for a copy that has not released; never for an absent row or a live install", func() {
		self := installbinding.Row{Bound: true, Mark: "m"}
		Expect(installbinding.Pending("m", self, true)).To(BeFalse(), "a live install with its mark")
		Expect(installbinding.Pending("m", installbinding.Row{}, false)).To(BeFalse(), "the first upgrade of an existing install")
		Expect(installbinding.Pending("", installbinding.Row{Bound: true, Mark: "x"}, true)).To(BeFalse(), "no mark, no binding")
		Expect(installbinding.Pending("m", installbinding.Row{Bound: true, Mark: "x"}, true)).To(BeTrue(), "another install's mark")
		Expect(installbinding.Pending("m", installbinding.Row{Bound: true, Mark: "x", ReleasedBy: "m"}, true)).To(BeFalse(), "a copy that released")
	})

	It("revokes the sessions and keeps the removals waiting without the accept; the hold is released once", func(ctx SpecContext) {
		seedCopy(ctx)
		api := newBinding("hash-copy", false)
		res, err := api.Settle(ctx, []installbinding.Removal{prov, conn}, false)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Released).To(BeTrue())
		Expect(res.RevokedSessions).To(Equal(1))
		Expect(res.Allowed(prov)).To(BeFalse())
		Expect(res.Allowed(conn)).To(BeFalse())
		Expect(res.Allowed(installbinding.Removal{Kind: "mcp-server", Org: "default", Secret: "declared-later"})).To(BeTrue(), "an item the restore did not bring in follows the normal rules")
		Expect(res.Waiting).To(ConsistOf(prov, conn))
		Expect(count(ctx, `SELECT count(*) FROM sessions`)).To(BeZero())
		Expect(count(ctx, `SELECT count(*) FROM install_binding WHERE held_since IS NULL AND released_by = 'hash-copy'`)).To(Equal(1))
		Expect(count(ctx, `SELECT count(*) FROM install_audit_events WHERE action = 'restore.release'`)).To(Equal(1))
		Expect(count(ctx, `SELECT count(*) FROM install_audit_events WHERE action = 'restore.sessions_revoked'`)).To(Equal(1))

		By("a later pass is not a release and the removals still wait")
		res, err = api.Settle(ctx, []installbinding.Removal{prov, conn}, false)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Released).To(BeFalse())
		Expect(res.Allowed(prov)).To(BeFalse())
		Expect(count(ctx, `SELECT count(*) FROM install_audit_events WHERE action = 'restore.release'`)).To(Equal(1))

		By("the accept turns waiting into accepted, audited, and revokes no session")
		res, err = api.Settle(ctx, []installbinding.Removal{prov, conn}, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Accepted).To(Equal(2))
		Expect(res.Allowed(prov)).To(BeTrue())
		Expect(count(ctx, `SELECT count(*) FROM install_audit_events WHERE action = 'restore.removals_accepted'`)).To(Equal(1))

		By("a Secret that came back, or a removal that was done, leaves the list")
		_, err = api.Settle(ctx, nil, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(count(ctx, `SELECT count(*) FROM restore_accepted_removals`)).To(BeZero())
	})

	It("accepts the removals at the release when the value is set", func(ctx SpecContext) {
		seedCopy(ctx)
		res, err := newBinding("hash-copy", false).Settle(ctx, []installbinding.Removal{prov}, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Released).To(BeTrue())
		Expect(res.Allowed(prov)).To(BeTrue())
		Expect(res.Waiting).To(BeEmpty())
	})

	It("settles nothing for a live install", func(ctx SpecContext) {
		live := newBinding("hash-live", false)
		_, err := live.Claim(ctx, installbinding.Targets{})
		Expect(err).NotTo(HaveOccurred())
		res, err := live.Settle(ctx, []installbinding.Removal{prov}, false)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Released).To(BeFalse())
		Expect(res.Allowed(prov)).To(BeTrue(), "an install that was not restored removes what its deleted Secret declared")
	})
})
