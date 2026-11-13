// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	connect "connectrpc.com/connect/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/connectors/github"
	"github.com/stateracloud/koinon/internal/connectors/github/githubtest"
	"github.com/stateracloud/koinon/internal/db/store"
	agentv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/agent/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/agent/v1alpha1/agentv1alpha1connect"
	approvalv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/approval/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/approval/v1alpha1/approvalv1alpha1connect"
	auditv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/audit/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/audit/v1alpha1/auditv1alpha1connect"
	authv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/auth/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/auth/v1alpha1/authv1alpha1connect"
	budgetv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/budget/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/budget/v1alpha1/budgetv1alpha1connect"
	connectionv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/connection/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/connection/v1alpha1/connectionv1alpha1connect"
	"github.com/stateracloud/koinon/internal/gen/koinon/gateway/v1alpha1/gatewayv1alpha1connect"
	leakguardv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/leakguard/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/leakguard/v1alpha1/leakguardv1alpha1connect"
	mcpserverv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/mcpserver/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/mcpserver/v1alpha1/mcpserverv1alpha1connect"
	orgsettingsv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/orgsettings/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/orgsettings/v1alpha1/orgsettingsv1alpha1connect"
	outwardv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/outward/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/outward/v1alpha1/outwardv1alpha1connect"
	_ "github.com/stateracloud/koinon/internal/gen/koinon/platform/v1alpha1" // registers its descriptor
	providerv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/provider/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/provider/v1alpha1/providerv1alpha1connect"
	questionv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/question/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/question/v1alpha1/questionv1alpha1connect"
	runv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/run/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/run/v1alpha1/runv1alpha1connect"
	taskv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/task/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/task/v1alpha1/taskv1alpha1connect"
	"github.com/stateracloud/koinon/internal/gen/koinon/template/v1alpha1/templatev1alpha1connect"
	ticketv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/ticket/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/ticket/v1alpha1/ticketv1alpha1connect"
	tokenv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/token/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/token/v1alpha1/tokenv1alpha1connect"
	tracev1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/trace/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/trace/v1alpha1/tracev1alpha1connect"
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/orgsettings"
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
	agentv1alpha1connect.AgentServiceSetAgentTriggersProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.agents.SetAgentTriggers(ctx, &agentv1alpha1.SetAgentTriggersRequest{
				OrgId: org, AgentId: f.agent.GetId(), BaseVersion: f.agent.GetLatestVersion(),
			})
		},
	},
	agentv1alpha1connect.AgentServiceResumeAgentProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.agents.ResumeAgent(ctx, &agentv1alpha1.ResumeAgentRequest{OrgId: org, AgentId: f.agent.GetId()})
		},
	},
	agentv1alpha1connect.AgentServiceGetAgentModelProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.agents.GetAgentModel(ctx, &agentv1alpha1.GetAgentModelRequest{OrgId: org, AgentId: f.agent.GetId()})
		},
	},
	agentv1alpha1connect.AgentServiceSetAgentModelProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.agents.SetAgentModel(ctx, &agentv1alpha1.SetAgentModelRequest{OrgId: org, AgentId: f.agent.GetId(), Model: "default"})
		},
	},
	agentv1alpha1connect.AgentServiceSetAgentThinkingProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.agents.SetAgentThinking(ctx, &agentv1alpha1.SetAgentThinkingRequest{OrgId: org, AgentId: f.agent.GetId(), Thinking: "default"})
		},
	},
	agentv1alpha1connect.AgentServiceSetAgentKeepWarmProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.agents.SetAgentKeepWarm(ctx, &agentv1alpha1.SetAgentKeepWarmRequest{OrgId: org, AgentId: f.agent.GetId(), KeepWarm: false})
		},
	},
	agentv1alpha1connect.AgentServiceGetAgentSandboxResourcesProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.agents.GetAgentSandboxResources(ctx, &agentv1alpha1.GetAgentSandboxResourcesRequest{OrgId: org, AgentId: f.agent.GetId()})
		},
	},
	agentv1alpha1connect.AgentServiceSetAgentSandboxResourcesProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.agents.SetAgentSandboxResources(ctx, &agentv1alpha1.SetAgentSandboxResourcesRequest{OrgId: org, AgentId: f.agent.GetId(), Resources: &agentv1alpha1.SandboxResources{MemoryLimit: "2Gi"}})
		},
	},
	agentv1alpha1connect.AgentServiceRunAgentTriggerProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.agents.RunAgentTrigger(ctx, &agentv1alpha1.RunAgentTriggerRequest{OrgId: org, AgentId: f.agent.GetId(), TriggerId: "daily-check"})
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

	questionv1alpha1connect.QuestionServiceListQuestionsProcedure: {
		ownOrgOK: true,
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.questions.ListQuestions(ctx, &questionv1alpha1.ListQuestionsRequest{OrgId: org})
		},
	},
	questionv1alpha1connect.QuestionServiceListAskableMembersProcedure: {
		ownOrgOK: true,
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.questions.ListAskableMembers(ctx, &questionv1alpha1.ListAskableMembersRequest{OrgId: org})
		},
	},
	questionv1alpha1connect.QuestionServiceAnswerQuestionProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.questions.AnswerQuestion(ctx, &questionv1alpha1.AnswerQuestionRequest{
				OrgId: org, QuestionId: f.question.ID.String(), Answer: &questionv1alpha1.AnswerQuestionRequest_Text{Text: "intruder"},
			})
		},
	},
	questionv1alpha1connect.QuestionServiceDismissQuestionProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.questions.DismissQuestion(ctx, &questionv1alpha1.DismissQuestionRequest{OrgId: org, QuestionId: f.question.ID.String()})
		},
	},

	taskv1alpha1connect.TaskServiceCreateTaskProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.tasks.CreateTask(ctx, &taskv1alpha1.CreateTaskRequest{
				OrgId: org, AgentId: f.agent.GetId(), Title: "intruder",
			})
		},
	},
	taskv1alpha1connect.TaskServiceListTasksProcedure: {
		ownOrgOK: true,
		call: func(ctx context.Context, c *client, org string, _ orgFixture) (proto.Message, error) {
			return c.tasks.ListTasks(ctx, &taskv1alpha1.ListTasksRequest{OrgId: org})
		},
	},
	taskv1alpha1connect.TaskServiceGetTaskProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.tasks.GetTask(ctx, &taskv1alpha1.GetTaskRequest{OrgId: org, TaskId: f.ticket.GetId()})
		},
	},
	taskv1alpha1connect.TaskServiceAppendMessageProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.tasks.AppendMessage(ctx, &taskv1alpha1.AppendMessageRequest{
				OrgId: org, TaskId: f.ticket.GetId(), Text: "intruder",
			})
		},
	},
	taskv1alpha1connect.TaskServiceSendDirectMessageProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.tasks.SendDirectMessage(ctx, &taskv1alpha1.SendDirectMessageRequest{
				OrgId: org, AgentId: f.agent.GetId(), TaskId: f.ticket.GetId(), Text: "intruder", MessageId: "00000000-0000-4000-8000-000000000001",
			})
		},
	},
	taskv1alpha1connect.TaskServiceCancelTaskProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.tasks.CancelTask(ctx, &taskv1alpha1.CancelTaskRequest{OrgId: org, TaskId: f.ticket.GetId()})
		},
	},

	taskv1alpha1connect.TaskServiceNotifyTypingProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.tasks.NotifyTyping(ctx, &taskv1alpha1.NotifyTypingRequest{OrgId: org, TaskId: f.ticket.GetId()})
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
	connectionv1alpha1connect.ConnectionServiceCreateGiteaConnectionProcedure: {
		ownOrgOK: true,
		call: func(ctx context.Context, c *client, org string, _ orgFixture) (proto.Message, error) {
			return c.conns.CreateGiteaConnection(ctx, &connectionv1alpha1.CreateGiteaConnectionRequest{
				OrgId: org, BaseUrl: "https://git.example.test", ReadToken: "koinon_test_not-a-real-read-token", WriteToken: "koinon_test_not-a-real-write-token",
			})
		},
	},
	connectionv1alpha1connect.ConnectionServiceDeleteConnectionProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.conns.DeleteConnection(ctx, &connectionv1alpha1.DeleteConnectionRequest{OrgId: org, ConnectionId: f.connection})
		},
	},
	connectionv1alpha1connect.ConnectionServiceAddConnectionMCPHostProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.conns.AddConnectionMCPHost(ctx, &connectionv1alpha1.AddConnectionMCPHostRequest{OrgId: org, ConnectionId: f.connection, Host: "gitea-mcp.example.test"})
		},
	},
	connectionv1alpha1connect.ConnectionServiceRemoveConnectionMCPHostProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.conns.RemoveConnectionMCPHost(ctx, &connectionv1alpha1.RemoveConnectionMCPHostRequest{OrgId: org, ConnectionId: f.connection, Host: "gitea-mcp.example.test"})
		},
	},
	connectionv1alpha1connect.ConnectionServiceSetConnectionSecretProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.conns.SetConnectionSecret(ctx, &connectionv1alpha1.SetConnectionSecretRequest{
				OrgId: org, ConnectionId: f.connection,
				Secret: &connectionv1alpha1.SetConnectionSecretRequest_Github{Github: &connectionv1alpha1.GitHubConnectionSecret{PrivateKey: string(f.appKey)}},
			})
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
	approvalv1alpha1connect.ApprovalServiceListApprovalScopesProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.approvals.ListApprovalScopes(ctx, &approvalv1alpha1.ListApprovalScopesRequest{OrgId: org, AgentId: f.agent.GetId()})
		},
	},
	approvalv1alpha1connect.ApprovalServiceRevokeApprovalScopeProcedure: {
		call: func(ctx context.Context, c *client, org string, _ orgFixture) (proto.Message, error) {
			return c.approvals.RevokeApprovalScope(ctx, &approvalv1alpha1.RevokeApprovalScopeRequest{OrgId: org, ScopeId: "0197a2f0-0000-7000-8000-000000000000"})
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
	// Platform admins only (ADR 0110): no org role, owner included, can re-enable a token.
	tokenv1alpha1connect.TokenServiceRebindTokenProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.tokens.RebindToken(ctx, &tokenv1alpha1.RebindTokenRequest{OrgId: org, TokenId: f.token})
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
	auditv1alpha1connect.AuditServiceListProcedure: {
		ownOrgOK: true,
		call: func(ctx context.Context, c *client, org string, _ orgFixture) (proto.Message, error) {
			return auditClient(c).List(ctx, &auditv1alpha1.ListRequest{OrgId: org})
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
	budgetv1alpha1connect.BudgetServiceGetAgentRunLimitsProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return budgetClient(c).GetAgentRunLimits(ctx, &budgetv1alpha1.GetAgentRunLimitsRequest{OrgId: org, AgentId: f.agent.GetId()})
		},
	},
	budgetv1alpha1connect.BudgetServiceSetAgentRunLimitsProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return budgetClient(c).SetAgentRunLimits(ctx, &budgetv1alpha1.SetAgentRunLimitsRequest{
				OrgId: org, AgentId: f.agent.GetId(), Override: &budgetv1alpha1.RunLimitValues{MaxModelRequests: 10},
			})
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
	providerv1alpha1connect.ProviderServiceCheckProviderEchoProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.providers.CheckProviderEcho(ctx, &providerv1alpha1.CheckProviderEchoRequest{OrgId: org, ProviderId: f.provider, Model: "any"})
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
	providerv1alpha1connect.ProviderServiceModelFormatsProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.providers.ModelFormats(ctx, &providerv1alpha1.ModelFormatsRequest{OrgId: org, ProviderId: f.provider, Name: "intruder"})
		},
	},
	providerv1alpha1connect.ProviderServiceProviderTypeInfoProcedure: {
		ownOrgOK: true,
		call: func(ctx context.Context, c *client, org string, _ orgFixture) (proto.Message, error) {
			return c.providers.ProviderTypeInfo(ctx, &providerv1alpha1.ProviderTypeInfoRequest{OrgId: org, Type: "anthropic"})
		},
	},
	providerv1alpha1connect.ProviderServiceDeleteModelProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.providers.DeleteModel(ctx, &providerv1alpha1.DeleteModelRequest{OrgId: org, ModelId: f.model})
		},
	},
	providerv1alpha1connect.ProviderServiceGetDefaultModelProcedure: {
		ownOrgOK: true,
		call: func(ctx context.Context, c *client, org string, _ orgFixture) (proto.Message, error) {
			return c.providers.GetDefaultModel(ctx, &providerv1alpha1.GetDefaultModelRequest{OrgId: org})
		},
	},
	providerv1alpha1connect.ProviderServiceSetDefaultModelProcedure: {
		// The model name is the org-owned input: another org's model is never served here.
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.providers.SetDefaultModel(ctx, &providerv1alpha1.SetDefaultModelRequest{OrgId: org, Model: f.modelName})
		},
	},

	// Every org kind is tried with org A's target ID; the first answer that is not NotFound is
	// returned. The platform kind takes no org input and needs a platform admin, so a member of org
	// B is refused before it reads anything; the spec in internal/services covers that.
	outwardv1alpha1connect.OutwardServiceSetOutwardTargetDisabledProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			var last proto.Message
			var lastErr error
			for kind, id := range map[outwardv1alpha1.OutwardTargetKind]string{
				outwardv1alpha1.OutwardTargetKind_OUTWARD_TARGET_KIND_MCP_SERVER:        f.mcpServer,
				outwardv1alpha1.OutwardTargetKind_OUTWARD_TARGET_KIND_MODEL_PROVIDER:    f.provider,
				outwardv1alpha1.OutwardTargetKind_OUTWARD_TARGET_KIND_CONNECTION:        f.connection,
				outwardv1alpha1.OutwardTargetKind_OUTWARD_TARGET_KIND_TRACE_DESTINATION: f.traceDest,
			} {
				resp, err := c.outward.SetOutwardTargetDisabled(ctx, &outwardv1alpha1.SetOutwardTargetDisabledRequest{
					OrgId: org, Kind: kind, TargetId: id, Disabled: true,
				})
				if err == nil {
					return resp, nil
				}
				if connect.CodeOf(err) != connect.CodeNotFound {
					return nil, err
				}
				last, lastErr = nil, err
			}
			return last, lastErr
		},
	},
	mcpserverv1alpha1connect.McpServerServiceListServersProcedure: {
		ownOrgOK: true,
		call: func(ctx context.Context, c *client, org string, _ orgFixture) (proto.Message, error) {
			return c.mcps.ListServers(ctx, &mcpserverv1alpha1.ListServersRequest{OrgId: org})
		},
	},
	mcpserverv1alpha1connect.McpServerServiceCreateServerProcedure: {
		ownOrgOK: true,
		call: func(ctx context.Context, c *client, org string, _ orgFixture) (proto.Message, error) {
			return c.mcps.CreateServer(ctx, &mcpserverv1alpha1.CreateServerRequest{
				OrgId: org, Name: fmt.Sprintf("intruder-%d", dbSeq.Add(1)), Url: "https://intruder.example.com/mcp",
			})
		},
	},
	mcpserverv1alpha1connect.McpServerServiceUpdateServerProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.mcps.UpdateServer(ctx, &mcpserverv1alpha1.UpdateServerRequest{
				OrgId: org, ServerId: f.mcpServer, Url: new("https://intruder.example.com/mcp"),
			})
		},
	},
	mcpserverv1alpha1connect.McpServerServiceSetServerClassProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.mcps.SetServerClass(ctx, &mcpserverv1alpha1.SetServerClassRequest{
				OrgId: org, ServerId: f.mcpServer, Class: &mcpserverv1alpha1.ServerClass{Kind: "fetch"},
			})
		},
	},
	mcpserverv1alpha1connect.McpServerServiceSetToolSinkProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.mcps.SetToolSink(ctx, &mcpserverv1alpha1.SetToolSinkRequest{
				OrgId: org, ServerId: f.mcpServer, Tool: f.mcpTool, ContentHash: "koinon_test_hash",
				Sink: &mcpserverv1alpha1.ToolSink{SinkClass: "none", Derivation: "fixed"},
			})
		},
	},
	leakguardv1alpha1connect.LeakGuardServiceGetLeakGuardProcedure: {
		ownOrgOK: true,
		call: func(ctx context.Context, c *client, org string, _ orgFixture) (proto.Message, error) {
			return c.leakguard.GetLeakGuard(ctx, &leakguardv1alpha1.GetLeakGuardRequest{OrgId: org})
		},
	},
	leakguardv1alpha1connect.LeakGuardServiceSetMatchThresholdProcedure: {
		ownOrgOK: true,
		call: func(ctx context.Context, c *client, org string, _ orgFixture) (proto.Message, error) {
			return c.leakguard.SetMatchThreshold(ctx, &leakguardv1alpha1.SetMatchThresholdRequest{OrgId: org, MatchThreshold: 99})
		},
	},
	leakguardv1alpha1connect.LeakGuardServiceAddTrustedHostProcedure: {
		ownOrgOK: true,
		call: func(ctx context.Context, c *client, org string, _ orgFixture) (proto.Message, error) {
			return c.leakguard.AddTrustedHost(ctx, &leakguardv1alpha1.AddTrustedHostRequest{OrgId: org, Host: fmt.Sprintf("intruder-%d.example.com", dbSeq.Add(1))})
		},
	},
	leakguardv1alpha1connect.LeakGuardServiceRemoveTrustedHostProcedure: {
		// The host is the org-owned input: another org's entry is never found here.
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.leakguard.RemoveTrustedHost(ctx, &leakguardv1alpha1.RemoveTrustedHostRequest{OrgId: org, Host: f.trustedHost})
		},
	},
	orgsettingsv1alpha1connect.OrgSettingsServiceGetRunContentRetentionProcedure: {
		ownOrgOK: true,
		call: func(ctx context.Context, c *client, org string, _ orgFixture) (proto.Message, error) {
			return c.orgsettings.GetRunContentRetention(ctx, &orgsettingsv1alpha1.GetRunContentRetentionRequest{OrgId: org})
		},
	},
	orgsettingsv1alpha1connect.OrgSettingsServiceSetRunContentRetentionProcedure: {
		ownOrgOK: true,
		call: func(ctx context.Context, c *client, org string, _ orgFixture) (proto.Message, error) {
			return c.orgsettings.SetRunContentRetention(ctx, &orgsettingsv1alpha1.SetRunContentRetentionRequest{OrgId: org, Days: 7})
		},
	},
	orgsettingsv1alpha1connect.OrgSettingsServiceGetHomeBackupRetentionProcedure: {
		ownOrgOK: true,
		call: func(ctx context.Context, c *client, org string, _ orgFixture) (proto.Message, error) {
			return c.orgsettings.GetHomeBackupRetention(ctx, &orgsettingsv1alpha1.GetHomeBackupRetentionRequest{OrgId: org})
		},
	},
	orgsettingsv1alpha1connect.OrgSettingsServiceSetHomeBackupRetentionProcedure: {
		ownOrgOK: true,
		call: func(ctx context.Context, c *client, org string, _ orgFixture) (proto.Message, error) {
			return c.orgsettings.SetHomeBackupRetention(ctx, &orgsettingsv1alpha1.SetHomeBackupRetentionRequest{OrgId: org, Days: 14})
		},
	},
	mcpserverv1alpha1connect.McpServerServiceDeleteServerProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.mcps.DeleteServer(ctx, &mcpserverv1alpha1.DeleteServerRequest{OrgId: org, ServerId: f.mcpServer})
		},
	},
	mcpserverv1alpha1connect.McpServerServiceListToolsProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.mcps.ListTools(ctx, &mcpserverv1alpha1.ListToolsRequest{OrgId: org, ServerId: f.mcpServer})
		},
	},
	mcpserverv1alpha1connect.McpServerServiceSetToolTierProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.mcps.SetToolTier(ctx, &mcpserverv1alpha1.SetToolTierRequest{
				OrgId: org, ServerId: f.mcpServer, Tool: f.mcpTool, Tier: mcpserverv1alpha1.Tier_TIER_AUTO,
			})
		},
	},
	mcpserverv1alpha1connect.McpServerServiceSetToolTwoApproversProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.mcps.SetToolTwoApprovers(ctx, &mcpserverv1alpha1.SetToolTwoApproversRequest{OrgId: org, ServerId: f.mcpServer, Tool: f.mcpTool, Required: true})
		},
	},
	mcpserverv1alpha1connect.McpServerServiceAcknowledgeToolReviewProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.mcps.AcknowledgeToolReview(ctx, &mcpserverv1alpha1.AcknowledgeToolReviewRequest{OrgId: org, ServerId: f.mcpServer, Tool: f.mcpTool})
		},
	},
	mcpserverv1alpha1connect.McpServerServiceGetAgentServersProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.mcps.GetAgentServers(ctx, &mcpserverv1alpha1.GetAgentServersRequest{OrgId: org, AgentId: f.agent.GetId()})
		},
	},
	mcpserverv1alpha1connect.McpServerServiceSetAgentServersProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return c.mcps.SetAgentServers(ctx, &mcpserverv1alpha1.SetAgentServersRequest{
				OrgId: org, AgentId: f.agent.GetId(), Servers: []*mcpserverv1alpha1.Allowance{{ServerId: f.mcpServer}},
			})
		},
	},

	tracev1alpha1connect.TraceDestinationServiceListDestinationsProcedure: {
		ownOrgOK: true,
		call: func(ctx context.Context, c *client, org string, _ orgFixture) (proto.Message, error) {
			return traceClient(c).ListDestinations(ctx, &tracev1alpha1.ListDestinationsRequest{OrgId: org})
		},
	},
	tracev1alpha1connect.TraceDestinationServiceCreateDestinationProcedure: {
		ownOrgOK: true,
		call: func(ctx context.Context, c *client, org string, _ orgFixture) (proto.Message, error) {
			otlp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
			defer otlp.Close()
			return traceClient(c).CreateDestination(ctx, &tracev1alpha1.CreateDestinationRequest{OrgId: org, Destination: &tracev1alpha1.DestinationInput{
				Name: fmt.Sprintf("intruder-%d", dbSeq.Add(1)), Backend: tracev1alpha1.Backend_BACKEND_GENERIC, EndpointUrl: otlp.URL,
				Secrets: map[string]string{"X-Token": "koinon_test_not-a-real-token"},
			}})
		},
	},
	tracev1alpha1connect.TraceDestinationServiceUpdateDestinationProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return traceClient(c).UpdateDestination(ctx, &tracev1alpha1.UpdateDestinationRequest{OrgId: org, DestinationId: f.traceDest, Destination: &tracev1alpha1.DestinationInput{
				Name: "intruder", Backend: tracev1alpha1.Backend_BACKEND_GENERIC, EndpointUrl: "https://intruder.example.com/v1/traces",
				Secrets: map[string]string{"X-Token": "koinon_test_not-a-real-token"},
			}})
		},
	},
	tracev1alpha1connect.TraceDestinationServiceDeleteDestinationProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return traceClient(c).DeleteDestination(ctx, &tracev1alpha1.DeleteDestinationRequest{OrgId: org, DestinationId: f.traceDest})
		},
	},
	tracev1alpha1connect.TraceDestinationServiceTestDestinationProcedure: {
		call: func(ctx context.Context, c *client, org string, f orgFixture) (proto.Message, error) {
			return traceClient(c).TestDestination(ctx, &tracev1alpha1.TestDestinationRequest{OrgId: org, DestinationId: f.traceDest})
		},
	},
	tracev1alpha1connect.TraceDestinationServiceGetSettingsProcedure: {
		ownOrgOK: true,
		call: func(ctx context.Context, c *client, org string, _ orgFixture) (proto.Message, error) {
			return traceClient(c).GetSettings(ctx, &tracev1alpha1.GetSettingsRequest{OrgId: org})
		},
	},
	tracev1alpha1connect.TraceDestinationServiceUpdateSettingsProcedure: {
		ownOrgOK: true,
		call: func(ctx context.Context, c *client, org string, _ orgFixture) (proto.Message, error) {
			return traceClient(c).UpdateSettings(ctx, &tracev1alpha1.UpdateSettingsRequest{
				OrgId: org, ContentLevel: tracev1alpha1.ContentLevel_CONTENT_LEVEL_REDACTED, SampleRate: new(float64(1)), PlatformCopyLevel: tracev1alpha1.ContentLevel_CONTENT_LEVEL_NONE,
			})
		},
	},
	tracev1alpha1connect.TraceDestinationServiceGetPlatformProcedure: {
		call: func(ctx context.Context, c *client, _ string, _ orgFixture) (proto.Message, error) {
			return traceClient(c).GetPlatform(ctx, &tracev1alpha1.GetPlatformRequest{})
		},
	},
	tracev1alpha1connect.TraceDestinationServiceSetPlatformDestinationProcedure: {
		call: func(ctx context.Context, c *client, _ string, _ orgFixture) (proto.Message, error) {
			return traceClient(c).SetPlatformDestination(ctx, &tracev1alpha1.SetPlatformDestinationRequest{Destination: &tracev1alpha1.DestinationInput{
				Backend: tracev1alpha1.Backend_BACKEND_GENERIC, EndpointUrl: "https://intruder.example.com/v1/traces",
				Secrets: map[string]string{"X-Token": "koinon_test_not-a-real-token"},
			}})
		},
	},
	tracev1alpha1connect.TraceDestinationServiceDeletePlatformDestinationProcedure: {
		call: func(ctx context.Context, c *client, _ string, _ orgFixture) (proto.Message, error) {
			return traceClient(c).DeletePlatformDestination(ctx, &tracev1alpha1.DeletePlatformDestinationRequest{})
		},
	},
	tracev1alpha1connect.TraceDestinationServiceTestPlatformDestinationProcedure: {
		call: func(ctx context.Context, c *client, _ string, _ orgFixture) (proto.Message, error) {
			return traceClient(c).TestPlatformDestination(ctx, &tracev1alpha1.TestPlatformDestinationRequest{})
		},
	},
	tracev1alpha1connect.TraceDestinationServiceSetPlatformCeilingProcedure: {
		call: func(ctx context.Context, c *client, _ string, _ orgFixture) (proto.Message, error) {
			return traceClient(c).SetPlatformCeiling(ctx, &tracev1alpha1.SetPlatformCeilingRequest{Ceiling: tracev1alpha1.ContentLevel_CONTENT_LEVEL_NONE})
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
// platformPricesReason: the platform price list is install-wide data (ADR 0098), not org-owned; every RPC
// takes no org ID and the server refuses everyone but a platform admin (the "platform price RPCs" specs in
// platformprices_test.go).
const platformPricesReason = "platform price list (ADR 0098): install-wide data, platform admins only; refused to " +
	"everyone else in platformprices_test.go"

var isolationExempt = map[string]string{
	budgetv1alpha1connect.PriceServiceListPriceProposalsProcedure:        platformPricesReason,
	budgetv1alpha1connect.PriceServiceGetPriceProposalProcedure:          platformPricesReason,
	budgetv1alpha1connect.PriceServiceAcceptPriceProposalProcedure:       platformPricesReason,
	budgetv1alpha1connect.PriceServiceDismissPriceProposalProcedure:      platformPricesReason,
	budgetv1alpha1connect.PriceServiceListPriceSourceFetchesProcedure:    platformPricesReason,
	budgetv1alpha1connect.PriceServiceListPlatformPriceVersionsProcedure: platformPricesReason,
	budgetv1alpha1connect.PriceServiceGetPlatformPriceVersionProcedure:   platformPricesReason,
	budgetv1alpha1connect.PriceServiceDiffPlatformPriceVersionsProcedure: platformPricesReason,
	budgetv1alpha1connect.PriceServiceRollbackPlatformPricesProcedure:    platformPricesReason,
	budgetv1alpha1connect.PriceServiceGetPriceRefreshSettingsProcedure:   platformPricesReason,
	budgetv1alpha1connect.PriceServiceCheckPriceSourcesProcedure:         platformPricesReason,
	budgetv1alpha1connect.PriceServiceExplainPlatformPriceProcedure:      platformPricesReason,
	authv1alpha1connect.AuthServiceWhoAmIProcedure:                       "describes the caller's own session; takes no IDs",
	authv1alpha1connect.AuthServiceChangePasswordProcedure:               "changes the caller's own password; takes no IDs",
	budgetv1alpha1connect.BudgetServiceListOrgBudgetsProcedure: "lists every org, for platform admins only; the server " +
		"refuses everyone else (TestListOrgBudgets in budgets_test.go)",
	templatev1alpha1connect.TemplateServiceListTemplatesProcedure: "templates are platform data shared by every org; " +
		"takes no IDs",
	connectionv1alpha1connect.ConnectionServiceListConnectorTypesProcedure: "connector types are platform data shared by " +
		"every org; takes no IDs",
	gatewayv1alpha1connect.GatewayStateServiceStreamStateProcedure: "the install-wide gateway State stream (ADR 0097); " +
		"served by internal/gateway/stateserver on the internal listener, not on this API: only a holder of the gateway's " +
		"bearer token reaches it (TestStateServer refuses everyone else), and its request names the copy, not an org",
	gatewayv1alpha1connect.GatewayStateServiceAckRevisionProcedure: "records a gateway copy's confirmation of an " +
		"install-wide revision (ADR 0097); served by internal/gateway/stateserver behind the gateway's bearer token, " +
		"not on this API; the request names the copy, not an org",
}

var _ = Describe("org isolation", func() {
	It("never serves or changes one org's data for another org's user", func(ctx SpecContext) {
		gh := githubtest.New()
		DeferCleanup(gh.Server.Close)
		e := newEnv(ctx, envOptions{
			keys:   secretstoretest.Keyring(),
			github: github.NewClient(github.Options{BaseURL: gh.URL()}),
			ghFake: gh,
			// The create case sends a test trace to a server on 127.0.0.1.
			allowedHosts: []string{"127.0.0.1"},
		})
		a := e.newIsolationFixture(ctx, gh.PEM)
		b := e.newIsolationFixture(ctx, gh.PEM)
		foreign := []string{a.org, a.agent.GetId(), a.draft.GetId(), a.ticket.GetId(), a.run.GetId(), a.connection, a.approval.ID.String(), a.provider, a.model, a.trustedHost}
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
	f.question = e.openQuestion(ctx, f)
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
	f.modelName = fmt.Sprintf("isolated-model-%d", dbSeq.Add(1))
	mod, err := f.owner.providers.CreateModel(ctx, &providerv1alpha1.CreateModelRequest{
		OrgId: f.org, ProviderId: f.provider, Name: f.modelName,
	})
	Expect(err).NotTo(HaveOccurred())
	f.model = mod.GetModel().GetId()
	srv, err := f.owner.mcps.CreateServer(ctx, &mcpserverv1alpha1.CreateServerRequest{
		OrgId: f.org, Name: "isolated", Url: "https://mcp.example.com/mcp", CredentialValue: "not-a-real-token",
	})
	Expect(err).NotTo(HaveOccurred())
	f.mcpServer, f.mcpTool = srv.GetServer().GetId(), "search"
	var sid pgtype.UUID
	Expect(sid.Scan(f.mcpServer)).To(Succeed())
	var orgID pgtype.UUID
	Expect(orgID.Scan(f.org)).To(Succeed())
	_, err = mcpservers.New(e.pool, nil, nil).SyncTools(ctx, orgID, sid, []mcpservers.ListedTool{{Name: f.mcpTool, Description: "Search"}})
	Expect(err).NotTo(HaveOccurred())
	// The leak-guard settings: a trusted host and a threshold of the org's own.
	f.trustedHost = fmt.Sprintf("trusted-%d.example.com", dbSeq.Add(1))
	lg := orgsettings.NewService(e.pool)
	_, err = lg.AddTrustedHost(ctx, orgID, audit.System("koinon-test:spec"), f.trustedHost)
	Expect(err).NotTo(HaveOccurred())
	thirty := 30
	Expect(lg.SetMatchThreshold(ctx, orgID, audit.System("koinon-test:spec"), &thirty)).To(Succeed())
	// A trace destination, inserted directly: the isolation cases only name it.
	Expect(e.pool.QueryRow(ctx,
		`INSERT INTO trace_destinations (org_id, name, backend, endpoint_url) VALUES ($1, 'isolated', 'generic', 'https://otlp.example.com/v1/traces') RETURNING id`,
		f.org).Scan(&f.traceDest)).To(Succeed())
	return f
}

// snapshot is everything about org A that a cross-org call could change.
func snapshot(ctx context.Context, e *env, f orgFixture) []int {
	org := f.org
	return []int{
		count(ctx, e, `SELECT count(*) FROM agents WHERE org_id = $1`, org),
		count(ctx, e, `SELECT count(*) FROM agents WHERE org_id = $1 AND status = 'active'`, org),
		count(ctx, e, `SELECT count(*) FROM agents WHERE org_id = $1 AND status = 'draft'`, org),
		count(ctx, e, `SELECT count(*) FROM tasks WHERE org_id = $1`, org),
		count(ctx, e, `SELECT count(*) FROM tasks WHERE org_id = $1 AND state = 'submitted'`, org),
		count(ctx, e, `SELECT count(*) FROM task_events WHERE org_id = $1`, org),
		count(ctx, e, `SELECT count(*) FROM dedupe_records WHERE org_id = $1`, org),
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
		count(ctx, e, `SELECT count(*) FROM mcp_servers WHERE org_id = $1`, org),
		count(ctx, e, `SELECT count(*) FROM mcp_servers WHERE org_id = $1 AND url = 'https://mcp.example.com/mcp'`, org),
		count(ctx, e, `SELECT count(*) FROM mcp_tools WHERE org_id = $1 AND tier = 'ask' AND review_reason IS NULL`, org),
		count(ctx, e, `SELECT count(*) FROM agent_mcp_servers WHERE org_id = $1`, org),
		count(ctx, e, `SELECT count(*) FROM trace_destinations WHERE org_id = $1 AND name = 'isolated'`, org),
		count(ctx, e, `SELECT count(*) FROM org_trace_settings WHERE org_id = $1`, org),
		count(ctx, e, `SELECT count(*) FROM mcp_tools WHERE org_id = $1 AND requires_two_approvers`, org),
		count(ctx, e, `SELECT count(*) FROM mcp_servers WHERE org_id = $1 AND kind = 'other' AND exposes_private_data`, org),
		count(ctx, e, `SELECT count(*) FROM org_fetch_trusted_hosts WHERE org_id = $1`, org),
		count(ctx, e, `SELECT count(*) FROM org_settings WHERE org_id = $1 AND leak_match_threshold = 30`, org),
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
