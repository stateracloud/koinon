// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"strings"

	"github.com/jackc/pgx/v5"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
)

var _ = Describe("templates", func() {
	It("versions templates by content hash", func(ctx SpecContext) {
		slug := uniq("tpl")
		v1 := strings.Repeat("a", 64)
		v2 := strings.Repeat("b", 64)
		upsert := func(hash string) store.Template {
			t, err := q.UpsertTemplate(ctx, store.UpsertTemplateParams{
				Slug: slug, ContentHash: hash, DisplayName: "Repo helper", Spec: []byte(`{}`),
			})
			Expect(err).NotTo(HaveOccurred())
			return t
		}
		first := upsert(v1)
		Expect(upsert(v1).ID).To(Equal(first.ID), "re-syncing the same content keeps the row")
		second := upsert(v2)
		Expect(second.ID).NotTo(Equal(first.ID))

		current, err := q.ListCurrentTemplates(ctx)
		Expect(err).NotTo(HaveOccurred())
		var mine []store.Template
		for _, t := range current {
			if t.Slug == slug {
				mine = append(mine, t)
			}
		}
		Expect(mine).To(HaveLen(1))
		Expect(mine[0].ID).To(Equal(second.ID))

		_, err = q.UpsertTemplate(ctx, store.UpsertTemplateParams{
			Slug: slug, ContentHash: "not-a-hash", DisplayName: "x", Spec: []byte(`{}`),
		})
		Expect(err).To(MatchError(ContainSubstring("templates_content_hash_check")))
	})
})

var _ = Describe("agents", func() {
	It("goes draft -> active -> draft -> archived", func(ctx SpecContext) {
		org, user := newOrg(ctx)
		tpl, err := q.UpsertTemplate(ctx, store.UpsertTemplateParams{
			Slug: uniq("tpl"), ContentHash: strings.Repeat("c", 64), DisplayName: "t", Spec: []byte(`{}`),
		})
		Expect(err).NotTo(HaveOccurred())
		a, err := q.CreateAgent(ctx, store.CreateAgentParams{
			OrgID: org, DisplayName: "Repo helper", Harness: "opencode", TemplateID: tpl.ID, CreatedBy: user,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(a.Status).To(Equal(store.AgentStatusDraft))
		Expect(a.TemplateID).To(Equal(tpl.ID))

		v1, err := q.CreateAgentVersion(ctx, store.CreateAgentVersionParams{OrgID: org, AgentID: a.ID, Config: []byte(`{}`)})
		Expect(err).NotTo(HaveOccurred())
		v2, err := q.CreateAgentVersion(ctx, store.CreateAgentVersionParams{OrgID: org, AgentID: a.ID, Config: []byte(`{}`)})
		Expect(err).NotTo(HaveOccurred())
		Expect([]int32{v1.Version, v2.Version}).To(Equal([]int32{1, 2}))

		a, err = q.ActivateAgent(ctx, store.ActivateAgentParams{OrgID: org, ID: a.ID, VersionID: v2.ID, ActivatedBy: user})
		Expect(err).NotTo(HaveOccurred())
		Expect(a.Status).To(Equal(store.AgentStatusActive))
		Expect(a.ActiveVersionID).To(Equal(v2.ID))

		_, err = q.ActivateAgent(ctx, store.ActivateAgentParams{OrgID: org, ID: a.ID, VersionID: v1.ID})
		Expect(err).To(MatchError(pgx.ErrNoRows), "an active agent only moves to a newer version")
		_, err = q.ActivateAgent(ctx, store.ActivateAgentParams{OrgID: org, ID: a.ID, VersionID: v2.ID})
		Expect(err).To(MatchError(pgx.ErrNoRows), "a duplicate activation of the running version gets no row")

		a, err = q.ReturnAgentToDraft(ctx, store.ReturnAgentToDraftParams{OrgID: org, ID: a.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(a.Status).To(Equal(store.AgentStatusDraft))

		a, err = q.ArchiveAgent(ctx, store.ArchiveAgentParams{OrgID: org, ID: a.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(a.Status).To(Equal(store.AgentStatusArchived))
		_, err = q.ArchiveAgent(ctx, store.ArchiveAgentParams{OrgID: org, ID: a.ID})
		Expect(err).To(MatchError(pgx.ErrNoRows))
		_, err = q.CreateAgentVersion(ctx, store.CreateAgentVersionParams{OrgID: org, AgentID: a.ID, Config: []byte(`{}`)})
		Expect(err).To(MatchError(pgx.ErrNoRows), "archived agents get no new versions")

		versions, err := q.ListAgentVersions(ctx, store.ListAgentVersionsParams{OrgID: org, AgentID: a.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(versions).To(HaveLen(2))
	})

	It("rejects activating another agent's version", func(ctx SpecContext) {
		org, user := newOrg(ctx)
		_, otherVersion := newAgent(ctx, org, user)
		a, err := q.CreateAgent(ctx, store.CreateAgentParams{OrgID: org, DisplayName: "x", Harness: "opencode"})
		Expect(err).NotTo(HaveOccurred())
		_, err = q.ActivateAgent(ctx, store.ActivateAgentParams{OrgID: org, ID: a.ID, VersionID: otherVersion.ID})
		Expect(err).To(MatchError(ContainSubstring("agents_org_id_id_active_version_id_fkey")))
	})

	It("is scoped to its org", func(ctx SpecContext) {
		f := newFixture(ctx)
		other, _ := newOrg(ctx)
		_, err := q.GetAgent(ctx, store.GetAgentParams{OrgID: other, ID: f.agent.ID})
		Expect(err).To(MatchError(pgx.ErrNoRows))
		_, err = q.GetAgentVersion(ctx, store.GetAgentVersionParams{OrgID: other, ID: f.version.ID})
		Expect(err).To(MatchError(pgx.ErrNoRows))
		list, err := q.ListAgents(ctx, other)
		Expect(err).NotTo(HaveOccurred())
		Expect(list).To(BeEmpty())
		list, err = q.ListAgents(ctx, f.org)
		Expect(err).NotTo(HaveOccurred())
		Expect(list).To(HaveLen(1))
	})
})

var _ = Describe("tasks", func() {
	It("moves between states", func(ctx SpecContext) {
		f := newFixture(ctx)
		Expect(f.task.State).To(Equal(store.TaskStateSubmitted))
		for _, s := range []store.TaskState{store.TaskStateWorking, store.TaskStateInputRequired, store.TaskStateCompleted} {
			t, err := q.SetTaskState(ctx, store.SetTaskStateParams{OrgID: f.org, ID: f.task.ID, State: s})
			Expect(err).NotTo(HaveOccurred())
			Expect(t.State).To(Equal(s))
			Expect(t.FinishedAt.Valid).To(Equal(s == store.TaskStateCompleted))
		}
		done, err := q.ListTasksByStates(ctx, store.ListTasksByStatesParams{OrgID: f.org, States: []string{string(store.TaskStateCompleted)}})
		Expect(err).NotTo(HaveOccurred())
		Expect(done).To(HaveLen(1))

		t, err := q.SetTaskState(ctx, store.SetTaskStateParams{OrgID: f.org, ID: f.task.ID, State: store.TaskStateSubmitted})
		Expect(err).NotTo(HaveOccurred())
		Expect(t.FinishedAt.Valid).To(BeFalse(), "reopening clears finished_at")
	})

	It("rejects an agent from another org", func(ctx SpecContext) {
		f := newFixture(ctx)
		other, user := newOrg(ctx)
		_, err := q.CreateTask(ctx, store.CreateTaskParams{OrgID: other, Title: "x", AgentID: f.agent.ID, CreatedByKind: store.TaskCreatorKindUser, CreatedByUserID: user})
		Expect(err).To(MatchError(ContainSubstring("tasks_org_id_agent_id_fkey")))
	})

	It("stores the harness session for the current run owner only", func(ctx SpecContext) {
		f := newFixture(ctx)
		enqueue(ctx, f.org, f.agent.ID, f.task.ID)
		r := mustClaim(ctx, f.org, "agentd-a")
		t, err := q.SetTaskHarnessSession(ctx, store.SetTaskHarnessSessionParams{
			OrgID: f.org, ID: f.task.ID, RunID: r.ID, Epoch: r.Epoch,
			HarnessSessionID: "ses_1", HarnessSessionCwd: "/sandbox/work",
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(t.HarnessSessionID.String).To(Equal("ses_1"))
		Expect(t.HarnessSessionCwd.String).To(Equal("/sandbox/work"))

		other := newTask(ctx, f.org, f.user, f.agent.ID)
		_, err = q.SetTaskHarnessSession(ctx, store.SetTaskHarnessSessionParams{
			OrgID: f.org, ID: other.ID, RunID: r.ID, Epoch: r.Epoch,
			HarnessSessionID: "ses_2", HarnessSessionCwd: "/sandbox/work",
		})
		Expect(err).To(MatchError(pgx.ErrNoRows), "the run belongs to another task")
	})

	It("orders the timeline per task", func(ctx SpecContext) {
		f := newFixture(ctx)
		r := enqueue(ctx, f.org, f.agent.ID, f.task.ID)
		e1, err := q.AppendTaskEvent(ctx, store.AppendTaskEventParams{
			OrgID: f.org, TaskID: f.task.ID, Kind: "message", ActorKind: store.TaskActorKindUser,
			ActorUserID: f.user, Payload: []byte(`{"text":"please fix the README"}`),
		})
		Expect(err).NotTo(HaveOccurred())
		e2, err := q.AppendTaskEvent(ctx, store.AppendTaskEventParams{
			OrgID: f.org, TaskID: f.task.ID, Kind: "run.canceled", ActorKind: store.TaskActorKindSystem,
			RunID: r.ID, Payload: []byte(`{"reason":"wall-clock cap"}`),
		})
		Expect(err).NotTo(HaveOccurred())
		Expect([]int64{e1.Seq, e2.Seq}).To(Equal([]int64{1, 2}))

		events, err := q.ListTaskEvents(ctx, store.ListTaskEventsParams{OrgID: f.org, TaskID: f.task.ID, AfterSeq: 1, MaxEvents: 10})
		Expect(err).NotTo(HaveOccurred())
		Expect(events).To(HaveLen(1))
		Expect(events[0].ID).To(Equal(e2.ID))

		_, err = q.AppendTaskEvent(ctx, store.AppendTaskEventParams{
			OrgID: f.org, TaskID: f.task.ID, Kind: "message", ActorKind: store.TaskActorKindAgent,
			Payload: []byte(`{}`),
		})
		Expect(err).To(MatchError(ContainSubstring("task_events_check")), "an agent event needs the agent")

		otherTask := newTask(ctx, f.org, f.user, f.agent.ID)
		_, err = q.AppendTaskEvent(ctx, store.AppendTaskEventParams{
			OrgID: f.org, TaskID: otherTask.ID, Kind: "run.canceled", ActorKind: store.TaskActorKindSystem,
			RunID: r.ID, Payload: []byte(`{}`),
		})
		Expect(err).To(MatchError(ContainSubstring("task_events_org_id_task_id_run_id_fkey")))
	})
})
