// SPDX-License-Identifier: Apache-2.0

package durable_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/durable"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

// newOrgWithAgent creates an org and an agent in it; a task needs an agent.
func newOrgWithAgent(ctx context.Context) (org, agent pgtype.UUID) {
	GinkgoHelper()
	org, _, _ = pgtest.NewOrg(ctx, pool)
	Expect(pool.QueryRow(ctx, `INSERT INTO agents (org_id, display_name, harness) VALUES ($1, 'helper', 'opencode') RETURNING id`, org).Scan(&agent)).To(Succeed())
	return org, agent
}

var _ = Describe("Once", func() {
	var org, agent pgtype.UUID

	BeforeEach(func(ctx SpecContext) {
		org, agent = newOrgWithAgent(ctx)
	})

	// effect inserts a task and returns it as the effect; tasks counts the rows.
	insertTask := func(ctx context.Context, q *store.Queries) (string, pgtype.UUID, error) {
		t, err := q.CreateTask(ctx, store.CreateTaskParams{
			OrgID: org, Title: "t", AgentID: agent, CreatedByKind: store.TaskCreatorKindTrigger,
			CreatedByTriggerID: pgtype.UUID{Bytes: [16]byte{1}, Valid: true},
		})
		return "task", t.ID, err
	}
	count := func(ctx context.Context) int {
		var n int
		Expect(pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE org_id = $1`, org).Scan(&n)).To(Succeed())
		return n
	}
	once := func(ctx context.Context, source, key string) (durable.Outcome, error) {
		var out durable.Outcome
		err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			var err error
			out, err = durable.Once(ctx, store.New(tx), org, source, key, func(q *store.Queries) (string, pgtype.UUID, error) {
				return insertTask(ctx, q)
			})
			return err
		})
		return out, err
	}

	It("has no second effect for a replayed key, and returns the first effect", func(ctx SpecContext) {
		key := durable.TriggerFireKey("trg-1", time.Unix(1_800_000_000, 0))
		first, err := once(ctx, durable.SourceTrigger, key)
		Expect(err).NotTo(HaveOccurred())
		Expect(first.First).To(BeTrue())
		Expect(first.EffectKind).To(Equal("task"))

		again, err := once(ctx, durable.SourceTrigger, key)
		Expect(err).NotTo(HaveOccurred())
		Expect(again.First).To(BeFalse())
		Expect(again.EffectID).To(Equal(first.EffectID))
		Expect(count(ctx)).To(Equal(1))
	})

	It("scopes a key to its source and its org", func(ctx SpecContext) {
		_, err := once(ctx, durable.SourceWebhook, "delivery-1")
		Expect(err).NotTo(HaveOccurred())
		other, err := once(ctx, durable.SourceSlack, "delivery-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(other.First).To(BeTrue(), "the same key from another source is another event")

		otherOrg, _, _ := pgtest.NewOrg(ctx, pool)
		var out durable.Outcome
		Expect(pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			var err error
			out, err = durable.Once(ctx, store.New(tx), otherOrg, durable.SourceWebhook, "delivery-1",
				func(*store.Queries) (string, pgtype.UUID, error) { return "", pgtype.UUID{}, nil })
			return err
		})).To(Succeed())
		Expect(out.First).To(BeTrue(), "the same key in another org is another event")
	})

	It("lets exactly one of many concurrent deliveries through", func(ctx SpecContext) {
		var wg sync.WaitGroup
		var firsts atomic.Int32
		for range 12 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer GinkgoRecover()
				out, err := once(ctx, durable.SourceWebhook, "delivery-race")
				Expect(err).NotTo(HaveOccurred())
				if out.First {
					firsts.Add(1)
				}
			}()
		}
		wg.Wait()
		Expect(firsts.Load()).To(Equal(int32(1)))
		Expect(count(ctx)).To(Equal(1))
	})

	It("leaves the key free when the effect fails, so the redelivery runs it", func(ctx SpecContext) {
		boom := errors.New("boom")
		err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			_, err := durable.Once(ctx, store.New(tx), org, durable.SourceSlack, "ev-1",
				func(q *store.Queries) (string, pgtype.UUID, error) {
					_, _, _ = insertTask(ctx, q)
					return "", pgtype.UUID{}, boom
				})
			return err
		})
		Expect(err).To(MatchError(boom))
		Expect(count(ctx)).To(BeZero())

		out, err := once(ctx, durable.SourceSlack, "ev-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(out.First).To(BeTrue())
		Expect(count(ctx)).To(Equal(1))
	})

	It("rejects an empty key", func(ctx SpecContext) {
		_, err := once(ctx, durable.SourceWebhook, "")
		Expect(err).To(HaveOccurred())
	})
})

func uuidText(id pgtype.UUID) string {
	v, _ := id.Value()
	return v.(string)
}
