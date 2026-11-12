// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/budget"
	"github.com/stateracloud/koinon/internal/db/store"
	ticketv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/ticket/v1alpha1"
	"github.com/stateracloud/koinon/internal/money"
	"github.com/stateracloud/koinon/internal/orgsettings"
	"github.com/stateracloud/koinon/internal/runlimits"
	"github.com/stateracloud/koinon/internal/scheduler"
)

// Continuation runs after a time cap, and the per-task USD cap (issue #99).
var _ = Describe("continuation runs", func() {
	var (
		e      *env
		org    string
		owner  *client
		member *client
		agentI string
		ad     *agentd
		q      *store.Queries
	)
	BeforeEach(func(ctx SpecContext) {
		e = newEnv(ctx, envOptions{})
		org, owner = e.newTeamOrg(ctx)
		agentI = owner.activeAgent(ctx, org).GetId()
		member = e.userIn(ctx, org, store.OrgRoleMember)
		ad = e.agentd("agentd-a")
		q = store.New(e.pool)
	})

	// timeCap makes the claimed run pass its wall-clock cap and sweeps it.
	timeCap := func(ctx context.Context, run store.Run) {
		GinkgoHelper()
		_, err := e.pool.Exec(ctx, `UPDATE runs SET started_at = now() - interval '31 minutes' WHERE id = $1`, run.ID)
		Expect(err).NotTo(HaveOccurred())
		n, err := sweep(ctx, e)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(1))
	}
	spend := func(ctx context.Context, run store.Run, usd string) {
		GinkgoHelper()
		_, err := e.pool.Exec(ctx, `INSERT INTO spend_entries (org_id, agent_id, run_id, task_id, request_id,
			key_id, model, input_tokens, output_tokens, cost_usd, priced, occurred_at)
			VALUES ($1, $2, $3, $4, $5, 'k-1', 'm', 100, 20, $6::numeric, true, now())`,
			run.OrgID, run.AgentID, run.ID, run.TaskID, fmt.Sprintf("req-%s-%s", run.ID.String(), usd), usd)
		Expect(err).NotTo(HaveOccurred())
	}
	task := func(ctx context.Context, run store.Run) store.Task {
		GinkgoHelper()
		t, err := q.GetTask(ctx, store.GetTaskParams{OrgID: run.OrgID, ID: run.TaskID})
		Expect(err).NotTo(HaveOccurred())
		return t
	}
	queued := func(ctx context.Context, run store.Run) bool {
		GinkgoHelper()
		ok, err := q.HasQueuedRunForTask(ctx, store.HasQueuedRunForTaskParams{OrgID: run.OrgID, TaskID: run.TaskID})
		Expect(err).NotTo(HaveOccurred())
		return ok
	}
	statusChanged := func(ctx context.Context, run store.Run) map[string]any {
		GinkgoHelper()
		var raw []byte
		Expect(e.pool.QueryRow(ctx, `SELECT payload FROM task_events WHERE task_id = $1 AND kind = 'status.changed'
			ORDER BY seq DESC LIMIT 1`, run.TaskID).Scan(&raw)).To(Succeed())
		return payloadMap(raw)
	}
	orgID := func() pgtype.UUID { return uuid(org) }

	It("queues a continuation run of the same task when a run hits its time cap", func(ctx SpecContext) {
		member.createTicket(ctx, org, agentI, "long job")
		first := ad.mustClaim(ctx, org)
		timeCap(ctx, first)

		Expect(queued(ctx, first)).To(BeTrue())
		t := task(ctx, first)
		Expect(t.ContinuationCount).To(Equal(int32(1)))
		Expect(t.State).NotTo(Equal(store.TaskStateInputRequired))
		var msg []byte
		Expect(e.pool.QueryRow(ctx, `SELECT payload FROM task_events WHERE task_id = $1 AND kind = 'message'
			AND payload ->> 'source' = 'continuation'`, first.TaskID).Scan(&msg)).To(Succeed())
		Expect(payloadMap(msg)).To(HaveKeyWithValue("continuation", BeNumerically("==", 1)))

		next := ad.mustClaim(ctx, org)
		Expect(next.ID).NotTo(Equal(first.ID))
		Expect(next.TaskID).To(Equal(first.TaskID))
	})

	It("stops after 3 continuations and blocks the task on its owner", func(ctx SpecContext) {
		member.createTicket(ctx, org, agentI, "endless job")
		run := ad.mustClaim(ctx, org)
		for i := 1; i <= 3; i++ {
			timeCap(ctx, run)
			Expect(task(ctx, run).ContinuationCount).To(Equal(int32(i)))
			run = ad.mustClaim(ctx, org)
		}
		timeCap(ctx, run)

		Expect(queued(ctx, run)).To(BeFalse())
		t := task(ctx, run)
		Expect(t.ContinuationCount).To(Equal(int32(3)))
		Expect(t.State).To(Equal(store.TaskStateInputRequired))
		Expect(statusChanged(ctx, run)).To(And(
			HaveKeyWithValue("to", "input_required"),
			HaveKeyWithValue("reason", scheduler.ReasonContinuationLimit),
		))
	})

	Describe("the per-task USD cap", func() {
		It("uses the org's default: no continuation once the task has spent it", func(ctx SpecContext) {
			Expect(orgsettings.SetContinuationTaskSpend(ctx, q, orgID(), money.FromCents(100))).To(Succeed())
			member.createTicket(ctx, org, agentI, "costly job")
			run := ad.mustClaim(ctx, org)
			spend(ctx, run, "1.25")
			timeCap(ctx, run)

			Expect(queued(ctx, run)).To(BeFalse())
			Expect(task(ctx, run).ContinuationCount).To(BeZero())
			Expect(task(ctx, run).State).To(Equal(store.TaskStateInputRequired))
			Expect(statusChanged(ctx, run)).To(And(
				HaveKeyWithValue("reason", scheduler.ReasonTaskSpendCap),
				HaveKeyWithValue("task_spent_usd", "1.25"),
				HaveKeyWithValue("task_spend_cap_usd", "1.00"),
			))
		})

		It("sums the spend of every run of the task, continuations included", func(ctx SpecContext) {
			Expect(orgsettings.SetContinuationTaskSpend(ctx, q, orgID(), money.FromCents(100))).To(Succeed())
			member.createTicket(ctx, org, agentI, "two runs")
			run := ad.mustClaim(ctx, org)
			spend(ctx, run, "0.60")
			timeCap(ctx, run)
			Expect(queued(ctx, run)).To(BeTrue(), "0.60 of 1.00 is within the cap")
			run = ad.mustClaim(ctx, org)
			spend(ctx, run, "0.60")
			timeCap(ctx, run)
			Expect(queued(ctx, run)).To(BeFalse(), "1.20 of 1.00 is over it")
			Expect(statusChanged(ctx, run)).To(HaveKeyWithValue("reason", scheduler.ReasonTaskSpendCap))
		})

		It("falls back to the platform default when the org sets none", func(ctx SpecContext) {
			member.createTicket(ctx, org, agentI, "default cap")
			run := ad.mustClaim(ctx, org)
			spend(ctx, run, "19.99")
			timeCap(ctx, run)
			Expect(queued(ctx, run)).To(BeTrue(), "below the $20.00 default")
		})

		It("lets the agent's own cap override the org's default, in both directions", func(ctx SpecContext) {
			Expect(orgsettings.SetContinuationTaskSpend(ctx, q, orgID(), money.FromCents(100))).To(Succeed())
			Expect(q.SetAgentContinuationLimits(ctx, store.SetAgentContinuationLimitsParams{
				OrgID: orgID(), AgentID: uuid(agentI), UpdatedBy: "test",
				TaskSpendUsd: pgtype.Text{String: money.FromCents(1000).Numeric(), Valid: true},
			})).To(Succeed())
			member.createTicket(ctx, org, agentI, "agent cap higher")
			run := ad.mustClaim(ctx, org)
			spend(ctx, run, "1.50")
			timeCap(ctx, run)
			Expect(queued(ctx, run)).To(BeTrue(), "the agent's $10.00 beats the org's $1.00")

			Expect(orgsettings.SetContinuationTaskSpend(ctx, q, orgID(), money.FromCents(5000))).To(Succeed())
			Expect(q.SetAgentContinuationLimits(ctx, store.SetAgentContinuationLimitsParams{
				OrgID: orgID(), AgentID: uuid(agentI), UpdatedBy: "test",
				TaskSpendUsd: pgtype.Text{String: money.FromCents(100).Numeric(), Valid: true},
			})).To(Succeed())
			run = ad.mustClaim(ctx, org)
			timeCap(ctx, run)
			Expect(queued(ctx, run)).To(BeFalse(), "the agent's $1.00 beats the org's $50.00")
			Expect(statusChanged(ctx, run)).To(HaveKeyWithValue("reason", scheduler.ReasonTaskSpendCap))
		})
	})

	Describe("the number of continuations per agent", func() {
		It("is the agent's override over the template's", func(ctx SpecContext) {
			two := pgtype.Int4{Int32: 2, Valid: true}
			Expect(q.SetAgentContinuationLimits(ctx, store.SetAgentContinuationLimitsParams{
				OrgID: orgID(), AgentID: uuid(agentI), UpdatedBy: "test", MaxContinuations: two,
			})).To(Succeed())
			member.createTicket(ctx, org, agentI, "two only")
			run := ad.mustClaim(ctx, org)
			for range 2 {
				timeCap(ctx, run)
				run = ad.mustClaim(ctx, org)
			}
			timeCap(ctx, run)
			Expect(queued(ctx, run)).To(BeFalse())
			Expect(task(ctx, run).ContinuationCount).To(Equal(int32(2)))
			Expect(statusChanged(ctx, run)).To(HaveKeyWithValue("reason", scheduler.ReasonContinuationLimit))
		})

		It("switches continuation off at 0 and leaves the task as a cancel leaves it", func(ctx SpecContext) {
			Expect(q.SetAgentContinuationLimits(ctx, store.SetAgentContinuationLimitsParams{
				OrgID: orgID(), AgentID: uuid(agentI), UpdatedBy: "test", MaxContinuations: pgtype.Int4{Valid: true},
			})).To(Succeed())
			member.createTicket(ctx, org, agentI, "no continuation")
			run := ad.mustClaim(ctx, org)
			timeCap(ctx, run)
			Expect(queued(ctx, run)).To(BeFalse())
			Expect(task(ctx, run).State).NotTo(Equal(store.TaskStateInputRequired))
		})
	})

	It("queues nothing when a follow-up message already queued the next run", func(ctx SpecContext) {
		t, _ := member.createTicket(ctx, org, agentI, "followed up")
		run := ad.mustClaim(ctx, org)
		_, err := e.pool.Exec(ctx, `UPDATE runs SET started_at = now() - interval '31 minutes' WHERE id = $1`, run.ID)
		Expect(err).NotTo(HaveOccurred())
		_, err = member.tickets.AppendMessage(ctx, &ticketv1alpha1.AppendMessageRequest{OrgId: org, TicketId: t.GetId(), Text: "also this"})
		Expect(err).NotTo(HaveOccurred())
		_, err = sweep(ctx, e)
		Expect(err).NotTo(HaveOccurred())
		Expect(task(ctx, run).ContinuationCount).To(BeZero())
	})
	It("blocks a task again at its next time cap after a person's message resumed it", func(ctx SpecContext) {
		t, _ := member.createTicket(ctx, org, agentI, "blocked then resumed")
		run := ad.mustClaim(ctx, org)
		for range 3 {
			timeCap(ctx, run)
			run = ad.mustClaim(ctx, org)
		}
		timeCap(ctx, run)
		Expect(task(ctx, run).State).To(Equal(store.TaskStateInputRequired))

		_, err := member.tickets.AppendMessage(ctx, &ticketv1alpha1.AppendMessageRequest{OrgId: org, TicketId: t.GetId(), Text: "keep going"})
		Expect(err).NotTo(HaveOccurred())
		run = ad.mustClaim(ctx, org)
		timeCap(ctx, run)

		Expect(queued(ctx, run)).To(BeFalse())
		var n int
		Expect(e.pool.QueryRow(ctx, `SELECT count(*) FROM task_events WHERE task_id = $1 AND kind = 'status.changed'
			AND payload ->> 'reason' = $2`, run.TaskID, scheduler.ReasonContinuationLimit).Scan(&n)).To(Succeed())
		Expect(n).To(Equal(2))
		Expect(statusChanged(ctx, run)).To(HaveKeyWithValue("from", "input_required"))
	})

	It("validates and audits the budget service's continuation setters", func(ctx SpecContext) {
		svc := budget.New(e.pool)
		actor := audit.System("koinon-test:spec")
		four := 4
		Expect(svc.SetAgentContinuationLimits(ctx, orgID(), uuid(agentI), actor, runlimits.ContinuationOverride{MaxContinuations: &four, TaskSpend: money.FromCents(500)})).To(Succeed())
		got, err := runlimits.GetContinuationOverride(ctx, q, orgID(), uuid(agentI))
		Expect(err).NotTo(HaveOccurred())
		Expect(*got.MaxContinuations).To(Equal(4))
		Expect(got.TaskSpend).To(Equal(money.FromCents(500)))

		tooMany := budget.MaxContinuationsLimit + 1
		Expect(svc.SetAgentContinuationLimits(ctx, orgID(), uuid(agentI), actor, runlimits.ContinuationOverride{MaxContinuations: &tooMany})).To(BeAssignableToTypeOf(&budget.InvalidError{}))
		Expect(svc.SetAgentContinuationLimits(ctx, orgID(), uuid(agentI), actor, runlimits.ContinuationOverride{TaskSpend: -1})).To(BeAssignableToTypeOf(&budget.InvalidError{}))
		Expect(svc.SetOrgContinuationTaskSpend(ctx, orgID(), actor, -1)).To(BeAssignableToTypeOf(&budget.InvalidError{}))

		Expect(svc.SetOrgContinuationTaskSpend(ctx, orgID(), actor, money.FromCents(700))).To(Succeed())
		s, err := orgsettings.Get(ctx, q, orgID(), orgsettings.Defaults())
		Expect(err).NotTo(HaveOccurred())
		Expect(s.ContinuationTaskSpend).To(Equal(money.FromCents(700)))
		Expect(svc.SetOrgContinuationTaskSpend(ctx, orgID(), actor, 0)).To(Succeed())
		s, err = orgsettings.Get(ctx, q, orgID(), orgsettings.Defaults())
		Expect(err).NotTo(HaveOccurred())
		Expect(s.ContinuationTaskSpend).To(Equal(orgsettings.DefaultContinuationTaskSpend))
	})
})

func payloadMap(raw []byte) map[string]any {
	GinkgoHelper()
	var m map[string]any
	Expect(json.Unmarshal(raw, &m)).To(Succeed())
	return m
}
