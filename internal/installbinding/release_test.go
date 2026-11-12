// SPDX-License-Identifier: Apache-2.0

package installbinding_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/stateracloud/koinon/internal/installbinding"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

var _ = Describe("the release step (ADR 0110 decision 1)", func() {
	prov := installbinding.Removal{Kind: "model-provider", Org: "default", Secret: "src-provider"}
	conn := installbinding.Removal{Kind: "connection", Org: "default", Secret: "src-conn"}

	BeforeEach(func(ctx SpecContext) {
		_, _, slug := pgtest.NewOrg(ctx, pool)
		prov.Org, conn.Org = slug, slug
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
		reg := prometheus.NewRegistry()
		api.ReleasePendingMetric(reg)
		gauge := func() float64 {
			GinkgoHelper()
			mfs, err := reg.Gather()
			Expect(err).NotTo(HaveOccurred())
			Expect(mfs).To(HaveLen(1))
			return mfs[0].GetMetric()[0].GetGauge().GetValue()
		}
		Expect(gauge()).To(BeEquivalentTo(1), "pending before the release")
		pending, err := api.ReleasePending(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(pending).To(BeTrue(), "the session gate stays closed while the step fails")
		res, err := api.Settle(ctx, []installbinding.Removal{prov, conn}, false)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Released).To(BeTrue())
		Expect(res.RevokedSessions).To(Equal(1))
		pending, err = api.ReleasePending(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(pending).To(BeFalse(), "a replica whose step fails after another one released opens its gate")
		Expect(res.Allowed(prov)).To(BeFalse())
		Expect(res.Allowed(conn)).To(BeFalse())
		Expect(res.Allowed(installbinding.Removal{Kind: "mcp-server", Org: "default", Secret: "declared-later"})).To(BeTrue(), "an item the restore did not bring in follows the normal rules")
		Expect(res.Waiting).To(ConsistOf(prov, conn))
		Expect(count(ctx, `SELECT count(*) FROM sessions`)).To(BeZero())
		Expect(count(ctx, `SELECT count(*) FROM install_binding WHERE held_since IS NULL AND released_by = 'hash-copy'`)).To(Equal(1))
		Expect(count(ctx, `SELECT count(*) FROM install_audit_events WHERE action = 'restore.release'`)).To(Equal(1))
		Expect(count(ctx, `SELECT count(*) FROM install_audit_events WHERE action = 'restore.sessions_revoked'`)).To(Equal(1))
		Expect(count(ctx, `SELECT count(*) FROM install_audit_events WHERE action = 'restore.release' AND summary->'after'->>'planHash' = $1`,
			installbinding.PlanHash([]installbinding.Removal{conn, prov}))).To(Equal(1), "the release records the plan hash")
		Expect(gauge()).To(BeEquivalentTo(1), "removals wait for the accept")

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
		Expect(gauge()).To(BeZero(), "nothing pending, nothing waiting")

		By("a Secret that came back, or a removal that was done, leaves the list")
		_, err = api.Settle(ctx, nil, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(count(ctx, `SELECT count(*) FROM restore_accepted_removals`)).To(BeZero())
	})

	It("keeps the list when no plan is available (no Kubernetes access), and refuses a removal of an unknown org", func(ctx SpecContext) {
		seedCopy(ctx)
		api := newBinding("hash-copy", false)
		_, err := api.Settle(ctx, []installbinding.Removal{prov}, false)
		Expect(err).NotTo(HaveOccurred())
		res, err := api.SettleWith(ctx, nil, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Accepted).To(BeZero(), "without a plan nothing is accepted")
		Expect(res.Allowed(prov)).To(BeFalse(), "an importer with a plan still finds the item on the list")
		Expect(count(ctx, `SELECT count(*) FROM restore_accepted_removals`)).To(Equal(1))

		By("a release whose plan names an org that is gone fails and releases nothing")
		_, err = pool.Exec(ctx, `DELETE FROM install_binding`)
		Expect(err).NotTo(HaveOccurred())
		_, err = newBinding("hash-copy", true).Claim(ctx, installbinding.Targets{})
		Expect(err).NotTo(HaveOccurred())
		_, err = api.Settle(ctx, []installbinding.Removal{{Kind: "connection", Org: "no-such-org", Secret: "x"}}, false)
		Expect(err).To(HaveOccurred())
		Expect(count(ctx, `SELECT count(*) FROM install_binding WHERE held_since IS NOT NULL`)).To(Equal(1))
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

	It("computes the plan after another replica's release commits, never before the lock", func(ctx SpecContext) {
		for _, stmt := range []string{`DELETE FROM install_binding`, `DELETE FROM sessions`, `DELETE FROM restore_accepted_removals`} {
			_, err := pool.Exec(ctx, stmt)
			Expect(err).NotTo(HaveOccurred())
		}
		_, err := newBinding("hash-copy", true).Claim(ctx, installbinding.Targets{})
		Expect(err).NotTo(HaveOccurred())

		prov := installbinding.Removal{Kind: "model-provider", Org: "default", Secret: "src-provider"}
		var before int
		Expect(pool.QueryRow(ctx, `SELECT count(*) FROM install_audit_events WHERE action = 'restore.release'`).Scan(&before)).To(Succeed())
		inA, releaseA := make(chan struct{}), make(chan struct{})
		doneA := make(chan error, 1)
		a := newBinding("hash-copy", false)
		go func() {
			defer GinkgoRecover()
			_, err := a.SettleWith(ctx, func(context.Context) ([]installbinding.Removal, error) {
				close(inA)
				<-releaseA // the plan is slow: A holds the row lock meanwhile
				return nil, nil
			}, false)
			doneA <- err
		}()
		Eventually(inA).Should(BeClosed())

		releasedWhenPlanned := make(chan bool, 1)
		doneB := make(chan error, 1)
		b := newBinding("hash-copy", false)
		go func() {
			defer GinkgoRecover()
			_, err := b.SettleWith(ctx, func(c context.Context) ([]installbinding.Removal, error) {
				var n int
				if err := pool.QueryRow(c, `SELECT count(*) FROM install_audit_events WHERE action = 'restore.release'`).Scan(&n); err != nil {
					return nil, err
				}
				releasedWhenPlanned <- n == before+1
				return []installbinding.Removal{prov}, nil
			}, false)
			doneB <- err
		}()
		Consistently(releasedWhenPlanned, "500ms").ShouldNot(Receive(), "B plans only once it holds the row lock")
		close(releaseA)
		Eventually(doneA).Should(Receive(BeNil()))
		Eventually(doneB).Should(Receive(BeNil()))
		Expect(<-releasedWhenPlanned).To(BeTrue(), "B's plan was computed after A's release was visible")
	})
})
