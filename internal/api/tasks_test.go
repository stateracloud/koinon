// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	connect "connectrpc.com/connect/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	agentv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/agent/v1alpha1"
	runv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/run/v1alpha1"
	taskv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/task/v1alpha1"
	ticketv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/ticket/v1alpha1"
)

var _ = Describe("TaskService", func() {
	var (
		e      *env
		org    string
		owner  *client
		member *client
		agent  *agentv1alpha1.Agent
		ad     *agentd
	)
	BeforeEach(func(ctx SpecContext) {
		e = newEnv(ctx, envOptions{})
		org, owner = e.newTeamOrg(ctx)
		agent = owner.activeAgent(ctx, org)
		member = e.userIn(ctx, org, store.OrgRoleMember)
		ad = e.agentd("agentd-a")
	})

	create := func(ctx SpecContext, c *client, title string) (*taskv1alpha1.Task, *runv1alpha1.Run) {
		GinkgoHelper()
		res, err := c.tasks.CreateTask(ctx, &taskv1alpha1.CreateTaskRequest{
			OrgId: org, AgentId: agent.GetId(), Title: title, Message: "please " + title,
		})
		Expect(err).NotTo(HaveOccurred())
		return res.GetTask(), res.GetRun()
	}

	It("creates a submitted task owned by the agent, made by the caller, with a queued run", func(ctx SpecContext) {
		t, r := create(ctx, member, "add a badge")
		Expect(t.GetState()).To(Equal(taskv1alpha1.TaskState_TASK_STATE_SUBMITTED))
		Expect(t.GetAgentId()).To(Equal(agent.GetId()))
		Expect(t.GetCreatedBy().GetKind()).To(Equal(taskv1alpha1.CreatorKind_CREATOR_KIND_USER))
		Expect(t.GetCreatedBy().GetUserId()).To(Equal(member.userID))
		Expect(t.GetCreatedBy().GetAgentId()).To(BeEmpty())
		Expect(t.GetCreatedBy().GetTriggerId()).To(BeEmpty())
		Expect(t.GetFinishedAt()).To(BeNil())
		Expect(r.GetState()).To(Equal(runv1alpha1.RunState_RUN_STATE_QUEUED))

		got, err := member.tasks.GetTask(ctx, &taskv1alpha1.GetTaskRequest{OrgId: org, TaskId: t.GetId()})
		Expect(err).NotTo(HaveOccurred())
		var kinds []string
		for _, ev := range got.GetEvents() {
			kinds = append(kinds, ev.GetKind())
		}
		Expect(kinds).To(Equal([]string{"task.created", "message", "run.queued"}))
		Expect(got.GetRuns()).To(ConsistOf(HaveField("Id", r.GetId())))
		Expect(got.GetTask().GetCost()).NotTo(BeNil())
	})

	It("is the same record the ticket API shows", func(ctx SpecContext) {
		t, _ := create(ctx, member, "one record")
		tk := member.getTicket(ctx, org, t.GetId())
		Expect(tk.GetTicket().GetStatus()).To(Equal(ticketv1alpha1.TicketStatus_TICKET_STATUS_TODO))
		Expect(tk.GetTicket().GetCreatedBy()).To(Equal(member.userID))
		Expect(ticketKinds(tk)).To(Equal([]string{"ticket.created", "message", "run.queued"}), "the ticket API keeps its vocabulary")
	})

	It("refuses draft agents, empty titles and viewers", func(ctx SpecContext) {
		draft := owner.draftAgent(ctx, org)
		_, err := member.tasks.CreateTask(ctx, &taskv1alpha1.CreateTaskRequest{OrgId: org, AgentId: draft.GetId(), Title: "x"})
		Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition))
		_, err = member.tasks.CreateTask(ctx, &taskv1alpha1.CreateTaskRequest{OrgId: org, AgentId: agent.GetId(), Title: "  "})
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
		viewer := e.userIn(ctx, org, store.OrgRoleViewer)
		_, err = viewer.tasks.CreateTask(ctx, &taskv1alpha1.CreateTaskRequest{OrgId: org, AgentId: agent.GetId(), Title: "x"})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
	})

	It("lists tasks, all or by state, for viewers", func(ctx SpecContext) {
		t1, _ := create(ctx, member, "first")
		t2, _ := create(ctx, member, "second")
		_, err := member.tasks.CancelTask(ctx, &taskv1alpha1.CancelTaskRequest{OrgId: org, TaskId: t2.GetId()})
		Expect(err).NotTo(HaveOccurred())

		viewer := e.userIn(ctx, org, store.OrgRoleViewer)
		all, err := viewer.tasks.ListTasks(ctx, &taskv1alpha1.ListTasksRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		Expect(all.GetTasks()).To(HaveLen(2))
		submitted, err := viewer.tasks.ListTasks(ctx, &taskv1alpha1.ListTasksRequest{OrgId: org, State: taskv1alpha1.TaskState_TASK_STATE_SUBMITTED})
		Expect(err).NotTo(HaveOccurred())
		Expect(submitted.GetTasks()).To(ConsistOf(HaveField("Id", t1.GetId())))
		canceled, err := viewer.tasks.ListTasks(ctx, &taskv1alpha1.ListTasksRequest{OrgId: org, State: taskv1alpha1.TaskState_TASK_STATE_CANCELED})
		Expect(err).NotTo(HaveOccurred())
		Expect(canceled.GetTasks()).To(ConsistOf(HaveField("Id", t2.GetId())))
	})

	It("queues one run per message unless one is queued, and refuses messages on an ended task", func(ctx SpecContext) {
		t, r1 := create(ctx, member, "chat")
		res, err := member.tasks.AppendMessage(ctx, &taskv1alpha1.AppendMessageRequest{OrgId: org, TaskId: t.GetId(), Text: "also this"})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetRun()).To(BeNil(), "the queued run will see the message")
		Expect(res.GetEvent().GetKind()).To(Equal("message"))
		Expect(res.GetEvent().GetActorKind()).To(Equal(taskv1alpha1.ActorKind_ACTOR_KIND_USER))

		running := ad.mustClaim(ctx, org)
		Expect(running.ID.String()).To(Equal(r1.GetId()))
		res, err = member.tasks.AppendMessage(ctx, &taskv1alpha1.AppendMessageRequest{OrgId: org, TaskId: t.GetId(), Text: "and now?"})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetRun()).NotTo(BeNil(), "the running run has passed its prompt")

		_, err = member.tasks.AppendMessage(ctx, &taskv1alpha1.AppendMessageRequest{OrgId: org, TaskId: t.GetId(), Text: " "})
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))

		_, err = member.tasks.CancelTask(ctx, &taskv1alpha1.CancelTaskRequest{OrgId: org, TaskId: t.GetId()})
		Expect(err).NotTo(HaveOccurred())
		_, err = member.tasks.AppendMessage(ctx, &taskv1alpha1.AppendMessageRequest{OrgId: org, TaskId: t.GetId(), Text: "hello?"})
		Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition))
	})

	It("reopens a completed task for a follow-up message (old issue 324)", func(ctx SpecContext) {
		t, _ := create(ctx, member, "chat")
		_, err := e.pool.Exec(ctx, `UPDATE tasks SET state = 'completed', finished_at = now() WHERE org_id = $1 AND id = $2`, org, t.GetId())
		Expect(err).NotTo(HaveOccurred())
		res, err := member.tasks.AppendMessage(ctx, &taskv1alpha1.AppendMessageRequest{OrgId: org, TaskId: t.GetId(), Text: "one more thing"})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetEvent().GetKind()).To(Equal("message"))
		got, err := member.tasks.GetTask(ctx, &taskv1alpha1.GetTaskRequest{OrgId: org, TaskId: t.GetId()})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.GetTask().GetState()).To(Equal(taskv1alpha1.TaskState_TASK_STATE_SUBMITTED))
	})

	It("cancels a task: its queued runs are canceled, its running run is not touched, and it cannot be canceled twice", func(ctx SpecContext) {
		create(ctx, member, "blocker")
		running := ad.mustClaim(ctx, org)
		t, r := create(ctx, member, "queued")

		res, err := member.tasks.CancelTask(ctx, &taskv1alpha1.CancelTaskRequest{OrgId: org, TaskId: t.GetId()})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetTask().GetState()).To(Equal(taskv1alpha1.TaskState_TASK_STATE_CANCELED))
		Expect(res.GetTask().GetFinishedAt()).NotTo(BeNil())
		Expect(res.GetCanceledRuns()).To(ConsistOf(HaveField("Id", r.GetId())))

		got, err := member.tasks.GetTask(ctx, &taskv1alpha1.GetTaskRequest{OrgId: org, TaskId: t.GetId()})
		Expect(err).NotTo(HaveOccurred())
		var change *taskv1alpha1.TaskEvent
		for _, ev := range got.GetEvents() {
			if ev.GetKind() == "status.changed" {
				change = ev
			}
		}
		Expect(change).NotTo(BeNil())
		Expect(change.GetPayload().AsMap()).To(Equal(map[string]any{"from": "submitted", "to": "canceled"}))
		Expect(change.GetActorUserId()).To(Equal(member.userID))
		Expect(member.getRun(ctx, org, running.ID.String()).GetState()).To(Equal(runv1alpha1.RunState_RUN_STATE_RUNNING))

		_, err = member.tasks.CancelTask(ctx, &taskv1alpha1.CancelTaskRequest{OrgId: org, TaskId: t.GetId()})
		Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition))
	})

	It("shows a canceled task as Done in the ticket API, with the column in its timeline", func(ctx SpecContext) {
		t, _ := create(ctx, member, "board view")
		_, err := member.tasks.CancelTask(ctx, &taskv1alpha1.CancelTaskRequest{OrgId: org, TaskId: t.GetId()})
		Expect(err).NotTo(HaveOccurred())
		tk := member.getTicket(ctx, org, t.GetId())
		Expect(tk.GetTicket().GetStatus()).To(Equal(ticketv1alpha1.TicketStatus_TICKET_STATUS_DONE))
		var payload map[string]any
		for _, ev := range tk.GetEvents() {
			if ev.GetKind() == "status.changed" {
				payload = ev.GetPayload().AsMap()
			}
		}
		Expect(payload).To(Equal(map[string]any{"from": "todo", "to": "done"}))
	})

	It("wakes the agent's sandbox when a person types, once per ten seconds, and when a task is created", func(ctx SpecContext) {
		_, err := e.pool.Exec(ctx, `INSERT INTO org_workspaces (org_id, workspace) VALUES ($1, 'ws-typing')`, org)
		Expect(err).NotTo(HaveOccurred())
		_, err = e.pool.Exec(ctx, `INSERT INTO agent_sandboxes (org_id, agent_id, workspace, sandbox_name, provider_name, key_secret_name, home_generation)
			VALUES ($1, $2, 'ws-typing', 'sb-typing', 'pv-typing', 'key-typing', 'gen-1')`, org, agent.GetId())
		Expect(err).NotTo(HaveOccurred())
		woken := func() int {
			return count(ctx, e, `SELECT count(*) FROM agent_sandboxes WHERE agent_id = $1 AND woken_at IS NOT NULL`, agent.GetId())
		}
		reset := func() {
			_, err := e.pool.Exec(ctx, `UPDATE agent_sandboxes SET woken_at = NULL WHERE agent_id = $1`, agent.GetId())
			Expect(err).NotTo(HaveOccurred())
		}

		t, _ := create(ctx, member, "type")
		Expect(woken()).To(Equal(1), "creating a task is a pre-wake signal")
		reset()

		_, err = member.tasks.NotifyTyping(ctx, &taskv1alpha1.NotifyTypingRequest{OrgId: org, TaskId: t.GetId()})
		Expect(err).NotTo(HaveOccurred())
		Expect(woken()).To(Equal(1))
		reset()
		_, err = member.tasks.NotifyTyping(ctx, &taskv1alpha1.NotifyTypingRequest{OrgId: org, TaskId: t.GetId()})
		Expect(err).NotTo(HaveOccurred())
		Expect(woken()).To(BeZero(), "a second call within the throttle is ignored")

		viewer := e.userIn(ctx, org, store.OrgRoleViewer)
		_, err = viewer.tasks.NotifyTyping(ctx, &taskv1alpha1.NotifyTypingRequest{OrgId: org, TaskId: t.GetId()})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
	})

	It("is not found in another org", func(ctx SpecContext) {
		t, _ := create(ctx, member, "mine")
		otherOrg, other := e.newTeamOrg(ctx)
		_, err := other.tasks.GetTask(ctx, &taskv1alpha1.GetTaskRequest{OrgId: otherOrg, TaskId: t.GetId()})
		Expect(codeOf(err)).To(Equal(connect.CodeNotFound))
	})
})
