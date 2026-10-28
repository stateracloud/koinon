// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/durable"
	"github.com/stateracloud/koinon/internal/services"
	"github.com/stateracloud/koinon/pricing"
)

var _ = Describe("CreateTriggeredTask (dedupe, plan item DB3)", func() {
	It("has no second effect for a replayed delivery or a duplicate fire", func(ctx SpecContext) {
		e := newEnv(ctx, envOptions{})
		org, owner := e.newTeamOrg(ctx)
		agent := owner.activeAgent(ctx, org)
		var orgID, agentID pgtype.UUID
		Expect(orgID.Scan(org)).To(Succeed())
		Expect(agentID.Scan(agent.GetId())).To(Succeed())

		notifier := durable.NewNotifier(nil, nil)
		var wakes atomic.Int32
		notifier.Handle(services.NotifyWakeAgent, func(_ context.Context, _ durable.Notification) error { wakes.Add(1); return nil })
		svc := services.New(e.pool, services.Options{Prices: pricing.Embedded(), Hub: e.hub, Notifier: notifier})

		in := services.TriggeredTask{
			Org: orgID, Agent: agentID, Trigger: pgtype.UUID{Bytes: [16]byte{9}, Valid: true},
			Source: durable.SourceWebhook, Key: "delivery-42", Title: "review PR 7", Message: "PR 7 opened",
		}
		var wg sync.WaitGroup
		var created atomic.Int32
		for range 6 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer GinkgoRecover()
				task, made, err := svc.CreateTriggeredTask(ctx, in)
				Expect(err).NotTo(HaveOccurred())
				Expect(task.ID.Valid).To(BeTrue())
				if made {
					created.Add(1)
				}
			}()
		}
		wg.Wait()
		Expect(created.Load()).To(Equal(int32(1)))

		n := func(q string) int { return count(ctx, e, q, org) }
		Expect(n(`SELECT count(*) FROM tasks WHERE org_id = $1`)).To(Equal(1))
		Expect(n(`SELECT count(*) FROM task_events WHERE org_id = $1 AND kind = 'task.created'`)).To(Equal(1))
		Expect(n(`SELECT count(*) FROM runs WHERE org_id = $1`)).To(Equal(1))
		Expect(n(`SELECT count(*) FROM runs WHERE org_id = $1 AND origin = 'scheduled'`)).To(Equal(1))
		Expect(wakes.Load()).To(Equal(int32(1)), "only the first delivery pre-wakes the sandbox")

		// A different key is a different event.
		in.Key = "delivery-43"
		_, made, err := svc.CreateTriggeredTask(ctx, in)
		Expect(err).NotTo(HaveOccurred())
		Expect(made).To(BeTrue())
		Expect(n(`SELECT count(*) FROM tasks WHERE org_id = $1`)).To(Equal(2))
	})
})
