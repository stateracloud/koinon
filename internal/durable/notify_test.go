// SPDX-License-Identifier: Apache-2.0

package durable_test

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/durable"
)

var _ = Describe("Notifier", func() {
	launch := func(ctx context.Context, n func(rt *durable.Runtime) *durable.Notifier) *durable.Notifier {
		dsn := newDatabase(ctx)
		Expect(durable.Migrate(ctx, dsn, nil)).To(Succeed())
		rt := durable.New(durable.Config{DatabaseURL: dsn, ApplicationVersion: "v1"})
		notifier := n(rt)
		Expect(rt.Register(notifier.Registrar())).To(Succeed())
		Expect(rt.Launch(ctx)).To(Succeed())
		DeferCleanup(func() { _ = rt.Shutdown(5 * time.Second) })
		return notifier
	}

	It("notifies exactly once when the first attempts fail after the commit", func(ctx SpecContext) {
		org, agent := newOrgWithAgent(ctx)
		var attempts, delivered atomic.Int32
		var sawCommitted atomic.Bool
		notifier := launch(ctx, func(rt *durable.Runtime) *durable.Notifier {
			n := durable.NewNotifier(rt, nil)
			n.Handle("test.wake", func(ctx context.Context, note durable.Notification) error {
				// The state change is committed before any attempt runs.
				var c int
				_ = pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE org_id = $1`, org).Scan(&c)
				sawCommitted.Store(c == 1)
				if attempts.Add(1) <= 2 {
					return errors.New("connector down")
				}
				delivered.Add(1)
				return nil
			})
			return n
		})

		err := durable.Commit(ctx, pool, notifier, func(q *store.Queries, _ audit.Batch) ([]durable.Notification, error) {
			_, err := q.CreateTask(ctx, store.CreateTaskParams{
				OrgID: org, Title: "t", AgentID: agent, CreatedByKind: store.TaskCreatorKindTrigger,
				CreatedByTriggerID: pgtype.UUID{Bytes: [16]byte{2}, Valid: true},
			})
			return []durable.Notification{{ID: "test.wake:" + uuidText(org), Kind: "test.wake", Payload: "{}"}}, err
		})
		Expect(err).NotTo(HaveOccurred())

		Eventually(delivered.Load, 30*time.Second, 100*time.Millisecond).Should(Equal(int32(1)))
		Expect(sawCommitted.Load()).To(BeTrue())

		// The same effect sent again (a replayed delivery, a retried request) is the same workflow.
		Expect(notifier.Send(ctx, durable.Notification{ID: "test.wake:" + uuidText(org), Kind: "test.wake", Payload: "{}"})).To(Succeed())
		Consistently(delivered.Load, 2*time.Second, 200*time.Millisecond).Should(Equal(int32(1)))
		Expect(attempts.Load()).To(Equal(int32(3)), "two failures, one success, no repeat")
	}, SpecTimeout(2*time.Minute))

	It("sends nothing when the transaction rolls back", func(ctx SpecContext) {
		org, agent := newOrgWithAgent(ctx)
		var delivered atomic.Int32
		notifier := durable.NewNotifier(nil, nil)
		notifier.Handle("test.wake", func(context.Context, durable.Notification) error { delivered.Add(1); return nil })

		boom := errors.New("boom")
		err := durable.Commit(ctx, pool, notifier, func(q *store.Queries, _ audit.Batch) ([]durable.Notification, error) {
			_, err := q.CreateTask(ctx, store.CreateTaskParams{
				OrgID: org, Title: "t", AgentID: agent, CreatedByKind: store.TaskCreatorKindTrigger,
				CreatedByTriggerID: pgtype.UUID{Bytes: [16]byte{3}, Valid: true},
			})
			Expect(err).NotTo(HaveOccurred())
			return []durable.Notification{{ID: "x", Kind: "test.wake"}}, boom
		})
		Expect(err).To(MatchError(boom))
		Expect(delivered.Load()).To(BeZero())
		var n int
		Expect(pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE org_id = $1`, org).Scan(&n)).To(Succeed())
		Expect(n).To(BeZero())
	})

	It("reports a failed send after the commit as a SendError and keeps the change", func(ctx SpecContext) {
		org, agent := newOrgWithAgent(ctx)
		notifier := durable.NewNotifier(nil, nil) // inline, and no handler for the kind
		err := durable.Commit(ctx, pool, notifier, func(q *store.Queries, _ audit.Batch) ([]durable.Notification, error) {
			_, err := q.CreateTask(ctx, store.CreateTaskParams{
				OrgID: org, Title: "t", AgentID: agent, CreatedByKind: store.TaskCreatorKindTrigger,
				CreatedByTriggerID: pgtype.UUID{Bytes: [16]byte{4}, Valid: true},
			})
			return []durable.Notification{{ID: "x", Kind: "unknown"}}, err
		})
		var se *durable.SendError
		Expect(errors.As(err, &se)).To(BeTrue())
		var n int
		Expect(pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE org_id = $1`, org).Scan(&n)).To(Succeed())
		Expect(n).To(Equal(1))
	})
})
