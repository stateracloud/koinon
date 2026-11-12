// SPDX-License-Identifier: Apache-2.0

package staterev_test

import (
	"context"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/staterev"
	gwv1 "github.com/stateracloud/koinon/internal/gen/koinon/gateway/v1alpha1"
	"github.com/stateracloud/koinon/internal/money"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

var _ = Describe("Publisher and acks", Ordered, func() {
	var (
		pool *pgxpool.Pool
		pub  staterev.Publisher
	)

	BeforeAll(func(ctx SpecContext) {
		// Skips (or fails, under KOINON_REQUIRE) without Docker. Its own database: nothing here
		// reads another suite's rows.
		pool = pgtest.Start(ctx)
	}, NodeTimeout(3*time.Minute))

	BeforeEach(func(ctx SpecContext) {
		_, err := pool.Exec(ctx, "DELETE FROM gateway_state_revisions")
		Expect(err).NotTo(HaveOccurred())
		pub = staterev.Publisher{DB: pool}
	})

	rows := func(ctx context.Context) int {
		var n int
		Expect(pool.QueryRow(ctx, "SELECT count(*) FROM gateway_state_revisions").Scan(&n)).To(Succeed())
		return n
	}

	It("stores the first state, and nothing for the same state again", func(ctx SpecContext) {
		r1, err := pub.Publish(ctx, richState())
		Expect(err).NotTo(HaveOccurred())
		Expect(r1.Changed).To(BeTrue())

		r2, err := pub.Publish(ctx, richState())
		Expect(err).NotTo(HaveOccurred())
		Expect(r2.Changed).To(BeFalse())
		Expect(r2.Revision).To(Equal(r1.Revision))
		Expect(rows(ctx)).To(Equal(1))
	})

	It("stores a new revision when the state changes, and reads the latest back", func(ctx SpecContext) {
		r1, err := pub.Publish(ctx, richState())
		Expect(err).NotTo(HaveOccurred())
		s := richState()
		s.SetLimit(agentA, &gateway.Limit{Unit: gateway.USD, Amount: money.MustParse("9"), Window: time.Hour})
		r2, err := pub.Publish(ctx, s)
		Expect(err).NotTo(HaveOccurred())
		Expect(r2.Changed).To(BeTrue())
		Expect(r2.Revision).To(BeNumerically(">", r1.Revision))

		u, err := staterev.Latest(ctx, pool)
		Expect(err).NotTo(HaveOccurred())
		Expect(u.Revision).To(Equal(r2.Revision))
		Expect(staterev.FromProto(u).Limits[agentA].Amount).To(Equal(money.MustParse("9")))

		// A State that goes back to the first is a change again.
		r3, err := pub.Publish(ctx, richState())
		Expect(err).NotTo(HaveOccurred())
		Expect(r3.Changed).To(BeTrue())
	})

	It("has no latest revision before the first publish", func(ctx SpecContext) {
		u, err := staterev.Latest(ctx, pool)
		Expect(err).NotTo(HaveOccurred())
		Expect(u).To(BeNil())
	})

	It("notifies on commit with the revision", func(ctx SpecContext) {
		conn, err := pool.Acquire(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer conn.Release()
		_, err = conn.Exec(ctx, "LISTEN "+staterev.NotifyChannel)
		Expect(err).NotTo(HaveOccurred())

		r, err := pub.Publish(ctx, richState())
		Expect(err).NotTo(HaveOccurred())
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		n, err := conn.Conn().WaitForNotification(wctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(n.Payload).To(Equal(itoa(r.Revision)))
	})

	It("keeps only the newest revisions, and drops their acks with the rest", func(ctx SpecContext) {
		pub.Keep = 3
		var revs []int64
		for i := 0; i < 7; i++ {
			s := richState()
			s.SetLimit(agentA, &gateway.Limit{Unit: gateway.USD, Amount: money.USD(int64(i+1) * money.PerUSD)})
			r, err := pub.Publish(ctx, s)
			Expect(err).NotTo(HaveOccurred())
			Expect(r.Changed).To(BeTrue())
			revs = append(revs, r.Revision)
			if i == 0 {
				Expect(staterev.Ack(ctx, pool, "copy-1", r.Revision, gwv1.Half_HALF_MODEL)).To(Succeed())
			}
		}
		Expect(rows(ctx)).To(Equal(3))
		var oldest int64
		Expect(pool.QueryRow(ctx, "SELECT min(revision) FROM gateway_state_revisions").Scan(&oldest)).To(Succeed())
		Expect(oldest).To(Equal(revs[4]))
		var acks int
		Expect(pool.QueryRow(ctx, "SELECT count(*) FROM gateway_state_acks").Scan(&acks)).To(Succeed())
		Expect(acks).To(BeZero(), "the ack of a pruned revision goes with it")
	})

	Describe("AllAcked", func() {
		It("is true only when every live copy confirmed the half", func(ctx SpecContext) {
			r, err := pub.Publish(ctx, richState())
			Expect(err).NotTo(HaveOccurred())
			live := []string{"copy-1", "copy-2"}

			ok, err := staterev.AllAcked(ctx, pool, r.Revision, gwv1.Half_HALF_MODEL, live)
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeFalse())

			Expect(staterev.Ack(ctx, pool, "copy-1", r.Revision, gwv1.Half_HALF_MODEL)).To(Succeed())
			Expect(staterev.Ack(ctx, pool, "copy-1", r.Revision, gwv1.Half_HALF_MODEL)).To(Succeed(), "idempotent")
			ok, _ = staterev.AllAcked(ctx, pool, r.Revision, gwv1.Half_HALF_MODEL, live)
			Expect(ok).To(BeFalse(), "copy-2 has not confirmed")

			Expect(staterev.Ack(ctx, pool, "copy-2", r.Revision, gwv1.Half_HALF_MODEL)).To(Succeed())
			ok, _ = staterev.AllAcked(ctx, pool, r.Revision, gwv1.Half_HALF_MODEL, live)
			Expect(ok).To(BeTrue())
			ok, _ = staterev.AllAcked(ctx, pool, r.Revision, gwv1.Half_HALF_MODEL, []string{"copy-1", "copy-2", "copy-1"})
			Expect(ok).To(BeTrue(), "a repeated name counts once")

			ok, _ = staterev.AllAcked(ctx, pool, r.Revision, gwv1.Half_HALF_MCP, live)
			Expect(ok).To(BeFalse(), "the MCP half is confirmed apart from the model half")
		})

		It("counts a later ack for an earlier revision, and ignores copies that are gone", func(ctx SpecContext) {
			r1, _ := pub.Publish(ctx, richState())
			s := richState()
			s.SetLimit(agentA, &gateway.Limit{Unit: gateway.USD, Amount: money.MustParse("2")})
			r2, err := pub.Publish(ctx, s)
			Expect(err).NotTo(HaveOccurred())

			Expect(staterev.Ack(ctx, pool, "copy-1", r2.Revision, gwv1.Half_HALF_MCP)).To(Succeed())
			ok, _ := staterev.AllAcked(ctx, pool, r1.Revision, gwv1.Half_HALF_MCP, []string{"copy-1"})
			Expect(ok).To(BeTrue(), "serving r2 covers r1")
			ok, _ = staterev.AllAcked(ctx, pool, r2.Revision, gwv1.Half_HALF_MCP, []string{"copy-1"})
			Expect(ok).To(BeTrue(), "the copy that is not live (copy-0) is not asked")
		})

		It("is false for no live copy, never 'everyone has it'", func(ctx SpecContext) {
			r, _ := pub.Publish(ctx, richState())
			ok, err := staterev.AllAcked(ctx, pool, r.Revision, gwv1.Half_HALF_MODEL, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeFalse())
		})

		It("refuses an ack for an unknown revision, an unset half and a nameless copy", func(ctx SpecContext) {
			Expect(staterev.Ack(ctx, pool, "copy-1", 999999, gwv1.Half_HALF_MODEL)).NotTo(Succeed())
			r, _ := pub.Publish(ctx, richState())
			Expect(staterev.Ack(ctx, pool, "copy-1", r.Revision, gwv1.Half_HALF_UNSPECIFIED)).NotTo(Succeed())
			Expect(staterev.Ack(ctx, pool, "", r.Revision, gwv1.Half_HALF_MODEL)).NotTo(Succeed())
		})
	})

	It("stores no secret material in the row", func(ctx SpecContext) {
		issued, err := gateway.NewKey(agentA)
		Expect(err).NotTo(HaveOccurred())
		var s gateway.State
		s.AddKey(issued.Key)
		_, err = pub.Publish(ctx, s)
		Expect(err).NotTo(HaveOccurred())
		var text string
		Expect(pool.QueryRow(ctx, "SELECT snapshot::text FROM gateway_state_revisions").Scan(&text)).To(Succeed())
		Expect(text).NotTo(ContainSubstring(issued.Material))
		Expect(text).To(ContainSubstring(issued.Hash))
	})
})

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
