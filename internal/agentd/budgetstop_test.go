// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"context"
	"time"

	sdk "github.com/coder/acp-go-sdk"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/budget"
	"github.com/stateracloud/koinon/internal/db/store"
)

// A spent budget revokes the agent's key while the harness is mid-turn: the harness sees the
// gateway's 401 (or 429) before the ledger's cancel reaches agentd (issue #188).
var _ = Describe("A gateway refusal on a spent budget", func() {
	// spend records spent USD for the agent now, which a daily limit of limit USD counts.
	spend := func(ctx context.Context, e *env, limit, spent string) {
		_, err := q.SetAgentBudget(ctx, store.SetAgentBudgetParams{OrgID: e.org, AgentID: e.agent.ID, DailyLimit: limit, UpdatedBy: "test"})
		Expect(err).NotTo(HaveOccurred())
		_, err = q.InsertSpendEntry(ctx, store.InsertSpendEntryParams{
			OrgID: e.org, AgentID: e.agent.ID, RequestID: uuid.NewString(), KeyID: "k-test", Model: "m",
			CostUsd: spent, Priced: true, OccurredAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		})
		Expect(err).NotTo(HaveOccurred())
	}
	unauthorized := &sdk.RequestError{Code: -32000, Message: "Authentication required", Data: map[string]any{"details": "provider authentication required"}}

	DescribeTable("ends the run as a budget stop, recorded like the ledger's cancel",
		func(ctx SpecContext, refusal error) {
			e := newEnv(ctx, store.AgentFlavorOpencode)
			spend(ctx, e, "2.40", "2.4038787")
			e.harness.modelFault = func(context.Context, string) error { return refusal }
			t := e.task(ctx, "hello")
			r := e.enqueue(ctx, t)
			e.pool("agentd-a")
			Eventually(runState(ctx, r)).WithTimeout(15 * time.Second).Should(Equal(store.RunStateCanceled))

			got := getRun(ctx, r)
			Expect(got.FailureClass.RunFailureClass).To(Equal(store.RunFailureClassBudget))
			Expect(got.OutcomeReason.String).To(HavePrefix("agent daily limit reached ($2.4038787 of $2.4"))
			Expect(got.OutcomeReason.String).NotTo(ContainSubstring("authentication"))
			for _, ev := range []map[string]any{
				payload(eventOf(events(ctx, r), "run.canceled")[0]),
				func() map[string]any {
					evs := taskEvents(ctx, e, t)
					return payload(store.RunEvent{Payload: evs[len(evs)-1].Payload})
				}(),
			} {
				Expect(ev).To(HaveKeyWithValue("failureClass", "budget"))
				Expect(ev).To(HaveKeyWithValue("limit", budget.AgentDaily))
				Expect(ev).To(HaveKeyWithValue("reason", "spend_limit"))
				Expect(ev).To(HaveKey("message"))
				Expect(ev).To(HaveKey("spent_usd"))
			}
		},
		Entry("a 401 with its status", &sdk.RequestError{Code: -32000, Message: "Authentication required", Data: map[string]any{"statusCode": 401}}),
		Entry("OpenCode's 401 without a status", unauthorized),
		Entry("a 429", &sdk.RequestError{Code: -32603, Message: "limit", Data: map[string]any{"statusCode": 429}}),
	)

	It("stops on the org's monthly cap too", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		spend(ctx, e, "100", "3")
		_, err := q.SetOrgMonthlyCap(ctx, store.SetOrgMonthlyCapParams{OrgID: e.org, MonthlyCap: pgtype.Text{String: "3", Valid: true}, UpdatedBy: "test"})
		Expect(err).NotTo(HaveOccurred())
		e.harness.modelFault = func(context.Context, string) error { return unauthorized }
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(15 * time.Second).Should(Equal(store.RunStateCanceled))
		Expect(payload(eventOf(events(ctx, r), "run.canceled")[0])).To(HaveKeyWithValue("limit", budget.OrgMonthly))
	})

	It("keeps an authentication error as a failure when no limit is reached", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		spend(ctx, e, "2.40", "0.5")
		e.harness.modelFault = func(context.Context, string) error { return unauthorized }
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(15 * time.Second).Should(Equal(store.RunStateFailed))
		got := getRun(ctx, r)
		Expect(got.FailureClass.RunFailureClass).To(Equal(store.RunFailureClassFatal))
		Expect(got.OutcomeReason.String).To(ContainSubstring("Authentication required"))
	})

	It("does not turn an error that is not the gateway's into a budget stop", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		spend(ctx, e, "2.40", "5")
		e.harness.modelFault = func(context.Context, string) error { return &sdk.RequestError{Code: -32603, Message: "boom"} }
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(15 * time.Second).Should(Equal(store.RunStateFailed))
		Expect(getRun(ctx, r).FailureClass.RunFailureClass).To(Equal(store.RunFailureClassFatal))
	})
})
