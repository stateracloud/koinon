// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"fmt"
	"strings"

	connect "connectrpc.com/connect/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"github.com/stateracloud/koinon/internal/connectors/github"
	"github.com/stateracloud/koinon/internal/connectors/github/githubtest"
	"github.com/stateracloud/koinon/internal/db/store"
	agentv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/agent/v1alpha1"
	runv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/run/v1alpha1"
	templatev1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/template/v1alpha1"
	ticketv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/ticket/v1alpha1"
	"github.com/stateracloud/koinon/internal/services"
	"github.com/stateracloud/koinon/templates"
)

var _ = Describe("template sync", func() {
	It("records each template content once, keyed by content hash", func(ctx SpecContext) {
		e := newEnv(ctx, envOptions{})
		count := func() int { return count(ctx, e, `SELECT count(*) FROM templates`) }
		Expect(count()).To(Equal(len(mustTemplates())))

		_, err := services.SyncTemplates(ctx, e.pool, mustTemplates())
		Expect(err).NotTo(HaveOccurred())
		Expect(count()).To(Equal(len(mustTemplates())), "re-syncing known content adds no rows")

		orig, _ := templates.Get("repo-helper")
		changed := *orig
		changed.Metadata.Version = "0.2.0"
		changed.Hash = "sha256:" + strings.Repeat("ab", 32)
		_, err = services.SyncTemplates(ctx, e.pool, []*templates.Template{&changed})
		Expect(err).NotTo(HaveOccurred())
		Expect(count()).To(Equal(len(mustTemplates()) + 1))

		_, owner := e.newTeamOrg(ctx)
		t := owner.repoHelper(ctx)
		Expect(t.GetVersion()).To(Equal("0.2.0"), "the most recently synced content is current")
		Expect(t.GetContentHash()).To(Equal(strings.Repeat("ab", 32)))
	})

	It("lists the repo-helper template with its harnesses, grants and caps", func(ctx SpecContext) {
		e := newEnv(ctx, envOptions{})
		_, owner := e.newTeamOrg(ctx)
		t := owner.repoHelper(ctx)
		orig, _ := templates.Get("repo-helper")
		Expect("sha256:" + t.GetContentHash()).To(Equal(orig.Hash))
		Expect(t.GetDefaultHarness()).To(Equal("opencode"))
		Expect(t.GetAllowedHarnesses()).To(ConsistOf("opencode"))
		// The deprecated enum fields stay filled for older clients.
		Expect(t.GetDefaultFlavor()).To(Equal(templatev1alpha1.Flavor_FLAVOR_OPENCODE))      //nolint:staticcheck // deprecated field
		Expect(t.GetAllowedFlavors()).To(ConsistOf(templatev1alpha1.Flavor_FLAVOR_OPENCODE)) //nolint:staticcheck // deprecated field
		Expect(t.GetGrants()).To(ConsistOf(SatisfyAll(
			HaveField("Connector", "github"),
			HaveField("Scopes", ConsistOf("repo.read", "pull_request.create", "issue.comment")),
			HaveField("Preset", templatev1alpha1.Preset_PRESET_STANDARD),
		)))
		Expect(t.GetRunLimits().GetMaxDurationSeconds()).To(Equal(int64(1800)))
		Expect(t.GetRunLimits().GetMaxModelRequests()).To(Equal(int64(300)))
		Expect(t.GetRunLimits().GetMaxSpendUsd()).To(Equal("5.00"))
		Expect(t.GetSpendLimitUsd()).To(Equal("10.00"))
	})
})

var _ = Describe("AgentService", func() {
	var (
		e     *env
		org   string
		owner *client
	)
	BeforeEach(func(ctx SpecContext) {
		e = newEnv(ctx, envOptions{})
		org, owner = e.newTeamOrg(ctx)
	})

	It("creates drafts from a template that propose the template's grants", func(ctx SpecContext) {
		member := e.userIn(ctx, org, store.OrgRoleMember)
		a := member.draftAgent(ctx, org)
		Expect(a.GetStatus()).To(Equal(agentv1alpha1.AgentStatus_AGENT_STATUS_DRAFT))
		Expect(a.GetHarness()).To(Equal("opencode"))
		Expect(a.GetFlavor()).To(Equal(templatev1alpha1.Flavor_FLAVOR_OPENCODE)) //nolint:staticcheck // deprecated field
		Expect(a.GetTemplateSlug()).To(Equal("repo-helper"))
		Expect(a.GetLatestVersion()).To(Equal(int32(1)))
		Expect(a.GetActiveVersion()).To(BeZero())
		Expect(a.GetProposedGrants()).To(HaveLen(1))
		Expect(a.GetApprovedGrants()).To(BeEmpty())
		Expect(a.GetInstructions()).To(ContainSubstring("Repo helper"))
	})

	Describe("repository binding", func() {
		create := func(ctx SpecContext, grants ...*agentv1alpha1.GrantBinding) (*agentv1alpha1.Agent, error) {
			res, err := owner.agents.CreateAgentFromTemplate(ctx, &agentv1alpha1.CreateAgentFromTemplateRequest{
				OrgId: org, TemplateId: owner.repoHelper(ctx).GetId(), Grants: grants,
			})
			return res.GetAgent(), err
		}
		binding := func(ctx SpecContext, repos ...string) *agentv1alpha1.GrantBinding {
			return e.repoGrant(ctx, org, repos...)[0]
		}

		It("stores the binding with the version, and activation approves it", func(ctx SpecContext) {
			a, err := create(ctx, binding(ctx, "acme/widgets"))
			Expect(err).NotTo(HaveOccurred())
			conn := e.githubConnection(ctx, org)
			Expect(a.GetProposedGrants()).To(ConsistOf(SatisfyAll(
				HaveField("Connector", "github"),
				HaveField("ConnectionId", conn),
				HaveField("Repositories", []string{"acme/widgets"}),
				HaveField("TargetsMin", int32(1)), HaveField("TargetsMax", int32(1)),
			)))
			var stored string
			Expect(e.pool.QueryRow(ctx, `SELECT config->'bindings'->0->>'targets' FROM agent_versions WHERE agent_id = $1`, a.GetId()).
				Scan(&stored)).To(Succeed())
			Expect(stored).To(Equal(`["acme/widgets"]`))

			res, err := owner.agents.ActivateAgent(ctx, &agentv1alpha1.ActivateAgentRequest{OrgId: org, AgentId: a.GetId(), Version: 1})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.GetAgent().GetApprovedGrants()).To(ConsistOf(SatisfyAll(
				HaveField("ConnectionId", conn), HaveField("Repositories", []string{"acme/widgets"}),
			)))
		})

		It("checks the repositories against the installation with a metadata-read token", func(ctx SpecContext) {
			before := len(e.opts.ghFake.Requests())
			_, err := create(ctx, binding(ctx, "acme/gadgets"))
			Expect(err).NotTo(HaveOccurred())
			reqs := e.opts.ghFake.Requests()[before:]
			Expect(reqs).To(ContainElement(SatisfyAll(
				HaveField("Repositories", []string{"gadgets"}),
				HaveField("Permissions", map[string]string{"metadata": "read"}),
			)))

			_, err = create(ctx, binding(ctx, "acme/not-installed"))
			Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
			Expect(err.Error()).To(ContainSubstring("cannot access"))
			_, err = create(ctx, binding(ctx, "someone-else/widgets"))
			Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument), "the owner must be the installation's account")
		})

		It("stores GitHub's spelling of the repository names", func(ctx SpecContext) {
			// GitHub matches names case-insensitively; OpenShell's read grant matches paths
			// case-sensitively, so the binding keeps the full_name GitHub reports.
			gh := githubtest.New()
			DeferCleanup(gh.Server.Close)
			gh.Account, gh.Repositories = "Acme", []string{"Widgets"}
			ce := newEnv(ctx, envOptions{github: github.NewClient(github.Options{BaseURL: gh.URL()}), ghFake: gh})
			corg, cowner := ce.newTeamOrg(ctx)
			res, err := cowner.agents.CreateAgentFromTemplate(ctx, &agentv1alpha1.CreateAgentFromTemplateRequest{
				OrgId: corg, TemplateId: cowner.repoHelper(ctx).GetId(), Grants: ce.repoGrant(ctx, corg, "acme/WIDGETS"),
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.GetAgent().GetProposedGrants()).To(ConsistOf(HaveField("Repositories", []string{"Acme/Widgets"})))
			var stored string
			Expect(ce.pool.QueryRow(ctx, `SELECT config->'bindings'->0->>'targets' FROM agent_versions WHERE agent_id = $1`,
				res.GetAgent().GetId()).Scan(&stored)).To(Succeed())
			Expect(stored).To(Equal(`["Acme/Widgets"]`))
		})

		DescribeTable("rejects bindings that do not fit the template or the org",
			func(ctx SpecContext, mutate func(ctx SpecContext, b *agentv1alpha1.GrantBinding) []*agentv1alpha1.GrantBinding, want string) {
				b := binding(ctx, "acme/widgets")
				_, err := create(ctx, mutate(ctx, b)...)
				Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument), "%v", err)
				Expect(err.Error()).To(ContainSubstring(want))
				Expect(count(ctx, e, `SELECT count(*) FROM agents`)).To(BeZero())
			},
			Entry("no binding for a grant with targets min 1", func(SpecContext, *agentv1alpha1.GrantBinding) []*agentv1alpha1.GrantBinding {
				return nil
			}, "needs a connection"),
			Entry("more repositories than targets max", func(_ SpecContext, b *agentv1alpha1.GrantBinding) []*agentv1alpha1.GrantBinding {
				b.Repositories = []string{"acme/widgets", "acme/gadgets"}
				return []*agentv1alpha1.GrantBinding{b}
			}, "1 to 1 repositories, not 2"),
			Entry("no repositories", func(_ SpecContext, b *agentv1alpha1.GrantBinding) []*agentv1alpha1.GrantBinding {
				b.Repositories = nil
				return []*agentv1alpha1.GrantBinding{b}
			}, "not 0"),
			Entry("a malformed repository", func(_ SpecContext, b *agentv1alpha1.GrantBinding) []*agentv1alpha1.GrantBinding {
				b.Repositories = []string{"acme/../etc"}
				return []*agentv1alpha1.GrantBinding{b}
			}, "is not a repository"),
			Entry("a connector the template does not grant", func(_ SpecContext, b *agentv1alpha1.GrantBinding) []*agentv1alpha1.GrantBinding {
				b.Connector = "slack"
				return []*agentv1alpha1.GrantBinding{b}
			}, "no grant on connector"),
			Entry("the grant bound twice", func(_ SpecContext, b *agentv1alpha1.GrantBinding) []*agentv1alpha1.GrantBinding {
				return []*agentv1alpha1.GrantBinding{b, b}
			}, "bound twice"),
			Entry("an unknown connection", func(_ SpecContext, b *agentv1alpha1.GrantBinding) []*agentv1alpha1.GrantBinding {
				b.ConnectionId = "019a0000-0000-7000-8000-000000000000"
				return []*agentv1alpha1.GrantBinding{b}
			}, "connection not found"),
			Entry("another org's connection", func(ctx SpecContext, b *agentv1alpha1.GrantBinding) []*agentv1alpha1.GrantBinding {
				other, _ := e.newTeamOrg(ctx)
				b.ConnectionId = e.githubConnection(ctx, other)
				return []*agentv1alpha1.GrantBinding{b}
			}, "connection not found"),
		)
	})

	It("rejects a harness the template does not allow", func(ctx SpecContext) {
		_, err := owner.agents.CreateAgentFromTemplate(ctx, &agentv1alpha1.CreateAgentFromTemplateRequest{
			OrgId: org, TemplateId: owner.repoHelper(ctx).GetId(), Harness: "no-such-harness",
		})
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
	})

	It("still reads the deprecated flavor field when the request names no harness", func(ctx SpecContext) {
		a, err := owner.agents.CreateAgentFromTemplate(ctx, &agentv1alpha1.CreateAgentFromTemplateRequest{
			OrgId: org, TemplateId: owner.repoHelper(ctx).GetId(), DisplayName: "By flavor",
			Flavor: templatev1alpha1.Flavor_FLAVOR_OPENCODE, //nolint:staticcheck // deprecated field
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(a.GetAgent().GetHarness()).To(Equal("opencode"))

		_, err = owner.agents.CreateAgentFromTemplate(ctx, &agentv1alpha1.CreateAgentFromTemplateRequest{
			OrgId: org, TemplateId: owner.repoHelper(ctx).GetId(),
			Flavor: templatev1alpha1.Flavor_FLAVOR_HERMES, //nolint:staticcheck // deprecated field and value
		})
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
	})

	It("lets only admins and owners activate, which approves the proposed grants", func(ctx SpecContext) {
		member := e.userIn(ctx, org, store.OrgRoleMember)
		a := member.draftAgent(ctx, org)
		_, err := member.agents.ActivateAgent(ctx, &agentv1alpha1.ActivateAgentRequest{
			OrgId: org, AgentId: a.GetId(), Version: 1,
		})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))

		admin := e.userIn(ctx, org, store.OrgRoleAdmin)
		_, err = admin.agents.ActivateAgent(ctx, &agentv1alpha1.ActivateAgentRequest{
			OrgId: org, AgentId: a.GetId(), Version: 2,
		})
		Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition), "must name the reviewed version")

		res, err := admin.agents.ActivateAgent(ctx, &agentv1alpha1.ActivateAgentRequest{
			OrgId: org, AgentId: a.GetId(), Version: 1,
		})
		Expect(err).NotTo(HaveOccurred())
		got := res.GetAgent()
		Expect(got.GetStatus()).To(Equal(agentv1alpha1.AgentStatus_AGENT_STATUS_ACTIVE))
		Expect(got.GetActiveVersion()).To(Equal(int32(1)))
		Expect(got.GetApprovedGrants()).To(Equal(got.GetProposedGrants()))
		Expect(got.GetActivatedAt()).NotTo(BeNil())

		_, err = admin.agents.ActivateAgent(ctx, &agentv1alpha1.ActivateAgentRequest{
			OrgId: org, AgentId: a.GetId(), Version: 1,
		})
		Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition))
	})

	It("lets viewers read but not create", func(ctx SpecContext) {
		a := owner.activeAgent(ctx, org)
		viewer := e.userIn(ctx, org, store.OrgRoleViewer)
		res, err := viewer.agents.ListAgents(ctx, &agentv1alpha1.ListAgentsRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetAgents()).To(ConsistOf(HaveField("Id", a.GetId())))
		_, err = viewer.agents.CreateAgentFromTemplate(ctx, &agentv1alpha1.CreateAgentFromTemplateRequest{
			OrgId: org, TemplateId: owner.repoHelper(ctx).GetId(),
		})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
		_, err = viewer.tickets.CreateTicket(ctx, &ticketv1alpha1.CreateTicketRequest{
			OrgId: org, AgentId: a.GetId(), Title: "nope",
		})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
	})

	It("archives an agent and cancels its unfinished runs; only admins may", func(ctx SpecContext) {
		a := owner.activeAgent(ctx, org)
		t1, r1 := owner.createTicket(ctx, org, a.GetId(), "one")
		_, r2 := owner.createTicket(ctx, org, a.GetId(), "two")
		e.agentd("agentd-a").mustClaim(ctx, org) // r1 runs, r2 queued

		member := e.userIn(ctx, org, store.OrgRoleMember)
		_, err := member.agents.ArchiveAgent(ctx, &agentv1alpha1.ArchiveAgentRequest{OrgId: org, AgentId: a.GetId()})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))

		res, err := owner.agents.ArchiveAgent(ctx, &agentv1alpha1.ArchiveAgentRequest{OrgId: org, AgentId: a.GetId()})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetAgent().GetStatus()).To(Equal(agentv1alpha1.AgentStatus_AGENT_STATUS_ARCHIVED))
		for _, r := range []*runv1alpha1.Run{r1, r2} {
			Expect(owner.getRun(ctx, org, r.GetId()).GetState()).To(Equal(runv1alpha1.RunState_RUN_STATE_CANCELED))
		}
		Expect(ticketKinds(owner.getTicket(ctx, org, t1.GetId()))).To(ContainElement("run.canceled"))

		_, err = owner.tickets.CreateTicket(ctx, &ticketv1alpha1.CreateTicketRequest{
			OrgId: org, AgentId: a.GetId(), Title: "after archive",
		})
		Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition))
	})
})

var _ = Describe("TicketService and run scheduling", func() {
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

	It("queues a run with the template's caps when a member creates a ticket", func(ctx SpecContext) {
		t, r := member.createTicket(ctx, org, agent.GetId(), "add a badge")
		Expect(t.GetStatus()).To(Equal(ticketv1alpha1.TicketStatus_TICKET_STATUS_TODO))
		Expect(r.GetState()).To(Equal(runv1alpha1.RunState_RUN_STATE_QUEUED))
		Expect(r.GetMaxWallClockSeconds()).To(Equal(int64(1800)))
		Expect(r.GetMaxModelRequests()).To(Equal(int64(300)))
		Expect(r.GetMaxSpendUsd()).To(Equal("5.00"))

		got := member.getTicket(ctx, org, t.GetId())
		Expect(ticketKinds(got)).To(Equal([]string{"ticket.created", "message", "run.queued"}))
		Expect(got.GetEvents()[1].GetPayload().AsMap()).To(HaveKeyWithValue("text", "please add a badge"))
		Expect(got.GetEvents()[1].GetActorUserId()).To(Equal(member.userID))
		Expect(got.GetRuns()).To(ConsistOf(HaveField("Id", r.GetId())))
	})

	It("refuses tickets for draft agents", func(ctx SpecContext) {
		draft := owner.draftAgent(ctx, org)
		_, err := member.tickets.CreateTicket(ctx, &ticketv1alpha1.CreateTicketRequest{
			OrgId: org, AgentId: draft.GetId(), Title: "x",
		})
		Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition))
	})

	It("queues one run per message unless one is already queued", func(ctx SpecContext) {
		t, r1 := member.createTicket(ctx, org, agent.GetId(), "first")
		res, err := member.tickets.AppendMessage(ctx, &ticketv1alpha1.AppendMessageRequest{
			OrgId: org, TicketId: t.GetId(), Text: "also this",
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetRun()).To(BeNil(), "the queued run will see the message")
		Expect(res.GetEvent().GetKind()).To(Equal("message"))

		running := ad.mustClaim(ctx, org)
		Expect(running.ID.String()).To(Equal(r1.GetId()))
		res, err = member.tickets.AppendMessage(ctx, &ticketv1alpha1.AppendMessageRequest{
			OrgId: org, TicketId: t.GetId(), Text: "and one more",
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetRun().GetState()).To(Equal(runv1alpha1.RunState_RUN_STATE_QUEUED))
	})

	It("queues concurrent messages on one ticket at most once", func(ctx SpecContext) {
		t, _ := member.createTicket(ctx, org, agent.GetId(), "first")
		ad.mustClaim(ctx, org)
		done := make(chan error, 8)
		for range 8 {
			go func() {
				_, err := member.tickets.AppendMessage(ctx, &ticketv1alpha1.AppendMessageRequest{
					OrgId: org, TicketId: t.GetId(), Text: "ping",
				})
				done <- err
			}()
		}
		for range 8 {
			Expect(<-done).To(Succeed())
		}
		Expect(count(ctx, e, `SELECT count(*) FROM runs WHERE state = 'queued'`)).To(Equal(1))
	})

	It("runs one ticket per agent at a time, in FIFO order", func(ctx SpecContext) {
		_, r1 := member.createTicket(ctx, org, agent.GetId(), "one")
		_, r2 := member.createTicket(ctx, org, agent.GetId(), "two")
		_, r3 := member.createTicket(ctx, org, agent.GetId(), "three")

		first := ad.mustClaim(ctx, org)
		Expect(first.ID.String()).To(Equal(r1.GetId()))
		_, err := e.agentd("agentd-b").claim(ctx, org)
		Expect(err).To(HaveOccurred(), "the agent's slot is taken")

		ad.finish(ctx, first)
		Expect(ad.mustClaim(ctx, org).ID.String()).To(Equal(r2.GetId()))
		Expect(member.getRun(ctx, org, r3.GetId()).GetState()).To(Equal(runv1alpha1.RunState_RUN_STATE_QUEUED))
	})

	// Behaviour from old issue 19, awaiting procoduck's decision: a parked run does not
	// hold the agent's slot, so another ticket runs while one waits for an
	// approval. A ticket's own next run still waits for its parked run.
	It("lets another ticket run while one is parked, but not the parked ticket's next run", func(ctx SpecContext) {
		t1, r1 := member.createTicket(ctx, org, agent.GetId(), "one")
		_, r2 := member.createTicket(ctx, org, agent.GetId(), "two")
		parked := ad.park(ctx, ad.mustClaim(ctx, org))
		Expect(parked.ID.String()).To(Equal(r1.GetId()))

		res, err := member.tickets.AppendMessage(ctx, &ticketv1alpha1.AppendMessageRequest{
			OrgId: org, TicketId: t1.GetId(), Text: "while you wait",
		})
		Expect(err).NotTo(HaveOccurred())
		follow := res.GetRun()
		Expect(follow).NotTo(BeNil())

		next := ad.mustClaim(ctx, org)
		Expect(next.ID.String()).To(Equal(r2.GetId()), "the parked run frees the agent")
		ad.finish(ctx, next)
		_, err = ad.claim(ctx, org)
		Expect(err).To(HaveOccurred(), "ticket one's follow-up waits for its parked run")
		Expect(member.getRun(ctx, org, follow.GetId()).GetState()).To(Equal(runv1alpha1.RunState_RUN_STATE_QUEUED))
	})

	It("cancels a ticket's queued runs when it moves to Done", func(ctx SpecContext) {
		member.createTicket(ctx, org, agent.GetId(), "blocker")
		running := ad.mustClaim(ctx, org)
		t, r := member.createTicket(ctx, org, agent.GetId(), "queued")

		res, err := member.tickets.MoveTicket(ctx, &ticketv1alpha1.MoveTicketRequest{
			OrgId: org, TicketId: t.GetId(), Status: ticketv1alpha1.TicketStatus_TICKET_STATUS_DONE,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetTicket().GetStatus()).To(Equal(ticketv1alpha1.TicketStatus_TICKET_STATUS_DONE))
		Expect(res.GetCanceledRuns()).To(ConsistOf(HaveField("Id", r.GetId())))
		got := member.getTicket(ctx, org, t.GetId())
		last := got.GetEvents()[len(got.GetEvents())-1]
		Expect(last.GetKind()).To(Equal("run.canceled"))
		Expect(last.GetPayload().AsMap()).To(HaveKeyWithValue("reason", "ticket_done"))
		Expect(ticketKinds(got)).To(ContainElement("status.changed"))

		// Done refuses messages; the running run of another ticket is untouched.
		_, err = member.tickets.AppendMessage(ctx, &ticketv1alpha1.AppendMessageRequest{
			OrgId: org, TicketId: t.GetId(), Text: "hello?",
		})
		Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition))
		Expect(member.getRun(ctx, org, running.ID.String()).GetState()).To(Equal(runv1alpha1.RunState_RUN_STATE_RUNNING))

		list, err := member.tickets.ListTickets(ctx, &ticketv1alpha1.ListTicketsRequest{
			OrgId: org, Status: ticketv1alpha1.TicketStatus_TICKET_STATUS_DONE,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(list.GetTickets()).To(ConsistOf(HaveField("Id", t.GetId())))
		list, err = member.tickets.ListTickets(ctx, &ticketv1alpha1.ListTicketsRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		Expect(list.GetTickets()).To(HaveLen(2))
	})

	It("cancels a run on request, with a ticket event naming the user", func(ctx SpecContext) {
		t, r := member.createTicket(ctx, org, agent.GetId(), "cancel me")
		res, err := member.runs.CancelRun(ctx, &runv1alpha1.CancelRunRequest{OrgId: org, RunId: r.GetId()})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetRun().GetState()).To(Equal(runv1alpha1.RunState_RUN_STATE_CANCELED))
		got := member.getTicket(ctx, org, t.GetId())
		last := got.GetEvents()[len(got.GetEvents())-1]
		Expect(last.GetKind()).To(Equal("run.canceled"))
		Expect(last.GetActorUserId()).To(Equal(member.userID))
		Expect(last.GetPayload().AsMap()).To(HaveKeyWithValue("reason", "user"))

		_, err = member.runs.CancelRun(ctx, &runv1alpha1.CancelRunRequest{OrgId: org, RunId: r.GetId()})
		Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition))
	})

	Describe("run caps", func() {
		It("cancels runs past the wall-clock cap, not counting parked time", func(ctx SpecContext) {
			tOver, rOver := member.createTicket(ctx, org, agent.GetId(), "slow")
			over := ad.mustClaim(ctx, org)
			Expect(over.ID.String()).To(Equal(rOver.GetId()))
			// 31 minutes owned: over the 30-minute cap.
			_, err := e.pool.Exec(ctx, `UPDATE runs SET started_at = now() - interval '31 minutes' WHERE id = $1`, over.ID)
			Expect(err).NotTo(HaveOccurred())

			n, err := sweep(ctx, e)
			Expect(err).NotTo(HaveOccurred())
			Expect(n).To(Equal(1))
			got := member.getRun(ctx, org, rOver.GetId())
			Expect(got.GetState()).To(Equal(runv1alpha1.RunState_RUN_STATE_CANCELED))
			Expect(got.GetOutcomeReason()).To(ContainSubstring("wall-clock cap"))
			ticket := member.getTicket(ctx, org, tOver.GetId())
			last := ticket.GetEvents()[len(ticket.GetEvents())-1]
			Expect(last.GetKind()).To(Equal("run.canceled"))
			Expect(last.GetActorKind()).To(Equal(ticketv1alpha1.ActorKind_ACTOR_KIND_SYSTEM))
			Expect(last.GetPayload().AsMap()).To(HaveKeyWithValue("reason", "wall_clock_cap"))

			// Started 40 minutes ago but parked for 20 of them: within the cap.
			_, rIn := member.createTicket(ctx, org, agent.GetId(), "patient")
			in := ad.park(ctx, ad.mustClaim(ctx, org))
			Expect(in.ID.String()).To(Equal(rIn.GetId()))
			_, err = e.pool.Exec(ctx, `UPDATE runs SET started_at = now() - interval '40 minutes',
				parked_at = now() - interval '20 minutes' WHERE id = $1`, in.ID)
			Expect(err).NotTo(HaveOccurred())
			_, err = e.agentd("x").q.ResolveRunPark(ctx, store.ResolveRunParkParams{OrgID: in.OrgID, ID: in.ID})
			Expect(err).NotTo(HaveOccurred())
			resumed := ad.mustClaim(ctx, org)
			Expect(resumed.ID).To(Equal(in.ID))
			Expect(resumed.ParkedSeconds).To(BeNumerically(">=", 20*60))
			n, err = sweep(ctx, e)
			Expect(err).NotTo(HaveOccurred())
			Expect(n).To(BeZero())
			Expect(member.getRun(ctx, org, rIn.GetId()).GetState()).To(Equal(runv1alpha1.RunState_RUN_STATE_RUNNING))
		})

		It("writes the agent's last reply into the note of a run it cancels at the cap", func(ctx SpecContext) {
			t, r := member.createTicket(ctx, org, agent.GetId(), "slow with a reply")
			run := ad.mustClaim(ctx, org)
			Expect(run.ID.String()).To(Equal(r.GetId()))
			_, err := ad.q.AppendRunEventOnce(ctx, store.AppendRunEventOnceParams{
				OrgID: run.OrgID, RunID: run.ID, Epoch: run.Epoch, Kind: "acp.agent_message_chunk",
				Payload: []byte(`{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"partial"}}`), SourceSeq: 1,
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = e.pool.Exec(ctx, `UPDATE runs SET started_at = now() - interval '31 minutes' WHERE id = $1`, run.ID)
			Expect(err).NotTo(HaveOccurred())

			n, err := sweep(ctx, e)
			Expect(err).NotTo(HaveOccurred())
			Expect(n).To(Equal(1))
			ticket := member.getTicket(ctx, org, t.GetId())
			last := ticket.GetEvents()[len(ticket.GetEvents())-1]
			Expect(last.GetKind()).To(Equal("run.canceled"))
			Expect(last.GetPayload().AsMap()).To(HaveKeyWithValue("reason", "wall_clock_cap"))
			Expect(last.GetPayload().AsMap()).To(HaveKeyWithValue("summary", "partial"))
			Expect(last.GetPayload().AsMap()).To(HaveKeyWithValue("summarySource", "reply"))
			Expect(last.GetPayload().AsMap()).To(HaveKeyWithValue("message", "Stopped at the time limit of 30m."))
		})

		It("reports the tokens a run used without ending it at any total (ADR 0081)", func(ctx SpecContext) {
			_, r := member.createTicket(ctx, org, agent.GetId(), "chatty")
			run := ad.mustClaim(ctx, org)
			_, err := recordTokens(ctx, e, run, 1_999_999)
			Expect(err).NotTo(HaveOccurred())
			got, err := recordTokens(ctx, e, run, 40_000_000)
			Expect(err).NotTo(HaveOccurred())
			Expect(got.State).To(Equal(store.RunStateRunning), "tokens are reported, not enforced")
			Expect(member.getRun(ctx, org, r.GetId()).GetTokensUsed()).To(Equal(int64(41_999_999)))
		})

		It("shows the recorded spend of each run and of the ticket", func(ctx SpecContext) {
			t, r := member.createTicket(ctx, org, agent.GetId(), "costly")
			run := ad.mustClaim(ctx, org)
			for i, cost := range []string{"0.0015", "0.25"} {
				_, err := e.pool.Exec(ctx, `INSERT INTO spend_entries (org_id, agent_id, run_id, task_id, request_id,
					key_id, model, input_tokens, output_tokens, cost_usd, priced, occurred_at)
					VALUES ($1, $2, $3, $4, $5, 'k-1', 'm', 100, 20, $6::numeric, true, now())`,
					run.OrgID, run.AgentID, run.ID, run.TaskID, fmt.Sprintf("req-%s-%d", r.GetId(), i), cost)
				Expect(err).NotTo(HaveOccurred())
			}
			want := &runv1alpha1.Cost{Requests: 2, InputTokens: 200, OutputTokens: 40, Usd: "0.2515", Priced: true}
			Expect(proto.Equal(member.getRun(ctx, org, r.GetId()).GetCost(), want)).To(BeTrue())

			ticket := member.getTicket(ctx, org, t.GetId())
			Expect(proto.Equal(ticket.GetTicket().GetCost(), want)).To(BeTrue())
			Expect(proto.Equal(ticket.GetRuns()[0].GetCost(), want)).To(BeTrue())

			// A ticket without spend shows zero.
			t2, _ := member.createTicket(ctx, org, agent.GetId(), "free")
			Expect(member.getTicket(ctx, org, t2.GetId()).GetTicket().GetCost().GetUsd()).To(Equal("0.00"))
		})
	})
})
