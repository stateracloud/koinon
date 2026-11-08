// SPDX-License-Identifier: Apache-2.0

package audit_test

import (
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

var _ = Describe("List", func() {
	var org, other pgtype.UUID
	var q *store.Queries

	BeforeEach(func(ctx SpecContext) {
		q = store.New(pool)
		org, _, _ = pgtest.NewOrg(ctx, pool)
		other, _, _ = pgtest.NewOrg(ctx, pool)
	})

	rec := func(ctx SpecContext, o pgtype.UUID, actor audit.Actor, action string) {
		GinkgoHelper()
		_, err := audit.InTx(ctx, pool, func(_ *store.Queries, b audit.Batch) error {
			b.Org(audit.Event{Org: o, Actor: actor, Action: action, TargetKind: "test"})
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
	}
	actions := func(rows []store.AuditEvent) []string {
		var out []string
		for _, r := range rows {
			out = append(out, r.Action)
		}
		return out
	}

	It("lists one org's events, newest first, and never another org's", func(ctx SpecContext) {
		rec(ctx, org, audit.System("koinon-test:spec"), "test.a")
		rec(ctx, other, audit.System("koinon-test:spec"), "test.secret")
		rec(ctx, org, audit.System("koinon-test:spec"), "test.b")
		rows, next, err := audit.List(ctx, q, audit.Filter{Org: org})
		Expect(err).NotTo(HaveOccurred())
		Expect(actions(rows)).To(Equal([]string{"test.b", "test.a"}))
		Expect(next).To(BeEmpty())

		all, _, err := audit.List(ctx, q, audit.Filter{AllOrgs: true, ActionPrefix: "test.secret"})
		Expect(err).NotTo(HaveOccurred())
		Expect(actions(all)).To(Equal([]string{"test.secret"}), "no org: every org")
	})

	It("filters by action prefix, actor and time range", func(ctx SpecContext) {
		u := pgtype.UUID{Bytes: [16]byte{1}, Valid: true}
		rec(ctx, org, audit.User(u), "approval.approve")
		rec(ctx, org, audit.User(u), "approval.deny")
		rec(ctx, org, audit.System("koinon-test:spec"), "agent.create")
		rec(ctx, org, audit.Token(u), "agent.archive")

		rows, _, err := audit.List(ctx, q, audit.Filter{Org: org, ActionPrefix: "approval."})
		Expect(err).NotTo(HaveOccurred())
		Expect(actions(rows)).To(ConsistOf("approval.approve", "approval.deny"))
		rows, _, err = audit.List(ctx, q, audit.Filter{Org: org, ActionPrefix: "agent.c"})
		Expect(err).NotTo(HaveOccurred())
		Expect(actions(rows)).To(Equal([]string{"agent.create"}))
		rows, _, err = audit.List(ctx, q, audit.Filter{Org: org, ActorKind: "user", ActorID: u.String()})
		Expect(err).NotTo(HaveOccurred())
		Expect(actions(rows)).To(ConsistOf("approval.approve", "approval.deny"))
		rows, _, err = audit.List(ctx, q, audit.Filter{Org: org, ActorKind: "system"})
		Expect(err).NotTo(HaveOccurred())
		Expect(actions(rows)).To(Equal([]string{"agent.create"}))

		rows, _, err = audit.List(ctx, q, audit.Filter{Org: org, Since: time.Now().Add(time.Hour)})
		Expect(err).NotTo(HaveOccurred())
		Expect(rows).To(BeEmpty())
		rows, _, err = audit.List(ctx, q, audit.Filter{Org: org, Until: time.Now().Add(-time.Hour)})
		Expect(err).NotTo(HaveOccurred())
		Expect(rows).To(BeEmpty())
		rows, _, err = audit.List(ctx, q, audit.Filter{Org: org, Since: time.Now().Add(-time.Hour), Until: time.Now().Add(time.Hour)})
		Expect(err).NotTo(HaveOccurred())
		Expect(rows).To(HaveLen(4))
	})

	It("pages with a cursor and reaches every event exactly once", func(ctx SpecContext) {
		for i := range 7 {
			rec(ctx, org, audit.System("koinon-test:spec"), fmt.Sprintf("test.p%d", i))
		}
		var got []string
		token := ""
		pages := 0
		for {
			rows, next, err := audit.List(ctx, q, audit.Filter{Org: org, PageSize: 3, PageToken: token})
			Expect(err).NotTo(HaveOccurred())
			got = append(got, actions(rows)...)
			pages++
			if next == "" {
				break
			}
			token = next
		}
		Expect(pages).To(Equal(3))
		Expect(got).To(Equal([]string{"test.p6", "test.p5", "test.p4", "test.p3", "test.p2", "test.p1", "test.p0"}))

		By("a page that ends exactly at the last event has no next token")
		rows, next, err := audit.List(ctx, q, audit.Filter{Org: org, PageSize: 7})
		Expect(err).NotTo(HaveOccurred())
		Expect(rows).To(HaveLen(7))
		Expect(next).To(BeEmpty())
	})

	It("refuses filters it cannot use", func(ctx SpecContext) {
		for name, f := range map[string]audit.Filter{
			"prefix":     {Org: org, ActionPrefix: "A%"},
			"actor kind": {Org: org, ActorKind: "robot"},
			"actor id":   {Org: org, ActorID: "x"},
			"range":      {Org: org, Since: time.Now(), Until: time.Now().Add(-time.Hour)},
			"token":      {Org: org, PageToken: "!!"},
			"page size":  {Org: org, PageSize: -1},
			"no org":     {},
			"both":       {Org: org, AllOrgs: true},
		} {
			_, _, err := audit.List(ctx, q, f)
			Expect(err).To(MatchError(audit.ErrBadFilter), name)
		}
	})
})
