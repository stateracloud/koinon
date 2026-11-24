// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"time"

	connect "connectrpc.com/connect/v2"
	"github.com/jackc/pgx/v5"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	agentv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/agent/v1alpha1"
	budgetv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/budget/v1alpha1"
	runv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/run/v1alpha1"
	templatev1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/template/v1alpha1"
)

// The agent cards of the org home (#1265, ADR 0126 section 4): the team and reports-to of an
// agent, the paged agent list and the one-query summary page.

func reportsToAgent(id string) *agentv1alpha1.AgentReportsTo {
	return &agentv1alpha1.AgentReportsTo{Target: &agentv1alpha1.AgentReportsTo_AgentId{AgentId: id}}
}

func reportsToUser(id string) *agentv1alpha1.AgentReportsTo {
	return &agentv1alpha1.AgentReportsTo{Target: &agentv1alpha1.AgentReportsTo_UserId{UserId: id}}
}

var _ = Describe("the agent's team and reports-to", func() {
	var (
		e     *env
		org   string
		owner *client
		a, b  *agentv1alpha1.Agent
	)
	BeforeEach(func(ctx SpecContext) {
		e = newEnv(ctx, envOptions{})
		org, owner = e.newTeamOrg(ctx)
		a = owner.activeAgent(ctx, org)
		b = owner.draftAgent(ctx, org)
	})
	place := func(ctx SpecContext, c *client, agent, team string, to *agentv1alpha1.AgentReportsTo) (*agentv1alpha1.Agent, error) {
		res, err := c.agents.SetAgentPlacement(ctx, &agentv1alpha1.SetAgentPlacementRequest{OrgId: org, AgentId: agent, Team: team, ReportsTo: to})
		return res.GetAgent(), err
	}
	audits := func(ctx SpecContext) int {
		return count(ctx, e, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'agent.placement.set'`, org)
	}

	It("sets the team and an agent parent, shows them on the agent, and audits the change once", func(ctx SpecContext) {
		Expect(a.GetTeam()).To(BeEmpty())
		Expect(a.GetReportsTo()).To(BeNil())
		got, err := place(ctx, owner, a.GetId(), "  Platform  ", reportsToAgent(b.GetId()))
		Expect(err).NotTo(HaveOccurred())
		Expect(got.GetTeam()).To(Equal("Platform"), "the label is trimmed")
		Expect(got.GetReportsTo().GetAgentId()).To(Equal(b.GetId()))
		Expect(got.GetLatestVersion()).To(Equal(a.GetLatestVersion()), "a placement is not a new version")
		res, err := owner.agents.GetAgent(ctx, &agentv1alpha1.GetAgentRequest{OrgId: org, AgentId: a.GetId()})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetAgent().GetTeam()).To(Equal("Platform"))
		Expect(res.GetAgent().GetReportsTo().GetAgentId()).To(Equal(b.GetId()))

		By("a repeat is no change and writes no audit event")
		_, err = place(ctx, owner, a.GetId(), "Platform", reportsToAgent(b.GetId()))
		Expect(err).NotTo(HaveOccurred())
		Expect(audits(ctx)).To(Equal(1))

		var before, after, actor string
		Expect(e.pool.QueryRow(ctx, `SELECT summary->>'before', summary->>'after', actor_id FROM audit_events
			WHERE org_id = $1 AND action = 'agent.placement.set' AND target_id = $2`, org, a.GetId()).
			Scan(&before, &after, &actor)).To(Succeed())
		Expect(before).To(MatchJSON(`{"team": "", "reportsToAgentId": "", "reportsToUserId": ""}`))
		Expect(after).To(MatchJSON(`{"team": "Platform", "reportsToAgentId": "` + b.GetId() + `", "reportsToUserId": ""}`))
		Expect(actor).To(Equal(owner.userID))

		By("no reports_to clears it")
		got, err = place(ctx, owner, a.GetId(), "", nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(got.GetTeam()).To(BeEmpty())
		Expect(got.GetReportsTo()).To(BeNil())
		Expect(audits(ctx)).To(Equal(2))
	})

	It("sets a person of the org, of any role, as the parent", func(ctx SpecContext) {
		viewer := e.userIn(ctx, org, store.OrgRoleViewer)
		got, err := place(ctx, owner, a.GetId(), "", reportsToUser(viewer.userID))
		Expect(err).NotTo(HaveOccurred())
		Expect(got.GetReportsTo().GetUserId()).To(Equal(viewer.userID))
	})

	It("is refused for a member, and changes nothing", func(ctx SpecContext) {
		member := e.userIn(ctx, org, store.OrgRoleMember)
		_, err := place(ctx, member, a.GetId(), "Platform", reportsToAgent(b.GetId()))
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
		Expect(count(ctx, e, `SELECT count(*) FROM agents WHERE org_id = $1 AND (team <> '' OR reports_to_agent_id IS NOT NULL)`, org)).To(BeZero())
		Expect(audits(ctx)).To(BeZero())
	})

	It("refuses a cycle, also through a longer chain", func(ctx SpecContext) {
		_, err := place(ctx, owner, a.GetId(), "", reportsToAgent(a.GetId()))
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument), "an agent cannot report to itself")

		_, err = place(ctx, owner, a.GetId(), "", reportsToAgent(b.GetId()))
		Expect(err).NotTo(HaveOccurred())
		_, err = place(ctx, owner, b.GetId(), "", reportsToAgent(a.GetId()))
		Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition))

		By("a -> b -> c, then c -> a")
		c := owner.draftAgent(ctx, org)
		_, err = place(ctx, owner, b.GetId(), "", reportsToAgent(c.GetId()))
		Expect(err).NotTo(HaveOccurred())
		_, err = place(ctx, owner, c.GetId(), "", reportsToAgent(a.GetId()))
		Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition))
		_, err = place(ctx, owner, c.GetId(), "", reportsToAgent(b.GetId()))
		Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition))

		By("moving a link that closes no loop is allowed")
		_, err = place(ctx, owner, a.GetId(), "", reportsToAgent(c.GetId()))
		Expect(err).NotTo(HaveOccurred())
		Expect(audits(ctx)).To(Equal(3))
	})

	It("does not find an agent or a person of another org as the parent", func(ctx SpecContext) {
		other, otherOwner := e.newTeamOrg(ctx)
		foreign := otherOwner.draftAgent(ctx, other)
		_, err := place(ctx, owner, a.GetId(), "", reportsToAgent(foreign.GetId()))
		Expect(codeOf(err)).To(Equal(connect.CodeNotFound))
		_, err = place(ctx, owner, a.GetId(), "", reportsToUser(otherOwner.userID))
		Expect(codeOf(err)).To(Equal(connect.CodeNotFound))
		_, err = place(ctx, owner, a.GetId(), "", reportsToUser("019a0000-0000-7000-8000-00000000ffff"))
		Expect(codeOf(err)).To(Equal(connect.CodeNotFound))
		Expect(audits(ctx)).To(BeZero())

		By("the database refuses a parent of another org as well")
		_, err = e.pool.Exec(ctx, `UPDATE agents SET reports_to_agent_id = $3 WHERE org_id = $1 AND id = $2`, org, a.GetId(), foreign.GetId())
		Expect(err).To(MatchError(ContainSubstring("agents_reports_to_agent_fk")))
	})

	It("checks the team label and the ids", func(ctx SpecContext) {
		for _, team := range []string{strings.Repeat("x", 65), "a\nb", "\x00", "ab\u202ecd", "a\u200bb"} {
			_, err := place(ctx, owner, a.GetId(), team, nil)
			Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument), "team %q", team)
		}
		got, err := place(ctx, owner, a.GetId(), strings.Repeat("ü", 64), nil)
		Expect(err).NotTo(HaveOccurred(), "64 characters fit")
		Expect(got.GetTeam()).To(HaveLen(128))
		_, err = place(ctx, owner, a.GetId(), "", reportsToAgent("not-a-uuid"))
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
		_, err = place(ctx, owner, a.GetId(), "", reportsToUser("not-a-uuid"))
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
		_, err = place(ctx, owner, "019a0000-0000-7000-8000-00000000ffff", "x", nil)
		Expect(codeOf(err)).To(Equal(connect.CodeNotFound))
	})

	It("keeps a parent that was archived or left the org when only the team changes", func(ctx SpecContext) {
		viewer := e.userIn(ctx, org, store.OrgRoleViewer)
		_, err := place(ctx, owner, a.GetId(), "", reportsToAgent(b.GetId()))
		Expect(err).NotTo(HaveOccurred())
		_, err = place(ctx, owner, b.GetId(), "", reportsToUser(viewer.userID))
		Expect(err).NotTo(HaveOccurred())
		_, err = owner.agents.ArchiveAgent(ctx, &agentv1alpha1.ArchiveAgentRequest{OrgId: org, AgentId: b.GetId()})
		Expect(err).NotTo(HaveOccurred())
		got, err := place(ctx, owner, a.GetId(), "Platform", reportsToAgent(b.GetId()))
		Expect(err).NotTo(HaveOccurred())
		Expect(got.GetReportsTo().GetAgentId()).To(Equal(b.GetId()))

		c := owner.draftAgent(ctx, org)
		_, err = place(ctx, owner, c.GetId(), "", reportsToUser(viewer.userID))
		Expect(err).NotTo(HaveOccurred())
		_, err = e.pool.Exec(ctx, `DELETE FROM org_memberships WHERE org_id = $1 AND user_id = $2`, org, viewer.userID)
		Expect(err).NotTo(HaveOccurred())
		_, err = place(ctx, owner, c.GetId(), "Platform", reportsToUser(viewer.userID))
		Expect(err).NotTo(HaveOccurred())
		_, err = place(ctx, owner, a.GetId(), "", reportsToUser(viewer.userID))
		Expect(codeOf(err)).To(Equal(connect.CodeNotFound), "a new parent must be a member")
	})

	It("refuses an archived agent and an archived parent", func(ctx SpecContext) {
		_, err := owner.agents.ArchiveAgent(ctx, &agentv1alpha1.ArchiveAgentRequest{OrgId: org, AgentId: b.GetId()})
		Expect(err).NotTo(HaveOccurred())
		_, err = place(ctx, owner, a.GetId(), "", reportsToAgent(b.GetId()))
		Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition))
		_, err = place(ctx, owner, b.GetId(), "x", nil)
		Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition))
		Expect(audits(ctx)).To(BeZero())
	})
})

var _ = Describe("the agent list pages", func() {
	It("pages in creation order with an opaque token and refuses a bad token", func(ctx SpecContext) {
		e := newEnv(ctx, envOptions{})
		org, owner := e.newTeamOrg(ctx)
		var want []string
		for range 3 {
			want = append(want, owner.draftAgent(ctx, org).GetId())
		}
		list := func(size int32, tok string) (*agentv1alpha1.ListAgentsResponse, error) {
			return owner.agents.ListAgents(ctx, &agentv1alpha1.ListAgentsRequest{OrgId: org, PageSize: size, PageToken: tok})
		}
		all, err := list(0, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(all.GetNextPageToken()).To(BeEmpty(), "the default page holds a small org")
		Expect(agentIDs(all.GetAgents())).To(Equal(want))

		var got []string
		tok := ""
		for pages := 0; ; pages++ {
			Expect(pages).To(BeNumerically("<", 3))
			res, err := list(2, tok)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(res.GetAgents())).To(BeNumerically("<=", 2))
			got = append(got, agentIDs(res.GetAgents())...)
			if tok = res.GetNextPageToken(); tok == "" {
				break
			}
		}
		Expect(got).To(Equal(want))

		for _, bad := range []string{"x", "e30", "eyJjIjoiMjAyNi0wMS0wMVQwMDowMDowMFoiLCJpIjoibm8ifQ"} {
			_, err = list(2, bad)
			Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument), "token %q", bad)
		}
		_, err = list(-1, "")
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))

		By("a summary token is not a list token")
		sum, err := owner.agents.ListAgentSummaries(ctx, &agentv1alpha1.ListAgentSummariesRequest{OrgId: org, PageSize: 1})
		Expect(err).NotTo(HaveOccurred())
		Expect(sum.GetNextPageToken()).NotTo(BeEmpty())
		_, err = list(2, sum.GetNextPageToken())
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
	})
})

func agentIDs(agents []*agentv1alpha1.Agent) []string {
	out := make([]string, 0, len(agents))
	for _, a := range agents {
		out = append(out, a.GetId())
	}
	return out
}

func summaryIDs(s []*agentv1alpha1.AgentSummary) []string {
	out := make([]string, 0, len(s))
	for _, a := range s {
		out = append(out, a.GetAgentId())
	}
	return out
}

var _ = Describe("the agent summaries", func() {
	var (
		e     *env
		org   string
		owner *client
	)
	BeforeEach(func(ctx SpecContext) {
		e = newEnv(ctx, envOptions{})
		org, owner = e.newTeamOrg(ctx)
	})
	summaries := func(ctx SpecContext, c *client) []*agentv1alpha1.AgentSummary {
		GinkgoHelper()
		res, err := c.agents.ListAgentSummaries(ctx, &agentv1alpha1.ListAgentSummariesRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetDayStart().AsTime()).To(Equal(time.Now().UTC().Truncate(24 * time.Hour)))
		return res.GetAgents()
	}
	byID := func(s []*agentv1alpha1.AgentSummary, id string) *agentv1alpha1.AgentSummary {
		GinkgoHelper()
		for _, a := range s {
			if a.GetAgentId() == id {
				return a
			}
		}
		Fail("no summary for agent " + id)
		return nil
	}

	It("shows the last run, the running runs, today's spend and the daily limit", func(ctx SpecContext) {
		a := owner.activeAgent(ctx, org)
		idle := owner.activeAgent(ctx, org)
		got := byID(summaries(ctx, owner), idle.GetId())
		Expect(got.GetLastRunAt()).To(BeNil())
		Expect(got.GetLastRunState()).To(Equal(runv1alpha1.RunState_RUN_STATE_UNSPECIFIED))
		Expect(got.GetSpentTodayUsd()).To(Equal("0.00"))
		Expect(got.GetHarness()).To(Equal(a.GetHarness()))
		Expect(got.GetModel()).NotTo(BeEmpty(), "the model of the latest version")

		_, first := owner.createTask(ctx, org, a.GetId(), "one")
		_, second := owner.createTask(ctx, org, a.GetId(), "two")
		_, err := e.pool.Exec(ctx, `UPDATE runs SET state = 'starting', ready_at = NULL, owner_id = 'test-owner', lease_expires_at = now() + interval '1 minute' WHERE org_id = $1 AND id = $2`, org, first.GetId())
		Expect(err).NotTo(HaveOccurred())
		for i, row := range []struct {
			cost      string
			input     int
			cacheRead int
			when      string
		}{{"1.25", 1000, 600, "now()"}, {"0.5", 200, 100, "now()"}, {"9", 5000, 0, "now() - interval '2 days'"}} {
			_, err := e.pool.Exec(ctx, `INSERT INTO spend_entries (org_id, request_id, agent_id, key_id, model, input_tokens, cache_read_tokens, output_tokens, occurred_at, cost_usd, priced)
				VALUES ($1, $2, $3, 'k', 'm', $4, $5, 1, `+row.when+`, $6::numeric, true)`, org, "req-"+string(rune('a'+i)), a.GetId(), row.input, row.cacheRead, row.cost)
			Expect(err).NotTo(HaveOccurred())
		}
		budget, err := budgetClient(owner).GetAgentBudget(ctx, &budgetv1alpha1.GetAgentBudgetRequest{OrgId: org, AgentId: a.GetId()})
		Expect(err).NotTo(HaveOccurred())

		got = byID(summaries(ctx, owner), a.GetId())
		Expect(got.GetLastRunState()).To(Equal(second.GetState()), "the newest run")
		Expect(got.GetLastRunAt()).NotTo(BeNil())
		Expect(got.GetRunningRuns()).To(Equal(int32(1)))
		Expect(got.GetSpentTodayUsd()).To(Equal("1.75"), "only today's spend")
		Expect(got.GetInputTokensToday()).To(Equal(int64(1200)))
		Expect(got.GetCacheReadTokensToday()).To(Equal(int64(700)))
		Expect(got.GetDailyLimitUsd()).To(Equal(budget.GetBudget().GetDailyLimitUsd()), "the template's spendUSD")

		By("an own daily limit replaces the version's")
		_, err = budgetClient(owner).SetAgentBudget(ctx, &budgetv1alpha1.SetAgentBudgetRequest{OrgId: org, AgentId: a.GetId(), DailyLimitUsd: "3.5"})
		Expect(err).NotTo(HaveOccurred())
		Expect(byID(summaries(ctx, owner), a.GetId()).GetDailyLimitUsd()).To(Equal("3.50"))
	})

	It("shows the next scheduled run of an active agent only", func(ctx SpecContext) {
		tpl := ""
		res, err := owner.templates.ListTemplates(ctx, &templatev1alpha1.ListTemplatesRequest{})
		Expect(err).NotTo(HaveOccurred())
		for _, t := range res.GetTemplates() {
			if t.GetSlug() == "finops-analyst" {
				tpl = t.GetId()
			}
		}
		Expect(tpl).NotTo(BeEmpty())
		created, err := owner.agents.CreateAgentFromTemplate(ctx, &agentv1alpha1.CreateAgentFromTemplateRequest{OrgId: org, TemplateId: tpl, DisplayName: "FinOps"})
		Expect(err).NotTo(HaveOccurred())
		draft := created.GetAgent()
		Expect(byID(summaries(ctx, owner), draft.GetId()).GetNextScheduledAt()).To(BeNil(), "a draft does not fire")
		_, err = owner.agents.ActivateAgent(ctx, &agentv1alpha1.ActivateAgentRequest{OrgId: org, AgentId: draft.GetId(), Version: draft.GetLatestVersion()})
		Expect(err).NotTo(HaveOccurred())

		now := time.Now().UTC()
		want := time.Date(now.Year(), now.Month(), now.Day(), 7, 0, 0, 0, time.UTC)
		if !want.After(now) {
			want = want.AddDate(0, 0, 1)
		}
		next := byID(summaries(ctx, owner), draft.GetId()).GetNextScheduledAt()
		Expect(next).NotTo(BeNil())
		Expect(next.AsTime()).To(BeTemporally("~", want, time.Minute), "daily at 07:00 UTC")
	})

	It("counts what waits on the caller by the inbox rules", func(ctx SpecContext) {
		a := owner.activeAgent(ctx, org)
		member := e.userIn(ctx, org, store.OrgRoleMember)
		t, _ := member.createTask(ctx, org, a.GetId(), "needs input")
		_, err := e.pool.Exec(ctx, `UPDATE tasks SET state = 'input_required' WHERE org_id = $1 AND id = $2`, org, t.GetId())
		Expect(err).NotTo(HaveOccurred())
		Expect(byID(summaries(ctx, member), a.GetId()).GetWaitingOnYou()).To(Equal(int32(1)), "the creator's task waits for input")
		Expect(byID(summaries(ctx, owner), a.GetId()).GetWaitingOnYou()).To(BeZero(), "not the owner's task")

		By("an unhealthy agent waits on admins only")
		_, err = e.pool.Exec(ctx, `UPDATE agents SET unhealthy_at = now(), unhealthy_reason = 'stuck three times' WHERE org_id = $1 AND id = $2`, org, a.GetId())
		Expect(err).NotTo(HaveOccurred())
		Expect(byID(summaries(ctx, owner), a.GetId()).GetWaitingOnYou()).To(Equal(int32(1)))
		Expect(byID(summaries(ctx, member), a.GetId()).GetWaitingOnYou()).To(Equal(int32(1)))

		By("a pending approval waits on admins and covers its task")
		_, err = e.pool.Exec(ctx, `INSERT INTO approval_requests (org_id, task_id, run_id, agent_id, connector, tool, target, args_json, args_sha256, decision, status, expires_at)
			SELECT org_id, id, (SELECT id FROM runs r WHERE r.org_id = t.org_id AND r.task_id = t.id LIMIT 1), agent_id,
			       'github', 'create_issue', 'github:acme/widgets', '{}', encode(sha256(convert_to('{}', 'UTF8')), 'hex'), 'ask', 'pending', now() + interval '1 day'
			FROM tasks t WHERE org_id = $1 AND id = $2`, org, t.GetId())
		Expect(err).NotTo(HaveOccurred())
		Expect(byID(summaries(ctx, owner), a.GetId()).GetWaitingOnYou()).To(Equal(int32(2)), "the approval and the health")
		Expect(byID(summaries(ctx, member), a.GetId()).GetWaitingOnYou()).To(BeZero(), "the approval covers the task")
	})

	It("counts open questions by the inbox rules", func(ctx SpecContext) {
		a := owner.activeAgent(ctx, org)
		creator := e.userIn(ctx, org, store.OrgRoleMember)
		answerer := e.userIn(ctx, org, store.OrgRoleMember)
		viewer := e.userIn(ctx, org, store.OrgRoleViewer)
		t, r := creator.createTask(ctx, org, a.GetId(), "asks")
		for _, q := range []string{
			`UPDATE runs SET state = 'starting', ready_at = NULL, owner_id = 'test-owner', lease_expires_at = now() + interval '1 minute' WHERE org_id = $1 AND id = $2`,
			`UPDATE runs SET state = 'running' WHERE org_id = $1 AND id = $2`,
			`UPDATE runs SET state = 'parked', park_reason = 'input_required', parked_at = now(), owner_id = NULL, lease_expires_at = NULL WHERE org_id = $1 AND id = $2`,
		} {
			_, err := e.pool.Exec(ctx, q, org, r.GetId())
			Expect(err).NotTo(HaveOccurred())
		}
		_, err := e.pool.Exec(ctx, `UPDATE tasks SET state = 'input_required' WHERE org_id = $1 AND id = $2`, org, t.GetId())
		Expect(err).NotTo(HaveOccurred())
		var qid string
		Expect(e.pool.QueryRow(ctx, `INSERT INTO questions (org_id, task_id, run_id, agent_id, asked_to, question, default_answer, state, call_sha256, expires_at, opened_at)
			VALUES ($1, $2, $3, $4, $5, 'which branch?', 'main', 'open', repeat('b', 64), now() + interval '1 day', now()) RETURNING id::text`,
			org, t.GetId(), r.GetId(), a.GetId(), viewer.userID).Scan(&qid)).To(Succeed())
		recipients := func(users ...string) {
			GinkgoHelper()
			for _, u := range users {
				_, err := e.pool.Exec(ctx, `INSERT INTO question_recipients (org_id, question_id, user_id, waiting_since) VALUES ($1, $2, $3, now())`, org, qid, u)
				Expect(err).NotTo(HaveOccurred())
			}
		}
		waiting := func(c *client) int32 { GinkgoHelper(); return byID(summaries(ctx, c), a.GetId()).GetWaitingOnYou() }

		By("a viewer recipient cannot answer; the task waits on nobody who can, so it is the creator's")
		// The legacy asked_to trigger (00036) makes the viewer the first recipient.
		Expect(waiting(viewer)).To(BeZero())
		Expect(waiting(creator)).To(Equal(int32(1)))

		By("a member recipient answers; the creator's task waits on that person")
		recipients(answerer.userID)
		Expect(waiting(answerer)).To(Equal(int32(1)))
		Expect(waiting(creator)).To(BeZero(), "the task waits on another person")
		Expect(waiting(owner)).To(BeZero())

		By("a creator who is a recipient too sees the question once")
		recipients(creator.userID)
		Expect(waiting(creator)).To(Equal(int32(1)))

		By("a removed recipient no longer counts")
		_, err = e.pool.Exec(ctx, `UPDATE question_recipients SET removed_at = now() WHERE org_id = $1 AND question_id = $2 AND user_id = $3`, org, qid, answerer.userID)
		Expect(err).NotTo(HaveOccurred())
		Expect(waiting(answerer)).To(BeZero())
	})

	It("leaves out archived agents, groups by team with no team last, and pages", func(ctx SpecContext) {
		mk := func(team string) string {
			id := owner.draftAgent(ctx, org).GetId()
			if team != "" {
				_, err := owner.agents.SetAgentPlacement(ctx, &agentv1alpha1.SetAgentPlacementRequest{OrgId: org, AgentId: id, Team: team})
				Expect(err).NotTo(HaveOccurred())
			}
			return id
		}
		none1, b1, a1, none2, b2 := mk(""), mk("beta"), mk("alpha"), mk(""), mk("beta")
		gone := mk("alpha")
		_, err := owner.agents.ArchiveAgent(ctx, &agentv1alpha1.ArchiveAgentRequest{OrgId: org, AgentId: gone})
		Expect(err).NotTo(HaveOccurred())
		want := []string{a1, b1, b2, none1, none2}

		viewer := e.userIn(ctx, org, store.OrgRoleViewer)
		Expect(summaryIDs(summaries(ctx, viewer))).To(Equal(want))
		for _, size := range []int32{1, 2, 3} {
			var got []string
			tok := ""
			for {
				res, err := viewer.agents.ListAgentSummaries(ctx, &agentv1alpha1.ListAgentSummariesRequest{OrgId: org, PageSize: size, PageToken: tok})
				Expect(err).NotTo(HaveOccurred())
				got = append(got, summaryIDs(res.GetAgents())...)
				if tok = res.GetNextPageToken(); tok == "" {
					break
				}
			}
			Expect(got).To(Equal(want), "page size %d", size)
		}
		_, err = viewer.agents.ListAgentSummaries(ctx, &agentv1alpha1.ListAgentSummariesRequest{OrgId: org, PageToken: "e30"})
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
	})

	It("names the parent, and hides a person who left the org", func(ctx SpecContext) {
		a := owner.activeAgent(ctx, org)
		b := owner.draftAgent(ctx, org)
		viewer := e.userIn(ctx, org, store.OrgRoleViewer)
		_, err := owner.agents.SetAgentPlacement(ctx, &agentv1alpha1.SetAgentPlacementRequest{OrgId: org, AgentId: a.GetId(), ReportsTo: reportsToAgent(b.GetId())})
		Expect(err).NotTo(HaveOccurred())
		_, err = owner.agents.SetAgentPlacement(ctx, &agentv1alpha1.SetAgentPlacementRequest{OrgId: org, AgentId: b.GetId(), ReportsTo: reportsToUser(viewer.userID)})
		Expect(err).NotTo(HaveOccurred())
		s := summaries(ctx, owner)
		Expect(byID(s, a.GetId()).GetReportsTo()).To(And(HaveField("DisplayName", b.GetDisplayName()), HaveField("Target", HaveField("AgentId", b.GetId()))))
		var name string
		Expect(e.pool.QueryRow(ctx, `SELECT display_name FROM users WHERE id = $1`, viewer.userID).Scan(&name)).To(Succeed())
		Expect(byID(s, b.GetId()).GetReportsTo()).To(And(HaveField("DisplayName", name), HaveField("Target", HaveField("UserId", viewer.userID))))

		_, err = e.pool.Exec(ctx, `DELETE FROM org_memberships WHERE org_id = $1 AND user_id = $2`, org, viewer.userID)
		Expect(err).NotTo(HaveOccurred())
		Expect(byID(summaries(ctx, owner), b.GetId()).GetReportsTo()).To(BeNil())
	})
})

// queryCounter counts the queries of a pool by their sqlc name.
type queryCounter struct {
	mu    sync.Mutex
	names map[string]int
	total int
}

var sqlcName = regexp.MustCompile(`^-- name: (\w+)`)

func (q *queryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.total++
	if m := sqlcName.FindStringSubmatch(data.SQL); m != nil {
		q.names[m[1]]++
	}
	return ctx
}

func (*queryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (q *queryCounter) reset() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.names, q.total = map[string]int{}, 0
}

func (q *queryCounter) snapshot() (int, map[string]int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make(map[string]int, len(q.names))
	for k, v := range q.names {
		out[k] = v
	}
	return q.total, out
}

var _ = Describe("the agent summaries at the scale track", func() {
	It("pages through 1000 agents with one query per page, in the time budget", func(ctx SpecContext) {
		base := newEnv(ctx, envOptions{})
		org, owner := base.newTeamOrg(ctx)
		a := owner.activeAgent(ctx, org)
		owner.createTask(ctx, org, a.GetId(), "one")

		By("1000 agents in five teams and no team, each with spend today")
		_, err := base.pool.Exec(ctx, `INSERT INTO agents (org_id, display_name, harness, team, created_by)
			SELECT $1, 'bulk ' || g, $2, CASE WHEN g % 6 = 0 THEN '' ELSE 'team ' || (g % 6) END, $3
			FROM generate_series(1, 999) g`, org, a.GetHarness(), owner.userID)
		Expect(err).NotTo(HaveOccurred())
		_, err = base.pool.Exec(ctx, `INSERT INTO spend_entries (org_id, request_id, agent_id, key_id, model, input_tokens, cache_read_tokens, output_tokens, occurred_at, cost_usd, priced)
			SELECT org_id, 'bulk-' || id, id, 'k', 'm', 100, 50, 1, now(), 0.01, true FROM agents WHERE org_id = $1`, org)
		Expect(err).NotTo(HaveOccurred())
		_, err = base.pool.Exec(ctx, `ANALYZE agents, spend_entries, runs`)
		Expect(err).NotTo(HaveOccurred())

		counter := &queryCounter{names: map[string]int{}}
		traced := newEnvOn(ctx, base, envOptions{queryTracer: counter})
		c := owner.on(traced)
		page := func(size int32, tok string) *agentv1alpha1.ListAgentSummariesResponse {
			GinkgoHelper()
			res, err := c.agents.ListAgentSummaries(ctx, &agentv1alpha1.ListAgentSummariesRequest{OrgId: org, PageSize: size, PageToken: tok})
			Expect(err).NotTo(HaveOccurred())
			return res
		}
		page(1, "") // warm the session and the plans

		By("a page of 1 and a page of 200 cost the same queries")
		counter.reset()
		page(1, "")
		small, smallNames := counter.snapshot()
		counter.reset()
		page(200, "")
		large, largeNames := counter.snapshot()
		Expect(largeNames).To(HaveKeyWithValue("ListAgentSummaries", 1))
		Expect(large).To(Equal(small), "no query per agent: %v", largeNames)
		Expect(largeNames).To(Equal(smallNames))

		By("walking every page")
		seen := map[string]bool{}
		tok := ""
		started := time.Now()
		pages := 0
		for {
			t0 := time.Now()
			res := page(200, tok)
			Expect(time.Since(t0)).To(BeNumerically("<", 2*time.Second), "one page")
			pages++
			for _, s := range res.GetAgents() {
				Expect(seen).NotTo(HaveKey(s.GetAgentId()))
				seen[s.GetAgentId()] = true
				Expect(s.GetSpentTodayUsd()).To(Equal("0.01"))
			}
			if tok = res.GetNextPageToken(); tok == "" {
				break
			}
		}
		Expect(seen).To(HaveLen(1000))
		Expect(pages).To(Equal(5))
		Expect(time.Since(started)).To(BeNumerically("<", 5*time.Second), "1000 agents")
	})
})
