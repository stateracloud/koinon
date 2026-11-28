// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	taskv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/task/v1alpha1"
)

// A message from a person without a Koinon account in Slack or Teams (ADR 0130, #1408) reads as
// ACTOR_KIND_EXTERNAL with the person's platform identity, and never with a name.
var _ = Describe("TaskEvent.actor_external", func() {
	var (
		e      *env
		org    string
		owner  *client
		taskID string
	)

	BeforeEach(func(ctx SpecContext) {
		e = newEnv(ctx, envOptions{})
		org, owner = e.newTeamOrg(ctx)
		agent := owner.activeAgent(ctx, org)
		res, err := owner.tasks.CreateTask(ctx, &taskv1alpha1.CreateTaskRequest{
			OrgId: org, AgentId: agent.GetId(), Title: "a question from Slack", Message: "first",
		})
		Expect(err).NotTo(HaveOccurred())
		taskID = res.GetTask().GetId()

		var orgID, task pgtype.UUID
		Expect(orgID.Scan(org)).To(Succeed())
		Expect(task.Scan(taskID)).To(Succeed())
		q := store.New(e.pool)
		person, err := q.EnsureExternalActor(ctx, store.EnsureExternalActorParams{
			OrgID: orgID, Platform: "slack", Team: "T0TEST", PlatformUserID: "U0TEST",
		})
		Expect(err).NotTo(HaveOccurred())
		again, err := q.EnsureExternalActor(ctx, store.EnsureExternalActorParams{
			OrgID: orgID, Platform: "slack", Team: "T0TEST", PlatformUserID: "U0TEST",
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(again.ID).To(Equal(person.ID), "one row for each person")
		_, err = q.AppendTaskEvent(ctx, store.AppendTaskEventParams{
			OrgID: orgID, TaskID: task, Kind: "message", ActorKind: store.TaskActorKindExternal,
			ActorExternalID: person.ID, Payload: []byte(`{"text":"the build is red","source":"slack"}`), Prompt: true,
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("shows the person's platform identity to every role that can read the task", func(ctx SpecContext) {
		viewer := e.userIn(ctx, org, store.OrgRoleViewer)
		for _, c := range []*client{viewer, owner} {
			got, err := c.tasks.GetTask(ctx, &taskv1alpha1.GetTaskRequest{OrgId: org, TaskId: taskID})
			Expect(err).NotTo(HaveOccurred())
			var external []*taskv1alpha1.TaskEvent
			for _, ev := range got.GetEvents() {
				if ev.GetActorKind() == taskv1alpha1.ActorKind_ACTOR_KIND_EXTERNAL {
					external = append(external, ev)
				} else {
					Expect(ev.GetActorExternal()).To(BeNil(), "only an external event names a person")
				}
			}
			Expect(external).To(HaveLen(1))
			ev := external[0]
			Expect(ev.GetKind()).To(Equal("message"))
			Expect(ev.GetActorUserId()).To(BeEmpty())
			Expect(ev.GetActorDisplayName()).To(BeEmpty(), "Koinon stores no name for an external person")
			Expect(ev.GetActorExternal().GetPlatform()).To(Equal("slack"))
			Expect(ev.GetActorExternal().GetTeam()).To(Equal("T0TEST"))
			Expect(ev.GetActorExternal().GetPlatformUserId()).To(Equal("U0TEST"))
			Expect(ev.GetEvent().GetMessage().GetText()).To(Equal("the build is red"))
			Expect(ev.GetEvent().GetMessage().GetSource()).To(Equal("slack"))
		}
	})
})
