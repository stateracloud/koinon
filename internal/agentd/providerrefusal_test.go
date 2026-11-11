// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"context"
	"sync/atomic"
	"time"

	sdk "github.com/coder/acp-go-sdk"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/agentd"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/testing/racescale"
)

// OpenCode 2.2.0 words a provider's 403 as "Authentication required", with no status. The run is
// classified from the gateway's record in the spend ledger (ADR 0030, amendment 2026-10-29): the
// record's shape is what the real agentgateway logs, checked in the ledger's agentgateway spec.
var _ = Describe("Who refused the model request", func() {
	opencode := &sdk.RequestError{Code: -32000, Message: "Authentication required: provider authentication required"}

	// refuse makes the harness fail its prompt with opencode's error. The gateway's record of the
	// provider's refusal (status 0: none) reaches the ledger recordDelay after the request.
	refuse := func(ctx SpecContext, e *env, status int, recordDelay time.Duration) store.Run {
		var run atomic.Value // store.Run
		e.harness.modelFault = func(context.Context, string) error {
			if status != 0 {
				go func() {
					defer GinkgoRecover()
					time.Sleep(recordDelay)
					r := run.Load().(store.Run)
					_, err := q.InsertSpendEntry(context.Background(), store.InsertSpendEntryParams{
						Principal: "agent", OrgID: e.org, AgentID: e.agent.ID, RunID: r.ID, TaskID: r.TaskID, RequestID: uuid.NewString(), KeyID: "k-test", Model: "m",
						CostUsd: "0", Priced: true, OccurredAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
						UpstreamStatus: pgtype.Int2{Int16: int16(status), Valid: true},
					})
					Expect(err).NotTo(HaveOccurred())
				}()
			}
			return opencode
		}
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		run.Store(r)
		return r
	}

	It("fails a provider's 403 as policy, naming the provider's refusal, once the gateway's record arrives", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		r := refuse(ctx, e, 403, time.Second)
		e.pool("agentd-a", func(c *agentd.Config) { c.GatewayRecordWait = 10 * time.Second })
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(30 * time.Second)).Should(Equal(store.RunStateFailed))
		got := getRun(ctx, r)
		Expect(got.FailureClass.RunFailureClass).To(Equal(store.RunFailureClassPolicy))
		Expect(got.OutcomeReason.String).To(HavePrefix("The model provider refused the request (HTTP 403) [as reported by the harness: "))
		task, err := q.GetTask(ctx, store.GetTaskParams{OrgID: e.org, ID: r.TaskID})
		Expect(err).NotTo(HaveOccurred())
		Expect(task.State).To(Equal(store.TaskStateSubmitted), "a policy failure leaves the task open")
	})

	It("fails a provider's 401 as a provider-key fault", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		r := refuse(ctx, e, 401, 0)
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateFailed))
		got := getRun(ctx, r)
		Expect(got.FailureClass.RunFailureClass).To(Equal(store.RunFailureClassFatal))
		Expect(got.OutcomeReason.String).To(ContainSubstring("rejected the API key Koinon holds for it (HTTP 401)"))
		Expect(got.OutcomeReason.String).To(ContainSubstring("provider's key in the catalog"))
		task, err := q.GetTask(ctx, store.GetTaskParams{OrgID: e.org, ID: r.TaskID})
		Expect(err).NotTo(HaveOccurred())
		Expect(task.State).To(Equal(store.TaskStateFailed), "a fatal run fails its task (old issue 324)")
	})

	It("keeps a gateway-key fault when the gateway has no record of the provider refusing anything", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		r := refuse(ctx, e, 0, 0)
		start := time.Now()
		e.pool("agentd-a", func(c *agentd.Config) { c.GatewayRecordWait = time.Second })
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateFailed))
		got := getRun(ctx, r)
		Expect(got.FailureClass.RunFailureClass).To(Equal(store.RunFailureClassFatal))
		Expect(got.OutcomeReason.String).To(ContainSubstring("Authentication required: provider authentication required"))
		Expect(time.Since(start)).To(BeNumerically(">=", time.Second), "it waited for the record")
	})

	It("does not read another run's refusal", func(ctx SpecContext) {
		e := newEnv(ctx, store.AgentFlavorOpencode)
		other := e.enqueue(ctx, e.task(ctx, "other"))
		_, err := q.InsertSpendEntry(ctx, store.InsertSpendEntryParams{
			Principal: "agent", OrgID: e.org, AgentID: e.agent.ID, RunID: other.ID, TaskID: other.TaskID, RequestID: uuid.NewString(), KeyID: "k-test", Model: "m",
			CostUsd: "0", Priced: true, OccurredAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
			UpstreamStatus: pgtype.Int2{Int16: 403, Valid: true},
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = db.Exec(ctx, `UPDATE runs SET state = 'canceled', outcome_reason = 'spec', finished_at = now(), ready_at = NULL, lease_expires_at = NULL WHERE org_id = $1 AND id = $2`, e.org, other.ID)
		Expect(err).NotTo(HaveOccurred())
		r := refuse(ctx, e, 0, 0)
		e.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateFailed))
		Expect(getRun(ctx, r).FailureClass.RunFailureClass).To(Equal(store.RunFailureClassFatal))
	})
})
