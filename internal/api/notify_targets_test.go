// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"time"

	connect "connectrpc.com/connect/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	agentv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/agent/v1alpha1"
	authv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/auth/v1alpha1"
	questionv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/question/v1alpha1"
	taskv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/task/v1alpha1"
	templatev1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/template/v1alpha1"
	"github.com/stateracloud/koinon/internal/questions"
)

// Notify targets: a question to a role has several recipients, the first answer wins, and a trigger
// carries its target to the tasks it creates (old issue 182, ADR 0065 and 0066 amendments 2026-10-28).
var _ = Describe("notify targets", func() {
	var (
		e     *env
		f     orgFixture
		admin *client
	)
	BeforeEach(func(ctx SpecContext) {
		e = newEnv(ctx, envOptions{})
		f = e.newOrgFixture(ctx)
		admin = e.userIn(ctx, f.org, store.OrgRoleAdmin)
	})

	// openAdminsQuestion asks the org's admins and owners and parks the run, as agentd does.
	openAdminsQuestion := func(ctx SpecContext) store.Question {
		GinkgoHelper()
		var row store.Question
		Expect(pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
			qq := store.New(tx)
			recipients, err := questions.Recipients(ctx, qq, uuid(f.org), questions.DefaultTarget)
			if err != nil {
				return err
			}
			row, _, err = questions.Create(ctx, qq, questions.Ask{
				Org: uuid(f.org), Task: uuid(f.ticket.GetId()), Run: uuid(f.run.GetId()), Agent: uuid(f.agent.GetId()),
				Target: questions.DefaultTarget, Recipients: recipients, Question: "Ship the report?",
				Choices: []string{"Yes", "No"}, Default: "No", ExpiresAt: time.Now().Add(time.Hour),
			})
			if err != nil {
				return err
			}
			_, err = questions.OpenForRun(ctx, qq, uuid(f.org), uuid(f.run.GetId()), uuid(f.ticket.GetId()))
			return err
		})).To(Succeed())
		ad := e.agentd("agentd-notify")
		run := ad.mustClaim(ctx, f.org)
		_, err := ad.q.ParkRun(ctx, store.ParkRunParams{OrgID: run.OrgID, ID: run.ID, Epoch: run.Epoch, ParkReason: store.RunParkReasonInputRequired})
		Expect(err).NotTo(HaveOccurred())
		return row
	}

	answer := func(ctx SpecContext, c *client, id string) error {
		_, err := c.questions.AnswerQuestion(ctx, &questionv1alpha1.AnswerQuestionRequest{
			OrgId: f.org, QuestionId: id, Answer: &questionv1alpha1.AnswerQuestionRequest_Choice{Choice: "Yes"},
		})
		return err
	}

	Describe("a question to the org's admins", func() {
		It("shows every recipient the label, the recipients and its own waiting time", func(ctx SpecContext) {
			q := openAdminsQuestion(ctx)
			for _, c := range []*client{f.owner, admin} {
				res, err := c.questions.ListQuestions(ctx, &questionv1alpha1.ListQuestionsRequest{OrgId: f.org})
				Expect(err).NotTo(HaveOccurred())
				Expect(res.GetQuestions()).To(HaveLen(1))
				got := res.GetQuestions()[0]
				Expect(got.GetId()).To(Equal(q.ID.String()))
				Expect(got.GetTargetLabel()).To(Equal("the org's admins"))
				Expect(got.GetTarget().GetKind()).To(Equal(templatev1alpha1.NotifyTargetKind_NOTIFY_TARGET_KIND_ROLE))
				Expect(got.GetTarget().GetRole()).To(Equal(authv1alpha1.OrgRole_ORG_ROLE_ADMIN))
				Expect(got.GetRecipients()).To(HaveLen(2))
				// The deprecated field is the lowest recipient ID, the same for every viewer.
				Expect(got.GetAskedTo()) //nolint:staticcheck // the deprecated field is still served.To(Equal(min(f.owner.userID, admin.userID)))
				Expect(got.GetWaitingSince()).NotTo(BeNil())
				Expect(got.GetCanAnswer()).To(BeTrue())
			}
			member := e.userIn(ctx, f.org, store.OrgRoleMember)
			res, err := member.questions.ListQuestions(ctx, &questionv1alpha1.ListQuestionsRequest{OrgId: f.org})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.GetQuestions()).To(BeEmpty(), "a member who is not a recipient does not see it")
		})

		It("tells the second recipient who answered first", func(ctx SpecContext) {
			q := openAdminsQuestion(ctx)
			Expect(answer(ctx, admin, q.ID.String())).To(Succeed())
			err := answer(ctx, f.owner, q.ID.String())
			Expect(codeOf(err)).To(Equal(connect.CodeAborted))
			Expect(err.Error()).To(ContainSubstring("already answered this question"))
			var name string
			Expect(e.pool.QueryRow(ctx, `SELECT display_name FROM users WHERE id = $1`, admin.userID).Scan(&name)).To(Succeed())
			Expect(err.Error()).To(ContainSubstring(name))
		})

		It("sets waiting_on_user_ids and waiting_on_name on the task", func(ctx SpecContext) {
			openAdminsQuestion(ctx)
			// agentd moves the task to input-required in the transaction that parks the run.
			_, err := e.pool.Exec(ctx, `UPDATE tasks SET state = 'input_required' WHERE id = $1`, f.ticket.GetId())
			Expect(err).NotTo(HaveOccurred())
			res, err := f.owner.tasks.ListTasks(ctx, &taskv1alpha1.ListTasksRequest{OrgId: f.org})
			Expect(err).NotTo(HaveOccurred())
			var task *taskv1alpha1.Task
			for _, t := range res.GetTasks() {
				if t.GetId() == f.ticket.GetId() {
					task = t
				}
			}
			Expect(task).NotTo(BeNil())
			Expect(task.GetWaitingOnUserIds()).To(ConsistOf(f.owner.userID, admin.userID))
			Expect(task.GetWaitingOnUserId()) //nolint:staticcheck // the deprecated field is still served.To(Equal(task.GetWaitingOnUserIds()[0]))
			Expect(task.GetWaitingOnName()).To(Equal("the org's admins"))
		})
	})

	Describe("ListAskableMembers", func() {
		It("lists members, admins and owners to admins, and to no one below", func(ctx SpecContext) {
			viewer := e.userIn(ctx, f.org, store.OrgRoleViewer)
			member := e.userIn(ctx, f.org, store.OrgRoleMember)
			_, err := member.questions.ListAskableMembers(ctx, &questionv1alpha1.ListAskableMembersRequest{OrgId: f.org})
			Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied), "the directory is for admins")
			res, err := admin.questions.ListAskableMembers(ctx, &questionv1alpha1.ListAskableMembersRequest{OrgId: f.org})
			Expect(err).NotTo(HaveOccurred())
			var ids []string
			for _, m := range res.GetMembers() {
				ids = append(ids, m.GetUserId())
			}
			Expect(ids).To(ConsistOf(f.owner.userID, admin.userID, member.userID))
			_, err = viewer.questions.ListAskableMembers(ctx, &questionv1alpha1.ListAskableMembersRequest{OrgId: f.org})
			Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
		})
	})

	Describe("a trigger's notify target", func() {
		var agent *agentv1alpha1.Agent
		withNotify := func(n *templatev1alpha1.NotifyTarget) *templatev1alpha1.Trigger {
			return &templatev1alpha1.Trigger{
				Id: "daily-check", Type: templatev1alpha1.TriggerType_TRIGGER_TYPE_SCHEDULE,
				Cron: "0 9 * * 1-5", Timezone: "UTC", Instruction: "Check costs.", Notify: n,
			}
		}
		set := func(ctx SpecContext, t *templatev1alpha1.Trigger) (*agentv1alpha1.Agent, error) {
			res, err := f.owner.agents.SetAgentTriggers(ctx, &agentv1alpha1.SetAgentTriggersRequest{
				OrgId: f.org, AgentId: agent.GetId(), BaseVersion: agent.GetLatestVersion(), Triggers: []*templatev1alpha1.Trigger{t},
			})
			return res.GetAgent(), err
		}
		BeforeEach(func(ctx SpecContext) {
			agent = f.owner.activeAgent(ctx, f.org)
		})

		It("refuses a person who is a viewer or not a member, and keeps one who can answer", func(ctx SpecContext) {
			viewer := e.userIn(ctx, f.org, store.OrgRoleViewer)
			_, err := set(ctx, withNotify(&templatev1alpha1.NotifyTarget{Kind: templatev1alpha1.NotifyTargetKind_NOTIFY_TARGET_KIND_USER, UserId: viewer.userID}))
			Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
			Expect(err.Error()).To(ContainSubstring("is not a member who can answer questions"))

			member := e.userIn(ctx, f.org, store.OrgRoleMember)
			got, err := set(ctx, withNotify(&templatev1alpha1.NotifyTarget{Kind: templatev1alpha1.NotifyTargetKind_NOTIFY_TARGET_KIND_USER, UserId: member.userID}))
			Expect(err).NotTo(HaveOccurred())
			Expect(got.GetProposedTriggers()[0].GetNotify().GetUserId()).To(Equal(member.userID))
			again, err := f.owner.agents.GetAgent(ctx, &agentv1alpha1.GetAgentRequest{OrgId: f.org, AgentId: agent.GetId()})
			Expect(err).NotTo(HaveOccurred())
			Expect(again.GetAgent().GetProposedTriggers()[0].GetNotify().GetKind()).To(Equal(templatev1alpha1.NotifyTargetKind_NOTIFY_TARGET_KIND_USER))
		})

		It("refuses a chat channel until chat connectors exist", func(ctx SpecContext) {
			_, err := set(ctx, withNotify(&templatev1alpha1.NotifyTarget{Kind: templatev1alpha1.NotifyTargetKind_NOTIFY_TARGET_KIND_CHANNEL, ChannelId: "00000000-0000-7000-8000-00000000c001"}))
			Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
			Expect(err.Error()).To(ContainSubstring("available from v0.2"))
		})

		It("gives the task a run now creates the trigger's target", func(ctx SpecContext) {
			p, err := set(ctx, withNotify(&templatev1alpha1.NotifyTarget{Kind: templatev1alpha1.NotifyTargetKind_NOTIFY_TARGET_KIND_ROLE, Role: authv1alpha1.OrgRole_ORG_ROLE_OWNER}))
			Expect(err).NotTo(HaveOccurred())
			_, err = f.owner.agents.ActivateAgent(ctx, &agentv1alpha1.ActivateAgentRequest{OrgId: f.org, AgentId: agent.GetId(), Version: p.GetLatestVersion()})
			Expect(err).NotTo(HaveOccurred())
			res, err := f.owner.agents.RunAgentTrigger(ctx, &agentv1alpha1.RunAgentTriggerRequest{OrgId: f.org, AgentId: agent.GetId(), TriggerId: "daily-check"})
			Expect(err).NotTo(HaveOccurred())
			var kind, role string
			Expect(e.pool.QueryRow(ctx, `SELECT notify_kind::text, notify_role::text FROM tasks WHERE id = $1`, res.GetTaskId()).Scan(&kind, &role)).To(Succeed())
			Expect(kind).To(Equal("role"))
			Expect(role).To(Equal("owner"))
		})
	})

	It("clears the sandbox wait of the agent's queued runs when it is resumed", func(ctx SpecContext) {
		agent := f.owner.activeAgent(ctx, f.org)
		_, r := f.owner.createTicket(ctx, f.org, agent.GetId(), "waits for a sandbox")
		_, err := e.pool.Exec(ctx, `UPDATE runs SET sandbox_wait_started_at = now() - interval '2 hours' WHERE id = $1`, r.GetId())
		Expect(err).NotTo(HaveOccurred())
		_, err = e.pool.Exec(ctx, `UPDATE agents SET unhealthy_at = now(), unhealthy_reason = 'its sandbox never became ready' WHERE id = $1`, agent.GetId())
		Expect(err).NotTo(HaveOccurred())
		_, err = admin.agents.ResumeAgent(ctx, &agentv1alpha1.ResumeAgentRequest{OrgId: f.org, AgentId: agent.GetId()})
		Expect(err).NotTo(HaveOccurred())
		var started pgtype.Timestamptz
		Expect(e.pool.QueryRow(ctx, `SELECT sandbox_wait_started_at FROM runs WHERE id = $1`, r.GetId()).Scan(&started)).To(Succeed())
		Expect(started.Valid).To(BeFalse())
	})
})
