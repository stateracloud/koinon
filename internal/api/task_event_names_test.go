// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	connect "connectrpc.com/connect/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	taskv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/task/v1alpha1"
)

// The display name of a message author is on the task event for every role that can read the task
// (#1031). It is the display name only, never an email, and never from outside the task's org.
var _ = Describe("TaskEvent.actor_display_name", func() {
	var (
		e      *env
		org    string
		owner  *client
		member *client
		taskID string
	)
	displayName := func(ctx SpecContext, c *client) string {
		GinkgoHelper()
		var name string
		Expect(e.pool.QueryRow(ctx, `SELECT display_name FROM users WHERE id = $1`, c.userID).Scan(&name)).To(Succeed())
		return name
	}
	// messageNames returns the display name on each user message of the task, as c reads it.
	messageNames := func(ctx SpecContext, c *client) map[string]string {
		GinkgoHelper()
		got, err := c.tasks.GetTask(ctx, &taskv1alpha1.GetTaskRequest{OrgId: org, TaskId: taskID})
		Expect(err).NotTo(HaveOccurred())
		out := map[string]string{}
		for _, ev := range got.GetEvents() {
			if ev.GetKind() == "message" && ev.GetActorKind() == taskv1alpha1.ActorKind_ACTOR_KIND_USER {
				out[ev.GetActorUserId()] = ev.GetActorDisplayName()
			}
		}
		return out
	}

	BeforeEach(func(ctx SpecContext) {
		e = newEnv(ctx, envOptions{})
		org, owner = e.newTeamOrg(ctx)
		member = e.userIn(ctx, org, store.OrgRoleMember)
		agent := owner.activeAgent(ctx, org)
		res, err := member.tasks.CreateTask(ctx, &taskv1alpha1.CreateTaskRequest{
			OrgId: org, AgentId: agent.GetId(), Title: "add a badge", Message: "first",
		})
		Expect(err).NotTo(HaveOccurred())
		taskID = res.GetTask().GetId()
		_, err = owner.tasks.AppendMessage(ctx, &taskv1alpha1.AppendMessageRequest{OrgId: org, TaskId: taskID, Text: "second"})
		Expect(err).NotTo(HaveOccurred())
	})

	It("names every message author to a viewer, a member and an owner", func(ctx SpecContext) {
		viewer := e.userIn(ctx, org, store.OrgRoleViewer)
		other := e.userIn(ctx, org, store.OrgRoleMember)
		want := map[string]string{
			member.userID: displayName(ctx, member),
			owner.userID:  displayName(ctx, owner),
		}
		Expect(want[member.userID]).NotTo(BeEmpty())
		for _, c := range []*client{viewer, other, member, owner} {
			Expect(messageNames(ctx, c)).To(Equal(want))
		}
	})

	It("names the author on the response of AppendMessage", func(ctx SpecContext) {
		res, err := member.tasks.AppendMessage(ctx, &taskv1alpha1.AppendMessageRequest{OrgId: org, TaskId: taskID, Text: "third"})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetEvent().GetActorDisplayName()).To(Equal(displayName(ctx, member)))
	})

	It("never carries an email address", func(ctx SpecContext) {
		viewer := e.userIn(ctx, org, store.OrgRoleViewer)
		got, err := viewer.tasks.GetTask(ctx, &taskv1alpha1.GetTaskRequest{OrgId: org, TaskId: taskID})
		Expect(err).NotTo(HaveOccurred())
		for _, ev := range got.GetEvents() {
			Expect(ev.GetActorDisplayName()).NotTo(ContainSubstring("@"))
		}
	})

	It("gives no name when the display name is an email address", func(ctx SpecContext) {
		_, err := e.pool.Exec(ctx, `UPDATE users SET display_name = email WHERE id = $1`, member.userID)
		Expect(err).NotTo(HaveOccurred())
		viewer := e.userIn(ctx, org, store.OrgRoleViewer)
		Expect(messageNames(ctx, viewer)).To(HaveKeyWithValue(member.userID, ""))
	})

	It("lets a user of another org read nothing of the task", func(ctx SpecContext) {
		_, stranger := e.newTeamOrg(ctx)
		_, err := stranger.tasks.GetTask(ctx, &taskv1alpha1.GetTaskRequest{OrgId: org, TaskId: taskID})
		Expect(codeOf(err)).To(Equal(connect.CodeNotFound))
	})

	It("gives no name for a user who is not a member of the task's org", func(ctx SpecContext) {
		_, err := e.pool.Exec(ctx, `DELETE FROM org_memberships WHERE user_id = $1 AND org_id = $2`, member.userID, org)
		Expect(err).NotTo(HaveOccurred())
		Expect(messageNames(ctx, owner)).To(HaveKeyWithValue(member.userID, ""))
	})
})
