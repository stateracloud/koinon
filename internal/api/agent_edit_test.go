// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	connect "connectrpc.com/connect/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/stateracloud/koinon/internal/db/store"
	agentv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/agent/v1alpha1"
	templatev1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/template/v1alpha1"
)

// The agent editor of ADR 0126 section 5: an agent without a template, and UpdateAgent. Every
// change is a proposed version; only ActivateAgent (admins and owners) puts it in effect (ADR 0020).
var _ = Describe("agent editor", func() {
	var (
		e     *env
		org   string
		owner *client
	)

	BeforeEach(func(ctx SpecContext) {
		e = newEnv(ctx, envOptions{})
		org, owner = e.newTeamOrg(ctx)
	})

	readGrant := func(ctx SpecContext) *templatev1alpha1.Grant {
		return &templatev1alpha1.Grant{
			Connector: "github", Scopes: []string{"repo.read"}, Preset: templatev1alpha1.Preset_PRESET_READ_ONLY,
			Purpose: "Read the repository.", ConnectionId: e.githubConnection(ctx, org), Repositories: []string{"acme/widgets"},
		}
	}
	blank := func(ctx SpecContext, c *client, d *agentv1alpha1.AgentDefinition) (*agentv1alpha1.Agent, error) {
		res, err := c.agents.CreateAgent(ctx, &agentv1alpha1.CreateAgentRequest{OrgId: org, Definition: d})
		return res.GetAgent(), err
	}
	definition := func(ctx SpecContext) *agentv1alpha1.AgentDefinition {
		return &agentv1alpha1.AgentDefinition{
			DisplayName: "Widget reader", Instructions: "Read the widgets repository and answer questions.",
			Grants: []*templatev1alpha1.Grant{readGrant(ctx)},
		}
	}
	update := func(ctx SpecContext, c *client, a *agentv1alpha1.Agent, d *agentv1alpha1.AgentDefinition, paths ...string) (*agentv1alpha1.Agent, error) {
		res, err := c.agents.UpdateAgent(ctx, &agentv1alpha1.UpdateAgentRequest{
			OrgId: org, AgentId: a.GetId(), BaseVersion: a.GetLatestVersion(), Definition: d,
			UpdateMask: &fieldmaskpb.FieldMask{Paths: paths},
		})
		return res.GetAgent(), err
	}
	activate := func(ctx SpecContext, c *client, a *agentv1alpha1.Agent) (*agentv1alpha1.Agent, error) {
		res, err := c.agents.ActivateAgent(ctx, &agentv1alpha1.ActivateAgentRequest{OrgId: org, AgentId: a.GetId(), Version: a.GetLatestVersion()})
		return res.GetAgent(), err
	}
	audits := func(ctx SpecContext, action, agent string) int {
		return count(ctx, e, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = $2 AND target_id = $3`, org, action, agent)
	}

	Describe("CreateAgent", func() {
		It("makes a draft that only an admin activates, with the same check as a template agent", func(ctx SpecContext) {
			member := e.userIn(ctx, org, store.OrgRoleMember)
			a, err := blank(ctx, member, definition(ctx))
			Expect(err).NotTo(HaveOccurred())
			Expect(a.GetStatus()).To(Equal(agentv1alpha1.AgentStatus_AGENT_STATUS_DRAFT))
			Expect(a.GetTemplateId()).To(BeEmpty())
			Expect(a.GetHarness()).To(Equal("opencode"), "the platform's default harness")
			Expect(a.GetLatestVersion()).To(Equal(int32(1)))
			Expect(a.GetActiveVersion()).To(BeZero())
			Expect(a.GetProposedGrants()).To(HaveLen(1))
			Expect(a.GetApprovedGrants()).To(BeEmpty())
			Expect(audits(ctx, "agent.create", a.GetId())).To(Equal(1))
			var limit string
			Expect(e.pool.QueryRow(ctx, `SELECT daily_limit::text FROM agent_budgets WHERE agent_id = $1`, a.GetId()).Scan(&limit)).To(Succeed())
			Expect(limit).To(HavePrefix("1.00"), "the blank default, not a caller's choice")

			_, err = activate(ctx, member, a)
			Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied), "a member cannot activate")

			got, err := activate(ctx, owner, a)
			Expect(err).NotTo(HaveOccurred())
			Expect(got.GetStatus()).To(Equal(agentv1alpha1.AgentStatus_AGENT_STATUS_ACTIVE))
			Expect(got.GetActiveVersion()).To(Equal(int32(1)))
			Expect(got.GetApprovedGrants()).To(HaveLen(1))
			Expect(audits(ctx, "agent.activate", a.GetId())).To(Equal(1))
		})

		It("checks the definition as a template is checked", func(ctx SpecContext) {
			bad := func(mod func(*agentv1alpha1.AgentDefinition)) error {
				d := definition(ctx)
				mod(d)
				_, err := blank(ctx, owner, d)
				return err
			}
			for name, mod := range map[string]func(*agentv1alpha1.AgentDefinition){
				"no name":         func(d *agentv1alpha1.AgentDefinition) { d.DisplayName = " " },
				"no instructions": func(d *agentv1alpha1.AgentDefinition) { d.Instructions = "" },
				"unknown harness": func(d *agentv1alpha1.AgentDefinition) { d.Harness = "nope" },
				"unserved model":  func(d *agentv1alpha1.AgentDefinition) { d.Model = "nobody/serves-this" },
				"unknown scope":   func(d *agentv1alpha1.AgentDefinition) { d.Grants[0].Scopes = []string{"repo.admin"} },
				"unknown type":    func(d *agentv1alpha1.AgentDefinition) { d.Grants[0].Connector = "nope" },
				"no preset": func(d *agentv1alpha1.AgentDefinition) {
					d.Grants[0].Preset = templatev1alpha1.Preset_PRESET_UNSPECIFIED
				},
				"two grants": func(d *agentv1alpha1.AgentDefinition) { d.Grants = append(d.Grants, readGrant(ctx)) },
				"long run": func(d *agentv1alpha1.AgentDefinition) {
					d.RunLimits = &templatev1alpha1.RunLimits{MaxDurationSeconds: 25 * 3600}
				},
				"bad spend": func(d *agentv1alpha1.AgentDefinition) {
					d.RunLimits = &templatev1alpha1.RunLimits{MaxSpendUsd: "1.234"}
				},
				"trigger notifies": notifyStranger,
				"many requests": func(d *agentv1alpha1.AgentDefinition) {
					d.RunLimits = &templatev1alpha1.RunLimits{MaxModelRequests: 100001}
				},
				"huge spend": func(d *agentv1alpha1.AgentDefinition) {
					d.RunLimits = &templatev1alpha1.RunLimits{MaxSpendUsd: "92233720368547758"}
				},
				"over max spend": func(d *agentv1alpha1.AgentDefinition) {
					d.RunLimits = &templatev1alpha1.RunLimits{MaxSpendUsd: "1000000"}
				},
			} {
				Expect(codeOf(bad(mod))).To(Equal(connect.CodeInvalidArgument), name)
			}
			Expect(count(ctx, e, `SELECT count(*) FROM agents WHERE org_id = $1`, org)).To(BeZero())
		})

		It("refuses a viewer and another org's member", func(ctx SpecContext) {
			viewer := e.userIn(ctx, org, store.OrgRoleViewer)
			_, err := blank(ctx, viewer, definition(ctx))
			Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))

			_, stranger := e.newTeamOrg(ctx)
			_, err = blank(ctx, stranger, definition(ctx))
			Expect(codeOf(err)).To(Equal(connect.CodeNotFound), "the org is not shown to a non-member")
			Expect(count(ctx, e, `SELECT count(*) FROM agents WHERE org_id = $1`, org)).To(BeZero())
		})

		It("does not bind another org's connection", func(ctx SpecContext) {
			other, _ := e.newTeamOrg(ctx)
			d := definition(ctx)
			d.Grants[0].ConnectionId = e.githubConnection(ctx, other)
			_, err := blank(ctx, owner, d)
			Expect(err).To(HaveOccurred())
			Expect(count(ctx, e, `SELECT count(*) FROM agents WHERE org_id = $1`, org)).To(BeZero())
		})
	})

	Describe("UpdateAgent", func() {
		It("proposes a new version on an active agent and leaves the running version alone", func(ctx SpecContext) {
			a := owner.activeAgent(ctx, org)
			admin := e.userIn(ctx, org, store.OrgRoleAdmin)
			d := &agentv1alpha1.AgentDefinition{
				DisplayName: "Renamed", Instructions: "New instructions.",
				Grants: []*templatev1alpha1.Grant{{
					Connector: "github", Scopes: []string{"repo.read", "pull_request.create"}, Preset: templatev1alpha1.Preset_PRESET_TRUSTED,
					ConnectionId: e.githubConnection(ctx, org), Repositories: []string{"acme/widgets"},
				}},
				RunLimits: &templatev1alpha1.RunLimits{MaxDurationSeconds: 600, MaxModelRequests: 50, MaxSpendUsd: "2.50"},
			}
			got, err := update(ctx, admin, a, d, "display_name", "instructions", "grants", "run_limits")
			Expect(err).NotTo(HaveOccurred())
			Expect(got.GetStatus()).To(Equal(agentv1alpha1.AgentStatus_AGENT_STATUS_ACTIVE))
			Expect(got.GetLatestVersion()).To(Equal(a.GetLatestVersion() + 1))
			Expect(got.GetActiveVersion()).To(Equal(a.GetActiveVersion()), "the running version does not change")
			Expect(got.GetApprovedGrants()).To(Equal(a.GetApprovedGrants()))
			Expect(got.GetProposedGrants()[0].GetPreset()).To(Equal(templatev1alpha1.Preset_PRESET_TRUSTED))
			Expect(got.GetProposedGrants()[0].GetTargetsMax()).To(Equal(int32(1)), "the template's bounds stay")
			Expect(got.GetInstructions()).To(Equal("New instructions."))
			Expect(got.GetDisplayName()).To(Equal("Renamed"))
			Expect(got.GetRunLimits().GetMaxSpendUsd()).To(Equal("2.50"))
			Expect(audits(ctx, "agent.update", a.GetId())).To(Equal(1))
			var actor string
			Expect(e.pool.QueryRow(ctx, `SELECT actor_id::text FROM audit_events WHERE org_id = $1 AND action = 'agent.update'`, org).Scan(&actor)).To(Succeed())
			Expect(actor).To(Equal(admin.userID))

			act, err := activate(ctx, owner, got)
			Expect(err).NotTo(HaveOccurred())
			Expect(act.GetActiveVersion()).To(Equal(got.GetLatestVersion()))
			Expect(act.GetApprovedGrants()[0].GetPreset()).To(Equal(templatev1alpha1.Preset_PRESET_TRUSTED))
		})

		It("changes only the name without a new version", func(ctx SpecContext) {
			a := owner.activeAgent(ctx, org)
			got, err := update(ctx, owner, a, &agentv1alpha1.AgentDefinition{DisplayName: "Only the name"}, "display_name")
			Expect(err).NotTo(HaveOccurred())
			Expect(got.GetDisplayName()).To(Equal("Only the name"))
			Expect(got.GetLatestVersion()).To(Equal(a.GetLatestVersion()))
		})

		It("lets a member change only their own draft, and never activate or raise an active agent", func(ctx SpecContext) {
			member := e.userIn(ctx, org, store.OrgRoleMember)
			mine, err := blank(ctx, member, definition(ctx))
			Expect(err).NotTo(HaveOccurred())
			trusted := definition(ctx)
			trusted.Grants[0].Preset = templatev1alpha1.Preset_PRESET_TRUSTED
			trusted.Grants[0].Scopes = []string{"repo.read", "pull_request.create", "issue.comment"}
			got, err := update(ctx, member, mine, trusted, "grants")
			Expect(err).NotTo(HaveOccurred())
			Expect(got.GetStatus()).To(Equal(agentv1alpha1.AgentStatus_AGENT_STATUS_DRAFT), "an update cannot activate")
			Expect(got.GetApprovedGrants()).To(BeEmpty(), "the raised grant is only proposed")
			Expect(got.GetCreatedBy()).To(Equal(member.userID), "the web shows the draft editor to its creator")
			got, err = update(ctx, member, got, &agentv1alpha1.AgentDefinition{Instructions: "Fix the flaky tests."}, "instructions")
			Expect(err).NotTo(HaveOccurred(), "the creator changes the instructions of their own draft")
			Expect(got.GetInstructions()).To(Equal("Fix the flaky tests."))
			_, err = activate(ctx, member, got)
			Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))

			others, err := blank(ctx, owner, definition(ctx))
			Expect(err).NotTo(HaveOccurred())
			_, err = update(ctx, member, others, trusted, "grants")
			Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied), "another person's draft")

			act, err := activate(ctx, owner, got)
			Expect(err).NotTo(HaveOccurred())
			_, err = update(ctx, member, act, &agentv1alpha1.AgentDefinition{Instructions: "Do more."}, "instructions")
			Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied), "an agent that was activated is an admin's to change")

			// Back to draft after it ran (ADR 0020): still a draft, but it was approved, so it stays an admin's.
			back, err := store.New(e.pool).ReturnAgentToDraft(ctx, store.ReturnAgentToDraftParams{OrgID: mustUUID(org), ID: mustUUID(act.GetId())})
			Expect(err).NotTo(HaveOccurred())
			Expect(back.Status).To(Equal(store.AgentStatusDraft))
			_, err = update(ctx, member, act, &agentv1alpha1.AgentDefinition{Instructions: "Do more."}, "instructions")
			Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied), "a draft that was activated before is an admin's to change")

			viewer := e.userIn(ctx, org, store.OrgRoleViewer)
			_, err = update(ctx, viewer, act, &agentv1alpha1.AgentDefinition{DisplayName: "x"}, "display_name")
			Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
			Expect(audits(ctx, "agent.update", act.GetId())).To(Equal(2), "only the member's own draft changes: grants, then instructions")
		})

		It("refuses another org's caller and does not show the agent", func(ctx SpecContext) {
			a := owner.activeAgent(ctx, org)
			_, stranger := e.newTeamOrg(ctx)
			_, err := update(ctx, stranger, a, &agentv1alpha1.AgentDefinition{DisplayName: "x"}, "display_name")
			Expect(codeOf(err)).To(Equal(connect.CodeNotFound), "the org is not shown to a non-member")

			strangerOrg, strangerOwner := e.newTeamOrg(ctx)
			res, err := strangerOwner.agents.UpdateAgent(ctx, &agentv1alpha1.UpdateAgentRequest{
				OrgId: strangerOrg, AgentId: a.GetId(), BaseVersion: a.GetLatestVersion(),
				Definition: &agentv1alpha1.AgentDefinition{DisplayName: "x"}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}},
			})
			Expect(codeOf(err)).To(Equal(connect.CodeNotFound))
			Expect(res).To(BeNil())
		})

		It("refuses a stale base version, a bad mask, a harness change after activation and an archived agent", func(ctx SpecContext) {
			a := owner.activeAgent(ctx, org)
			name := &agentv1alpha1.AgentDefinition{DisplayName: "x", Instructions: "y", Harness: "opencode"}

			_, err := update(ctx, owner, a, name)
			Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument), "an empty mask")
			_, err = update(ctx, owner, a, name, "status")
			Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument), "an unknown field")

			next, err := update(ctx, owner, a, name, "instructions")
			Expect(err).NotTo(HaveOccurred())
			_, err = update(ctx, owner, a, name, "instructions")
			Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition), "a stale base version")

			_, err = update(ctx, owner, next, &agentv1alpha1.AgentDefinition{Harness: "nope"}, "harness")
			Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))

			_, err = owner.agents.ArchiveAgent(ctx, &agentv1alpha1.ArchiveAgentRequest{OrgId: org, AgentId: a.GetId()})
			Expect(err).NotTo(HaveOccurred())
			_, err = update(ctx, owner, next, name, "instructions")
			Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition))
		})

		It("refuses a trigger that asks someone who cannot answer", func(ctx SpecContext) {
			a := owner.activeAgent(ctx, org)
			d := &agentv1alpha1.AgentDefinition{}
			notifyStranger(d)
			_, err := update(ctx, owner, a, d, "triggers")
			Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
		})
	})
})

// notifyStranger gives d a trigger that asks a person who is not in the org.
func notifyStranger(d *agentv1alpha1.AgentDefinition) {
	d.Triggers = []*templatev1alpha1.Trigger{{
		Id: "chat", Type: templatev1alpha1.TriggerType_TRIGGER_TYPE_DIRECT_MESSAGE, Instruction: "Answer.",
		Notify: &templatev1alpha1.NotifyTarget{Kind: templatev1alpha1.NotifyTargetKind_NOTIFY_TARGET_KIND_USER, UserId: "00000000-0000-4000-8000-000000000001"},
	}}
}
