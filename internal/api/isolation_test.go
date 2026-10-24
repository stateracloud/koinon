// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	connect "connectrpc.com/connect/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/stateracloud/koinon/internal/connectors/github"
	"github.com/stateracloud/koinon/internal/connectors/github/githubtest"
	"github.com/stateracloud/koinon/internal/db/store"
	agentv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/agent/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/agent/v1alpha1/agentv1alpha1connect"
	approvalv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/approval/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/approval/v1alpha1/approvalv1alpha1connect"
	authv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/auth/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/auth/v1alpha1/authv1alpha1connect"
	budgetv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/budget/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/budget/v1alpha1/budgetv1alpha1connect"
	connectionv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/connection/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/connection/v1alpha1/connectionv1alpha1connect"
	_ "github.com/stateracloud/koinon/internal/gen/koinon/platform/v1alpha1" // registers its descriptor
	providerv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/provider/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/provider/v1alpha1/providerv1alpha1connect"
	runv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/run/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/run/v1alpha1/runv1alpha1connect"
	"github.com/stateracloud/koinon/internal/gen/koinon/template/v1alpha1/templatev1alpha1connect"
	ticketv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/ticket/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/ticket/v1alpha1/ticketv1alpha1connect"
	tokenv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/token/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/token/v1alpha1/tokenv1alpha1connect"
	"github.com/stateracloud/koinon/internal/secretstore/secretstoretest"
)

// Org isolation suite (plan item D2). Every RPC is called by a user of org B
// with org A's IDs, and must return NotFound or PermissionDenied, never data.
// Every RPC in api/proto must be listed here, in isolationCases or with a
// reason in isolationExempt; the "org isolation registry" spec fails otherwise.

// isolationCase calls one RPC as c, naming org and the resources of f.
type isolationCase struct {
	call func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error)
	// ownOrgOK marks RPCs whose request names no resource besides the org
	// (lists and creates). Called in the caller's own org they succeed, and
	// the response must hold none of the other org's IDs.
	ownOrgOK bool
}

var isolationCases = map[string]isolationCase{
	agentv1alpha1connect.AgentServiceCreateAgentFromTemplateProcedure: {
		ownOrgOK: true,
		call: func(ctx context.Context, c *client, org string, _ orgFixture) (proto.Message, error) {
			return c.agents.CreateAgentFromTemplate(ctx, &agentv1alpha1.CreateAgentFromTemplateRequest{
				OrgId: org, TemplateId: c.repoHelper(ctx).GetId(), Grants: c.b.e.repoGrant(ctx, org, "acme/widgets"),
			})
		},
	},
	agentv1alpha1connect.AgentServiceGetAgentProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.agents.GetAgent(ctx, &agentv1alpha1.GetAgentRequest{OrgId: org, AgentId: f.agent.GetId()})
		},
	},
	agentv1alpha1connect.AgentServiceListAgentsProcedure: {
		ownOrgOK: true,
		call: func(ctx context.Context, c *client, org string, _ orgFixture) (proto.Message, error) {
			return c.agents.ListAgents(ctx, &agentv1alpha1.ListAgentsRequest{OrgId: org})
		},
	},
	agentv1alpha1connect.AgentServiceActivateAgentProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.agents.ActivateAgent(ctx, &agentv1alpha1.ActivateAgentRequest{
				OrgId: org, AgentId: f.draft.GetId(), Version: f.draft.GetLatestVersion(),
			})
		},
	},
	agentv1alpha1connect.AgentServiceArchiveAgentProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.agents.ArchiveAgent(ctx, &agentv1alpha1.ArchiveAgentRequest{OrgId: org, AgentId: f.agent.GetId()})
		},
	},

	ticketv1alpha1connect.TicketServiceCreateTicketProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.tickets.CreateTicket(ctx, &ticketv1alpha1.CreateTicketRequest{
				OrgId: org, AgentId: f.agent.GetId(), Title: "intruder",
			})
		},
	},
	ticketv1alpha1connect.TicketServiceListTicketsProcedure: {
		ownOrgOK: true,
		call: func(ctx context.Context, c *client, org string, _ orgFixture) (proto.Message, error) {
			return c.tickets.ListTickets(ctx, &ticketv1alpha1.ListTicketsRequest{OrgId: org})
		},
	},
	ticketv1alpha1connect.TicketServiceGetTicketProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.tickets.GetTicket(ctx, &ticketv1alpha1.GetTicketRequest{OrgId: org, TicketId: f.ticket.GetId()})
		},
	},
	ticketv1alpha1connect.TicketServiceAppendMessageProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.tickets.AppendMessage(ctx, &ticketv1alpha1.AppendMessageRequest{
				OrgId: org, TicketId: f.ticket.GetId(), Text: "intruder",
			})
		},
	},
	ticketv1alpha1connect.TicketServiceMoveTicketProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.tickets.MoveTicket(ctx, &ticketv1alpha1.MoveTicketRequest{
				OrgId: org, TicketId: f.ticket.GetId(), Status: ticketv1alpha1.TicketStatus_TICKET_STATUS_DONE,
			})
		},
	},

	runv1alpha1connect.RunServiceGetRunProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.runs.GetRun(ctx, &runv1alpha1.GetRunRequest{OrgId: org, RunId: f.run.GetId()})
		},
	},
	runv1alpha1connect.RunServiceCancelRunProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.runs.CancelRun(ctx, &runv1alpha1.CancelRunRequest{OrgId: org, RunId: f.run.GetId()})
		},
	},
	connectionv1alpha1connect.ConnectionServiceCreateGitHubConnectionProcedure: {
		ownOrgOK: true,
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.conns.CreateGitHubConnection(ctx, &connectionv1alpha1.CreateGitHubConnectionRequest{
				OrgId: org, AppId: 1, InstallationId: 2, PrivateKey: string(f.appKey),
			})
		},
	},
	connectionv1alpha1connect.ConnectionServiceDeleteConnectionProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.conns.DeleteConnection(ctx, &connectionv1alpha1.DeleteConnectionRequest{OrgId: org, ConnectionId: f.connection})
		},
	},
	connectionv1alpha1connect.ConnectionServiceGetConnectionProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.conns.GetConnection(ctx, &connectionv1alpha1.GetConnectionRequest{OrgId: org, ConnectionId: f.connection})
		},
	},
	connectionv1alpha1connect.ConnectionServiceListConnectionsProcedure: {
		ownOrgOK: true,
		call: func(ctx context.Context, c *client, org string, _ orgFixture) (proto.Message, error) {
			return c.conns.ListConnections(ctx, &connectionv1alpha1.ListConnectionsRequest{OrgId: org})
		},
	},
	connectionv1alpha1connect.ConnectionServiceTestConnectionProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.conns.TestConnection(ctx, &connectionv1alpha1.TestConnectionRequest{OrgId: org, ConnectionId: f.connection})
		},
	},
	connectionv1alpha1connect.ConnectionServiceListInstallationReposProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.conns.ListInstallationRepos(ctx, &connectionv1alpha1.ListInstallationReposRequest{OrgId: org, ConnectionId: f.connection})
		},
	},

	approvalv1alpha1connect.ApprovalServiceListApprovalsProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.approvals.ListApprovals(ctx, &approvalv1alpha1.ListApprovalsRequest{OrgId: org, TicketId: f.ticket.GetId()})
		},
	},
	approvalv1alpha1connect.ApprovalServiceGetApprovalProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.approvals.GetApproval(ctx, &approvalv1alpha1.GetApprovalRequest{OrgId: org, ApprovalId: f.approval.ID.String()})
		},
	},
	approvalv1alpha1connect.ApprovalServiceGetApprovalDiffProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.approvals.GetApprovalDiff(ctx, &approvalv1alpha1.GetApprovalDiffRequest{OrgId: org, ApprovalId: f.approval.ID.String()})
		},
	},
	approvalv1alpha1connect.ApprovalServiceApproveProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.approvals.Approve(ctx, &approvalv1alpha1.ApproveRequest{
				OrgId: org, ApprovalId: f.approval.ID.String(), ArgsSha256: f.approval.ArgsSha256,
			})
		},
	},
	approvalv1alpha1connect.ApprovalServiceDenyProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.approvals.Deny(ctx, &approvalv1alpha1.DenyRequest{OrgId: org, ApprovalId: f.approval.ID.String()})
		},
	},

	tokenv1alpha1connect.TokenServiceCreateTokenProcedure: {
		ownOrgOK: true,
		call: func(ctx context.Context, c *client, org string, _ orgFixture) (proto.Message, error) {
			return c.tokens.CreateToken(ctx, &tokenv1alpha1.CreateTokenRequest{
				OrgId: org, Name: "intruder", Role: authv1alpha1.OrgRole_ORG_ROLE_ADMIN,
			})
		},
	},
	tokenv1alpha1connect.TokenServiceListTokensProcedure: {
		ownOrgOK: true,
		call: func(ctx context.Context, c *client, org string, _ orgFixture) (proto.Message, error) {
			return c.tokens.ListTokens(ctx, &tokenv1alpha1.ListTokensRequest{OrgId: org})
		},
	},
	tokenv1alpha1connect.TokenServiceRevokeTokenProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.tokens.RevokeToken(ctx, &tokenv1alpha1.RevokeTokenRequest{OrgId: org, TokenId: f.token})
		},
	},

	budgetv1alpha1connect.BudgetServiceGetOrgBudgetProcedure: {
		ownOrgOK: true,
		call: func(ctx context.Context, c *client, org string, _ orgFixture) (proto.Message, error) {
			return budgetClient(c).GetOrgBudget(ctx, &budgetv1alpha1.GetOrgBudgetRequest{OrgId: org})
		},
	},
	budgetv1alpha1connect.BudgetServiceSetMonthlyCapProcedure: {
		ownOrgOK: true,
		call: func(ctx context.Context, c *client, org string, _ orgFixture) (proto.Message, error) {
			return budgetClient(c).SetMonthlyCap(ctx, &budgetv1alpha1.SetMonthlyCapRequest{OrgId: org, MonthlyCapUsd: "5"})
		},
	},
	budgetv1alpha1connect.BudgetServiceSetPlatformCeilingProcedure: {
		call: func(ctx context.Context, c *client, org string, _ orgFixture) (proto.Message, error) {
			return budgetClient(c).SetPlatformCeiling(ctx, &budgetv1alpha1.SetPlatformCeilingRequest{OrgId: org, PlatformCeilingUsd: "5"})
		},
	},
	budgetv1alpha1connect.BudgetServiceGetAgentBudgetProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return budgetClient(c).GetAgentBudget(ctx, &budgetv1alpha1.GetAgentBudgetRequest{OrgId: org, AgentId: f.agent.GetId()})
		},
	},
	budgetv1alpha1connect.BudgetServiceListAgentBudgetsProcedure: {
		ownOrgOK: true,
		call: func(ctx context.Context, c *client, org string, _ orgFixture) (proto.Message, error) {
			return budgetClient(c).ListAgentBudgets(ctx, &budgetv1alpha1.ListAgentBudgetsRequest{OrgId: org})
		},
	},
	budgetv1alpha1connect.BudgetServiceSetAgentBudgetProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return budgetClient(c).SetAgentBudget(ctx, &budgetv1alpha1.SetAgentBudgetRequest{OrgId: org, AgentId: f.agent.GetId(), DailyLimitUsd: "1"})
		},
	},
	budgetv1alpha1connect.BudgetServiceListNotificationsProcedure: {
		ownOrgOK: true,
		call: func(ctx context.Context, c *client, org string, _ orgFixture) (proto.Message, error) {
			return budgetClient(c).ListNotifications(ctx, &budgetv1alpha1.ListNotificationsRequest{OrgId: org})
		},
	},
	budgetv1alpha1connect.BudgetServiceMarkNotificationReadProcedure: {
		call: func(ctx context.Context, c *client, org string, _ orgFixture) (proto.Message, error) {
			return budgetClient(c).MarkNotificationRead(ctx, &budgetv1alpha1.MarkNotificationReadRequest{OrgId: org, NotificationId: "019a0000-0000-7000-8000-00000000ffff"})
		},
	},
	budgetv1alpha1connect.PriceServiceListPricesProcedure: {
		ownOrgOK: true,
		call: func(ctx context.Context, c *client, org string, _ orgFixture) (proto.Message, error) {
			return priceClient(c).ListPrices(ctx, &budgetv1alpha1.ListPricesRequest{OrgId: org})
		},
	},
	budgetv1alpha1connect.PriceServiceSetPriceProcedure: {
		ownOrgOK: true,
		call: func(ctx context.Context, c *client, org string, _ orgFixture) (proto.Message, error) {
			return priceClient(c).SetPrice(ctx, &budgetv1alpha1.SetPriceRequest{
				OrgId: org, Name: "intruder", Provider: "*", Match: &budgetv1alpha1.PriceMatch{Glob: "x"}, Rates: &budgetv1alpha1.Rates{Input: "1", Output: "1"}, MaxOutput: 100,
			})
		},
	},
	budgetv1alpha1connect.PriceServiceRetirePriceProcedure: {
		call: func(ctx context.Context, c *client, org string, _ orgFixture) (proto.Message, error) {
			return priceClient(c).RetirePrice(ctx, &budgetv1alpha1.RetirePriceRequest{OrgId: org, Name: "nothing"})
		},
	},
	budgetv1alpha1connect.PriceServiceGetDefaultPricesProcedure: {
		ownOrgOK: true,
		call: func(ctx context.Context, c *client, org string, _ orgFixture) (proto.Message, error) {
			return priceClient(c).GetDefaultPrices(ctx, &budgetv1alpha1.GetDefaultPricesRequest{OrgId: org})
		},
	},
	budgetv1alpha1connect.PriceServiceGetDefaultPriceChangesProcedure: {
		ownOrgOK: true,
		call: func(ctx context.Context, c *client, org string, _ orgFixture) (proto.Message, error) {
			return priceClient(c).GetDefaultPriceChanges(ctx, &budgetv1alpha1.GetDefaultPriceChangesRequest{OrgId: org})
		},
	},
	budgetv1alpha1connect.PriceServiceListFlaggedPricesProcedure: {
		ownOrgOK: true,
		call: func(ctx context.Context, c *client, org string, _ orgFixture) (proto.Message, error) {
			return priceClient(c).ListFlaggedPrices(ctx, &budgetv1alpha1.ListFlaggedPricesRequest{OrgId: org})
		},
	},

	providerv1alpha1connect.ProviderServiceListProvidersProcedure: {
		ownOrgOK: true,
		call: func(ctx context.Context, c *client, org string, _ orgFixture) (proto.Message, error) {
			return c.providers.ListProviders(ctx, &providerv1alpha1.ListProvidersRequest{OrgId: org})
		},
	},
	providerv1alpha1connect.ProviderServiceCreateProviderProcedure: {
		ownOrgOK: true,
		call: func(ctx context.Context, c *client, org string, _ orgFixture) (proto.Message, error) {
			return c.providers.CreateProvider(ctx, &providerv1alpha1.CreateProviderRequest{
				OrgId: org, Name: fmt.Sprintf("intruder-%d", dbSeq.Add(1)), Type: "openai", Url: "https://api.example.com/v1",
			})
		},
	},
	providerv1alpha1connect.ProviderServiceUpdateProviderProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.providers.UpdateProvider(ctx, &providerv1alpha1.UpdateProviderRequest{
				OrgId: org, ProviderId: f.provider, Url: new("https://intruder.example.com/v1"),
			})
		},
	},
	providerv1alpha1connect.ProviderServiceDeleteProviderProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.providers.DeleteProvider(ctx, &providerv1alpha1.DeleteProviderRequest{OrgId: org, ProviderId: f.provider})
		},
	},
	providerv1alpha1connect.ProviderServiceListModelsProcedure: {
		ownOrgOK: true,
		call: func(ctx context.Context, c *client, org string, _ orgFixture) (proto.Message, error) {
			return c.providers.ListModels(ctx, &providerv1alpha1.ListModelsRequest{OrgId: org})
		},
	},
	providerv1alpha1connect.ProviderServiceCreateModelProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.providers.CreateModel(ctx, &providerv1alpha1.CreateModelRequest{
				OrgId: org, ProviderId: f.provider, Name: "intruder",
			})
		},
	},
	providerv1alpha1connect.ProviderServiceUpdateModelProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.providers.UpdateModel(ctx, &providerv1alpha1.UpdateModelRequest{OrgId: org, ModelId: f.model, Guard: new(false)})
		},
	},
	providerv1alpha1connect.ProviderServiceDeleteModelProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.providers.DeleteModel(ctx, &providerv1alpha1.DeleteModelRequest{OrgId: org, ModelId: f.model})
		},
	},

	runv1alpha1connect.RunServiceWatchRunProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			stream, err := c.runs.WatchRun(ctx, &runv1alpha1.WatchRunRequest{OrgId: org, RunId: f.run.GetId()})
			if err != nil {
				return nil, err
			}
			defer func() { _ = stream.Close() }()
			return stream.Receive()
		},
	},
}

// isolationExempt lists RPCs that take no org-owned input, with the reason.
var isolationExempt = map[string]string{
	authv1alpha1connect.AuthServiceWhoAmIProcedure:         "describes the caller's own session; takes no IDs",
	authv1alpha1connect.AuthServiceChangePasswordProcedure: "changes the caller's own password; takes no IDs",
	templatev1alpha1connect.TemplateServiceListTemplatesProcedure: "templates are platform data shared by every org; " +
		"takes no IDs",
	connectionv1alpha1connect.ConnectionServiceListConnectorTypesProcedure: "connector types are platform data shared by " +
		"every org; takes no IDs",
}

var _ = Describe("org isolation", func() {
	It("never serves or changes one org's data for another org's user", func(ctx SpecContext) {
		gh := githubtest.New()
		DeferCleanup(gh.Server.Close)
		e := newEnv(ctx, envOptions{
			keys:   secretstoretest.Keyring(),
			github: github.NewClient(github.Options{BaseURL: gh.URL()}),
			ghFake: gh,
		})
		a := e.newIsolationFixture(ctx, gh.PEM)
		b := e.newIsolationFixture(ctx, gh.PEM)
		foreign := []string{a.org, a.agent.GetId(), a.draft.GetId(), a.ticket.GetId(), a.run.GetId(), a.connection, a.approval.ID.String(), a.provider, a.model}
		before := snapshot(ctx, e, a)

		// The strongest caller in B: its owner, and a B admin.
		for _, caller := range []*client{b.owner, e.userIn(ctx, b.org, store.OrgRoleAdmin)} {
			for _, proc := range sortedKeys(isolationCases) {
				tc := isolationCases[proc]
				By(proc + " naming org A")
				res, err := tc.call(ctx, caller, a.org, a)
				Expect(err).To(HaveOccurred(), "%s returned data for another org", proc)
				Expect(codeOf(err)).To(BeElementOf(connect.CodeNotFound, connect.CodePermissionDenied), proc)
				Expect(res).To(BeNil())

				By(proc + " naming org B with org A's IDs")
				res, err = tc.call(ctx, caller, b.org, a)
				if tc.ownOrgOK {
					Expect(err).NotTo(HaveOccurred(), proc)
				} else {
					Expect(err).To(HaveOccurred(), "%s accepted another org's IDs", proc)
					Expect(codeOf(err)).To(BeElementOf(connect.CodeNotFound, connect.CodePermissionDenied), proc)
				}
				if res != nil {
					js, merr := protojson.Marshal(res)
					Expect(merr).NotTo(HaveOccurred())
					for _, id := range foreign {
						Expect(string(js)).NotTo(ContainSubstring(id), "%s leaked an ID of org A", proc)
					}
				}
			}
		}
		Expect(snapshot(ctx, e, a)).To(Equal(before), "org A's data changed")
	})
})

// newIsolationFixture is an org fixture plus a draft agent, for ActivateAgent,
// and a connection.
func (e *env) newIsolationFixture(ctx context.Context, appKey []byte) orgFixture {
	f := e.newOrgFixture(ctx)
	f.draft = f.owner.draftAgent(ctx, f.org)
	f.appKey = appKey
	res, err := f.owner.conns.CreateGitHubConnection(ctx, &connectionv1alpha1.CreateGitHubConnectionRequest{
		OrgId: f.org, AppId: 1, InstallationId: 2, PrivateKey: string(appKey),
	})
	Expect(err).NotTo(HaveOccurred())
	f.connection = res.GetConnection().GetId()
	f.approval = e.pendingApproval(ctx, f)
	tok, err := f.owner.tokens.CreateToken(ctx, &tokenv1alpha1.CreateTokenRequest{
		OrgId: f.org, Name: "ci", Role: authv1alpha1.OrgRole_ORG_ROLE_ADMIN,
	})
	Expect(err).NotTo(HaveOccurred())
	f.token = tok.GetToken().GetId()
	prov, err := f.owner.providers.CreateProvider(ctx, &providerv1alpha1.CreateProviderRequest{
		OrgId: f.org, Name: "isolated", Type: "openrouter", Url: "https://openrouter.ai/api/v1", ApiKey: "sk-isolated",
	})
	Expect(err).NotTo(HaveOccurred())
	f.provider = prov.GetProvider().GetId()
	mod, err := f.owner.providers.CreateModel(ctx, &providerv1alpha1.CreateModelRequest{
		OrgId: f.org, ProviderId: f.provider, Name: "isolated-model",
	})
	Expect(err).NotTo(HaveOccurred())
	f.model = mod.GetModel().GetId()
	return f
}

// snapshot is everything about org A that a cross-org call could change.
func snapshot(ctx context.Context, e *env, f orgFixture) []int {
	org := f.org
	return []int{
		count(ctx, e, `SELECT count(*) FROM agents WHERE org_id = $1`, org),
		count(ctx, e, `SELECT count(*) FROM agents WHERE org_id = $1 AND status = 'active'`, org),
		count(ctx, e, `SELECT count(*) FROM agents WHERE org_id = $1 AND status = 'draft'`, org),
		count(ctx, e, `SELECT count(*) FROM tickets WHERE org_id = $1`, org),
		count(ctx, e, `SELECT count(*) FROM tickets WHERE org_id = $1 AND status = 'todo'`, org),
		count(ctx, e, `SELECT count(*) FROM ticket_events WHERE org_id = $1`, org),
		count(ctx, e, `SELECT count(*) FROM runs WHERE org_id = $1`, org),
		count(ctx, e, `SELECT count(*) FROM runs WHERE org_id = $1 AND state = 'queued'`, org),
		count(ctx, e, `SELECT count(*) FROM run_events WHERE org_id = $1`, org),
		count(ctx, e, `SELECT count(*) FROM connections WHERE org_id = $1`, org),
		count(ctx, e, `SELECT count(*) FROM connections WHERE org_id = $1 AND status = 'untested'`, org),
		count(ctx, e, `SELECT count(*) FROM approval_requests WHERE org_id = $1 AND status = 'pending'`, org),
		count(ctx, e, `SELECT count(*) FROM api_tokens WHERE org_id = $1`, org),
		count(ctx, e, `SELECT count(*) FROM api_tokens WHERE org_id = $1 AND revoked_at IS NULL`, org),
		count(ctx, e, `SELECT count(*) FROM providers WHERE org_id = $1 AND url = 'https://openrouter.ai/api/v1'`, org),
		count(ctx, e, `SELECT count(*) FROM providers WHERE org_id = $1`, org),
		count(ctx, e, `SELECT count(*) FROM models WHERE org_id = $1`, org),
		count(ctx, e, `SELECT count(*) FROM price_overrides WHERE org_id = $1`, org),
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// The registry spec fails when an RPC defined in api/proto is not listed in the isolation
// suite, or when the suite lists one that no longer exists. It uses no database.
var _ = Describe("org isolation registry", func() {
	It("lists every RPC defined in api/proto exactly once, and no other", func() {
		root := filepath.Join("..", "..", "api", "proto")
		var files []string
		Expect(filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && strings.HasSuffix(path, ".proto") {
				rel, err := filepath.Rel(root, path)
				if err != nil {
					return err
				}
				files = append(files, filepath.ToSlash(rel))
			}
			return nil
		})).To(Succeed())
		Expect(files).NotTo(BeEmpty(), "no proto files found")

		var problems []string
		defined := map[string]bool{}
		for _, name := range files {
			fd, err := protoregistry.GlobalFiles.FindFileByPath(name)
			if err != nil {
				problems = append(problems, name+" is not linked into this test; import its generated package in isolation_test.go")
				continue
			}
			services := fd.Services()
			for i := range services.Len() {
				addMethods(defined, services.Get(i))
			}
		}

		for _, proc := range sortedKeys(defined) {
			_, listed := isolationCases[proc]
			reason, exempt := isolationExempt[proc]
			switch {
			case listed && exempt:
				problems = append(problems, proc+" is both tested and exempt in the org isolation suite")
			case exempt && strings.TrimSpace(reason) == "":
				problems = append(problems, proc+" is exempt from the org isolation suite without a reason")
			case !listed && !exempt:
				problems = append(problems, proc+" is not covered by the org isolation suite: add it to isolationCases "+
					"(or, if it takes no org-owned input, to isolationExempt with a reason)")
			}
		}
		for _, m := range []map[string]bool{keysOf(isolationCases), keysOf(isolationExempt)} {
			for _, proc := range sortedKeys(m) {
				if !defined[proc] {
					problems = append(problems, proc+" is listed in the org isolation suite but no such RPC exists")
				}
			}
		}
		Expect(problems).To(BeEmpty())
	})
})

func addMethods(into map[string]bool, svc protoreflect.ServiceDescriptor) {
	methods := svc.Methods()
	for i := range methods.Len() {
		into["/"+string(svc.FullName())+"/"+string(methods.Get(i).Name())] = true
	}
}

func keysOf[V any](m map[string]V) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}
