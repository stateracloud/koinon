// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"context"
	"sync"
	"time"

	sdk "github.com/coder/acp-go-sdk"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/budget"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/ledger"
	"github.com/stateracloud/koinon/internal/testing/racescale"
)

// A spent budget revokes the agent's key while the harness is mid-turn: the harness sees the
// gateway's 401 (or 429) before the ledger's cancel reaches agentd (old issue 188).
var _ = Describe("A gateway refusal on a spent budget", func() {
	// spend records spent USD for the agent now, which a daily limit of limit USD counts.
	spend := func(ctx context.Context, e *env, limit, spent string) {
		_, err := q.SetAgentBudget(ctx, store.SetAgentBudgetParams{OrgID: e.org, AgentID: e.agent.ID, DailyLimit: limit, UpdatedBy: "test"})
		Expect(err).NotTo(HaveOccurred())
		_, err = q.InsertSpendEntry(ctx, store.InsertSpendEntryParams{
			Principal: "agent",
			OrgID:     e.org, AgentID: e.agent.ID, RequestID: uuid.NewString(), KeyID: "k-test", Model: "m",
			CostUsd: spent, Priced: true, OccurredAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		})
		Expect(err).NotTo(HaveOccurred())
	}
	unauthorized := &sdk.RequestError{Code: -32000, Message: "Authentication required", Data: map[string]any{"details": "provider authentication required"}}

	DescribeTable("ends the run as a budget stop, recorded like the ledger's cancel",
		func(ctx SpecContext, refusal error) {
			e := newEnv(ctx, harness.OpenCode)
			spend(ctx, e, "2.40", "2.4038787")
			e.harness.modelFault = func(context.Context, string) error { return refusal }
			t := e.task(ctx, "hello")
			r := e.enqueue(ctx, t)
			e.pool("agentd-a")
			Eventually(runState(ctx, r)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateCanceled))

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
		e := newEnv(ctx, harness.OpenCode)
		spend(ctx, e, "100", "3")
		_, err := q.SetOrgMonthlyCap(ctx, store.SetOrgMonthlyCapParams{OrgID: e.org, MonthlyCap: pgtype.Text{String: "3", Valid: true}, UpdatedBy: "test"})
		Expect(err).NotTo(HaveOccurred())
		e.harness.modelFault = func(context.Context, string) error { return unauthorized }
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateCanceled))
		Expect(payload(eventOf(events(ctx, r), "run.canceled")[0])).To(HaveKeyWithValue("limit", budget.OrgMonthly))
	})

	It("names the widest limit when several are reached, as the ledger does", func(ctx SpecContext) {
		e := newEnv(ctx, harness.OpenCode)
		spend(ctx, e, "1", "3")
		_, err := q.SetOrgMonthlyCap(ctx, store.SetOrgMonthlyCapParams{OrgID: e.org, MonthlyCap: pgtype.Text{String: "3", Valid: true}, UpdatedBy: "test"})
		Expect(err).NotTo(HaveOccurred())
		e.harness.modelFault = func(context.Context, string) error { return unauthorized }
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateCanceled))
		Expect(payload(eventOf(events(ctx, r), "run.canceled")[0])).To(HaveKeyWithValue("limit", budget.OrgMonthly))
	})

	It("names the run's spend limit when only the run level is reached (ADR 0081)", func(ctx SpecContext) {
		e := newEnv(ctx, harness.OpenCode)
		_, err := q.SetAgentBudget(ctx, store.SetAgentBudgetParams{OrgID: e.org, AgentID: e.agent.ID, DailyLimit: "100", UpdatedBy: "test"})
		Expect(err).NotTo(HaveOccurred())
		e.harness.modelFault = func(context.Context, string) error { return unauthorized }
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		_, err = db.Exec(ctx, `UPDATE runs SET max_spend_usd = 0.50 WHERE org_id = $1 AND id = $2`, e.org, r.ID)
		Expect(err).NotTo(HaveOccurred())
		_, err = q.InsertSpendEntry(ctx, store.InsertSpendEntryParams{
			Principal: "agent", OrgID: e.org, AgentID: e.agent.ID, RunID: r.ID, TaskID: r.TaskID, RequestID: uuid.NewString(), KeyID: "k-test",
			Model: "m", CostUsd: "0.60", Priced: true, OccurredAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		})
		Expect(err).NotTo(HaveOccurred())
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateCanceled))

		got := getRun(ctx, r)
		Expect(got.FailureClass.RunFailureClass).To(Equal(store.RunFailureClassBudget))
		Expect(got.OutcomeReason.String).To(HavePrefix("run spend limit reached ($0.60"))
		p := payload(eventOf(events(ctx, r), "run.canceled")[0])
		Expect(p).To(HaveKeyWithValue("limit", budget.RunSpend))
		Expect(p).To(HaveKeyWithValue("reason", "run_spend_cap"))
	})

	It("leaves the run to the control plane's cancel when that came first", func(ctx SpecContext) {
		e := newEnv(ctx, harness.OpenCode)
		spend(ctx, e, "2.40", "2.5")
		// The reconciler stops the agent's runs before it revokes the key; the harness's 401 follows.
		// The harness holds its first model request until the spec has stopped the run, so the
		// order is the spec's and nothing asserts on the ACP SDK's goroutine. The stop is the
		// reconciler's own (ledger.StopSpent): the cancel and its events commit together.
		requested, stopped := make(chan struct{}), make(chan struct{})
		var once sync.Once
		e.harness.modelFault = func(ctx context.Context, _ string) error {
			once.Do(func() { close(requested) })
			select {
			case <-stopped:
			case <-ctx.Done():
			}
			return unauthorized
		}
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(requested).WithTimeout(racescale.D(15 * time.Second)).Should(BeClosed())
		n, err := ledger.StopSpent(ctx, db, e.agent, time.Now())
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(1))
		close(stopped)
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateCanceled))
		Consistently(func() int { return len(eventOf(events(ctx, r), "run.canceled")) }, 500*time.Millisecond).Should(Equal(1))
		got := getRun(ctx, r)
		Expect(got.FailureClass.RunFailureClass).To(Equal(store.RunFailureClassBudget))
		Expect(eventOf(events(ctx, r), "run.failed")).To(BeEmpty())
	})

	It("keeps an authentication error as a failure when no limit is reached", func(ctx SpecContext) {
		e := newEnv(ctx, harness.OpenCode)
		spend(ctx, e, "2.40", "0.5")
		e.harness.modelFault = func(context.Context, string) error { return unauthorized }
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateFailed))
		got := getRun(ctx, r)
		Expect(got.FailureClass.RunFailureClass).To(Equal(store.RunFailureClassFatal))
		Expect(got.OutcomeReason.String).To(ContainSubstring("Authentication required"))
	})

	It("does not turn an error that is not the gateway's into a budget stop", func(ctx SpecContext) {
		e := newEnv(ctx, harness.OpenCode)
		spend(ctx, e, "2.40", "5")
		e.harness.modelFault = func(context.Context, string) error { return &sdk.RequestError{Code: -32603, Message: "boom"} }
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateFailed))
		Expect(getRun(ctx, r).FailureClass.RunFailureClass).To(Equal(store.RunFailureClassFatal))
	})
})
