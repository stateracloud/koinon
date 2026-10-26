// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	connect "connectrpc.com/connect/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	guuid "github.com/google/uuid"

	"github.com/stateracloud/koinon/internal/db/store"
	agentv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/agent/v1alpha1"
	approvalv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/approval/v1alpha1"
	ticketv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/ticket/v1alpha1"
	"github.com/stateracloud/koinon/internal/savedwork"
)

// Stuck sandboxes and fresh sessions at the service layer (plan item AG1; ADRs 0051, 0054).
var _ = Describe("agent health and fresh sessions", func() {
	var (
		e     *env
		org   string
		owner *client
		agent *agentv1alpha1.Agent
	)
	BeforeEach(func(ctx SpecContext) {
		e = newEnv(ctx, envOptions{})
		org, owner = e.newTeamOrg(ctx)
		agent = owner.activeAgent(ctx, org)
	})

	// unhealthy is what koinon-agentd does to an agent after the loop guard.
	unhealthy := func(ctx SpecContext) {
		_, err := e.pool.Exec(ctx, `UPDATE agents SET unhealthy_at = now(), unhealthy_reason = 'stuck three times' WHERE id = $1`, agent.GetId())
		Expect(err).NotTo(HaveOccurred())
	}

	Describe("an unhealthy agent", func() {
		It("takes no new runs: creating a ticket and sending a message are refused with the reason", func(ctx SpecContext) {
			t, r := owner.createTicket(ctx, org, agent.GetId(), "before")
			_, err := e.pool.Exec(ctx, `UPDATE runs SET state = 'canceled', outcome_reason = 'test', finished_at = now(), ready_at = NULL WHERE id = $1`, r.GetId())
			Expect(err).NotTo(HaveOccurred())
			unhealthy(ctx)
			_, err = owner.tickets.CreateTicket(ctx, &ticketv1alpha1.CreateTicketRequest{OrgId: org, AgentId: agent.GetId(), Title: "after", Message: "hi"})
			Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition))
			Expect(err.Error()).To(And(ContainSubstring("unhealthy"), ContainSubstring("stuck three times"), ContainSubstring("resumes")))
			_, err = owner.tickets.AppendMessage(ctx, &ticketv1alpha1.AppendMessageRequest{OrgId: org, TicketId: t.GetId(), Text: "more"})
			Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition))
		})

		It("shows the state on the agent", func(ctx SpecContext) {
			unhealthy(ctx)
			res, err := owner.agents.GetAgent(ctx, &agentv1alpha1.GetAgentRequest{OrgId: org, AgentId: agent.GetId()})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.GetAgent().GetUnhealthyAt()).NotTo(BeNil())
			Expect(res.GetAgent().GetUnhealthyReason()).To(Equal("stuck three times"))
		})

		It("is resumed by an admin: audited, the count starts again, the stuck sandbox is marked for recreation", func(ctx SpecContext) {
			_, err := e.pool.Exec(ctx, `INSERT INTO org_workspaces (org_id, workspace) VALUES ($1, 'ws-health')`, org)
			Expect(err).NotTo(HaveOccurred())
			_, err = e.pool.Exec(ctx, `INSERT INTO agent_sandboxes (org_id, agent_id, workspace, sandbox_name, provider_name, key_secret_name, home_generation)
				VALUES ($1, $2, 'ws-health', 'sb-health', 'pv-health', 'key-health', 'gen-1')`, org, agent.GetId())
			Expect(err).NotTo(HaveOccurred())
			for range 3 {
				t, r := owner.createTicket(ctx, org, agent.GetId(), "x")
				_, err := e.pool.Exec(ctx, `UPDATE runs SET state = 'canceled', outcome_reason = 'test', finished_at = now(), ready_at = NULL WHERE id = $1`, r.GetId())
				Expect(err).NotTo(HaveOccurred())
				_, err = e.pool.Exec(ctx, `INSERT INTO sandbox_incidents (org_id, agent_id, run_id, task_id, processes, outcome, work)
					VALUES ($1, $2, $3, $4, '[]', 'recreate', 'nothing')`, org, agent.GetId(), r.GetId(), t.GetId())
				Expect(err).NotTo(HaveOccurred())
			}
			unhealthy(ctx)
			member := e.userIn(ctx, org, store.OrgRoleMember)
			_, err = member.agents.ResumeAgent(ctx, &agentv1alpha1.ResumeAgentRequest{OrgId: org, AgentId: agent.GetId()})
			Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))

			admin := e.userIn(ctx, org, store.OrgRoleAdmin)
			res, err := admin.agents.ResumeAgent(ctx, &agentv1alpha1.ResumeAgentRequest{OrgId: org, AgentId: agent.GetId()})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.GetAgent().GetUnhealthyAt()).To(BeNil())
			Expect(count(ctx, e, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'agent.resume' AND target_id = $2`, org, agent.GetId())).To(Equal(1))
			Expect(count(ctx, e, `SELECT count(*) FROM agent_sandboxes WHERE agent_id = $1 AND unhealthy_at IS NOT NULL AND unhealthy_generation = 'gen-1'`, agent.GetId())).To(Equal(1))
			recent := store.New(e.pool)
			n, err := recent.CountRecentSandboxRecreations(ctx, store.CountRecentSandboxRecreationsParams{OrgID: mustUUID(org), AgentID: mustUUID(agent.GetId())})
			Expect(err).NotTo(HaveOccurred())
			Expect(n).To(BeZero(), "resuming starts a new count")

			_, err = admin.agents.ResumeAgent(ctx, &agentv1alpha1.ResumeAgentRequest{OrgId: org, AgentId: agent.GetId()})
			Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition), "it is not unhealthy any more")
			_, _ = owner.createTicket(ctx, org, agent.GetId(), "runs again")
		})
	})

	Describe("activating with 'apply to open tickets'", func() {
		// open makes a ticket with a stored session; done is a finished one.
		open := func(ctx SpecContext, title string, status string) string {
			t, _ := owner.createTicket(ctx, org, agent.GetId(), title)
			_, err := e.pool.Exec(ctx, `UPDATE tasks SET harness_session_id = 'ses_1', harness_session_cwd = '/sandbox/work',
				harness_session_generation = 'gen-1' WHERE id = $1`, t.GetId())
			Expect(err).NotTo(HaveOccurred())
			if status == "done" {
				_, err = e.pool.Exec(ctx, `UPDATE tasks SET state = 'completed', finished_at = now() WHERE id = $1`, t.GetId())
				Expect(err).NotTo(HaveOccurred())
			}
			return t.GetId()
		}
		reactivate := func(ctx SpecContext, apply bool) *agentv1alpha1.ActivateAgentResponse {
			_, err := e.pool.Exec(ctx, `UPDATE agents SET status = 'draft' WHERE id = $1`, agent.GetId())
			Expect(err).NotTo(HaveOccurred())
			res, err := owner.agents.ActivateAgent(ctx, &agentv1alpha1.ActivateAgentRequest{
				OrgId: org, AgentId: agent.GetId(), Version: agent.GetLatestVersion(), ApplyToOpenTickets: apply,
			})
			Expect(err).NotTo(HaveOccurred())
			return res
		}
		pending := func(ctx SpecContext, id string) bool {
			var p bool
			Expect(e.pool.QueryRow(ctx, `SELECT session_restart_pending FROM tasks WHERE id = $1`, id).Scan(&p)).To(Succeed())
			return p
		}

		It("is off by default: no ticket is marked, and the activation is audited with the flag off", func(ctx SpecContext) {
			id := open(ctx, "one", "todo")
			res := reactivate(ctx, false)
			Expect(res.GetTicketsRestarting()).To(BeZero())
			Expect(pending(ctx, id)).To(BeFalse())
			var after string
			Expect(e.pool.QueryRow(ctx, `SELECT summary->'after'::text FROM audit_events WHERE org_id = $1 AND action = 'agent.activate' ORDER BY created_at DESC LIMIT 1`, org).Scan(&after)).To(Succeed())
			Expect(after).To(ContainSubstring(`"applyToOpenTickets": false`))
		})

		It("marks the open tickets that have a session and not the finished ones, and audits it", func(ctx SpecContext) {
			a := open(ctx, "one", "todo")
			b := open(ctx, "two", "in_progress")
			c := open(ctx, "three", "done")
			res := reactivate(ctx, true)
			Expect(res.GetTicketsRestarting()).To(Equal(int32(2)))
			Expect(pending(ctx, a)).To(BeTrue())
			Expect(pending(ctx, b)).To(BeTrue())
			Expect(pending(ctx, c)).To(BeFalse())
			var after string
			Expect(e.pool.QueryRow(ctx, `SELECT summary->'after'::text FROM audit_events WHERE org_id = $1 AND action = 'agent.activate' ORDER BY created_at DESC LIMIT 1`, org).Scan(&after)).To(Succeed())
			Expect(after).To(And(ContainSubstring(`"applyToOpenTickets": true`), ContainSubstring(`"ticketsRestarting": 2`)))
		})

		It("leaves other agents' tickets alone", func(ctx SpecContext) {
			other := owner.activeAgent(ctx, org)
			t, _ := owner.createTicket(ctx, org, other.GetId(), "theirs")
			_, err := e.pool.Exec(ctx, `UPDATE tasks SET harness_session_id = 's', harness_session_cwd = '/w' WHERE id = $1`, t.GetId())
			Expect(err).NotTo(HaveOccurred())
			reactivate(ctx, true)
			Expect(pending(ctx, t.GetId())).To(BeFalse())
		})
	})

	It("shows the agent's notes to anyone who can see the ticket, in GetTicket", func(ctx SpecContext) {
		t, _ := owner.createTicket(ctx, org, agent.GetId(), "notes")
		_, err := e.pool.Exec(ctx, `UPDATE tasks SET agent_summary = 'Where things stand.', agent_summary_at = now() WHERE id = $1`, t.GetId())
		Expect(err).NotTo(HaveOccurred())
		viewer := e.userIn(ctx, org, store.OrgRoleViewer)
		got := viewer.getTicket(ctx, org, t.GetId()).GetTicket()
		Expect(got.GetAgentSummary()).To(Equal("Where things stand."))
		Expect(got.GetAgentSummaryAt()).NotTo(BeNil())
		list, err := viewer.tickets.ListTickets(ctx, &ticketv1alpha1.ListTicketsRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		for _, lt := range list.GetTickets() {
			Expect(lt.GetAgentSummary()).To(BeEmpty(), "the list stays small")
		}
	})

	It("renders the approval to push saved work as a diffstat, without opening the files", func(ctx SpecContext) {
		t, r := owner.createTicket(ctx, org, agent.GetId(), "saved")
		sid := guuid.NewString()
		manifest := `{"repo":"acme/widgets","base":"` + strings.Repeat("a", 40) + `","files":[` +
			`{"path":"src/a.go","status":"M","mode":"100644","size":10,"added":3,"deleted":1},{"path":"old.txt","status":"D","size":0,"added":0,"deleted":7}],` +
			`"skipped":["acme/widgets: link: a symbolic link or submodule is not saved"]}`
		_, err := e.pool.Exec(ctx, `INSERT INTO saved_work (org_id, id, task_id, run_id, agent_id, repo, base_sha, branch, manifest, size_bytes, envelope, expires_at)
			VALUES ($1, $2, $3, $4, $5, 'acme/widgets', $6, 'koinon/wip/abc-1', $7, 10, '{"fmt":1}', now() + interval '14 days')`,
			org, sid, t.GetId(), r.GetId(), agent.GetId(), strings.Repeat("a", 40), manifest)
		Expect(err).NotTo(HaveOccurred())
		args := savedwork.PushArgsJSON(sid, "acme/widgets", strings.Repeat("a", 40), "koinon/wip/abc-1", strings.Repeat("b", 64), strings.Repeat("c", 64))
		sum := sha256.Sum256([]byte(args))
		var aid string
		Expect(e.pool.QueryRow(ctx, `INSERT INTO approval_requests (org_id, task_id, run_id, agent_id, connector, tool, target, args_json, args_sha256, decision, status, expires_at, system)
			VALUES ($1, $2, $3, $4, 'github', 'push_saved_work', 'github:acme/widgets', $5, $6, 'ask', 'pending', now() + interval '14 days', true) RETURNING id::text`,
			org, t.GetId(), r.GetId(), agent.GetId(), args, hex.EncodeToString(sum[:])).Scan(&aid)).To(Succeed())

		viewer := e.userIn(ctx, org, store.OrgRoleViewer)
		res, err := viewer.approvals.GetApproval(ctx, &approvalv1alpha1.GetApprovalRequest{OrgId: org, ApprovalId: aid})
		Expect(err).NotTo(HaveOccurred())
		a := res.GetApproval()
		Expect(a.GetSystem()).To(BeTrue())
		Expect(a.GetCanDecide()).To(BeFalse())
		sw := a.GetSavedWork()
		Expect(sw).NotTo(BeNil())
		Expect(sw.GetRepository()).To(Equal("acme/widgets"))
		Expect(sw.GetBranch()).To(Equal("koinon/wip/abc-1"))
		Expect(sw.GetFiles()).To(ConsistOf(
			HaveField("Path", "src/a.go"), HaveField("Path", "old.txt"),
		))
		Expect(sw.GetFiles()[0].GetAdded()).To(Equal(int32(3)))
		Expect(sw.GetSkipped()).To(HaveLen(1))
		Expect(sw.GetStatus()).To(Equal("saved"))
		Expect(sw.GetExpiresAt()).NotTo(BeNil())
	})
})
