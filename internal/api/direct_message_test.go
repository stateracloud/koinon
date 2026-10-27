// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"time"

	connect "connectrpc.com/connect/v2"
	googleuuid "github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/durable"
	agentv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/agent/v1alpha1"
	taskv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/task/v1alpha1"
	templatev1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/template/v1alpha1"
	"github.com/stateracloud/koinon/internal/services"
)

// Direct messages in the inbox chat (plan item TR3, ADR 0066).
var _ = Describe("direct messages", func() {
	var (
		e     *env
		org   string
		owner *client
		agent *agentv1alpha1.Agent
	)
	dm := &templatev1alpha1.Trigger{
		Id: "chat", Type: templatev1alpha1.TriggerType_TRIGGER_TYPE_DIRECT_MESSAGE, Instruction: "Answer the person briefly.",
	}
	// withDM proposes the direct-message trigger and activates it.
	withDM := func(ctx SpecContext, c *client, a *agentv1alpha1.Agent) *agentv1alpha1.Agent {
		p, err := c.agents.SetAgentTriggers(ctx, &agentv1alpha1.SetAgentTriggersRequest{
			OrgId: org, AgentId: a.GetId(), BaseVersion: a.GetLatestVersion(), Triggers: []*templatev1alpha1.Trigger{dm},
		})
		Expect(err).NotTo(HaveOccurred())
		res, err := c.agents.ActivateAgent(ctx, &agentv1alpha1.ActivateAgentRequest{
			OrgId: org, AgentId: a.GetId(), Version: p.GetAgent().GetLatestVersion(),
		})
		Expect(err).NotTo(HaveOccurred())
		return res.GetAgent()
	}
	send := func(ctx SpecContext, c *client, task, text string) (*taskv1alpha1.SendDirectMessageResponse, error) {
		return c.tasks.SendDirectMessage(ctx, &taskv1alpha1.SendDirectMessageRequest{
			OrgId: org, AgentId: agent.GetId(), TaskId: task, Text: text, MessageId: googleuuid.NewString(),
		})
	}
	n := func(ctx SpecContext, q string) int { return count(ctx, e, q, org) }

	BeforeEach(func(ctx SpecContext) {
		e = newEnv(ctx, envOptions{})
		org, owner = e.newTeamOrg(ctx)
		agent = owner.activeAgent(ctx, org)
	})

	It("refuses an agent without a direct-message trigger, and one whose trigger is only proposed", func(ctx SpecContext) {
		_, err := send(ctx, owner, "", "hello")
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
		Expect(err.Error()).To(ContainSubstring("does not take direct messages"))

		_, err = owner.agents.SetAgentTriggers(ctx, &agentv1alpha1.SetAgentTriggersRequest{
			OrgId: org, AgentId: agent.GetId(), BaseVersion: agent.GetLatestVersion(), Triggers: []*templatev1alpha1.Trigger{dm},
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = send(ctx, owner, "", "hello")
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
		Expect(n(ctx, `SELECT count(*) FROM tasks WHERE org_id = $1`)).To(BeZero())
	})

	It("starts a task from a message, with the instruction, at interactive priority, once per message ID", func(ctx SpecContext) {
		agent = withDM(ctx, owner, agent)
		id := googleuuid.NewString()
		req := &taskv1alpha1.SendDirectMessageRequest{OrgId: org, AgentId: agent.GetId(), Text: "What changed today?\nSecond line.", MessageId: id}
		res, err := owner.tasks.SendDirectMessage(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetCreated()).To(BeTrue())
		Expect(res.GetTask().GetTitle()).To(Equal("What changed today?"))
		Expect(res.GetTask().GetCreatedBy().GetKind()).To(Equal(taskv1alpha1.CreatorKind_CREATOR_KIND_TRIGGER))
		Expect(res.GetTask().GetDirectMessageThread()).To(BeTrue())
		Expect(res.GetTask().GetState()).To(Equal(taskv1alpha1.TaskState_TASK_STATE_SUBMITTED))

		got, err := owner.tasks.GetTask(ctx, &taskv1alpha1.GetTaskRequest{OrgId: org, TaskId: res.GetTask().GetId()})
		Expect(err).NotTo(HaveOccurred())
		var msg *taskv1alpha1.TaskEvent
		for _, ev := range got.GetEvents() {
			if ev.GetKind() == "message" {
				msg = ev
			}
		}
		Expect(msg).NotTo(BeNil())
		Expect(msg.GetActorKind()).To(Equal(taskv1alpha1.ActorKind_ACTOR_KIND_USER))
		Expect(msg.GetActorUserId()).To(Equal(owner.userID))
		Expect(msg.GetPayload().AsMap()["text"]).To(Equal("Answer the person briefly.\n\nWhat changed today?\nSecond line."))
		Expect(msg.GetPayload().AsMap()["display"]).To(Equal("What changed today?\nSecond line."))
		Expect(n(ctx, `SELECT count(*) FROM runs WHERE org_id = $1 AND origin = 'interactive' AND state = 'queued'`)).To(Equal(1))

		again, err := owner.tasks.SendDirectMessage(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(again.GetCreated()).To(BeFalse())
		Expect(again.GetEvent()).To(BeNil())
		Expect(again.GetTask().GetId()).To(Equal(res.GetTask().GetId()))
		Expect(n(ctx, `SELECT count(*) FROM tasks WHERE org_id = $1`)).To(Equal(1))
		Expect(n(ctx, `SELECT count(*) FROM runs WHERE org_id = $1`)).To(Equal(1))
	})

	It("sends a reply in a thread to the same open task, without a second run", func(ctx SpecContext) {
		agent = withDM(ctx, owner, agent)
		first, err := send(ctx, owner, "", "Start here")
		Expect(err).NotTo(HaveOccurred())
		taskID := first.GetTask().GetId()

		reply, err := send(ctx, owner, taskID, "And one more thing")
		Expect(err).NotTo(HaveOccurred())
		Expect(reply.GetCreated()).To(BeFalse())
		Expect(reply.GetReopened()).To(BeFalse())
		Expect(reply.GetTask().GetId()).To(Equal(taskID))
		Expect(reply.GetEvent().GetPayload().AsMap()["text"]).To(Equal("And one more thing"), "only a new thread carries the instruction")
		Expect(n(ctx, `SELECT count(*) FROM tasks WHERE org_id = $1`)).To(Equal(1))
		Expect(n(ctx, `SELECT count(*) FROM task_events WHERE org_id = $1 AND kind = 'message'`)).To(Equal(2))
		Expect(n(ctx, `SELECT count(*) FROM runs WHERE org_id = $1`)).To(Equal(1), "the queued run will see the reply")
	})

	It("reopens a completed task for a reply, as a follow-up with a new run", func(ctx SpecContext) {
		agent = withDM(ctx, owner, agent)
		first, err := send(ctx, owner, "", "Do the thing")
		Expect(err).NotTo(HaveOccurred())
		taskID := first.GetTask().GetId()
		_, err = e.pool.Exec(ctx, `UPDATE runs SET state = 'canceled', outcome_reason = 'test', finished_at = now(), ready_at = NULL WHERE org_id = $1`, org)
		Expect(err).NotTo(HaveOccurred())
		_, err = e.pool.Exec(ctx, `UPDATE tasks SET state = 'completed', finished_at = now() WHERE org_id = $1`, org)
		Expect(err).NotTo(HaveOccurred())

		reply, err := send(ctx, owner, taskID, "One follow-up question")
		Expect(err).NotTo(HaveOccurred())
		Expect(reply.GetReopened()).To(BeTrue())
		Expect(reply.GetTask().GetId()).To(Equal(taskID))
		Expect(reply.GetTask().GetState()).To(Equal(taskv1alpha1.TaskState_TASK_STATE_SUBMITTED))
		Expect(reply.GetTask().GetFinishedAt()).To(BeNil())
		Expect(n(ctx, `SELECT count(*) FROM task_events WHERE org_id = $1 AND kind = 'status.changed' AND payload ->> 'reason' = 'follow_up'`)).To(Equal(1))
		Expect(n(ctx, `SELECT count(*) FROM runs WHERE org_id = $1 AND state = 'queued' AND origin = 'interactive'`)).To(Equal(1))
		Expect(n(ctx, `SELECT count(*) FROM tasks WHERE org_id = $1`)).To(Equal(1))
	})

	It("refuses a reply to a canceled task, a task that is not a thread, and another agent's thread", func(ctx SpecContext) {
		agent = withDM(ctx, owner, agent)
		first, err := send(ctx, owner, "", "Start")
		Expect(err).NotTo(HaveOccurred())
		_, err = owner.tasks.CancelTask(ctx, &taskv1alpha1.CancelTaskRequest{OrgId: org, TaskId: first.GetTask().GetId()})
		Expect(err).NotTo(HaveOccurred())
		_, err = send(ctx, owner, first.GetTask().GetId(), "Too late")
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))

		plain, err := owner.tasks.CreateTask(ctx, &taskv1alpha1.CreateTaskRequest{OrgId: org, AgentId: agent.GetId(), Title: "plain"})
		Expect(err).NotTo(HaveOccurred())
		_, err = send(ctx, owner, plain.GetTask().GetId(), "Hi")
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))

		second := withDM(ctx, owner, owner.activeAgent(ctx, org))
		_, err = owner.tasks.SendDirectMessage(ctx, &taskv1alpha1.SendDirectMessageRequest{
			OrgId: org, AgentId: second.GetId(), TaskId: first.GetTask().GetId(), Text: "Hi", MessageId: googleuuid.NewString(),
		})
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
	})

	It("needs a member, a message and a message ID", func(ctx SpecContext) {
		agent = withDM(ctx, owner, agent)
		viewer := e.userIn(ctx, org, store.OrgRoleViewer)
		_, err := send(ctx, viewer, "", "hello")
		Expect(connect.CodeOf(err)).To(Equal(connect.CodePermissionDenied))
		_, err = send(ctx, owner, "", "  ")
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		_, err = owner.tasks.SendDirectMessage(ctx, &taskv1alpha1.SendDirectMessageRequest{OrgId: org, AgentId: agent.GetId(), Text: "hello"})
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
	})

	It("goes through the org's queue when durable is on, and a replayed message ID has no second effect", func(ctx SpecContext) {
		agent = withDM(ctx, owner, agent)
		dsn := e.pool.Config().ConnString()
		Expect(durable.Migrate(ctx, dsn, nil)).To(Succeed())
		rt := durable.New(durable.Config{DatabaseURL: dsn, ApplicationVersion: "v1", ExecutorID: "koinon-api-test"})
		work, err := durable.NewOrgWork(rt, durable.OrgQueueConfig{PollingInterval: 50 * time.Millisecond})
		Expect(err).NotTo(HaveOccurred())
		queue, err := services.RegisterDirectMessages(rt, work)
		Expect(err).NotTo(HaveOccurred())
		Expect(rt.Launch(ctx)).To(Succeed())
		DeferCleanup(func() { _ = rt.Shutdown(5 * time.Second) })
		durableEnv := newEnvOn(ctx, e, envOptions{directMessages: queue})
		c := owner.on(durableEnv)

		req := &taskv1alpha1.SendDirectMessageRequest{OrgId: org, AgentId: agent.GetId(), Text: "Through the queue", MessageId: googleuuid.NewString()}
		res, err := c.tasks.SendDirectMessage(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetCreated()).To(BeTrue())
		Expect(res.GetEvent().GetPayload().AsMap()["display"]).To(Equal("Through the queue"))
		Expect(count(ctx, e, `SELECT count(*) FROM dbos.workflow_status WHERE queue_name = $1 AND name = $2`, durable.OrgQueueName, services.DirectMessageWorkflow)).To(Equal(1))

		again, err := c.tasks.SendDirectMessage(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(again.GetTask().GetId()).To(Equal(res.GetTask().GetId()))
		Expect(n(ctx, `SELECT count(*) FROM tasks WHERE org_id = $1`)).To(Equal(1))

		reply, err := c.tasks.SendDirectMessage(ctx, &taskv1alpha1.SendDirectMessageRequest{
			OrgId: org, AgentId: agent.GetId(), TaskId: res.GetTask().GetId(), Text: "reply", MessageId: googleuuid.NewString(),
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(reply.GetTask().GetId()).To(Equal(res.GetTask().GetId()))

		_, err = c.tasks.SendDirectMessage(ctx, &taskv1alpha1.SendDirectMessageRequest{
			OrgId: org, AgentId: agent.GetId(), TaskId: "00000000-0000-0000-0000-000000000000", Text: "x", MessageId: googleuuid.NewString(),
		})
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound), "an API error keeps its code through the workflow")
	}, SpecTimeout(2*time.Minute))
})
