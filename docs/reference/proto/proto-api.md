# Protocol Documentation
<a name="top"></a>

## Table of Contents

- [koinon/auth/v1alpha1/auth.proto](#koinon_auth_v1alpha1_auth-proto)
    - [ChangePasswordRequest](#koinon-auth-v1alpha1-ChangePasswordRequest)
    - [ChangePasswordResponse](#koinon-auth-v1alpha1-ChangePasswordResponse)
    - [Membership](#koinon-auth-v1alpha1-Membership)
    - [User](#koinon-auth-v1alpha1-User)
    - [WhoAmIRequest](#koinon-auth-v1alpha1-WhoAmIRequest)
    - [WhoAmIResponse](#koinon-auth-v1alpha1-WhoAmIResponse)
  
    - [AuthMethod](#koinon-auth-v1alpha1-AuthMethod)
    - [OrgRole](#koinon-auth-v1alpha1-OrgRole)
  
    - [AuthService](#koinon-auth-v1alpha1-AuthService)
  
- [koinon/template/v1alpha1/template.proto](#koinon_template_v1alpha1_template-proto)
    - [Grant](#koinon-template-v1alpha1-Grant)
    - [ListTemplatesRequest](#koinon-template-v1alpha1-ListTemplatesRequest)
    - [ListTemplatesResponse](#koinon-template-v1alpha1-ListTemplatesResponse)
    - [NotifyTarget](#koinon-template-v1alpha1-NotifyTarget)
    - [RunLimits](#koinon-template-v1alpha1-RunLimits)
    - [Template](#koinon-template-v1alpha1-Template)
    - [Trigger](#koinon-template-v1alpha1-Trigger)
  
    - [Flavor](#koinon-template-v1alpha1-Flavor)
    - [NotifyTargetKind](#koinon-template-v1alpha1-NotifyTargetKind)
    - [Preset](#koinon-template-v1alpha1-Preset)
    - [TriggerType](#koinon-template-v1alpha1-TriggerType)
  
    - [TemplateService](#koinon-template-v1alpha1-TemplateService)
  
- [koinon/agent/v1alpha1/agent.proto](#koinon_agent_v1alpha1_agent-proto)
    - [ActivateAgentRequest](#koinon-agent-v1alpha1-ActivateAgentRequest)
    - [ActivateAgentResponse](#koinon-agent-v1alpha1-ActivateAgentResponse)
    - [Agent](#koinon-agent-v1alpha1-Agent)
    - [ArchiveAgentRequest](#koinon-agent-v1alpha1-ArchiveAgentRequest)
    - [ArchiveAgentResponse](#koinon-agent-v1alpha1-ArchiveAgentResponse)
    - [CreateAgentFromTemplateRequest](#koinon-agent-v1alpha1-CreateAgentFromTemplateRequest)
    - [CreateAgentFromTemplateResponse](#koinon-agent-v1alpha1-CreateAgentFromTemplateResponse)
    - [GetAgentModelRequest](#koinon-agent-v1alpha1-GetAgentModelRequest)
    - [GetAgentModelResponse](#koinon-agent-v1alpha1-GetAgentModelResponse)
    - [GetAgentRequest](#koinon-agent-v1alpha1-GetAgentRequest)
    - [GetAgentResponse](#koinon-agent-v1alpha1-GetAgentResponse)
    - [GrantBinding](#koinon-agent-v1alpha1-GrantBinding)
    - [ListAgentsRequest](#koinon-agent-v1alpha1-ListAgentsRequest)
    - [ListAgentsResponse](#koinon-agent-v1alpha1-ListAgentsResponse)
    - [ResumeAgentRequest](#koinon-agent-v1alpha1-ResumeAgentRequest)
    - [ResumeAgentResponse](#koinon-agent-v1alpha1-ResumeAgentResponse)
    - [RunAgentTriggerRequest](#koinon-agent-v1alpha1-RunAgentTriggerRequest)
    - [RunAgentTriggerResponse](#koinon-agent-v1alpha1-RunAgentTriggerResponse)
    - [ServedModel](#koinon-agent-v1alpha1-ServedModel)
    - [SetAgentModelRequest](#koinon-agent-v1alpha1-SetAgentModelRequest)
    - [SetAgentModelResponse](#koinon-agent-v1alpha1-SetAgentModelResponse)
    - [SetAgentThinkingRequest](#koinon-agent-v1alpha1-SetAgentThinkingRequest)
    - [SetAgentThinkingResponse](#koinon-agent-v1alpha1-SetAgentThinkingResponse)
    - [SetAgentTriggersRequest](#koinon-agent-v1alpha1-SetAgentTriggersRequest)
    - [SetAgentTriggersResponse](#koinon-agent-v1alpha1-SetAgentTriggersResponse)
    - [ThinkingState](#koinon-agent-v1alpha1-ThinkingState)
  
    - [AgentStatus](#koinon-agent-v1alpha1-AgentStatus)
    - [ModelSource](#koinon-agent-v1alpha1-ModelSource)
  
    - [AgentService](#koinon-agent-v1alpha1-AgentService)
  
- [koinon/approval/v1alpha1/approval.proto](#koinon_approval_v1alpha1_approval-proto)
    - [Approval](#koinon-approval-v1alpha1-Approval)
    - [ApprovalScope](#koinon-approval-v1alpha1-ApprovalScope)
    - [ApproveRequest](#koinon-approval-v1alpha1-ApproveRequest)
    - [ApproveResponse](#koinon-approval-v1alpha1-ApproveResponse)
    - [CommentCall](#koinon-approval-v1alpha1-CommentCall)
    - [DenyRequest](#koinon-approval-v1alpha1-DenyRequest)
    - [DenyResponse](#koinon-approval-v1alpha1-DenyResponse)
    - [FileChange](#koinon-approval-v1alpha1-FileChange)
    - [FileDiff](#koinon-approval-v1alpha1-FileDiff)
    - [GetApprovalDiffRequest](#koinon-approval-v1alpha1-GetApprovalDiffRequest)
    - [GetApprovalDiffResponse](#koinon-approval-v1alpha1-GetApprovalDiffResponse)
    - [GetApprovalRequest](#koinon-approval-v1alpha1-GetApprovalRequest)
    - [GetApprovalResponse](#koinon-approval-v1alpha1-GetApprovalResponse)
    - [LeakGuard](#koinon-approval-v1alpha1-LeakGuard)
    - [LeakGuard.DestinationEntry](#koinon-approval-v1alpha1-LeakGuard-DestinationEntry)
    - [ListApprovalScopesRequest](#koinon-approval-v1alpha1-ListApprovalScopesRequest)
    - [ListApprovalScopesResponse](#koinon-approval-v1alpha1-ListApprovalScopesResponse)
    - [ListApprovalsRequest](#koinon-approval-v1alpha1-ListApprovalsRequest)
    - [ListApprovalsResponse](#koinon-approval-v1alpha1-ListApprovalsResponse)
    - [McpCall](#koinon-approval-v1alpha1-McpCall)
    - [PullRequestCall](#koinon-approval-v1alpha1-PullRequestCall)
    - [RevokeApprovalScopeRequest](#koinon-approval-v1alpha1-RevokeApprovalScopeRequest)
    - [RevokeApprovalScopeResponse](#koinon-approval-v1alpha1-RevokeApprovalScopeResponse)
    - [Risk](#koinon-approval-v1alpha1-Risk)
    - [SavedFile](#koinon-approval-v1alpha1-SavedFile)
    - [SavedWorkCall](#koinon-approval-v1alpha1-SavedWorkCall)
  
    - [Decision](#koinon-approval-v1alpha1-Decision)
    - [FileStatus](#koinon-approval-v1alpha1-FileStatus)
    - [ScopeKind](#koinon-approval-v1alpha1-ScopeKind)
    - [Status](#koinon-approval-v1alpha1-Status)
  
    - [ApprovalService](#koinon-approval-v1alpha1-ApprovalService)
  
- [koinon/audit/v1alpha1/audit.proto](#koinon_audit_v1alpha1_audit-proto)
    - [AuditEvent](#koinon-audit-v1alpha1-AuditEvent)
    - [ListRequest](#koinon-audit-v1alpha1-ListRequest)
    - [ListResponse](#koinon-audit-v1alpha1-ListResponse)
  
    - [AuditScope](#koinon-audit-v1alpha1-AuditScope)
  
    - [AuditService](#koinon-audit-v1alpha1-AuditService)
  
- [koinon/budget/v1alpha1/budget.proto](#koinon_budget_v1alpha1_budget-proto)
    - [AgentBudget](#koinon-budget-v1alpha1-AgentBudget)
    - [AgentRunLimits](#koinon-budget-v1alpha1-AgentRunLimits)
    - [GetAgentBudgetRequest](#koinon-budget-v1alpha1-GetAgentBudgetRequest)
    - [GetAgentBudgetResponse](#koinon-budget-v1alpha1-GetAgentBudgetResponse)
    - [GetAgentRunLimitsRequest](#koinon-budget-v1alpha1-GetAgentRunLimitsRequest)
    - [GetAgentRunLimitsResponse](#koinon-budget-v1alpha1-GetAgentRunLimitsResponse)
    - [GetOrgBudgetRequest](#koinon-budget-v1alpha1-GetOrgBudgetRequest)
    - [GetOrgBudgetResponse](#koinon-budget-v1alpha1-GetOrgBudgetResponse)
    - [ListAgentBudgetsRequest](#koinon-budget-v1alpha1-ListAgentBudgetsRequest)
    - [ListAgentBudgetsResponse](#koinon-budget-v1alpha1-ListAgentBudgetsResponse)
    - [ListNotificationsRequest](#koinon-budget-v1alpha1-ListNotificationsRequest)
    - [ListNotificationsResponse](#koinon-budget-v1alpha1-ListNotificationsResponse)
    - [ListOrgBudgetsRequest](#koinon-budget-v1alpha1-ListOrgBudgetsRequest)
    - [ListOrgBudgetsResponse](#koinon-budget-v1alpha1-ListOrgBudgetsResponse)
    - [MarkNotificationReadRequest](#koinon-budget-v1alpha1-MarkNotificationReadRequest)
    - [MarkNotificationReadResponse](#koinon-budget-v1alpha1-MarkNotificationReadResponse)
    - [Notification](#koinon-budget-v1alpha1-Notification)
    - [OrgBudget](#koinon-budget-v1alpha1-OrgBudget)
    - [OrgBudgetSummary](#koinon-budget-v1alpha1-OrgBudgetSummary)
    - [RunLimitValues](#koinon-budget-v1alpha1-RunLimitValues)
    - [SetAgentBudgetRequest](#koinon-budget-v1alpha1-SetAgentBudgetRequest)
    - [SetAgentBudgetResponse](#koinon-budget-v1alpha1-SetAgentBudgetResponse)
    - [SetAgentRunLimitsRequest](#koinon-budget-v1alpha1-SetAgentRunLimitsRequest)
    - [SetAgentRunLimitsResponse](#koinon-budget-v1alpha1-SetAgentRunLimitsResponse)
    - [SetMonthlyCapRequest](#koinon-budget-v1alpha1-SetMonthlyCapRequest)
    - [SetMonthlyCapResponse](#koinon-budget-v1alpha1-SetMonthlyCapResponse)
    - [SetPlatformCeilingRequest](#koinon-budget-v1alpha1-SetPlatformCeilingRequest)
    - [SetPlatformCeilingResponse](#koinon-budget-v1alpha1-SetPlatformCeilingResponse)
  
    - [BudgetService](#koinon-budget-v1alpha1-BudgetService)
  
- [koinon/budget/v1alpha1/price.proto](#koinon_budget_v1alpha1_price-proto)
    - [FlaggedPrice](#koinon-budget-v1alpha1-FlaggedPrice)
    - [GetDefaultPriceChangesRequest](#koinon-budget-v1alpha1-GetDefaultPriceChangesRequest)
    - [GetDefaultPriceChangesResponse](#koinon-budget-v1alpha1-GetDefaultPriceChangesResponse)
    - [GetDefaultPricesRequest](#koinon-budget-v1alpha1-GetDefaultPricesRequest)
    - [GetDefaultPricesResponse](#koinon-budget-v1alpha1-GetDefaultPricesResponse)
    - [ListFlaggedPricesRequest](#koinon-budget-v1alpha1-ListFlaggedPricesRequest)
    - [ListFlaggedPricesResponse](#koinon-budget-v1alpha1-ListFlaggedPricesResponse)
    - [ListPricesRequest](#koinon-budget-v1alpha1-ListPricesRequest)
    - [ListPricesResponse](#koinon-budget-v1alpha1-ListPricesResponse)
    - [PriceChange](#koinon-budget-v1alpha1-PriceChange)
    - [PriceEntry](#koinon-budget-v1alpha1-PriceEntry)
    - [PriceMatch](#koinon-budget-v1alpha1-PriceMatch)
    - [PriceTier](#koinon-budget-v1alpha1-PriceTier)
    - [PriceVersion](#koinon-budget-v1alpha1-PriceVersion)
    - [Rates](#koinon-budget-v1alpha1-Rates)
    - [RetirePriceRequest](#koinon-budget-v1alpha1-RetirePriceRequest)
    - [RetirePriceResponse](#koinon-budget-v1alpha1-RetirePriceResponse)
    - [SetPriceRequest](#koinon-budget-v1alpha1-SetPriceRequest)
    - [SetPriceResponse](#koinon-budget-v1alpha1-SetPriceResponse)
  
    - [PriceService](#koinon-budget-v1alpha1-PriceService)
  
- [koinon/outward/v1alpha1/outward.proto](#koinon_outward_v1alpha1_outward-proto)
    - [AdminDisabled](#koinon-outward-v1alpha1-AdminDisabled)
    - [OutwardTargetState](#koinon-outward-v1alpha1-OutwardTargetState)
    - [SetOutwardTargetDisabledRequest](#koinon-outward-v1alpha1-SetOutwardTargetDisabledRequest)
    - [SetOutwardTargetDisabledResponse](#koinon-outward-v1alpha1-SetOutwardTargetDisabledResponse)
  
    - [OutwardTargetKind](#koinon-outward-v1alpha1-OutwardTargetKind)
  
    - [OutwardService](#koinon-outward-v1alpha1-OutwardService)
  
- [koinon/connection/v1alpha1/connection.proto](#koinon_connection_v1alpha1_connection-proto)
    - [BoundAgent](#koinon-connection-v1alpha1-BoundAgent)
    - [BoundAgents](#koinon-connection-v1alpha1-BoundAgents)
    - [CheckResult](#koinon-connection-v1alpha1-CheckResult)
    - [Connection](#koinon-connection-v1alpha1-Connection)
    - [ConnectorType](#koinon-connection-v1alpha1-ConnectorType)
    - [CreateGitHubConnectionRequest](#koinon-connection-v1alpha1-CreateGitHubConnectionRequest)
    - [CreateGitHubConnectionResponse](#koinon-connection-v1alpha1-CreateGitHubConnectionResponse)
    - [CreateGiteaConnectionRequest](#koinon-connection-v1alpha1-CreateGiteaConnectionRequest)
    - [CreateGiteaConnectionResponse](#koinon-connection-v1alpha1-CreateGiteaConnectionResponse)
    - [DeleteConnectionRequest](#koinon-connection-v1alpha1-DeleteConnectionRequest)
    - [DeleteConnectionResponse](#koinon-connection-v1alpha1-DeleteConnectionResponse)
    - [GetConnectionRequest](#koinon-connection-v1alpha1-GetConnectionRequest)
    - [GetConnectionResponse](#koinon-connection-v1alpha1-GetConnectionResponse)
    - [GitHubConnection](#koinon-connection-v1alpha1-GitHubConnection)
    - [GitHubConnectionSecret](#koinon-connection-v1alpha1-GitHubConnectionSecret)
    - [GiteaConnection](#koinon-connection-v1alpha1-GiteaConnection)
    - [GiteaConnectionSecret](#koinon-connection-v1alpha1-GiteaConnectionSecret)
    - [InstallationRepo](#koinon-connection-v1alpha1-InstallationRepo)
    - [ListConnectionsRequest](#koinon-connection-v1alpha1-ListConnectionsRequest)
    - [ListConnectionsResponse](#koinon-connection-v1alpha1-ListConnectionsResponse)
    - [ListConnectorTypesRequest](#koinon-connection-v1alpha1-ListConnectorTypesRequest)
    - [ListConnectorTypesResponse](#koinon-connection-v1alpha1-ListConnectorTypesResponse)
    - [ListInstallationReposRequest](#koinon-connection-v1alpha1-ListInstallationReposRequest)
    - [ListInstallationReposResponse](#koinon-connection-v1alpha1-ListInstallationReposResponse)
    - [Scope](#koinon-connection-v1alpha1-Scope)
    - [SetConnectionSecretRequest](#koinon-connection-v1alpha1-SetConnectionSecretRequest)
    - [SetConnectionSecretResponse](#koinon-connection-v1alpha1-SetConnectionSecretResponse)
    - [TestConnectionRequest](#koinon-connection-v1alpha1-TestConnectionRequest)
    - [TestConnectionResponse](#koinon-connection-v1alpha1-TestConnectionResponse)
  
    - [ConnectionStatus](#koinon-connection-v1alpha1-ConnectionStatus)
    - [Tier](#koinon-connection-v1alpha1-Tier)
  
    - [ConnectionService](#koinon-connection-v1alpha1-ConnectionService)
  
- [koinon/leakguard/v1alpha1/leakguard.proto](#koinon_leakguard_v1alpha1_leakguard-proto)
    - [AddTrustedHostRequest](#koinon-leakguard-v1alpha1-AddTrustedHostRequest)
    - [AddTrustedHostResponse](#koinon-leakguard-v1alpha1-AddTrustedHostResponse)
    - [CodeHostScope](#koinon-leakguard-v1alpha1-CodeHostScope)
    - [GetLeakGuardRequest](#koinon-leakguard-v1alpha1-GetLeakGuardRequest)
    - [GetLeakGuardResponse](#koinon-leakguard-v1alpha1-GetLeakGuardResponse)
    - [LeakGuard](#koinon-leakguard-v1alpha1-LeakGuard)
    - [RemoveTrustedHostRequest](#koinon-leakguard-v1alpha1-RemoveTrustedHostRequest)
    - [RemoveTrustedHostResponse](#koinon-leakguard-v1alpha1-RemoveTrustedHostResponse)
    - [SetMatchThresholdRequest](#koinon-leakguard-v1alpha1-SetMatchThresholdRequest)
    - [SetMatchThresholdResponse](#koinon-leakguard-v1alpha1-SetMatchThresholdResponse)
  
    - [LeakGuardService](#koinon-leakguard-v1alpha1-LeakGuardService)
  
- [koinon/mcpserver/v1alpha1/mcpserver.proto](#koinon_mcpserver_v1alpha1_mcpserver-proto)
    - [AcknowledgeToolReviewRequest](#koinon-mcpserver-v1alpha1-AcknowledgeToolReviewRequest)
    - [AcknowledgeToolReviewResponse](#koinon-mcpserver-v1alpha1-AcknowledgeToolReviewResponse)
    - [Allowance](#koinon-mcpserver-v1alpha1-Allowance)
    - [CreateServerRequest](#koinon-mcpserver-v1alpha1-CreateServerRequest)
    - [CreateServerResponse](#koinon-mcpserver-v1alpha1-CreateServerResponse)
    - [DeleteServerRequest](#koinon-mcpserver-v1alpha1-DeleteServerRequest)
    - [DeleteServerResponse](#koinon-mcpserver-v1alpha1-DeleteServerResponse)
    - [GetAgentServersRequest](#koinon-mcpserver-v1alpha1-GetAgentServersRequest)
    - [GetAgentServersResponse](#koinon-mcpserver-v1alpha1-GetAgentServersResponse)
    - [ListServersRequest](#koinon-mcpserver-v1alpha1-ListServersRequest)
    - [ListServersResponse](#koinon-mcpserver-v1alpha1-ListServersResponse)
    - [ListToolsRequest](#koinon-mcpserver-v1alpha1-ListToolsRequest)
    - [ListToolsResponse](#koinon-mcpserver-v1alpha1-ListToolsResponse)
    - [Server](#koinon-mcpserver-v1alpha1-Server)
    - [ServerClass](#koinon-mcpserver-v1alpha1-ServerClass)
    - [SetAgentServersRequest](#koinon-mcpserver-v1alpha1-SetAgentServersRequest)
    - [SetAgentServersResponse](#koinon-mcpserver-v1alpha1-SetAgentServersResponse)
    - [SetServerClassRequest](#koinon-mcpserver-v1alpha1-SetServerClassRequest)
    - [SetServerClassResponse](#koinon-mcpserver-v1alpha1-SetServerClassResponse)
    - [SetToolSinkRequest](#koinon-mcpserver-v1alpha1-SetToolSinkRequest)
    - [SetToolSinkResponse](#koinon-mcpserver-v1alpha1-SetToolSinkResponse)
    - [SetToolTierRequest](#koinon-mcpserver-v1alpha1-SetToolTierRequest)
    - [SetToolTierResponse](#koinon-mcpserver-v1alpha1-SetToolTierResponse)
    - [SetToolTwoApproversRequest](#koinon-mcpserver-v1alpha1-SetToolTwoApproversRequest)
    - [SetToolTwoApproversResponse](#koinon-mcpserver-v1alpha1-SetToolTwoApproversResponse)
    - [Tool](#koinon-mcpserver-v1alpha1-Tool)
    - [ToolSink](#koinon-mcpserver-v1alpha1-ToolSink)
    - [UpdateServerRequest](#koinon-mcpserver-v1alpha1-UpdateServerRequest)
    - [UpdateServerResponse](#koinon-mcpserver-v1alpha1-UpdateServerResponse)
  
    - [Health](#koinon-mcpserver-v1alpha1-Health)
    - [Tier](#koinon-mcpserver-v1alpha1-Tier)
  
    - [McpServerService](#koinon-mcpserver-v1alpha1-McpServerService)
  
- [koinon/orgsettings/v1alpha1/orgsettings.proto](#koinon_orgsettings_v1alpha1_orgsettings-proto)
    - [GetHomeBackupRetentionRequest](#koinon-orgsettings-v1alpha1-GetHomeBackupRetentionRequest)
    - [GetHomeBackupRetentionResponse](#koinon-orgsettings-v1alpha1-GetHomeBackupRetentionResponse)
    - [GetRunContentRetentionRequest](#koinon-orgsettings-v1alpha1-GetRunContentRetentionRequest)
    - [GetRunContentRetentionResponse](#koinon-orgsettings-v1alpha1-GetRunContentRetentionResponse)
    - [HomeBackupRetention](#koinon-orgsettings-v1alpha1-HomeBackupRetention)
    - [RunContentRetention](#koinon-orgsettings-v1alpha1-RunContentRetention)
    - [SetHomeBackupRetentionRequest](#koinon-orgsettings-v1alpha1-SetHomeBackupRetentionRequest)
    - [SetHomeBackupRetentionResponse](#koinon-orgsettings-v1alpha1-SetHomeBackupRetentionResponse)
    - [SetRunContentRetentionRequest](#koinon-orgsettings-v1alpha1-SetRunContentRetentionRequest)
    - [SetRunContentRetentionResponse](#koinon-orgsettings-v1alpha1-SetRunContentRetentionResponse)
  
    - [OrgSettingsService](#koinon-orgsettings-v1alpha1-OrgSettingsService)
  
- [koinon/platform/v1alpha1/platform.proto](#koinon_platform_v1alpha1_platform-proto)
    - [PlatformService](#koinon-platform-v1alpha1-PlatformService)
  
- [koinon/provider/v1alpha1/provider.proto](#koinon_provider_v1alpha1_provider-proto)
    - [CheckProviderEchoRequest](#koinon-provider-v1alpha1-CheckProviderEchoRequest)
    - [CheckProviderEchoResponse](#koinon-provider-v1alpha1-CheckProviderEchoResponse)
    - [CreateModelRequest](#koinon-provider-v1alpha1-CreateModelRequest)
    - [CreateModelResponse](#koinon-provider-v1alpha1-CreateModelResponse)
    - [CreateProviderRequest](#koinon-provider-v1alpha1-CreateProviderRequest)
    - [CreateProviderResponse](#koinon-provider-v1alpha1-CreateProviderResponse)
    - [DefaultModelChoice](#koinon-provider-v1alpha1-DefaultModelChoice)
    - [DeleteModelRequest](#koinon-provider-v1alpha1-DeleteModelRequest)
    - [DeleteModelResponse](#koinon-provider-v1alpha1-DeleteModelResponse)
    - [DeleteProviderRequest](#koinon-provider-v1alpha1-DeleteProviderRequest)
    - [DeleteProviderResponse](#koinon-provider-v1alpha1-DeleteProviderResponse)
    - [EchoCheck](#koinon-provider-v1alpha1-EchoCheck)
    - [EchoDiffSegment](#koinon-provider-v1alpha1-EchoDiffSegment)
    - [GetDefaultModelRequest](#koinon-provider-v1alpha1-GetDefaultModelRequest)
    - [GetDefaultModelResponse](#koinon-provider-v1alpha1-GetDefaultModelResponse)
    - [ListModelsRequest](#koinon-provider-v1alpha1-ListModelsRequest)
    - [ListModelsResponse](#koinon-provider-v1alpha1-ListModelsResponse)
    - [ListProvidersRequest](#koinon-provider-v1alpha1-ListProvidersRequest)
    - [ListProvidersResponse](#koinon-provider-v1alpha1-ListProvidersResponse)
    - [Model](#koinon-provider-v1alpha1-Model)
    - [ModelFormat](#koinon-provider-v1alpha1-ModelFormat)
    - [ModelFormatsRequest](#koinon-provider-v1alpha1-ModelFormatsRequest)
    - [ModelFormatsResponse](#koinon-provider-v1alpha1-ModelFormatsResponse)
    - [Provider](#koinon-provider-v1alpha1-Provider)
    - [ProviderTypeInfoRequest](#koinon-provider-v1alpha1-ProviderTypeInfoRequest)
    - [ProviderTypeInfoResponse](#koinon-provider-v1alpha1-ProviderTypeInfoResponse)
    - [Rates](#koinon-provider-v1alpha1-Rates)
    - [SetDefaultModelRequest](#koinon-provider-v1alpha1-SetDefaultModelRequest)
    - [SetDefaultModelResponse](#koinon-provider-v1alpha1-SetDefaultModelResponse)
    - [UpdateModelRequest](#koinon-provider-v1alpha1-UpdateModelRequest)
    - [UpdateModelResponse](#koinon-provider-v1alpha1-UpdateModelResponse)
    - [UpdateProviderRequest](#koinon-provider-v1alpha1-UpdateProviderRequest)
    - [UpdateProviderResponse](#koinon-provider-v1alpha1-UpdateProviderResponse)
  
    - [EchoDiffSegment.Op](#koinon-provider-v1alpha1-EchoDiffSegment-Op)
  
    - [ProviderService](#koinon-provider-v1alpha1-ProviderService)
  
- [koinon/question/v1alpha1/question.proto](#koinon_question_v1alpha1_question-proto)
    - [AnswerQuestionRequest](#koinon-question-v1alpha1-AnswerQuestionRequest)
    - [AnswerQuestionResponse](#koinon-question-v1alpha1-AnswerQuestionResponse)
    - [AskableMember](#koinon-question-v1alpha1-AskableMember)
    - [DismissQuestionRequest](#koinon-question-v1alpha1-DismissQuestionRequest)
    - [DismissQuestionResponse](#koinon-question-v1alpha1-DismissQuestionResponse)
    - [ListAskableMembersRequest](#koinon-question-v1alpha1-ListAskableMembersRequest)
    - [ListAskableMembersResponse](#koinon-question-v1alpha1-ListAskableMembersResponse)
    - [ListQuestionsRequest](#koinon-question-v1alpha1-ListQuestionsRequest)
    - [ListQuestionsResponse](#koinon-question-v1alpha1-ListQuestionsResponse)
    - [Question](#koinon-question-v1alpha1-Question)
    - [Recipient](#koinon-question-v1alpha1-Recipient)
  
    - [AnswerKind](#koinon-question-v1alpha1-AnswerKind)
    - [QuestionState](#koinon-question-v1alpha1-QuestionState)
  
    - [QuestionService](#koinon-question-v1alpha1-QuestionService)
  
- [koinon/run/v1alpha1/run.proto](#koinon_run_v1alpha1_run-proto)
    - [CancelRunRequest](#koinon-run-v1alpha1-CancelRunRequest)
    - [CancelRunResponse](#koinon-run-v1alpha1-CancelRunResponse)
    - [Cost](#koinon-run-v1alpha1-Cost)
    - [GetRunRequest](#koinon-run-v1alpha1-GetRunRequest)
    - [GetRunResponse](#koinon-run-v1alpha1-GetRunResponse)
    - [Heartbeat](#koinon-run-v1alpha1-Heartbeat)
    - [LiveChunk](#koinon-run-v1alpha1-LiveChunk)
    - [LiveChunks](#koinon-run-v1alpha1-LiveChunks)
    - [Run](#koinon-run-v1alpha1-Run)
    - [RunEvent](#koinon-run-v1alpha1-RunEvent)
    - [WatchRunRequest](#koinon-run-v1alpha1-WatchRunRequest)
    - [WatchRunResponse](#koinon-run-v1alpha1-WatchRunResponse)
  
    - [FailureClass](#koinon-run-v1alpha1-FailureClass)
    - [ParkReason](#koinon-run-v1alpha1-ParkReason)
    - [RunState](#koinon-run-v1alpha1-RunState)
  
    - [RunService](#koinon-run-v1alpha1-RunService)
  
- [koinon/task/v1alpha1/task.proto](#koinon_task_v1alpha1_task-proto)
    - [AppendMessageRequest](#koinon-task-v1alpha1-AppendMessageRequest)
    - [AppendMessageResponse](#koinon-task-v1alpha1-AppendMessageResponse)
    - [CancelTaskRequest](#koinon-task-v1alpha1-CancelTaskRequest)
    - [CancelTaskResponse](#koinon-task-v1alpha1-CancelTaskResponse)
    - [CreateTaskRequest](#koinon-task-v1alpha1-CreateTaskRequest)
    - [CreateTaskResponse](#koinon-task-v1alpha1-CreateTaskResponse)
    - [Creator](#koinon-task-v1alpha1-Creator)
    - [GetTaskRequest](#koinon-task-v1alpha1-GetTaskRequest)
    - [GetTaskResponse](#koinon-task-v1alpha1-GetTaskResponse)
    - [ListTasksRequest](#koinon-task-v1alpha1-ListTasksRequest)
    - [ListTasksResponse](#koinon-task-v1alpha1-ListTasksResponse)
    - [NotifyTypingRequest](#koinon-task-v1alpha1-NotifyTypingRequest)
    - [NotifyTypingResponse](#koinon-task-v1alpha1-NotifyTypingResponse)
    - [SendDirectMessageRequest](#koinon-task-v1alpha1-SendDirectMessageRequest)
    - [SendDirectMessageResponse](#koinon-task-v1alpha1-SendDirectMessageResponse)
    - [Task](#koinon-task-v1alpha1-Task)
    - [TaskEvent](#koinon-task-v1alpha1-TaskEvent)
  
    - [ActorKind](#koinon-task-v1alpha1-ActorKind)
    - [CreatorKind](#koinon-task-v1alpha1-CreatorKind)
    - [TaskState](#koinon-task-v1alpha1-TaskState)
  
    - [TaskService](#koinon-task-v1alpha1-TaskService)
  
- [koinon/ticket/v1alpha1/ticket.proto](#koinon_ticket_v1alpha1_ticket-proto)
    - [AppendMessageRequest](#koinon-ticket-v1alpha1-AppendMessageRequest)
    - [AppendMessageResponse](#koinon-ticket-v1alpha1-AppendMessageResponse)
    - [CreateTicketRequest](#koinon-ticket-v1alpha1-CreateTicketRequest)
    - [CreateTicketResponse](#koinon-ticket-v1alpha1-CreateTicketResponse)
    - [GetTicketRequest](#koinon-ticket-v1alpha1-GetTicketRequest)
    - [GetTicketResponse](#koinon-ticket-v1alpha1-GetTicketResponse)
    - [ListTicketsRequest](#koinon-ticket-v1alpha1-ListTicketsRequest)
    - [ListTicketsResponse](#koinon-ticket-v1alpha1-ListTicketsResponse)
    - [MoveTicketRequest](#koinon-ticket-v1alpha1-MoveTicketRequest)
    - [MoveTicketResponse](#koinon-ticket-v1alpha1-MoveTicketResponse)
    - [Ticket](#koinon-ticket-v1alpha1-Ticket)
    - [TicketEvent](#koinon-ticket-v1alpha1-TicketEvent)
  
    - [ActorKind](#koinon-ticket-v1alpha1-ActorKind)
    - [TicketStatus](#koinon-ticket-v1alpha1-TicketStatus)
  
    - [TicketService](#koinon-ticket-v1alpha1-TicketService)
  
- [koinon/token/v1alpha1/token.proto](#koinon_token_v1alpha1_token-proto)
    - [CreateTokenRequest](#koinon-token-v1alpha1-CreateTokenRequest)
    - [CreateTokenResponse](#koinon-token-v1alpha1-CreateTokenResponse)
    - [ListTokensRequest](#koinon-token-v1alpha1-ListTokensRequest)
    - [ListTokensResponse](#koinon-token-v1alpha1-ListTokensResponse)
    - [RebindTokenRequest](#koinon-token-v1alpha1-RebindTokenRequest)
    - [RebindTokenResponse](#koinon-token-v1alpha1-RebindTokenResponse)
    - [RevokeTokenRequest](#koinon-token-v1alpha1-RevokeTokenRequest)
    - [RevokeTokenResponse](#koinon-token-v1alpha1-RevokeTokenResponse)
    - [Token](#koinon-token-v1alpha1-Token)
  
    - [TokenService](#koinon-token-v1alpha1-TokenService)
  
- [koinon/trace/v1alpha1/trace.proto](#koinon_trace_v1alpha1_trace-proto)
    - [CreateDestinationRequest](#koinon-trace-v1alpha1-CreateDestinationRequest)
    - [CreateDestinationResponse](#koinon-trace-v1alpha1-CreateDestinationResponse)
    - [DeleteDestinationRequest](#koinon-trace-v1alpha1-DeleteDestinationRequest)
    - [DeleteDestinationResponse](#koinon-trace-v1alpha1-DeleteDestinationResponse)
    - [DeletePlatformDestinationRequest](#koinon-trace-v1alpha1-DeletePlatformDestinationRequest)
    - [DeletePlatformDestinationResponse](#koinon-trace-v1alpha1-DeletePlatformDestinationResponse)
    - [Destination](#koinon-trace-v1alpha1-Destination)
    - [Destination.ParamsEntry](#koinon-trace-v1alpha1-Destination-ParamsEntry)
    - [Destination.ResourceAttributesEntry](#koinon-trace-v1alpha1-Destination-ResourceAttributesEntry)
    - [DestinationInput](#koinon-trace-v1alpha1-DestinationInput)
    - [DestinationInput.ParamsEntry](#koinon-trace-v1alpha1-DestinationInput-ParamsEntry)
    - [DestinationInput.ResourceAttributesEntry](#koinon-trace-v1alpha1-DestinationInput-ResourceAttributesEntry)
    - [DestinationInput.SecretsEntry](#koinon-trace-v1alpha1-DestinationInput-SecretsEntry)
    - [GetPlatformRequest](#koinon-trace-v1alpha1-GetPlatformRequest)
    - [GetPlatformResponse](#koinon-trace-v1alpha1-GetPlatformResponse)
    - [GetSettingsRequest](#koinon-trace-v1alpha1-GetSettingsRequest)
    - [GetSettingsResponse](#koinon-trace-v1alpha1-GetSettingsResponse)
    - [Health](#koinon-trace-v1alpha1-Health)
    - [ListDestinationsRequest](#koinon-trace-v1alpha1-ListDestinationsRequest)
    - [ListDestinationsResponse](#koinon-trace-v1alpha1-ListDestinationsResponse)
    - [SetPlatformCeilingRequest](#koinon-trace-v1alpha1-SetPlatformCeilingRequest)
    - [SetPlatformCeilingResponse](#koinon-trace-v1alpha1-SetPlatformCeilingResponse)
    - [SetPlatformDestinationRequest](#koinon-trace-v1alpha1-SetPlatformDestinationRequest)
    - [SetPlatformDestinationResponse](#koinon-trace-v1alpha1-SetPlatformDestinationResponse)
    - [Settings](#koinon-trace-v1alpha1-Settings)
    - [TestDestinationRequest](#koinon-trace-v1alpha1-TestDestinationRequest)
    - [TestDestinationResponse](#koinon-trace-v1alpha1-TestDestinationResponse)
    - [TestPlatformDestinationRequest](#koinon-trace-v1alpha1-TestPlatformDestinationRequest)
    - [TestPlatformDestinationResponse](#koinon-trace-v1alpha1-TestPlatformDestinationResponse)
    - [TestResult](#koinon-trace-v1alpha1-TestResult)
    - [UpdateDestinationRequest](#koinon-trace-v1alpha1-UpdateDestinationRequest)
    - [UpdateDestinationResponse](#koinon-trace-v1alpha1-UpdateDestinationResponse)
    - [UpdateSettingsRequest](#koinon-trace-v1alpha1-UpdateSettingsRequest)
    - [UpdateSettingsResponse](#koinon-trace-v1alpha1-UpdateSettingsResponse)
  
    - [Backend](#koinon-trace-v1alpha1-Backend)
    - [ContentLevel](#koinon-trace-v1alpha1-ContentLevel)
  
    - [TraceDestinationService](#koinon-trace-v1alpha1-TraceDestinationService)
  
- [Scalar Value Types](#scalar-value-types)



<a name="koinon_auth_v1alpha1_auth-proto"></a>
<p align="right"><a href="#top">Top</a></p>

## koinon/auth/v1alpha1/auth.proto



<a name="koinon-auth-v1alpha1-ChangePasswordRequest"></a>

### ChangePasswordRequest
ChangePasswordRequest carries the current and the new password.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| current_password | [string](#string) |  |  |
| new_password | [string](#string) |  | At least 12 characters and different from the current password. |






<a name="koinon-auth-v1alpha1-ChangePasswordResponse"></a>

### ChangePasswordResponse
ChangePasswordResponse is empty.






<a name="koinon-auth-v1alpha1-Membership"></a>

### Membership
Membership is the caller&#39;s effective role in one org.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| org_slug | [string](#string) |  |  |
| org_display_name | [string](#string) |  |  |
| role | [OrgRole](#koinon-auth-v1alpha1-OrgRole) |  |  |






<a name="koinon-auth-v1alpha1-User"></a>

### User
User is the authenticated human.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| id | [string](#string) |  |  |
| email | [string](#string) |  |  |
| display_name | [string](#string) |  |  |
| personal_org_id | [string](#string) |  |  |






<a name="koinon-auth-v1alpha1-WhoAmIRequest"></a>

### WhoAmIRequest
WhoAmIRequest is empty.






<a name="koinon-auth-v1alpha1-WhoAmIResponse"></a>

### WhoAmIResponse
WhoAmIResponse describes the caller.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| user | [User](#koinon-auth-v1alpha1-User) |  |  |
| platform_admin | [bool](#bool) |  |  |
| memberships | [Membership](#koinon-auth-v1alpha1-Membership) | repeated |  |
| auth_method | [AuthMethod](#koinon-auth-v1alpha1-AuthMethod) |  |  |
| password_change_required | [bool](#bool) |  | True until the bootstrap admin replaces the one-time password. While set, every call except WhoAmI and ChangePassword fails with PermissionDenied. |





 


<a name="koinon-auth-v1alpha1-AuthMethod"></a>

### AuthMethod
AuthMethod is how the session was established.

| Name | Number | Description |
| ---- | ------ | ----------- |
| AUTH_METHOD_UNSPECIFIED | 0 |  |
| AUTH_METHOD_PASSWORD | 1 | The bootstrap admin&#39;s local password. |
| AUTH_METHOD_OIDC | 2 | OpenID Connect authorization code flow. |



<a name="koinon-auth-v1alpha1-OrgRole"></a>

### OrgRole
OrgRole mirrors the org_role enum in Postgres, lowest to highest.

| Name | Number | Description |
| ---- | ------ | ----------- |
| ORG_ROLE_UNSPECIFIED | 0 |  |
| ORG_ROLE_VIEWER | 1 |  |
| ORG_ROLE_MEMBER | 2 |  |
| ORG_ROLE_ADMIN | 3 |  |
| ORG_ROLE_OWNER | 4 |  |


 

 


<a name="koinon-auth-v1alpha1-AuthService"></a>

### AuthService
AuthService describes the caller&#39;s session. Login and logout are plain
HTTP endpoints (/auth/login, /auth/oidc/start, /auth/oidc/callback,
/auth/logout) because they set and clear the session cookie and redirect.

| Method Name | Request Type | Response Type | Description |
| ----------- | ------------ | ------------- | ------------|
| WhoAmI | [WhoAmIRequest](#koinon-auth-v1alpha1-WhoAmIRequest) | [WhoAmIResponse](#koinon-auth-v1alpha1-WhoAmIResponse) | WhoAmI returns the authenticated principal. |
| ChangePassword | [ChangePasswordRequest](#koinon-auth-v1alpha1-ChangePasswordRequest) | [ChangePasswordResponse](#koinon-auth-v1alpha1-ChangePasswordResponse) | ChangePassword replaces the bootstrap admin&#39;s password. It is the only call besides WhoAmI allowed while a password change is required. Other sessions of the user are revoked. |

 



<a name="koinon_template_v1alpha1_template-proto"></a>
<p align="right"><a href="#top">Top</a></p>

## koinon/template/v1alpha1/template.proto



<a name="koinon-template-v1alpha1-Grant"></a>

### Grant
Grant is a grant on a connector type. On a template or a draft agent it is a
proposal; activating the agent approves it.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| connector | [string](#string) |  | Connector type, for example &#34;github&#34;. |
| scopes | [string](#string) | repeated |  |
| preset | [Preset](#koinon-template-v1alpha1-Preset) |  |  |
| targets_min | [int32](#int32) |  | How many targets (for GitHub: repositories) the user picks. Both zero when the template sets no bound. |
| targets_max | [int32](#int32) |  |  |
| purpose | [string](#string) |  |  |
| connection_id | [string](#string) |  | On an agent, the connection the grant uses and the targets (for GitHub: repositories, owner/name) it is narrowed to; activating the agent approves them with the grant. Empty on a template. |
| repositories | [string](#string) | repeated |  |






<a name="koinon-template-v1alpha1-ListTemplatesRequest"></a>

### ListTemplatesRequest
ListTemplatesRequest is empty.






<a name="koinon-template-v1alpha1-ListTemplatesResponse"></a>

### ListTemplatesResponse
ListTemplatesResponse lists the templates, sorted by slug.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| templates | [Template](#koinon-template-v1alpha1-Template) | repeated |  |






<a name="koinon-template-v1alpha1-NotifyTarget"></a>

### NotifyTarget
NotifyTarget is who is asked when the agent asks a person without naming one.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| kind | [NotifyTargetKind](#koinon-template-v1alpha1-NotifyTargetKind) |  |  |
| user_id | [string](#string) |  | Kind USER: the user&#39;s ID. |
| role | [koinon.auth.v1alpha1.OrgRole](#koinon-auth-v1alpha1-OrgRole) |  | Kind ROLE: MEMBER, ADMIN or OWNER. |
| channel_id | [string](#string) |  | Kind CHANNEL (v0.2): must be empty until then. |






<a name="koinon-template-v1alpha1-RunLimits"></a>

### RunLimits
RunLimits cap a single run; the first one reached cancels it (plan item A4, ADR 0081).


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| max_duration_seconds | [int64](#int64) |  | Wall-clock time a run may be owned (starting or running), in seconds. |
| max_tokens | [int64](#int64) |  | **Deprecated.** Deprecated: tokens are reported, not enforced (ADR 0081). Kept so older clients and stored templates still decode; it caps nothing and is zero on new templates and agents. |
| max_model_requests | [int64](#int64) |  | Model requests a run may make: every request through the agent&#39;s key counts, the harness&#39;s own title, summary and compaction calls included. |
| max_spend_usd | [string](#string) |  | USD a run may spend, by the ledger&#39;s settled cost, as an exact decimal string such as &#34;2.00&#34;. |






<a name="koinon-template-v1alpha1-Template"></a>

### Template
Template is one version of an agent template.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| id | [string](#string) |  | Identifies this exact content; agents are created from it. |
| slug | [string](#string) |  | Stable template name, for example &#34;repo-helper&#34;. |
| content_hash | [string](#string) |  | Hex SHA-256 of the canonical template document. |
| version | [string](#string) |  | The template&#39;s own version label, for example &#34;0.1.0&#34;. |
| display_name | [string](#string) |  |  |
| description | [string](#string) |  |  |
| allowed_flavors | [Flavor](#koinon-template-v1alpha1-Flavor) | repeated |  |
| default_flavor | [Flavor](#koinon-template-v1alpha1-Flavor) |  |  |
| model | [string](#string) |  | Logical model name; &#34;default&#34; is the install&#39;s default model. |
| instructions | [string](#string) |  |  |
| grants | [Grant](#koinon-template-v1alpha1-Grant) | repeated |  |
| run_limits | [RunLimits](#koinon-template-v1alpha1-RunLimits) |  |  |
| spend_limit_usd | [string](#string) |  | Spend limit as a decimal string, for example &#34;10.00&#34;. |
| triggers | [Trigger](#koinon-template-v1alpha1-Trigger) | repeated |  |
| ephemeral | [bool](#bool) |  | Agents made from the template are ephemeral by default: each task gets a fresh sandbox without a home, deleted when the task ends (ADR 0065 decision 13c). |
| thinking | [string](#string) |  | The starting value of the agent&#39;s thinking setting, &#34;on&#34; or &#34;off&#34;; empty when the template names none, which is the default, on (ADR 0069 amendment 2026-10-29). |






<a name="koinon-template-v1alpha1-Trigger"></a>

### Trigger
Trigger is one way work starts on an agent, with the instruction it gets when the trigger fires.
It belongs to a version and takes effect when that version is activated.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| id | [string](#string) |  | Names the trigger within the agent, a lowercase slug; stable across versions. |
| type | [TriggerType](#koinon-template-v1alpha1-TriggerType) |  |  |
| cron | [string](#string) |  | Schedule only: a five-field cron expression (minute hour day-of-month month day-of-week). |
| timezone | [string](#string) |  | Schedule only: an IANA zone name, for example &#34;Europe/Tallinn&#34;. |
| instruction | [string](#string) |  | Text given to the agent when the trigger fires. |
| notify | [NotifyTarget](#koinon-template-v1alpha1-NotifyTarget) |  | Who is asked when the agent asks a person without naming one. Every type but direct_message (which asks the person in the chat). Unset: the org&#39;s default, today its admins and owners. |





 


<a name="koinon-template-v1alpha1-Flavor"></a>

### Flavor
Flavor is the harness an agent runs on (ADR 0035).

| Name | Number | Description |
| ---- | ------ | ----------- |
| FLAVOR_UNSPECIFIED | 0 |  |
| FLAVOR_OPENCODE | 1 |  |
| FLAVOR_HERMES | 2 | Hermes and pi were dropped before v0.1 shipped (ADR 0059). The values stay so the API stays wire- and source-compatible; no flavor matches them, so the API rejects them. |
| FLAVOR_PI | 3 |  |



<a name="koinon-template-v1alpha1-NotifyTargetKind"></a>

### NotifyTargetKind
NotifyTargetKind says who a trigger&#39;s task asks (ADR 0066 amendment 2026-10-28).

| Name | Number | Description |
| ---- | ------ | ----------- |
| NOTIFY_TARGET_KIND_UNSPECIFIED | 0 |  |
| NOTIFY_TARGET_KIND_USER | 1 | One member of the org. |
| NOTIFY_TARGET_KIND_ROLE | 2 | Everyone with the role or higher (member, admin or owner). |
| NOTIFY_TARGET_KIND_CHANNEL | 3 | A chat channel. Reserved for v0.2; refused until chat connectors exist. |



<a name="koinon-template-v1alpha1-Preset"></a>

### Preset
Preset maps a grant&#39;s risk tiers to allow, ask or deny (ADR 0008, 0049).

| Name | Number | Description |
| ---- | ------ | ----------- |
| PRESET_UNSPECIFIED | 0 |  |
| PRESET_READ_ONLY | 1 |  |
| PRESET_STANDARD | 2 |  |
| PRESET_TRUSTED | 3 |  |



<a name="koinon-template-v1alpha1-TriggerType"></a>

### TriggerType
TriggerType says what starts work on an agent (ADR 0066).

| Name | Number | Description |
| ---- | ------ | ----------- |
| TRIGGER_TYPE_UNSPECIFIED | 0 |  |
| TRIGGER_TYPE_SCHEDULE | 1 |  |
| TRIGGER_TYPE_DIRECT_MESSAGE | 2 |  |
| TRIGGER_TYPE_CODE_HOST_EVENT | 3 | Reserved for v0.2; rejected until then. |
| TRIGGER_TYPE_CHANNEL_EVENT | 4 | Reserved for v0.2; rejected until then. |


 

 


<a name="koinon-template-v1alpha1-TemplateService"></a>

### TemplateService
TemplateService lists the agent templates shipped with Koinon (ADR 0020).
Templates are platform data: they belong to no org, and every signed-in
user can list them.

| Method Name | Request Type | Response Type | Description |
| ----------- | ------------ | ------------- | ------------|
| ListTemplates | [ListTemplatesRequest](#koinon-template-v1alpha1-ListTemplatesRequest) | [ListTemplatesResponse](#koinon-template-v1alpha1-ListTemplatesResponse) | ListTemplates returns the current content of every template. |

 



<a name="koinon_agent_v1alpha1_agent-proto"></a>
<p align="right"><a href="#top">Top</a></p>

## koinon/agent/v1alpha1/agent.proto



<a name="koinon-agent-v1alpha1-ActivateAgentRequest"></a>

### ActivateAgentRequest
ActivateAgentRequest names the draft and the version the caller reviewed.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| agent_id | [string](#string) |  |  |
| version | [int32](#int32) |  | Must equal the agent&#39;s latest_version, so the caller approves exactly the grants they saw; otherwise the call fails with FailedPrecondition. |
| apply_to_open_tickets | [bool](#bool) |  | Start fresh sessions, with a handoff, on the agent&#39;s open tickets: their next run begins a new session from the agent&#39;s last notes and replies instead of loading the old one, so the new version&#39;s instructions apply (ADR 0051 amendment). Default off; audited. |






<a name="koinon-agent-v1alpha1-ActivateAgentResponse"></a>

### ActivateAgentResponse
ActivateAgentResponse holds the active agent.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| agent | [Agent](#koinon-agent-v1alpha1-Agent) |  |  |
| tickets_restarting | [int32](#int32) |  | How many open tickets were marked to start a fresh session. |






<a name="koinon-agent-v1alpha1-Agent"></a>

### Agent
Agent is an agent and the definition it runs.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| id | [string](#string) |  |  |
| org_id | [string](#string) |  |  |
| display_name | [string](#string) |  |  |
| status | [AgentStatus](#koinon-agent-v1alpha1-AgentStatus) |  |  |
| flavor | [koinon.template.v1alpha1.Flavor](#koinon-template-v1alpha1-Flavor) |  |  |
| template_id | [string](#string) |  | The template content the agent was created from, if any. |
| template_slug | [string](#string) |  |  |
| latest_version | [int32](#int32) |  | The newest version. ActivateAgent must name it. |
| active_version | [int32](#int32) |  | The approved version runs execute; 0 before the first activation. |
| proposed_grants | [koinon.template.v1alpha1.Grant](#koinon-template-v1alpha1-Grant) | repeated | Grants the latest version proposes. |
| approved_grants | [koinon.template.v1alpha1.Grant](#koinon-template-v1alpha1-Grant) | repeated | Grants approved with the active version. |
| instructions | [string](#string) |  |  |
| model | [string](#string) |  |  |
| run_limits | [koinon.template.v1alpha1.RunLimits](#koinon-template-v1alpha1-RunLimits) |  |  |
| created_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| activated_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| archived_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| unhealthy_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  | Set while the agent is unhealthy: its sandbox got stuck three times in 24 hours and was not recreated again (ADR 0054). New runs are refused until ResumeAgent. |
| unhealthy_reason | [string](#string) |  |  |
| proposed_triggers | [koinon.template.v1alpha1.Trigger](#koinon-template-v1alpha1-Trigger) | repeated | Triggers the latest version proposes; they have no effect until it is activated. |
| approved_triggers | [koinon.template.v1alpha1.Trigger](#koinon-template-v1alpha1-Trigger) | repeated | Triggers of the active version: the ones in effect. |
| ephemeral | [bool](#bool) |  | The latest version is ephemeral: each task gets a fresh sandbox without a home, deleted when the task ends (ADR 0065 decision 13c). |
| sandbox_archived_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  | Set while the agent&#39;s sandbox and volume are archived (ADR 0032 amendment): the home is in its last checkpoint and the next run restores it into a fresh sandbox. |
| model_setting | [string](#string) |  | The agent&#39;s own model setting (SetAgentModel); empty when it follows the version and the org default. When set it wins over `model`, which is the latest version&#39;s. |






<a name="koinon-agent-v1alpha1-ArchiveAgentRequest"></a>

### ArchiveAgentRequest
ArchiveAgentRequest names the agent.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| agent_id | [string](#string) |  |  |






<a name="koinon-agent-v1alpha1-ArchiveAgentResponse"></a>

### ArchiveAgentResponse
ArchiveAgentResponse holds the archived agent.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| agent | [Agent](#koinon-agent-v1alpha1-Agent) |  |  |






<a name="koinon-agent-v1alpha1-CreateAgentFromTemplateRequest"></a>

### CreateAgentFromTemplateRequest
CreateAgentFromTemplateRequest names the template and the user&#39;s choices.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| template_id | [string](#string) |  | A Template.id from ListTemplates. |
| display_name | [string](#string) |  |  |
| flavor | [koinon.template.v1alpha1.Flavor](#koinon-template-v1alpha1-Flavor) |  | One of the template&#39;s allowed flavors; unspecified picks its default. |
| grants | [GrantBinding](#koinon-agent-v1alpha1-GrantBinding) | repeated | Where the template&#39;s grants apply: one entry per grant on a connector type with targets. A grant whose template sets targets min above zero needs one. Stored with the agent&#39;s version; ActivateAgent approves it with the grant. |
| ephemeral | [bool](#bool) | optional | Overrides the template&#39;s ephemeral setting; unset keeps it. |






<a name="koinon-agent-v1alpha1-CreateAgentFromTemplateResponse"></a>

### CreateAgentFromTemplateResponse
CreateAgentFromTemplateResponse holds the new draft.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| agent | [Agent](#koinon-agent-v1alpha1-Agent) |  |  |






<a name="koinon-agent-v1alpha1-GetAgentModelRequest"></a>

### GetAgentModelRequest
GetAgentModelRequest names an agent.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| agent_id | [string](#string) |  |  |






<a name="koinon-agent-v1alpha1-GetAgentModelResponse"></a>

### GetAgentModelResponse
GetAgentModelResponse is the agent&#39;s model setting and what it resolves to.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| setting | [string](#string) |  | The agent&#39;s setting; empty for default. |
| version_model | [string](#string) |  | The active version&#39;s model; empty when the version names none (&#34;default&#34; in a template). |
| effective | [string](#string) |  | The model the next run uses; empty with MODEL_SOURCE_NONE. |
| source | [ModelSource](#koinon-agent-v1alpha1-ModelSource) |  |  |
| problem | [string](#string) |  | Set when the agent&#39;s setting is no longer served or priced: runs fail until it is changed. |
| available | [ServedModel](#koinon-agent-v1alpha1-ServedModel) | repeated | Models the org serves and has a price for, by provider and name: the picker&#39;s choices. |
| thinking | [ThinkingState](#koinon-agent-v1alpha1-ThinkingState) |  | The agent&#39;s thinking setting for the effective model. |






<a name="koinon-agent-v1alpha1-GetAgentRequest"></a>

### GetAgentRequest
GetAgentRequest names an agent.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| agent_id | [string](#string) |  |  |






<a name="koinon-agent-v1alpha1-GetAgentResponse"></a>

### GetAgentResponse
GetAgentResponse holds the agent.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| agent | [Agent](#koinon-agent-v1alpha1-Agent) |  |  |






<a name="koinon-agent-v1alpha1-GrantBinding"></a>

### GrantBinding
GrantBinding binds a template grant to one of the org&#39;s connections and
the targets it is narrowed to.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| connector | [string](#string) |  | The grant&#39;s connector type, for example &#34;github&#34;. |
| connection_id | [string](#string) |  | A connection of that type in the org (ConnectionService). |
| repositories | [string](#string) | repeated | For GitHub: repositories as owner/name. Their number must be within the template grant&#39;s targets min and max, and the connection&#39;s installation must be able to access each of them. |






<a name="koinon-agent-v1alpha1-ListAgentsRequest"></a>

### ListAgentsRequest
ListAgentsRequest names the org.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |






<a name="koinon-agent-v1alpha1-ListAgentsResponse"></a>

### ListAgentsResponse
ListAgentsResponse lists the org&#39;s agents.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| agents | [Agent](#koinon-agent-v1alpha1-Agent) | repeated |  |






<a name="koinon-agent-v1alpha1-ResumeAgentRequest"></a>

### ResumeAgentRequest
ResumeAgentRequest names the agent.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| agent_id | [string](#string) |  |  |






<a name="koinon-agent-v1alpha1-ResumeAgentResponse"></a>

### ResumeAgentResponse
ResumeAgentResponse holds the agent, healthy again.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| agent | [Agent](#koinon-agent-v1alpha1-Agent) |  |  |






<a name="koinon-agent-v1alpha1-RunAgentTriggerRequest"></a>

### RunAgentTriggerRequest
RunAgentTriggerRequest names the trigger to fire.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| agent_id | [string](#string) |  |  |
| trigger_id | [string](#string) |  | The trigger&#39;s ID in the agent&#39;s active version. It must be a schedule trigger. |






<a name="koinon-agent-v1alpha1-RunAgentTriggerResponse"></a>

### RunAgentTriggerResponse
RunAgentTriggerResponse names the task the fire created.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| task_id | [string](#string) |  |  |






<a name="koinon-agent-v1alpha1-ServedModel"></a>

### ServedModel
ServedModel is a model the org serves and has a price for.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| name | [string](#string) |  |  |
| provider | [string](#string) |  |  |






<a name="koinon-agent-v1alpha1-SetAgentModelRequest"></a>

### SetAgentModelRequest
SetAgentModelRequest sets the agent&#39;s model.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| agent_id | [string](#string) |  |  |
| model | [string](#string) |  | &#34;default&#34; (or empty) clears the setting; otherwise a model the org serves and has a price for. |






<a name="koinon-agent-v1alpha1-SetAgentModelResponse"></a>

### SetAgentModelResponse
SetAgentModelResponse holds the result, as GetAgentModel would return it.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| model | [GetAgentModelResponse](#koinon-agent-v1alpha1-GetAgentModelResponse) |  |  |






<a name="koinon-agent-v1alpha1-SetAgentThinkingRequest"></a>

### SetAgentThinkingRequest
SetAgentThinkingRequest sets the agent&#39;s thinking setting.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| agent_id | [string](#string) |  |  |
| thinking | [string](#string) |  | &#34;on&#34; or &#34;off&#34;; empty clears the setting. |






<a name="koinon-agent-v1alpha1-SetAgentThinkingResponse"></a>

### SetAgentThinkingResponse
SetAgentThinkingResponse holds the result, as GetAgentModel would return it.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| model | [GetAgentModelResponse](#koinon-agent-v1alpha1-GetAgentModelResponse) |  |  |






<a name="koinon-agent-v1alpha1-SetAgentTriggersRequest"></a>

### SetAgentTriggersRequest
SetAgentTriggersRequest replaces the triggers of the agent&#39;s latest version in a new version.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| agent_id | [string](#string) |  |  |
| base_version | [int32](#int32) |  | Must equal the agent&#39;s latest_version, so a concurrent edit is not overwritten; otherwise the call fails with FailedPrecondition. |
| triggers | [koinon.template.v1alpha1.Trigger](#koinon-template-v1alpha1-Trigger) | repeated | The complete list; empty removes every trigger. |






<a name="koinon-agent-v1alpha1-SetAgentTriggersResponse"></a>

### SetAgentTriggersResponse
SetAgentTriggersResponse holds the agent with its new proposed version.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| agent | [Agent](#koinon-agent-v1alpha1-Agent) |  |  |






<a name="koinon-agent-v1alpha1-ThinkingState"></a>

### ThinkingState
ThinkingState is the agent&#39;s thinking setting and what it resolves to (ADR 0069 amendment
2026-10-29).


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| setting | [string](#string) |  | The agent&#39;s own setting: &#34;on&#34;, &#34;off&#34;, or empty when it follows the version and the default. |
| version_setting | [string](#string) |  | The active version&#39;s value (a template&#39;s spec.model.thinking); empty when it names none. |
| effective | [string](#string) |  | What the layers resolve to, &#34;on&#34; or &#34;off&#34;, whether or not the model can think. |
| supported | [bool](#bool) |  | Whether the effective model thinks. When false the setting has no effect and the UI says &#34;not supported by this model&#34;. |
| mode | [string](#string) |  | The form the next run sends (internal/thinking.Mode): &#34;none&#34; when the model does not think, &#34;adaptive-summarized&#34; or &#34;default&#34; for thinking on, &#34;disabled&#34; or &#34;effort-none&#34; for thinking off. |
| always_thinks | [bool](#bool) |  | Whether the effective model thinks whatever the setting. A UI then offers no off choice and says &#34;this model always thinks&#34;; setting &#34;off&#34; is refused with that message. |
| off_overridden | [bool](#bool) |  | Whether the setting resolves to off and the model thinks anyway (it always thinks). The setting is kept for a model that can stop; runs think. |
| notice | [string](#string) |  | What a UI shows about the setting: &#34;this model always thinks; your off setting doesn&#39;t apply&#34; when off_overridden, empty otherwise. |





 


<a name="koinon-agent-v1alpha1-AgentStatus"></a>

### AgentStatus
AgentStatus is the agent lifecycle (ADR 0020).

| Name | Number | Description |
| ---- | ------ | ----------- |
| AGENT_STATUS_UNSPECIFIED | 0 |  |
| AGENT_STATUS_DRAFT | 1 |  |
| AGENT_STATUS_ACTIVE | 2 |  |
| AGENT_STATUS_ARCHIVED | 3 |  |



<a name="koinon-agent-v1alpha1-ModelSource"></a>

### ModelSource
ModelSource says where an agent&#39;s effective model comes from, in order of precedence.

| Name | Number | Description |
| ---- | ------ | ----------- |
| MODEL_SOURCE_UNSPECIFIED | 0 |  |
| MODEL_SOURCE_AGENT | 1 | The agent&#39;s own setting (SetAgentModel). |
| MODEL_SOURCE_VERSION | 2 | The model of the agent&#39;s active version (a template&#39;s spec.model). |
| MODEL_SOURCE_ORG_DEFAULT | 3 | The default model: the org&#39;s stored default model (ADR 0086), or the platform&#39;s --default-model when the org has none. |
| MODEL_SOURCE_NONE | 4 | Nothing: the agent, its version and the platform name no model and the org has no default model, so a run would fail until an admin sets one. |


 

 


<a name="koinon-agent-v1alpha1-AgentService"></a>

### AgentService
AgentService manages an org&#39;s agents (ADR 0020). Every request names the
org it acts in; IDs from another org are not found.

| Method Name | Request Type | Response Type | Description |
| ----------- | ------------ | ------------- | ------------|
| CreateAgentFromTemplate | [CreateAgentFromTemplateRequest](#koinon-agent-v1alpha1-CreateAgentFromTemplateRequest) | [CreateAgentFromTemplateResponse](#koinon-agent-v1alpha1-CreateAgentFromTemplateResponse) | CreateAgentFromTemplate creates a draft agent from a template. Members and above. |
| GetAgent | [GetAgentRequest](#koinon-agent-v1alpha1-GetAgentRequest) | [GetAgentResponse](#koinon-agent-v1alpha1-GetAgentResponse) | GetAgent returns one agent. Viewers and above. |
| ListAgents | [ListAgentsRequest](#koinon-agent-v1alpha1-ListAgentsRequest) | [ListAgentsResponse](#koinon-agent-v1alpha1-ListAgentsResponse) | ListAgents returns the org&#39;s agents, oldest first. Viewers and above. |
| ActivateAgent | [ActivateAgentRequest](#koinon-agent-v1alpha1-ActivateAgentRequest) | [ActivateAgentResponse](#koinon-agent-v1alpha1-ActivateAgentResponse) | ActivateAgent activates a draft, or moves an active agent to its newer proposed version (ADR 0066), and so approves the grants that version proposes. Admins and owners only. |
| SetAgentTriggers | [SetAgentTriggersRequest](#koinon-agent-v1alpha1-SetAgentTriggersRequest) | [SetAgentTriggersResponse](#koinon-agent-v1alpha1-SetAgentTriggersResponse) | SetAgentTriggers proposes a new version with the given triggers (everything else carried over from the latest version). The triggers fire nothing until ActivateAgent activates that version; an active agent keeps running its active version meanwhile. Audited. Admins and owners only. |
| ResumeAgent | [ResumeAgentRequest](#koinon-agent-v1alpha1-ResumeAgentRequest) | [ResumeAgentResponse](#koinon-agent-v1alpha1-ResumeAgentResponse) | ResumeAgent clears the unhealthy state of an agent whose sandbox got stuck again and again (ADR 0054), so it takes runs again; its stuck sandbox is recreated. Audited. Admins and owners only. |
| GetAgentModel | [GetAgentModelRequest](#koinon-agent-v1alpha1-GetAgentModelRequest) | [GetAgentModelResponse](#koinon-agent-v1alpha1-GetAgentModelResponse) | GetAgentModel says which model the agent&#39;s next run uses and where that choice comes from, and lists the models an admin may pick. Admins and owners only. |
| SetAgentModel | [SetAgentModelRequest](#koinon-agent-v1alpha1-SetAgentModelRequest) | [SetAgentModelResponse](#koinon-agent-v1alpha1-SetAgentModelResponse) | SetAgentModel sets the agent&#39;s model, or clears it with &#34;default&#34;. It is a setting of the agent, not a new version: it applies to the agent&#39;s next run. The model must be one the org serves and has a price for. Audited, with the old and the new value. Admins and owners only. |
| SetAgentThinking | [SetAgentThinkingRequest](#koinon-agent-v1alpha1-SetAgentThinkingRequest) | [SetAgentThinkingResponse](#koinon-agent-v1alpha1-SetAgentThinkingResponse) | SetAgentThinking sets whether the agent&#39;s model thinks: &#34;on&#34; or &#34;off&#34;, or clears the setting with an empty value so the version&#39;s value (a template&#39;s spec.model.thinking) and then the default, on, apply. Thinking is on unless somebody turns it off (ADR 0069 amendment 2026-10-29). Like the model it is a setting of the agent, not a new version, and applies to the next run. A model that cannot think ignores it (GetAgentModel says so). Audited, with the old and the new value. Admins and owners only. |
| RunAgentTrigger | [RunAgentTriggerRequest](#koinon-agent-v1alpha1-RunAgentTriggerRequest) | [RunAgentTriggerResponse](#koinon-agent-v1alpha1-RunAgentTriggerResponse) | RunAgentTrigger fires one schedule trigger of the active version now, as the scheduler would at a due time, with the trigger&#39;s instruction. The task is marked as a manual fire. Audited. Admins and owners only. |
| ArchiveAgent | [ArchiveAgentRequest](#koinon-agent-v1alpha1-ArchiveAgentRequest) | [ArchiveAgentResponse](#koinon-agent-v1alpha1-ArchiveAgentResponse) | ArchiveAgent archives a draft or active agent and cancels its unfinished runs. Admins and owners only. |

 



<a name="koinon_approval_v1alpha1_approval-proto"></a>
<p align="right"><a href="#top">Top</a></p>

## koinon/approval/v1alpha1/approval.proto



<a name="koinon-approval-v1alpha1-Approval"></a>

### Approval
Approval is one recorded ask-tier call.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| id | [string](#string) |  |  |
| org_id | [string](#string) |  |  |
| ticket_id | [string](#string) |  |  |
| run_id | [string](#string) |  |  |
| agent_id | [string](#string) |  |  |
| connector | [string](#string) |  | Connector type, e.g. &#34;github&#34;. |
| tool | [string](#string) |  |  |
| target | [string](#string) |  | What the call acts on, e.g. &#34;github:acme/widgets&#34;. |
| args_json | [string](#string) |  | The canonical JSON of the arguments, exactly as hashed. |
| args_sha256 | [string](#string) |  |  |
| decision | [Decision](#koinon-approval-v1alpha1-Decision) |  |  |
| status | [Status](#koinon-approval-v1alpha1-Status) |  |  |
| risk | [Risk](#koinon-approval-v1alpha1-Risk) |  |  |
| pull_request | [PullRequestCall](#koinon-approval-v1alpha1-PullRequestCall) |  |  |
| comment | [CommentCall](#koinon-approval-v1alpha1-CommentCall) |  |  |
| mcp | [McpCall](#koinon-approval-v1alpha1-McpCall) |  | mcp is set for a call to a remote MCP server (connector &#34;mcp&#34;). |
| saved_work | [SavedWorkCall](#koinon-approval-v1alpha1-SavedWorkCall) |  | saved_work is set for &#34;Push saved work to a branch&#34;, a request Koinon makes itself after a stuck sandbox (ADR 0054). |
| decided_by | [string](#string) |  |  |
| decided_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| decision_note | [string](#string) |  |  |
| result | [google.protobuf.Struct](#google-protobuf-Struct) |  | The executed call&#39;s outcome, e.g. {&#34;url&#34;: &#34;.../pull/1&#34;, &#34;number&#34;: 1}. |
| error | [string](#string) |  | Why execution failed. |
| executions | [int64](#int64) |  |  |
| expires_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| created_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| executed_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| can_decide | [bool](#bool) |  | Whether the caller may approve or deny it (admins and owners, while it is pending). |
| approvals_required | [int32](#int32) |  | approvals_required is 2 when the tool needs two approvers (a distinct second person, neither of them the ticket&#39;s creator), else 1. |
| first_approver | [string](#string) |  | first_approver is the user ID of the first of two approvals, once given; the request stays pending. |
| system | [bool](#bool) |  | system is true for a request Koinon made itself rather than an agent&#39;s tool call: the run neither waits for it nor is told its outcome (saved work from a stuck sandbox). |
| ci_paths | [string](#string) | repeated | ci_paths are the paths whose changes run CI on the connection&#39;s host: the connector&#39;s CI paths plus the connection&#39;s configured directories. A directory ends with a slash. Only set by GetApproval, for a request on a code host. |
| ci_fold_case | [bool](#bool) |  | ci_fold_case is whether the host&#39;s file system ignores case, so ci_paths match without regard to case. |
| scope_id | [string](#string) |  | scope_id is the scope that let this call run without a decision; empty when a person (or the Trusted preset) decided it. scope_kind says which kind it was. |
| scope_kind | [ScopeKind](#koinon-approval-v1alpha1-ScopeKind) |  |  |






<a name="koinon-approval-v1alpha1-ApprovalScope"></a>

### ApprovalScope
ApprovalScope is an approval that reaches beyond one call. Matching is exact on agent,
connector, tool and target; arguments are not part of it.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| id | [string](#string) |  |  |
| org_id | [string](#string) |  |  |
| agent_id | [string](#string) |  |  |
| connector | [string](#string) |  |  |
| tool | [string](#string) |  |  |
| target | [string](#string) |  |  |
| kind | [ScopeKind](#koinon-approval-v1alpha1-ScopeKind) |  |  |
| task_id | [string](#string) |  | Set for a task scope. |
| expires_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| created_by | [string](#string) |  |  |
| created_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| source_approval_id | [string](#string) |  |  |
| revoked_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| revoked_by | [string](#string) |  |  |






<a name="koinon-approval-v1alpha1-ApproveRequest"></a>

### ApproveRequest
ApproveRequest names a request and the hash the approver saw.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| approval_id | [string](#string) |  |  |
| args_sha256 | [string](#string) |  |  |
| note | [string](#string) |  | Optional; shown on the ticket. |
| scope | [ScopeKind](#koinon-approval-v1alpha1-ScopeKind) |  | scope widens the approval beyond this call: SCOPE_KIND_TASK or SCOPE_KIND_AGENT (unspecified and once are this call only). Needs an admin or owner (the same role as approving), and fails with INVALID_ARGUMENT for a request that needs two approvers or that Koinon made itself. Applies when the approval takes effect (the second approval of two is never scoped). |
| scope_until | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  | scope_until is when a wider scope ends. Required for SCOPE_KIND_AGENT (at most one year ahead); a task scope defaults to 30 days and may not be longer than that. |






<a name="koinon-approval-v1alpha1-ApproveResponse"></a>

### ApproveResponse
ApproveResponse holds the request after execution: executed with its
result, failed with an error, or still approved if execution is retried.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| approval | [Approval](#koinon-approval-v1alpha1-Approval) |  |  |






<a name="koinon-approval-v1alpha1-CommentCall"></a>

### CommentCall
CommentCall is comment_on_issue&#39;s arguments.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| issue_number | [int64](#int64) |  |  |
| body | [string](#string) |  |  |






<a name="koinon-approval-v1alpha1-DenyRequest"></a>

### DenyRequest
DenyRequest names a request.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| approval_id | [string](#string) |  |  |
| note | [string](#string) |  | Optional; sent to the agent and shown on the ticket. |






<a name="koinon-approval-v1alpha1-DenyResponse"></a>

### DenyResponse
DenyResponse holds the denied request.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| approval | [Approval](#koinon-approval-v1alpha1-Approval) |  |  |






<a name="koinon-approval-v1alpha1-FileChange"></a>

### FileChange
FileChange is one file of a proposed pull request, with its full new
content: the diff data the card shows collapsed.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| path | [string](#string) |  |  |
| content | [string](#string) |  |  |
| delete | [bool](#bool) |  |  |
| executable | [bool](#bool) |  | The file is written as executable (git mode 100755). Otherwise a new file is regular (100644) and a modified file keeps its mode on the base branch. Symbolic links and submodules are never written. |






<a name="koinon-approval-v1alpha1-FileDiff"></a>

### FileDiff
FileDiff compares one file with its current version on the base branch.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| path | [string](#string) |  |  |
| status | [FileStatus](#koinon-approval-v1alpha1-FileStatus) |  |  |
| unified_diff | [string](#string) |  | A unified diff with three lines of context: &#34;--- a/&lt;path&gt;&#34; and &#34;&#43;&#43;&#43; b/&lt;path&gt;&#34; (/dev/null for an added or deleted file), then &#34;@@&#34; hunks of &#34; &#34;, &#34;-&#34; and &#34;&#43;&#34; lines; &#34;\ No newline at end of file&#34; follows a last line without one. Empty when the file is unchanged, binary, or fallback_reason is set. |
| binary | [bool](#bool) |  | The new or the current version is not text; no content is shown. |
| fallback_reason | [string](#string) |  | Why there is no diff, e.g. &#34;the current version is larger than 1 MiB&#34;. The card then shows the full new content. |
| old_mode | [string](#string) |  | The file&#39;s git mode on the base branch (&#34;100644&#34;, &#34;100755&#34;; empty for a new file) and the mode Koinon writes it with (empty for a deleted file). Both are empty when the base tree could not be read. A modified file whose modes differ becomes executable. |
| new_mode | [string](#string) |  |  |






<a name="koinon-approval-v1alpha1-GetApprovalDiffRequest"></a>

### GetApprovalDiffRequest
GetApprovalDiffRequest names a request.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| approval_id | [string](#string) |  |  |






<a name="koinon-approval-v1alpha1-GetApprovalDiffResponse"></a>

### GetApprovalDiffResponse
GetApprovalDiffResponse holds one FileDiff per file of the call, in the
call&#39;s order.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| base | [string](#string) |  | The branch compared against: the call&#39;s base, or the repository&#39;s default branch. Empty if it could not be resolved. |
| files | [FileDiff](#koinon-approval-v1alpha1-FileDiff) | repeated |  |






<a name="koinon-approval-v1alpha1-GetApprovalRequest"></a>

### GetApprovalRequest
GetApprovalRequest names a request.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| approval_id | [string](#string) |  |  |






<a name="koinon-approval-v1alpha1-GetApprovalResponse"></a>

### GetApprovalResponse
GetApprovalResponse holds the request with its decoded call.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| approval | [Approval](#koinon-approval-v1alpha1-Approval) |  |  |






<a name="koinon-approval-v1alpha1-LeakGuard"></a>

### LeakGuard
LeakGuard is what the MCP leak guard recorded on a card (ADR 0091): the reasons it stopped the
call, the tool&#39;s sink class, the call&#39;s address arguments, and the two legs of the session.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| reasons | [string](#string) | repeated | reasons are the guard&#39;s reason codes, e.g. &#34;both_legs&#34; and &#34;taint&#34;. |
| sink_class | [string](#string) |  |  |
| destination | [LeakGuard.DestinationEntry](#koinon-approval-v1alpha1-LeakGuard-DestinationEntry) | repeated | destination holds the call&#39;s address arguments by name. |
| untrusted_input | [bool](#bool) |  | untrusted_input: the session read input an attacker could have written. |
| private_data | [bool](#bool) |  | private_data: the session read data the org keeps private. |






<a name="koinon-approval-v1alpha1-LeakGuard-DestinationEntry"></a>

### LeakGuard.DestinationEntry



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| key | [string](#string) |  |  |
| value | [string](#string) |  |  |






<a name="koinon-approval-v1alpha1-ListApprovalScopesRequest"></a>

### ListApprovalScopesRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| agent_id | [string](#string) |  |  |






<a name="koinon-approval-v1alpha1-ListApprovalScopesResponse"></a>

### ListApprovalScopesResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| scopes | [ApprovalScope](#koinon-approval-v1alpha1-ApprovalScope) | repeated |  |






<a name="koinon-approval-v1alpha1-ListApprovalsRequest"></a>

### ListApprovalsRequest
ListApprovalsRequest names the org and, optionally, a ticket.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| ticket_id | [string](#string) |  | With a ticket, every request of the ticket in any status; without, the org&#39;s pending requests. |






<a name="koinon-approval-v1alpha1-ListApprovalsResponse"></a>

### ListApprovalsResponse
ListApprovalsResponse holds the requests, without their decoded calls.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| approvals | [Approval](#koinon-approval-v1alpha1-Approval) | repeated |  |






<a name="koinon-approval-v1alpha1-McpCall"></a>

### McpCall
McpCall describes a call to a remote MCP server (ADR 0057). The call is bound to the server, its URL
and the version of the tool&#39;s description and schema at the time of the request.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| server_id | [string](#string) |  |  |
| server_name | [string](#string) |  |  |
| content_hash | [string](#string) |  | content_hash is the SHA-256 of the tool&#39;s description and schema the request was made against. |
| current_content_hash | [string](#string) |  | current_content_hash is the tool&#39;s version now; when it differs the call will not run. |
| review_reason | [string](#string) |  | review_reason is set while the tool is flagged for review (its description or schema changed). |
| tool_description | [string](#string) |  | tool_description is the tool&#39;s current description, so the approver reads what the agent read. |
| leak_guard | [LeakGuard](#koinon-approval-v1alpha1-LeakGuard) |  | leak_guard is set when the leak guard (ADR 0091) put this call on a card: why, and which legs the session held. |






<a name="koinon-approval-v1alpha1-PullRequestCall"></a>

### PullRequestCall
PullRequestCall is create_pull_request&#39;s arguments.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| branch | [string](#string) |  | The new branch Koinon creates (always under koinon/). |
| base | [string](#string) |  | The branch to merge into; empty means the repository&#39;s default branch. |
| title | [string](#string) |  |  |
| body | [string](#string) |  |  |
| files | [FileChange](#koinon-approval-v1alpha1-FileChange) | repeated |  |






<a name="koinon-approval-v1alpha1-RevokeApprovalScopeRequest"></a>

### RevokeApprovalScopeRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| scope_id | [string](#string) |  |  |






<a name="koinon-approval-v1alpha1-RevokeApprovalScopeResponse"></a>

### RevokeApprovalScopeResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| scope | [ApprovalScope](#koinon-approval-v1alpha1-ApprovalScope) |  |  |






<a name="koinon-approval-v1alpha1-Risk"></a>

### Risk
Risk is the push-and-run summary the approval card leads with (plan item
W2). Lists hold repository paths.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| files | [int64](#int64) |  |  |
| bytes | [int64](#int64) |  |  |
| deletions | [string](#string) | repeated |  |
| ci | [string](#string) | repeated | CI and automation configuration (workflow files are refused outright). |
| scripts | [string](#string) | repeated | Files tooling or people execute: scripts, Makefiles, hooks. |
| dependencies | [string](#string) | repeated | Dependency manifests and lock files. |
| binary | [string](#string) | repeated |  |
| executable | [string](#string) | repeated | Files the call marks executable (git mode 100755); a marked file that is already executable on the base branch is listed too. Unmarked files keep their mode. |






<a name="koinon-approval-v1alpha1-SavedFile"></a>

### SavedFile
SavedFile is one file of saved work.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| path | [string](#string) |  |  |
| status | [string](#string) |  | status is A (added), M (modified) or D (deleted). |
| added | [int32](#int32) |  |  |
| deleted | [int32](#int32) |  |  |
| size | [int64](#int64) |  |  |
| binary | [bool](#bool) |  |  |






<a name="koinon-approval-v1alpha1-SavedWorkCall"></a>

### SavedWorkCall
SavedWorkCall describes uncommitted work saved from a stuck sandbox, as a diffstat. The files
stay sealed in Koinon until the request is approved and pushed.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| repository | [string](#string) |  | repository is owner/name; base the commit the changes apply to; branch where they are pushed (never the default branch). |
| base | [string](#string) |  |  |
| branch | [string](#string) |  |  |
| files | [SavedFile](#koinon-approval-v1alpha1-SavedFile) | repeated |  |
| size_bytes | [int64](#int64) |  |  |
| skipped | [string](#string) | repeated | Notes on changes that were not saved (symbolic links, submodules). |
| status | [string](#string) |  | status is saved, pushed or expired; the work is kept 14 days. |
| expires_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |





 


<a name="koinon-approval-v1alpha1-Decision"></a>

### Decision
Decision is what the grant&#39;s preset decided for the call.

| Name | Number | Description |
| ---- | ------ | ----------- |
| DECISION_UNSPECIFIED | 0 |  |
| DECISION_ASK | 1 | A human decides (the Standard preset). |
| DECISION_ALLOW | 2 | Executed without a card (the Trusted preset). |



<a name="koinon-approval-v1alpha1-FileStatus"></a>

### FileStatus
FileStatus is what a proposed pull request does to one file.

| Name | Number | Description |
| ---- | ------ | ----------- |
| FILE_STATUS_UNSPECIFIED | 0 | The current version could not be read (see fallback_reason). |
| FILE_STATUS_ADDED | 1 |  |
| FILE_STATUS_MODIFIED | 2 |  |
| FILE_STATUS_DELETED | 3 |  |



<a name="koinon-approval-v1alpha1-ScopeKind"></a>

### ScopeKind
ScopeKind is how far an approval reaches (ADR 0042 amendment, ADR 0065 decision 10a).

| Name | Number | Description |
| ---- | ------ | ----------- |
| SCOPE_KIND_UNSPECIFIED | 0 | SCOPE_KIND_UNSPECIFIED means once: this exact call, as before. |
| SCOPE_KIND_ONCE | 1 | SCOPE_KIND_ONCE is this exact call only. |
| SCOPE_KIND_TASK | 2 | SCOPE_KIND_TASK: later calls of this agent to the same connector, tool and target while working on the same task, until the scope expires. |
| SCOPE_KIND_AGENT | 3 | SCOPE_KIND_AGENT: later calls of this agent to the same connector, tool and target, in any task, until a date. |



<a name="koinon-approval-v1alpha1-Status"></a>

### Status
Status is the ADR 0042 lifecycle: pending -&gt; approved -&gt; executed | failed,
or pending -&gt; denied | expired | canceled.

| Name | Number | Description |
| ---- | ------ | ----------- |
| STATUS_UNSPECIFIED | 0 |  |
| STATUS_PENDING | 1 |  |
| STATUS_APPROVED | 2 |  |
| STATUS_DENIED | 3 |  |
| STATUS_EXECUTED | 4 |  |
| STATUS_FAILED | 5 |  |
| STATUS_EXPIRED | 6 |  |
| STATUS_CANCELED | 7 |  |


 

 


<a name="koinon-approval-v1alpha1-ApprovalService"></a>

### ApprovalService
ApprovalService shows and decides ask-tier tool calls (ADRs 0008, 0042,
0049; plan item P1). An agent&#39;s call is recorded exactly; approving it binds
to the SHA-256 of its canonical arguments, and Koinon then executes exactly
that call (for create_pull_request: the branch, the commit and the pull
request through the GitHub API; the agent never pushes) and resumes the
parked run with the result. Viewers see every request; only admins and
owners decide (ADR 0049: the default approver policy).

| Method Name | Request Type | Response Type | Description |
| ----------- | ------------ | ------------- | ------------|
| ListApprovals | [ListApprovalsRequest](#koinon-approval-v1alpha1-ListApprovalsRequest) | [ListApprovalsResponse](#koinon-approval-v1alpha1-ListApprovalsResponse) | ListApprovals returns the org&#39;s requests, oldest first: by default those waiting for a decision (Waiting on you), or every request of one ticket. Viewers and above. |
| GetApproval | [GetApprovalRequest](#koinon-approval-v1alpha1-GetApprovalRequest) | [GetApprovalResponse](#koinon-approval-v1alpha1-GetApprovalResponse) | GetApproval returns one request with its canonical arguments, the push-and-run risk summary and the full change. Viewers and above. |
| GetApprovalDiff | [GetApprovalDiffRequest](#koinon-approval-v1alpha1-GetApprovalDiffRequest) | [GetApprovalDiffResponse](#koinon-approval-v1alpha1-GetApprovalDiffResponse) | GetApprovalDiff compares a create_pull_request request with the branch its pull request will target (plan item W2: the full diff, collapsed below the risk summary). Koinon reads the current version of each file through the run&#39;s GitHub connection (contents:read, the one repository) and returns a unified diff per file. A file it cannot compare (GitHub error, larger than 1 MiB, not a file) carries a fallback_reason instead, and the card shows its full new content from GetApproval. Other tools fail with FAILED_PRECONDITION. Viewers and above. |
| Approve | [ApproveRequest](#koinon-approval-v1alpha1-ApproveRequest) | [ApproveResponse](#koinon-approval-v1alpha1-ApproveResponse) | Approve approves a pending request and executes it. args_sha256 must be the hash shown with the request; a different hash fails with ABORTED (HTTP 409) and nothing is executed. A request that is no longer pending fails with FAILED_PRECONDITION. Approving again with the same hash returns the request as it is (the same pull request), without executing it again. Admins and owners. |
| Deny | [DenyRequest](#koinon-approval-v1alpha1-DenyRequest) | [DenyResponse](#koinon-approval-v1alpha1-DenyResponse) | Deny denies a pending request; the run resumes with the denial and the note. Admins and owners. |
| ListApprovalScopes | [ListApprovalScopesRequest](#koinon-approval-v1alpha1-ListApprovalScopesRequest) | [ListApprovalScopesResponse](#koinon-approval-v1alpha1-ListApprovalScopesResponse) | ListApprovalScopes returns the scopes that are active now (not revoked, not expired) for one agent, oldest first: what the agent&#39;s page shows next to its grants. Viewers and above. |
| RevokeApprovalScope | [RevokeApprovalScopeRequest](#koinon-approval-v1alpha1-RevokeApprovalScopeRequest) | [RevokeApprovalScopeResponse](#koinon-approval-v1alpha1-RevokeApprovalScopeResponse) | RevokeApprovalScope ends a scope at once; the agent&#39;s next matching call needs a new approval. Revoking a scope that is already revoked returns it as it is. Admins and owners. |

 



<a name="koinon_audit_v1alpha1_audit-proto"></a>
<p align="right"><a href="#top">Top</a></p>

## koinon/audit/v1alpha1/audit.proto



<a name="koinon-audit-v1alpha1-AuditEvent"></a>

### AuditEvent
AuditEvent is one entry of the log. summary holds {&#34;before&#34;: .., &#34;after&#34;: ..} without secret values.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| id | [string](#string) |  |  |
| org_id | [string](#string) |  | org_id is empty for an install event. |
| actor_kind | [string](#string) |  | actor_kind is user, token, agent, secret, system or cli; actor_id is that actor&#39;s ID, the declaring Secret&#39;s name for secret, or the command for cli (keys forget). |
| actor_id | [string](#string) |  |  |
| action | [string](#string) |  | action reads &lt;area&gt;.&lt;verb&gt;, for example approval.approve or agent.archive. |
| target_kind | [string](#string) |  |  |
| target_id | [string](#string) |  |  |
| summary | [google.protobuf.Struct](#google-protobuf-Struct) |  |  |
| request_id | [string](#string) |  |  |
| created_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| chain_seq | [int64](#int64) |  | chain_seq, prev_hash and hash are the org&#39;s hash chain (the install chain for an install event), the hashes in lower-case hex. See ADR 0023 and ADR 0100. |
| prev_hash | [string](#string) |  |  |
| hash | [string](#string) |  |  |






<a name="koinon-audit-v1alpha1-ListRequest"></a>

### ListRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  | org_id names the org. Only a platform admin may leave it empty, to read every org. With scope AUDIT_SCOPE_INSTALL it must be empty. |
| since | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  | since and until bound created_at: since &lt;= created_at &lt; until. Both are optional. |
| until | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| action_prefix | [string](#string) |  | action_prefix keeps the events whose action starts with it, for example &#34;approval.&#34; or &#34;agent.create&#34;. Lower-case letters, digits and . _ - only. |
| actor_kind | [string](#string) |  | actor_kind keeps the events of one kind of actor; actor_id narrows that to one actor and needs actor_kind. |
| actor_id | [string](#string) |  |  |
| page_size | [int32](#int32) |  | page_size is 50 by default and at most 200. |
| page_token | [string](#string) |  | page_token is the next_page_token of the previous page. |
| scope | [AuditScope](#koinon-audit-v1alpha1-AuditScope) |  | scope defaults to the org scope (unspecified or AUDIT_SCOPE_ORG). AUDIT_SCOPE_INSTALL needs a platform admin. |






<a name="koinon-audit-v1alpha1-ListResponse"></a>

### ListResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| events | [AuditEvent](#koinon-audit-v1alpha1-AuditEvent) | repeated |  |
| next_page_token | [string](#string) |  | next_page_token is empty on the last page. |





 


<a name="koinon-audit-v1alpha1-AuditScope"></a>

### AuditScope
AuditScope selects which audit chain List reads.

| Name | Number | Description |
| ---- | ------ | ----------- |
| AUDIT_SCOPE_UNSPECIFIED | 0 | AUDIT_SCOPE_UNSPECIFIED reads the org scope, as before the install chain existed. |
| AUDIT_SCOPE_ORG | 1 | AUDIT_SCOPE_ORG reads the events of an org (or, for a platform admin with no org_id, of every org). |
| AUDIT_SCOPE_INSTALL | 2 | AUDIT_SCOPE_INSTALL reads the install chain: actions that belong to no org (ADR 0100). |


 

 


<a name="koinon-audit-v1alpha1-AuditService"></a>

### AuditService
AuditService reads the audit log (ADR 0023). The log is written by the services themselves and is
append-only; there is no way to change it here.

| Method Name | Request Type | Response Type | Description |
| ----------- | ------------ | ------------- | ------------|
| List | [ListRequest](#koinon-audit-v1alpha1-ListRequest) | [ListResponse](#koinon-audit-v1alpha1-ListResponse) | List returns audit events, newest first. An org admin or owner lists the events of their org (org_id is required for them). A platform admin names any org, or leaves org_id empty to read every org. A caller with a lower role in the org is refused, and an org the caller is not part of is NotFound. With scope AUDIT_SCOPE_INSTALL it returns the install chain (ADR 0100): platform admins only, org_id must be empty, and everyone else gets PermissionDenied. |

 



<a name="koinon_budget_v1alpha1_budget-proto"></a>
<p align="right"><a href="#top">Top</a></p>

## koinon/budget/v1alpha1/budget.proto



<a name="koinon-budget-v1alpha1-AgentBudget"></a>

### AgentBudget



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| agent_id | [string](#string) |  |  |
| daily_limit_usd | [string](#string) |  |  |
| spent_today_usd | [string](#string) |  |  |
| day_start | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| agent_name | [string](#string) |  | agent_name and template_default_usd are set by ListAgentBudgets. The default is the spendUSD of the agent&#39;s active version. |
| template_default_usd | [string](#string) |  |  |






<a name="koinon-budget-v1alpha1-AgentRunLimits"></a>

### AgentRunLimits



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| agent_id | [string](#string) |  |  |
| agent_name | [string](#string) |  |  |
| effective | [RunLimitValues](#koinon-budget-v1alpha1-RunLimitValues) |  | effective is what a run queued now gets: the override, else the template&#39;s value. |
| template_default | [RunLimitValues](#koinon-budget-v1alpha1-RunLimitValues) |  | template_default is the agent&#39;s active version (the template&#39;s values). |
| override | [RunLimitValues](#koinon-budget-v1alpha1-RunLimitValues) |  | override is what an admin set; unset fields follow the template. |






<a name="koinon-budget-v1alpha1-GetAgentBudgetRequest"></a>

### GetAgentBudgetRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| agent_id | [string](#string) |  |  |






<a name="koinon-budget-v1alpha1-GetAgentBudgetResponse"></a>

### GetAgentBudgetResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| budget | [AgentBudget](#koinon-budget-v1alpha1-AgentBudget) |  |  |






<a name="koinon-budget-v1alpha1-GetAgentRunLimitsRequest"></a>

### GetAgentRunLimitsRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| agent_id | [string](#string) |  |  |






<a name="koinon-budget-v1alpha1-GetAgentRunLimitsResponse"></a>

### GetAgentRunLimitsResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| limits | [AgentRunLimits](#koinon-budget-v1alpha1-AgentRunLimits) |  |  |






<a name="koinon-budget-v1alpha1-GetOrgBudgetRequest"></a>

### GetOrgBudgetRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |






<a name="koinon-budget-v1alpha1-GetOrgBudgetResponse"></a>

### GetOrgBudgetResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| budget | [OrgBudget](#koinon-budget-v1alpha1-OrgBudget) |  |  |






<a name="koinon-budget-v1alpha1-ListAgentBudgetsRequest"></a>

### ListAgentBudgetsRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |






<a name="koinon-budget-v1alpha1-ListAgentBudgetsResponse"></a>

### ListAgentBudgetsResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| budgets | [AgentBudget](#koinon-budget-v1alpha1-AgentBudget) | repeated |  |






<a name="koinon-budget-v1alpha1-ListNotificationsRequest"></a>

### ListNotificationsRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| max_results | [int32](#int32) |  | max_results defaults to 50 and is at most 200. |






<a name="koinon-budget-v1alpha1-ListNotificationsResponse"></a>

### ListNotificationsResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| notifications | [Notification](#koinon-budget-v1alpha1-Notification) | repeated |  |






<a name="koinon-budget-v1alpha1-ListOrgBudgetsRequest"></a>

### ListOrgBudgetsRequest







<a name="koinon-budget-v1alpha1-ListOrgBudgetsResponse"></a>

### ListOrgBudgetsResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| orgs | [OrgBudgetSummary](#koinon-budget-v1alpha1-OrgBudgetSummary) | repeated |  |
| month_start | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |






<a name="koinon-budget-v1alpha1-MarkNotificationReadRequest"></a>

### MarkNotificationReadRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| notification_id | [string](#string) |  |  |






<a name="koinon-budget-v1alpha1-MarkNotificationReadResponse"></a>

### MarkNotificationReadResponse







<a name="koinon-budget-v1alpha1-Notification"></a>

### Notification



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| id | [string](#string) |  |  |
| kind | [string](#string) |  | kind is budget.warning (80%), budget.reached (100%) or agent.unhealthy. |
| level | [string](#string) |  | level is agent_daily, org_monthly or platform_ceiling. |
| threshold | [int32](#int32) |  | threshold is 80 or 100. |
| limit_usd | [string](#string) |  |  |
| spent_usd | [string](#string) |  |  |
| agent_id | [string](#string) |  | agent_id is set for agent_daily. |
| window_start | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| created_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| read_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  | read_at is unset while the notification is unread. |
| message | [string](#string) |  | message is the text of notifications that are not about a budget (kind agent.unhealthy: an agent whose sandbox keeps getting stuck; agent_id names it). |






<a name="koinon-budget-v1alpha1-OrgBudget"></a>

### OrgBudget



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| monthly_cap_usd | [string](#string) |  | monthly_cap_usd is empty when there is no cap. |
| platform_ceiling_usd | [string](#string) |  | platform_ceiling_usd is empty when the ceiling is off. |
| spent_this_month_usd | [string](#string) |  |  |
| month_start | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| estimated_this_month_usd | [string](#string) |  | estimated_this_month_usd is the part of the spend that came from estimated usage. |
| system_checks_this_month_usd | [string](#string) |  | system_checks_this_month_usd is the part of the spend that came from Koinon&#39;s own requests (the provider echo check), which belong to no agent. It counts toward the cap. |






<a name="koinon-budget-v1alpha1-OrgBudgetSummary"></a>

### OrgBudgetSummary



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| org_slug | [string](#string) |  |  |
| org_name | [string](#string) |  |  |
| monthly_cap_usd | [string](#string) |  | monthly_cap_usd and platform_ceiling_usd are empty when unset. |
| platform_ceiling_usd | [string](#string) |  |  |
| spent_this_month_usd | [string](#string) |  |  |






<a name="koinon-budget-v1alpha1-RunLimitValues"></a>

### RunLimitValues
RunLimitValues are the limits of one run. Zero (or empty) means unset.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| max_model_requests | [int64](#int64) |  | max_model_requests counts every model request through the agent&#39;s key. |
| max_spend_usd | [string](#string) |  | max_spend_usd is a USD decimal string. |
| max_duration_seconds | [int64](#int64) |  | max_duration_seconds is the wall-clock limit. |






<a name="koinon-budget-v1alpha1-SetAgentBudgetRequest"></a>

### SetAgentBudgetRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| agent_id | [string](#string) |  |  |
| daily_limit_usd | [string](#string) |  |  |






<a name="koinon-budget-v1alpha1-SetAgentBudgetResponse"></a>

### SetAgentBudgetResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| budget | [AgentBudget](#koinon-budget-v1alpha1-AgentBudget) |  |  |






<a name="koinon-budget-v1alpha1-SetAgentRunLimitsRequest"></a>

### SetAgentRunLimitsRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| agent_id | [string](#string) |  |  |
| override | [RunLimitValues](#koinon-budget-v1alpha1-RunLimitValues) |  |  |






<a name="koinon-budget-v1alpha1-SetAgentRunLimitsResponse"></a>

### SetAgentRunLimitsResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| limits | [AgentRunLimits](#koinon-budget-v1alpha1-AgentRunLimits) |  |  |






<a name="koinon-budget-v1alpha1-SetMonthlyCapRequest"></a>

### SetMonthlyCapRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| monthly_cap_usd | [string](#string) |  |  |






<a name="koinon-budget-v1alpha1-SetMonthlyCapResponse"></a>

### SetMonthlyCapResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| budget | [OrgBudget](#koinon-budget-v1alpha1-OrgBudget) |  |  |






<a name="koinon-budget-v1alpha1-SetPlatformCeilingRequest"></a>

### SetPlatformCeilingRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| platform_ceiling_usd | [string](#string) |  |  |






<a name="koinon-budget-v1alpha1-SetPlatformCeilingResponse"></a>

### SetPlatformCeilingResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| budget | [OrgBudget](#koinon-budget-v1alpha1-OrgBudget) |  |  |





 

 

 


<a name="koinon-budget-v1alpha1-BudgetService"></a>

### BudgetService
BudgetService sets an org&#39;s monthly cap and its agents&#39; daily limits and lists the in-app
notifications about them (plan item BG1, ADR 0056). Amounts are USD decimal strings.
The cap is per calendar month in UTC, an agent&#39;s limit per UTC day. Admins and owners write;
the platform ceiling is for platform admins. Every change is audited.

| Method Name | Request Type | Response Type | Description |
| ----------- | ------------ | ------------- | ------------|
| GetOrgBudget | [GetOrgBudgetRequest](#koinon-budget-v1alpha1-GetOrgBudgetRequest) | [GetOrgBudgetResponse](#koinon-budget-v1alpha1-GetOrgBudgetResponse) | GetOrgBudget returns the cap, the platform ceiling and this month&#39;s spend. |
| ListOrgBudgets | [ListOrgBudgetsRequest](#koinon-budget-v1alpha1-ListOrgBudgetsRequest) | [ListOrgBudgetsResponse](#koinon-budget-v1alpha1-ListOrgBudgetsResponse) | ListOrgBudgets returns every org&#39;s cap, platform ceiling and spend this month. Platform admins only. |
| SetMonthlyCap | [SetMonthlyCapRequest](#koinon-budget-v1alpha1-SetMonthlyCapRequest) | [SetMonthlyCapResponse](#koinon-budget-v1alpha1-SetMonthlyCapResponse) | SetMonthlyCap sets the cap, or clears it with an empty amount. It cannot exceed the ceiling. |
| SetPlatformCeiling | [SetPlatformCeilingRequest](#koinon-budget-v1alpha1-SetPlatformCeilingRequest) | [SetPlatformCeilingResponse](#koinon-budget-v1alpha1-SetPlatformCeilingResponse) | SetPlatformCeiling sets the org&#39;s ceiling, or clears it with an empty amount. Platform admins only. The org&#39;s cap must already fit under it. |
| GetAgentBudget | [GetAgentBudgetRequest](#koinon-budget-v1alpha1-GetAgentBudgetRequest) | [GetAgentBudgetResponse](#koinon-budget-v1alpha1-GetAgentBudgetResponse) | GetAgentBudget returns an agent&#39;s effective daily limit and today&#39;s spend. |
| ListAgentBudgets | [ListAgentBudgetsRequest](#koinon-budget-v1alpha1-ListAgentBudgetsRequest) | [ListAgentBudgetsResponse](#koinon-budget-v1alpha1-ListAgentBudgetsResponse) | ListAgentBudgets returns every agent&#39;s effective daily limit, template default and today&#39;s spend. |
| SetAgentBudget | [SetAgentBudgetRequest](#koinon-budget-v1alpha1-SetAgentBudgetRequest) | [SetAgentBudgetResponse](#koinon-budget-v1alpha1-SetAgentBudgetResponse) | SetAgentBudget sets an agent&#39;s daily limit. A template&#39;s spendUSD is only the default for a new agent. |
| GetAgentRunLimits | [GetAgentRunLimitsRequest](#koinon-budget-v1alpha1-GetAgentRunLimitsRequest) | [GetAgentRunLimitsResponse](#koinon-budget-v1alpha1-GetAgentRunLimitsResponse) | GetAgentRunLimits returns the limits a run of the agent gets: the effective values, the template&#39;s defaults and the admin&#39;s overrides (ADR 0081). |
| SetAgentRunLimits | [SetAgentRunLimitsRequest](#koinon-budget-v1alpha1-SetAgentRunLimitsRequest) | [SetAgentRunLimitsResponse](#koinon-budget-v1alpha1-SetAgentRunLimitsResponse) | SetAgentRunLimits replaces the agent&#39;s overrides of its run limits. A zero value clears that override; all zero clears them all. It applies to runs queued afterwards. |
| ListNotifications | [ListNotificationsRequest](#koinon-budget-v1alpha1-ListNotificationsRequest) | [ListNotificationsResponse](#koinon-budget-v1alpha1-ListNotificationsResponse) | ListNotifications returns the caller&#39;s notifications in this org, newest first: budget warnings at 80% and stops at 100%. Owners and admins receive them; platform admins also those about the platform ceiling. |
| MarkNotificationRead | [MarkNotificationReadRequest](#koinon-budget-v1alpha1-MarkNotificationReadRequest) | [MarkNotificationReadResponse](#koinon-budget-v1alpha1-MarkNotificationReadResponse) | MarkNotificationRead marks one of the caller&#39;s notifications read. |

 



<a name="koinon_budget_v1alpha1_price-proto"></a>
<p align="right"><a href="#top">Top</a></p>

## koinon/budget/v1alpha1/price.proto



<a name="koinon-budget-v1alpha1-FlaggedPrice"></a>

### FlaggedPrice



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| price_ref | [string](#string) |  |  |
| model | [string](#string) |  |  |
| requests | [int64](#int64) |  |  |
| last_seen | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| cost_usd | [string](#string) |  |  |
| gateway_cost_usd | [string](#string) |  |  |
| gateway_diff | [int64](#int64) |  | Requests per reason; one request can have several. gateway_diff: Koinon&#39;s cost differs from the gateway&#39;s by more than 1%. usage_missing: the provider reported no usage, so the request was estimated. max_output_capped: an estimated output was capped at the price&#39;s max_output. price_error: the stored price could not be used. |
| usage_missing | [int64](#int64) |  |  |
| max_output_capped | [int64](#int64) |  |  |
| price_error | [int64](#int64) |  |  |






<a name="koinon-budget-v1alpha1-GetDefaultPriceChangesRequest"></a>

### GetDefaultPriceChangesRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| from_version | [string](#string) |  |  |
| to_version | [string](#string) |  |  |






<a name="koinon-budget-v1alpha1-GetDefaultPriceChangesResponse"></a>

### GetDefaultPriceChangesResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| from_version | [string](#string) |  |  |
| to_version | [string](#string) |  |  |
| changes | [PriceChange](#koinon-budget-v1alpha1-PriceChange) | repeated |  |






<a name="koinon-budget-v1alpha1-GetDefaultPricesRequest"></a>

### GetDefaultPricesRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |






<a name="koinon-budget-v1alpha1-GetDefaultPricesResponse"></a>

### GetDefaultPricesResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| version | [string](#string) |  |  |
| previous_versions | [string](#string) | repeated |  |
| entries | [PriceEntry](#koinon-budget-v1alpha1-PriceEntry) | repeated |  |






<a name="koinon-budget-v1alpha1-ListFlaggedPricesRequest"></a>

### ListFlaggedPricesRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| since | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  | since defaults to 7 days ago. |






<a name="koinon-budget-v1alpha1-ListFlaggedPricesResponse"></a>

### ListFlaggedPricesResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| flagged | [FlaggedPrice](#koinon-budget-v1alpha1-FlaggedPrice) | repeated |  |






<a name="koinon-budget-v1alpha1-ListPricesRequest"></a>

### ListPricesRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |






<a name="koinon-budget-v1alpha1-ListPricesResponse"></a>

### ListPricesResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| prices | [PriceVersion](#koinon-budget-v1alpha1-PriceVersion) | repeated |  |






<a name="koinon-budget-v1alpha1-PriceChange"></a>

### PriceChange



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| kind | [string](#string) |  | kind is added, removed or changed. |
| id | [string](#string) |  |  |
| valid_from | [string](#string) |  |  |
| before | [PriceEntry](#koinon-budget-v1alpha1-PriceEntry) |  |  |
| after | [PriceEntry](#koinon-budget-v1alpha1-PriceEntry) |  |  |
| fields | [string](#string) | repeated |  |






<a name="koinon-budget-v1alpha1-PriceEntry"></a>

### PriceEntry



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| id | [string](#string) |  |  |
| provider | [string](#string) |  |  |
| match | [PriceMatch](#koinon-budget-v1alpha1-PriceMatch) |  |  |
| valid_from | [string](#string) |  |  |
| rates | [Rates](#koinon-budget-v1alpha1-Rates) |  |  |
| tiers | [PriceTier](#koinon-budget-v1alpha1-PriceTier) | repeated |  |
| tokenizer | [string](#string) |  |  |
| ratio | [string](#string) |  |  |
| tool_overhead_tokens | [int32](#int32) |  |  |
| hidden_thinking | [bool](#bool) |  |  |
| source_url | [string](#string) |  |  |
| source_accessed | [string](#string) |  |  |
| note | [string](#string) |  |  |
| max_output | [int32](#int32) |  |  |






<a name="koinon-budget-v1alpha1-PriceMatch"></a>

### PriceMatch
PriceMatch holds exactly one of glob and regex.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| glob | [string](#string) |  |  |
| regex | [string](#string) |  |  |






<a name="koinon-budget-v1alpha1-PriceTier"></a>

### PriceTier
PriceTier replaces the rates it names once the input side of a request (input, cache reads
and cache writes) exceeds context_over tokens.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| context_over | [int64](#int64) |  |  |
| rates | [Rates](#koinon-budget-v1alpha1-Rates) |  |  |






<a name="koinon-budget-v1alpha1-PriceVersion"></a>

### PriceVersion



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| id | [string](#string) |  | id is cited by ledger entries as org#&lt;id&gt;. |
| name | [string](#string) |  |  |
| provider | [string](#string) |  |  |
| match | [PriceMatch](#koinon-budget-v1alpha1-PriceMatch) |  |  |
| rates | [Rates](#koinon-budget-v1alpha1-Rates) |  |  |
| tiers | [PriceTier](#koinon-budget-v1alpha1-PriceTier) | repeated |  |
| tokenizer | [string](#string) |  |  |
| ratio | [string](#string) |  |  |
| valid_from | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| retired_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| created_by | [string](#string) |  |  |
| created_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| max_output | [int32](#int32) |  |  |
| tool_overhead_tokens | [int32](#int32) |  |  |
| hidden_thinking | [bool](#bool) |  |  |
| note | [string](#string) |  |  |
| needs_review | [bool](#bool) |  | needs_review is set for a price migrated from a model&#39;s rates whose estimation settings are generic because no default entry matched; an admin should check them. |






<a name="koinon-budget-v1alpha1-Rates"></a>

### Rates
Rates are USD per million tokens as decimal strings with at most 6 fraction digits. Empty
means unset; a free model needs an explicit &#34;0&#34;.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| input | [string](#string) |  |  |
| output | [string](#string) |  |  |
| cache_read | [string](#string) |  |  |
| cache_write | [string](#string) |  |  |
| reasoning | [string](#string) |  |  |






<a name="koinon-budget-v1alpha1-RetirePriceRequest"></a>

### RetirePriceRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| name | [string](#string) |  |  |






<a name="koinon-budget-v1alpha1-RetirePriceResponse"></a>

### RetirePriceResponse







<a name="koinon-budget-v1alpha1-SetPriceRequest"></a>

### SetPriceRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| name | [string](#string) |  |  |
| provider | [string](#string) |  |  |
| match | [PriceMatch](#koinon-budget-v1alpha1-PriceMatch) |  |  |
| rates | [Rates](#koinon-budget-v1alpha1-Rates) |  |  |
| tiers | [PriceTier](#koinon-budget-v1alpha1-PriceTier) | repeated |  |
| tokenizer | [string](#string) |  | tokenizer is bytes (default), tiktoken:o200k_base or tiktoken:cl100k_base. |
| ratio | [string](#string) |  | ratio is UTF-8 bytes per token for estimates; default 3.0. |
| tool_overhead_tokens | [int32](#int32) |  |  |
| hidden_thinking | [bool](#bool) |  |  |
| valid_from | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  | valid_from defaults to now. |
| max_output | [int32](#int32) |  | max_output is the largest output in tokens; required unless every rate is &#34;0&#34;. An aborted response without usage settles at no more than this. |






<a name="koinon-budget-v1alpha1-SetPriceResponse"></a>

### SetPriceResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| price | [PriceVersion](#koinon-budget-v1alpha1-PriceVersion) |  |  |
| warnings | [string](#string) | repeated | warnings are non-fatal notes about the saved entry, for example that a model with a cache_read price runs without prompt caching because Koinon has no cache mode for its provider format (ADR 0068). |





 

 

 


<a name="koinon-budget-v1alpha1-PriceService"></a>

### PriceService
PriceService manages an org&#39;s own prices and shows the default price list (plan item PV2,
ADR 0056). A price is a rate per usage type, in USD per million tokens, for non-overlapping
buckets of a request; org prices win over the default list. Admins and owners.

| Method Name | Request Type | Response Type | Description |
| ----------- | ------------ | ------------- | ------------|
| ListPrices | [ListPricesRequest](#koinon-budget-v1alpha1-ListPricesRequest) | [ListPricesResponse](#koinon-budget-v1alpha1-ListPricesResponse) | ListPrices returns every version of the org&#39;s prices, newest first per name. |
| SetPrice | [SetPriceRequest](#koinon-budget-v1alpha1-SetPriceRequest) | [SetPriceResponse](#koinon-budget-v1alpha1-SetPriceResponse) | SetPrice adds a version of a named price. Earlier versions never change, and requests before valid_from keep the price they were charged at. |
| RetirePrice | [RetirePriceRequest](#koinon-budget-v1alpha1-RetirePriceRequest) | [RetirePriceResponse](#koinon-budget-v1alpha1-RetirePriceResponse) | RetirePrice ends a named price now. Its versions stay for the ledger entries that cite them. |
| GetDefaultPrices | [GetDefaultPricesRequest](#koinon-budget-v1alpha1-GetDefaultPricesRequest) | [GetDefaultPricesResponse](#koinon-budget-v1alpha1-GetDefaultPricesResponse) | GetDefaultPrices returns the default list this installation runs. |
| GetDefaultPriceChanges | [GetDefaultPriceChangesRequest](#koinon-budget-v1alpha1-GetDefaultPriceChangesRequest) | [GetDefaultPriceChangesResponse](#koinon-budget-v1alpha1-GetDefaultPriceChangesResponse) | GetDefaultPriceChanges returns what changed between two versions of the default list. An empty from_version means the version before to_version; an empty to_version the current one. |
| ListFlaggedPrices | [ListFlaggedPricesRequest](#koinon-budget-v1alpha1-ListFlaggedPricesRequest) | [ListFlaggedPricesResponse](#koinon-budget-v1alpha1-ListFlaggedPricesResponse) | ListFlaggedPrices returns price entries whose recomputed cost differs from the gateway&#39;s by more than 1%. |

 



<a name="koinon_outward_v1alpha1_outward-proto"></a>
<p align="right"><a href="#top">Top</a></p>

## koinon/outward/v1alpha1/outward.proto



<a name="koinon-outward-v1alpha1-AdminDisabled"></a>

### AdminDisabled
AdminDisabled is the switch as Get and List of a target show it. A target without it is enabled.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| by | [string](#string) |  | by is the actor as the audit log reads it, for example &#34;token &lt;id&gt;&#34; or &#34;secret &lt;name&gt;&#34;. |
| note | [string](#string) |  |  |
| declared | [bool](#bool) |  | declared is true when the declaring Secret says disabled: true; the API cannot enable such a target. |






<a name="koinon-outward-v1alpha1-OutwardTargetState"></a>

### OutwardTargetState



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| kind | [OutwardTargetKind](#koinon-outward-v1alpha1-OutwardTargetKind) |  |  |
| target_id | [string](#string) |  |  |
| name | [string](#string) |  |  |
| disabled | [bool](#bool) |  |  |
| disabled_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| disabled_by | [string](#string) |  | disabled_by is the actor as the audit log reads it, for example &#34;user &lt;id&gt;&#34; or &#34;secret &lt;name&gt;&#34;. |
| note | [string](#string) |  |  |






<a name="koinon-outward-v1alpha1-SetOutwardTargetDisabledRequest"></a>

### SetOutwardTargetDisabledRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| kind | [OutwardTargetKind](#koinon-outward-v1alpha1-OutwardTargetKind) |  |  |
| target_id | [string](#string) |  |  |
| disabled | [bool](#bool) |  |  |
| note | [string](#string) |  | note says why, at most 500 characters. Kept while the target is disabled. |






<a name="koinon-outward-v1alpha1-SetOutwardTargetDisabledResponse"></a>

### SetOutwardTargetDisabledResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| state | [OutwardTargetState](#koinon-outward-v1alpha1-OutwardTargetState) |  |  |





 


<a name="koinon-outward-v1alpha1-OutwardTargetKind"></a>

### OutwardTargetKind
OutwardTargetKind is what kind of target the switch is for.

| Name | Number | Description |
| ---- | ------ | ----------- |
| OUTWARD_TARGET_KIND_UNSPECIFIED | 0 |  |
| OUTWARD_TARGET_KIND_MCP_SERVER | 1 |  |
| OUTWARD_TARGET_KIND_MODEL_PROVIDER | 2 |  |
| OUTWARD_TARGET_KIND_CONNECTION | 3 |  |
| OUTWARD_TARGET_KIND_TRACE_DESTINATION | 4 |  |
| OUTWARD_TARGET_KIND_PLATFORM_TRACE_DESTINATION | 5 | The platform trace destination has one row: target_id is ignored and org_id names any org. |


 

 


<a name="koinon-outward-v1alpha1-OutwardService"></a>

### OutwardService
OutwardService is the one switch for everything Koinon calls outside the install (ADR 0110 decision 5):
MCP servers, model providers, code-host connections and trace destinations. A disabled target is not
called, not probed and not offered to runs; it keeps its bindings and its credential. It is the
connect-rpc face of the same service layer the admin REST API uses. Every change is audited as
outward.disable or outward.enable.

| Method Name | Request Type | Response Type | Description |
| ----------- | ------------ | ------------- | ------------|
| SetOutwardTargetDisabled | [SetOutwardTargetDisabledRequest](#koinon-outward-v1alpha1-SetOutwardTargetDisabledRequest) | [SetOutwardTargetDisabledResponse](#koinon-outward-v1alpha1-SetOutwardTargetDisabledResponse) | SetOutwardTargetDisabled disables or enables one target. Org admins and owners switch the org&#39;s own targets; the platform trace destination needs a platform admin. Disabling is allowed while the install is held for a restore; enabling is refused then with FAILED_PRECONDITION. A target whose declaring Secret says disabled: true cannot be enabled here (FAILED_PRECONDITION). Setting the state a target already has changes and audits nothing. |

 



<a name="koinon_connection_v1alpha1_connection-proto"></a>
<p align="right"><a href="#top">Top</a></p>

## koinon/connection/v1alpha1/connection.proto



<a name="koinon-connection-v1alpha1-BoundAgent"></a>

### BoundAgent



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| id | [string](#string) |  |  |
| display_name | [string](#string) |  |  |






<a name="koinon-connection-v1alpha1-BoundAgents"></a>

### BoundAgents
BoundAgents is the error detail of a DeleteConnection refused with FAILED_PRECONDITION because
agents are still bound to the connection.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| agents | [BoundAgent](#koinon-connection-v1alpha1-BoundAgent) | repeated |  |






<a name="koinon-connection-v1alpha1-CheckResult"></a>

### CheckResult
CheckResult is one setup-guide check.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| check | [string](#string) |  | The check&#39;s name, e.g. github.installation. |
| step | [string](#string) |  | The setup-guide step it validates, so the UI can point at it. |
| ok | [bool](#bool) |  |  |
| message | [string](#string) |  | What went wrong, or what was found. Never contains secrets. |






<a name="koinon-connection-v1alpha1-Connection"></a>

### Connection
Connection is a configured connector instance.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| id | [string](#string) |  |  |
| org_id | [string](#string) |  |  |
| connector | [string](#string) |  | Connector type ID, e.g. github. |
| display_name | [string](#string) |  |  |
| status | [ConnectionStatus](#koinon-connection-v1alpha1-ConnectionStatus) |  |  |
| last_tested_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| last_test | [CheckResult](#koinon-connection-v1alpha1-CheckResult) | repeated |  |
| created_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| source | [string](#string) |  | source is &#34;ui&#34; for a connection entered here and &#34;secret&#34; for one declared by a labelled Kubernetes Secret (ADR 0056), which is managed_by_secret. |
| managed_by_secret | [string](#string) |  |  |
| removed | [bool](#bool) |  | removed is set when the declaring Secret was deleted. The connection stays, for the audit log and the agents bound to it, but is not usable: bound agents report why and runs that need it fail with that reason. Restoring the Secret restores the connection. |
| credential_lost_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  | credential_lost_at is set when the secret was lost with the key that encrypted it (`koinon-api keys forget`, ADR 0085 decision 13): the connection is not usable (bound agents report it and runs that need it fail) until the secret is entered again. Unset otherwise. |
| admin_disabled | [koinon.outward.v1alpha1.AdminDisabled](#koinon-outward-v1alpha1-AdminDisabled) |  | admin_disabled is set while an admin holds the target off (the outward switch, ADR 0110 decision 5): it is not called, probed or offered to runs. Unset otherwise. |
| github | [GitHubConnection](#koinon-connection-v1alpha1-GitHubConnection) |  |  |
| gitea | [GiteaConnection](#koinon-connection-v1alpha1-GiteaConnection) |  |  |






<a name="koinon-connection-v1alpha1-ConnectorType"></a>

### ConnectorType
ConnectorType is a platform-curated connector type.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| id | [string](#string) |  |  |
| version | [string](#string) |  |  |
| display_name | [string](#string) |  |  |
| description | [string](#string) |  |  |
| auth_type | [string](#string) |  | &#34;app-credential&#34;, &#34;federated&#34; or &#34;delegated-user&#34;. |
| scopes | [Scope](#koinon-connection-v1alpha1-Scope) | repeated |  |
| document | [google.protobuf.Struct](#google-protobuf-Struct) |  | The whole connector type document as JSON, setup guide included; its shape is connectors/schema/connector.schema.json. |






<a name="koinon-connection-v1alpha1-CreateGitHubConnectionRequest"></a>

### CreateGitHubConnectionRequest
CreateGitHubConnectionRequest carries the setup guide&#39;s fields.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| display_name | [string](#string) |  |  |
| app_id | [int64](#int64) |  |  |
| installation_id | [int64](#int64) |  |  |
| private_key | [string](#string) |  | Secure input (ADR 0021): the App&#39;s PEM private key. Stored only as an envelope in Postgres (ADR 0056), never returned or logged. |
| test | [bool](#bool) |  | Run TestConnection right after creating it. |
| base_url | [string](#string) |  | Web base URL of a GitHub Enterprise Server host, https://host[:port]. Empty is github.com. The REST API is reached at /api/v3 on the same host. |
| ca_bundle | [string](#string) |  | PEM certificates that sign the host&#39;s TLS certificate, when the system roots do not. Used for Koinon&#39;s API calls and for git in sandboxes. |
| ci_dirs | [string](#string) | repeated | Directories that run CI on this host besides the connector&#39;s own CI paths, relative to the repository root. Optional; at most 20. |






<a name="koinon-connection-v1alpha1-CreateGitHubConnectionResponse"></a>

### CreateGitHubConnectionResponse
CreateGitHubConnectionResponse holds the new connection and, when asked
for, the test results.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| connection | [Connection](#koinon-connection-v1alpha1-Connection) |  |  |
| results | [CheckResult](#koinon-connection-v1alpha1-CheckResult) | repeated |  |






<a name="koinon-connection-v1alpha1-CreateGiteaConnectionRequest"></a>

### CreateGiteaConnectionRequest
CreateGiteaConnectionRequest carries the setup guide&#39;s fields.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| display_name | [string](#string) |  |  |
| base_url | [string](#string) |  | The server&#39;s web base URL, https://host[:port][/path]. Required. |
| read_token | [string](#string) |  | Secure input (ADR 0021): the bot&#39;s read token and write token. Stored only as an envelope in Postgres (ADR 0056), never returned or logged. |
| write_token | [string](#string) |  |  |
| ca_bundle | [string](#string) |  | PEM certificates that sign the server&#39;s TLS certificate, when the system roots do not. |
| ci_dirs | [string](#string) | repeated | Directories that run CI on this server besides the connector&#39;s own CI paths. At most 20. |
| test | [bool](#bool) |  | Run TestConnection right after creating it. |






<a name="koinon-connection-v1alpha1-CreateGiteaConnectionResponse"></a>

### CreateGiteaConnectionResponse
CreateGiteaConnectionResponse holds the new connection and, when asked for, the test results.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| connection | [Connection](#koinon-connection-v1alpha1-Connection) |  |  |
| results | [CheckResult](#koinon-connection-v1alpha1-CheckResult) | repeated |  |






<a name="koinon-connection-v1alpha1-DeleteConnectionRequest"></a>

### DeleteConnectionRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| connection_id | [string](#string) |  |  |






<a name="koinon-connection-v1alpha1-DeleteConnectionResponse"></a>

### DeleteConnectionResponse







<a name="koinon-connection-v1alpha1-GetConnectionRequest"></a>

### GetConnectionRequest
GetConnectionRequest names a connection.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| connection_id | [string](#string) |  |  |






<a name="koinon-connection-v1alpha1-GetConnectionResponse"></a>

### GetConnectionResponse
GetConnectionResponse holds the connection.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| connection | [Connection](#koinon-connection-v1alpha1-Connection) |  |  |






<a name="koinon-connection-v1alpha1-GitHubConnection"></a>

### GitHubConnection
GitHubConnection is the non-secret part of a GitHub App connection.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| app_id | [int64](#int64) |  |  |
| installation_id | [int64](#int64) |  |  |
| key_fingerprint | [string](#string) |  | SHA-256 fingerprint of the private key&#39;s public half, as GitHub shows it under the App&#39;s private keys. |
| base_url | [string](#string) |  | The host&#39;s web base URL when it is not github.com (GitHub Enterprise Server), https://host[:port]; empty for github.com. |
| ca_bundle | [string](#string) |  | PEM certificates trusted for that host besides the system roots. Public; empty when the host&#39;s certificate chains to the system roots. |
| ci_dirs | [string](#string) | repeated | Directories, relative to the repository root, that run CI on this host besides the connector&#39;s own CI paths (a Gitea server&#39;s WORKFLOW_DIRS). create_pull_request refuses changes to them. |






<a name="koinon-connection-v1alpha1-GitHubConnectionSecret"></a>

### GitHubConnectionSecret
GitHubConnectionSecret is the secret field of a GitHub App connection.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| private_key | [string](#string) |  | Secure input (ADR 0021): the App&#39;s PEM private key, a new one or the one GitHub still lists. Stored only as an envelope in Postgres (ADR 0056), never returned or logged. |






<a name="koinon-connection-v1alpha1-GiteaConnection"></a>

### GiteaConnection
GiteaConnection is the non-secret part of a Gitea connection (ADR 0063).


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| base_url | [string](#string) |  | The server&#39;s web base URL, https://host[:port][/path]; the REST API is at /api/v1 under it. |
| ca_bundle | [string](#string) |  | PEM certificates trusted for that server besides the system roots. Public. |
| ci_dirs | [string](#string) | repeated | Directories, relative to the repository root, that run CI on this server besides the connector&#39;s own CI paths (WORKFLOW_DIRS). |
| read_token_set_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  | When each token was entered in Koinon. Gitea does not expose a token&#39;s creation date, so this is the age Koinon can show (ADR 0063). |
| write_token_set_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| token_warn_after_seconds | [int64](#int64) |  | The age after which Koinon warns that a token is old, from the connector type. Nothing blocks on age and Koinon never rotates a token. Zero for no warning. |






<a name="koinon-connection-v1alpha1-GiteaConnectionSecret"></a>

### GiteaConnectionSecret
GiteaConnectionSecret is the secret fields of a Gitea connection (ADR 0063).


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| read_token | [string](#string) |  | Secure input (ADR 0021): the bot&#39;s read token and write token, both required. A token equal to the stored one keeps the date it was entered. Stored only as an envelope, never returned or logged. |
| write_token | [string](#string) |  |  |






<a name="koinon-connection-v1alpha1-InstallationRepo"></a>

### InstallationRepo
InstallationRepo is one repository the installation can access.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| full_name | [string](#string) |  | owner/name as GitHub spells it: the name to bind. |
| private | [bool](#bool) |  |  |
| default_branch | [string](#string) |  |  |






<a name="koinon-connection-v1alpha1-ListConnectionsRequest"></a>

### ListConnectionsRequest
ListConnectionsRequest names the org.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |






<a name="koinon-connection-v1alpha1-ListConnectionsResponse"></a>

### ListConnectionsResponse
ListConnectionsResponse lists the org&#39;s connections.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| connections | [Connection](#koinon-connection-v1alpha1-Connection) | repeated |  |






<a name="koinon-connection-v1alpha1-ListConnectorTypesRequest"></a>

### ListConnectorTypesRequest
ListConnectorTypesRequest is empty.






<a name="koinon-connection-v1alpha1-ListConnectorTypesResponse"></a>

### ListConnectorTypesResponse
ListConnectorTypesResponse lists the connector types.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| connector_types | [ConnectorType](#koinon-connection-v1alpha1-ConnectorType) | repeated |  |






<a name="koinon-connection-v1alpha1-ListInstallationReposRequest"></a>

### ListInstallationReposRequest
ListInstallationReposRequest names a GitHub connection and a page.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| connection_id | [string](#string) |  |  |
| page_token | [string](#string) |  | Empty for the first page; otherwise next_page_token of the previous response, sent with the same query. |
| query | [string](#string) |  | Keeps only repositories whose owner/name contains it, ignoring case. At most 200 characters. |






<a name="koinon-connection-v1alpha1-ListInstallationReposResponse"></a>

### ListInstallationReposResponse
ListInstallationReposResponse is one page, in GitHub&#39;s order.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| repos | [InstallationRepo](#koinon-connection-v1alpha1-InstallationRepo) | repeated |  |
| next_page_token | [string](#string) |  | Empty on the last page. A filtered page can hold fewer repositories than an unfiltered one, or none, and still have a next page. |
| total_count | [int64](#int64) |  | How many repositories the installation can access, before the query. |






<a name="koinon-connection-v1alpha1-Scope"></a>

### Scope
Scope is a grantable capability of a connector type.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| id | [string](#string) |  |  |
| tier | [Tier](#koinon-connection-v1alpha1-Tier) |  |  |
| description | [string](#string) |  |  |
| tools | [string](#string) | repeated | MCP tools the scope exposes behind agentgateway. |






<a name="koinon-connection-v1alpha1-SetConnectionSecretRequest"></a>

### SetConnectionSecretRequest
SetConnectionSecretRequest names a connection and carries its new secret fields. The secret&#39;s
connector must be the connection&#39;s.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| connection_id | [string](#string) |  |  |
| github | [GitHubConnectionSecret](#koinon-connection-v1alpha1-GitHubConnectionSecret) |  |  |
| gitea | [GiteaConnectionSecret](#koinon-connection-v1alpha1-GiteaConnectionSecret) |  |  |
| test | [bool](#bool) |  | Run TestConnection right after storing the secret. |






<a name="koinon-connection-v1alpha1-SetConnectionSecretResponse"></a>

### SetConnectionSecretResponse
SetConnectionSecretResponse holds the connection and, when asked for, the test results.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| connection | [Connection](#koinon-connection-v1alpha1-Connection) |  |  |
| results | [CheckResult](#koinon-connection-v1alpha1-CheckResult) | repeated |  |






<a name="koinon-connection-v1alpha1-TestConnectionRequest"></a>

### TestConnectionRequest
TestConnectionRequest names a connection.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| connection_id | [string](#string) |  |  |






<a name="koinon-connection-v1alpha1-TestConnectionResponse"></a>

### TestConnectionResponse
TestConnectionResponse holds the checks in setup-guide order.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| connection | [Connection](#koinon-connection-v1alpha1-Connection) |  |  |
| results | [CheckResult](#koinon-connection-v1alpha1-CheckResult) | repeated |  |





 


<a name="koinon-connection-v1alpha1-ConnectionStatus"></a>

### ConnectionStatus
ConnectionStatus is the outcome of the last test.

| Name | Number | Description |
| ---- | ------ | ----------- |
| CONNECTION_STATUS_UNSPECIFIED | 0 |  |
| CONNECTION_STATUS_UNTESTED | 1 |  |
| CONNECTION_STATUS_OK | 2 |  |
| CONNECTION_STATUS_FAILING | 3 |  |



<a name="koinon-connection-v1alpha1-Tier"></a>

### Tier
Tier is a scope&#39;s risk tier (ADR 0008).

| Name | Number | Description |
| ---- | ------ | ----------- |
| TIER_UNSPECIFIED | 0 |  |
| TIER_READ | 1 |  |
| TIER_REVERSIBLE_WRITE | 2 |  |
| TIER_EXTERNAL_OR_IRREVERSIBLE | 3 |  |
| TIER_FORBIDDEN | 4 |  |


 

 


<a name="koinon-connection-v1alpha1-ConnectionService"></a>

### ConnectionService
ConnectionService manages an org&#39;s connections to external systems
(ADR 0007): one configured instance of a connector type, such as one GitHub
App installation. Secrets entered here are sealed straight into the encrypted
secret store (ADR 0021, 0056); no response ever contains them.

| Method Name | Request Type | Response Type | Description |
| ----------- | ------------ | ------------- | ------------|
| ListConnectorTypes | [ListConnectorTypesRequest](#koinon-connection-v1alpha1-ListConnectorTypesRequest) | [ListConnectorTypesResponse](#koinon-connection-v1alpha1-ListConnectorTypesResponse) | ListConnectorTypes returns the connector types this install ships, with their scopes and setup guides. Any signed-in user. |
| CreateGitHubConnection | [CreateGitHubConnectionRequest](#koinon-connection-v1alpha1-CreateGitHubConnectionRequest) | [CreateGitHubConnectionResponse](#koinon-connection-v1alpha1-CreateGitHubConnectionResponse) | CreateGitHubConnection creates a GitHub App connection. The private key is stored encrypted and never returned. Admins and owners. |
| CreateGiteaConnection | [CreateGiteaConnectionRequest](#koinon-connection-v1alpha1-CreateGiteaConnectionRequest) | [CreateGiteaConnectionResponse](#koinon-connection-v1alpha1-CreateGiteaConnectionResponse) | CreateGiteaConnection creates a Gitea connection: a bot account on a Gitea server with a read token and a write token (ADR 0063). Both tokens are stored encrypted and never returned. Admins and owners. |
| SetConnectionSecret | [SetConnectionSecretRequest](#koinon-connection-v1alpha1-SetConnectionSecretRequest) | [SetConnectionSecretResponse](#koinon-connection-v1alpha1-SetConnectionSecretResponse) | SetConnectionSecret replaces the secret fields of a connection entered here: a GitHub App&#39;s private key, or a Gitea connection&#39;s two tokens. It is how a secret lost with its key-encryption key is entered again (ADR 0085 decision 13), and how one is rotated. Every secret field of the connector is required; the other settings stay. It clears the lost mark and the last test result. A declared connection&#39;s secret is changed in its Kubernetes Secret instead (FAILED_PRECONDITION). Audited without the secret. Admins and owners. |
| GetConnection | [GetConnectionRequest](#koinon-connection-v1alpha1-GetConnectionRequest) | [GetConnectionResponse](#koinon-connection-v1alpha1-GetConnectionResponse) | GetConnection returns one connection. Viewers and above. |
| ListConnections | [ListConnectionsRequest](#koinon-connection-v1alpha1-ListConnectionsRequest) | [ListConnectionsResponse](#koinon-connection-v1alpha1-ListConnectionsResponse) | ListConnections returns the org&#39;s connections, oldest first. Viewers and above. |
| TestConnection | [TestConnectionRequest](#koinon-connection-v1alpha1-TestConnectionRequest) | [TestConnectionResponse](#koinon-connection-v1alpha1-TestConnectionResponse) | TestConnection runs the setup guide&#39;s checks against the provider and records the result. Nothing is written to the provider. Admins and owners. |
| DeleteConnection | [DeleteConnectionRequest](#koinon-connection-v1alpha1-DeleteConnectionRequest) | [DeleteConnectionResponse](#koinon-connection-v1alpha1-DeleteConnectionResponse) | DeleteConnection deletes a connection and its stored secret fields for good. A connection entered here can be deleted; a declared one only after its Secret was deleted (removed). While agents are bound to it the call fails with FAILED_PRECONDITION and lists them. Audited. Admins and owners. |
| ListInstallationRepos | [ListInstallationReposRequest](#koinon-connection-v1alpha1-ListInstallationReposRequest) | [ListInstallationReposResponse](#koinon-connection-v1alpha1-ListInstallationReposResponse) | ListInstallationRepos lists the repositories a GitHub connection&#39;s App installation can access, for choosing what to bind an agent to. Koinon asks GitHub with a metadata-read installation token; nothing is written. Admins and owners. |

 



<a name="koinon_leakguard_v1alpha1_leakguard-proto"></a>
<p align="right"><a href="#top">Top</a></p>

## koinon/leakguard/v1alpha1/leakguard.proto



<a name="koinon-leakguard-v1alpha1-AddTrustedHostRequest"></a>

### AddTrustedHostRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| host | [string](#string) |  | host is a bare DNS name such as docs.example.com (exactly that host), or *. and a DNS name such as *.example.com (any subdomain at any depth, not example.com itself). A wildcard over a public suffix is refused. |






<a name="koinon-leakguard-v1alpha1-AddTrustedHostResponse"></a>

### AddTrustedHostResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| leak_guard | [LeakGuard](#koinon-leakguard-v1alpha1-LeakGuard) |  |  |






<a name="koinon-leakguard-v1alpha1-CodeHostScope"></a>

### CodeHostScope
CodeHostScope counts the repositories of the org&#39;s code-host connections by the visibility the
connector last read. The code-host derivation calls a write internal only for a listed repository
that is not public and whose reading is fresh; unknown and stale ones count as external-high.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| repositories | [int32](#int32) |  |  |
| private | [int32](#int32) |  |  |
| internal | [int32](#int32) |  |  |
| public | [int32](#int32) |  |  |
| unknown | [int32](#int32) |  |  |
| stale | [int32](#int32) |  | stale counts the readings older than visibility_max_age_seconds. |
| oldest_checked_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  | oldest_checked_at is the oldest reading; unset for an empty scope. |
| visibility_max_age_seconds | [int32](#int32) |  |  |






<a name="koinon-leakguard-v1alpha1-GetLeakGuardRequest"></a>

### GetLeakGuardRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |






<a name="koinon-leakguard-v1alpha1-GetLeakGuardResponse"></a>

### GetLeakGuardResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| leak_guard | [LeakGuard](#koinon-leakguard-v1alpha1-LeakGuard) |  |  |






<a name="koinon-leakguard-v1alpha1-LeakGuard"></a>

### LeakGuard
LeakGuard is the org&#39;s effective configuration.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| match_threshold | [int32](#int32) |  | match_threshold is the shortest tainted text, in characters after normalisation, that blocks a call. |
| default_match_threshold | [int32](#int32) |  |  |
| min_match_threshold | [int32](#int32) |  |  |
| max_match_threshold | [int32](#int32) |  |  |
| trusted_hosts | [string](#string) | repeated | trusted_hosts is the fetch-trusted host list, sorted; empty for a new org. |
| code_host_scope | [CodeHostScope](#koinon-leakguard-v1alpha1-CodeHostScope) |  | code_host_scope summarises the org&#39;s code-host scope (ADR 0091 decision 3). |






<a name="koinon-leakguard-v1alpha1-RemoveTrustedHostRequest"></a>

### RemoveTrustedHostRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| host | [string](#string) |  |  |






<a name="koinon-leakguard-v1alpha1-RemoveTrustedHostResponse"></a>

### RemoveTrustedHostResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| leak_guard | [LeakGuard](#koinon-leakguard-v1alpha1-LeakGuard) |  |  |






<a name="koinon-leakguard-v1alpha1-SetMatchThresholdRequest"></a>

### SetMatchThresholdRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| match_threshold | [int32](#int32) |  | match_threshold in characters; 0 returns it to the default. |






<a name="koinon-leakguard-v1alpha1-SetMatchThresholdResponse"></a>

### SetMatchThresholdResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| leak_guard | [LeakGuard](#koinon-leakguard-v1alpha1-LeakGuard) |  |  |





 

 

 


<a name="koinon-leakguard-v1alpha1-LeakGuardService"></a>

### LeakGuardService
LeakGuardService manages an org&#39;s leak-guard settings (ADR 0084): the match threshold and the
fetch-trusted host list the automatic URL rule exempts. It is the connect-rpc face of the same
service layer the admin API uses. Admins and owners only. Every change is audited.

| Method Name | Request Type | Response Type | Description |
| ----------- | ------------ | ------------- | ------------|
| GetLeakGuard | [GetLeakGuardRequest](#koinon-leakguard-v1alpha1-GetLeakGuardRequest) | [GetLeakGuardResponse](#koinon-leakguard-v1alpha1-GetLeakGuardResponse) |  |
| SetMatchThreshold | [SetMatchThresholdRequest](#koinon-leakguard-v1alpha1-SetMatchThresholdRequest) | [SetMatchThresholdResponse](#koinon-leakguard-v1alpha1-SetMatchThresholdResponse) | SetMatchThreshold sets the match threshold, or returns it to the default (24) when match_threshold is 0. Audited as org.leak_guard.threshold.set. |
| AddTrustedHost | [AddTrustedHostRequest](#koinon-leakguard-v1alpha1-AddTrustedHostRequest) | [AddTrustedHostResponse](#koinon-leakguard-v1alpha1-AddTrustedHostResponse) | AddTrustedHost puts a host on the fetch-trusted list. A host already on it changes nothing. Audited as org.fetch_trusted_host.add. |
| RemoveTrustedHost | [RemoveTrustedHostRequest](#koinon-leakguard-v1alpha1-RemoveTrustedHostRequest) | [RemoveTrustedHostResponse](#koinon-leakguard-v1alpha1-RemoveTrustedHostResponse) | RemoveTrustedHost takes a host off the list; NOT_FOUND when it was not on it. Audited as org.fetch_trusted_host.remove. |

 



<a name="koinon_mcpserver_v1alpha1_mcpserver-proto"></a>
<p align="right"><a href="#top">Top</a></p>

## koinon/mcpserver/v1alpha1/mcpserver.proto



<a name="koinon-mcpserver-v1alpha1-AcknowledgeToolReviewRequest"></a>

### AcknowledgeToolReviewRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| server_id | [string](#string) |  |  |
| tool | [string](#string) |  |  |






<a name="koinon-mcpserver-v1alpha1-AcknowledgeToolReviewResponse"></a>

### AcknowledgeToolReviewResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| tool | [Tool](#koinon-mcpserver-v1alpha1-Tool) |  |  |






<a name="koinon-mcpserver-v1alpha1-Allowance"></a>

### Allowance
Allowance is a server an agent may use, optionally narrowed to some of its tools.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| server_id | [string](#string) |  |  |
| tools | [string](#string) | repeated | tools narrows the allowance; empty allows the whole server, including tools it adds later (which enter the ask tier). |






<a name="koinon-mcpserver-v1alpha1-CreateServerRequest"></a>

### CreateServerRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| name | [string](#string) |  |  |
| url | [string](#string) |  |  |
| transport | [string](#string) |  | transport defaults to &#34;streamable-http&#34;. |
| credential_header | [string](#string) |  | credential_header defaults to Authorization when credential_value is set. |
| credential_prefix | [string](#string) |  | credential_prefix precedes the secret in the header value, for example &#34;Bearer &#34;. |
| credential_value | [string](#string) |  |  |
| class | [ServerClass](#koinon-mcpserver-v1alpha1-ServerClass) |  | class is what the server exposes; leaving it out is the unclassified default (kind &#34;other&#34;, both legs). |






<a name="koinon-mcpserver-v1alpha1-CreateServerResponse"></a>

### CreateServerResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| server | [Server](#koinon-mcpserver-v1alpha1-Server) |  |  |






<a name="koinon-mcpserver-v1alpha1-DeleteServerRequest"></a>

### DeleteServerRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| server_id | [string](#string) |  |  |






<a name="koinon-mcpserver-v1alpha1-DeleteServerResponse"></a>

### DeleteServerResponse







<a name="koinon-mcpserver-v1alpha1-GetAgentServersRequest"></a>

### GetAgentServersRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| agent_id | [string](#string) |  |  |






<a name="koinon-mcpserver-v1alpha1-GetAgentServersResponse"></a>

### GetAgentServersResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| servers | [Allowance](#koinon-mcpserver-v1alpha1-Allowance) | repeated |  |






<a name="koinon-mcpserver-v1alpha1-ListServersRequest"></a>

### ListServersRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |






<a name="koinon-mcpserver-v1alpha1-ListServersResponse"></a>

### ListServersResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| servers | [Server](#koinon-mcpserver-v1alpha1-Server) | repeated |  |






<a name="koinon-mcpserver-v1alpha1-ListToolsRequest"></a>

### ListToolsRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| server_id | [string](#string) |  |  |






<a name="koinon-mcpserver-v1alpha1-ListToolsResponse"></a>

### ListToolsResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| tools | [Tool](#koinon-mcpserver-v1alpha1-Tool) | repeated |  |






<a name="koinon-mcpserver-v1alpha1-Server"></a>

### Server
Server is a remote MCP server of the org. It never holds the credential.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| id | [string](#string) |  |  |
| name | [string](#string) |  |  |
| url | [string](#string) |  |  |
| transport | [string](#string) |  | transport is &#34;streamable-http&#34; or &#34;sse&#34;. |
| has_credential | [bool](#bool) |  |  |
| credential_header | [string](#string) |  |  |
| credential_prefix | [string](#string) |  |  |
| source | [string](#string) |  | source is &#34;ui&#34; for a server entered here and &#34;secret&#34; for one declared by a Secret. |
| managed_by_secret | [string](#string) |  |  |
| disabled_reason | [string](#string) |  | disabled_reason says why the server is not served, empty when it is. |
| removed | [bool](#bool) |  | removed is set when the declaring Secret was deleted. |
| health | [Health](#koinon-mcpserver-v1alpha1-Health) |  |  |
| health_reason | [string](#string) |  |  |
| health_checked_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| health_since | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| tools_synced_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| created_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| updated_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| gateway_target | [string](#string) |  | gateway_target is the name of the server&#39;s target in the gateway, &#34;&lt;org slug&gt;-&lt;name&gt;&#34;, which is also the prefix of its tools in an agent&#39;s tool list. |
| kind | [string](#string) |  | kind is &#34;fetch&#34;, &#34;search&#34; or &#34;other&#34;; data_class lists the session legs the server&#39;s results can set, in the order untrusted_input, private_data (see ServerClass). A server nobody classified is kind &#34;other&#34; with both legs. |
| data_class | [string](#string) | repeated |  |
| code_host_connection_id | [string](#string) |  | code_host_connection_id is the org&#39;s code-host connection the server acts through, empty for none. The code-host sink class of its tools is looked up in that connection&#39;s repository list only; a server without one is external-high. A changed URL clears it. |
| credential_lost_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  | credential_lost_at is set when the credential was lost with the key that encrypted it (`koinon-api keys forget`, ADR 0085 decision 13): the server is not served until the credential is entered again; has_credential is false and credential_header and credential_prefix are kept, so only the value is asked for. Unset otherwise. |
| admin_disabled | [koinon.outward.v1alpha1.AdminDisabled](#koinon-outward-v1alpha1-AdminDisabled) |  | admin_disabled is set while an admin holds the target off (the outward switch, ADR 0110 decision 5): it is not called, probed or offered to runs. Unset otherwise. |






<a name="koinon-mcpserver-v1alpha1-ServerClass"></a>

### ServerClass
ServerClass is what a server declares it exposes (ADR 0084 decision 8).


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| kind | [string](#string) |  | kind is &#34;fetch&#34; (reads web pages or other untrusted documents), &#34;search&#34; (sends a query to a search provider) or &#34;other&#34;. |
| data_class | [string](#string) | repeated | data_class are the session legs the server&#39;s results can set (ADR 0091), any of &#34;untrusted_input&#34; (returns text an outsider controls) and &#34;private_data&#34; (returns data the org does not want to leave). A server may carry both; none is allowed. Where a call goes is not a server leg: it is the sink class of each tool (ToolSink). |
| code_host_connection_id | [string](#string) |  | code_host_connection_id links the server to one of the org&#39;s code-host connections (see Server). Empty means none. |






<a name="koinon-mcpserver-v1alpha1-SetAgentServersRequest"></a>

### SetAgentServersRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| agent_id | [string](#string) |  |  |
| servers | [Allowance](#koinon-mcpserver-v1alpha1-Allowance) | repeated |  |






<a name="koinon-mcpserver-v1alpha1-SetAgentServersResponse"></a>

### SetAgentServersResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| servers | [Allowance](#koinon-mcpserver-v1alpha1-Allowance) | repeated |  |






<a name="koinon-mcpserver-v1alpha1-SetServerClassRequest"></a>

### SetServerClassRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| server_id | [string](#string) |  |  |
| class | [ServerClass](#koinon-mcpserver-v1alpha1-ServerClass) |  |  |






<a name="koinon-mcpserver-v1alpha1-SetServerClassResponse"></a>

### SetServerClassResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| server | [Server](#koinon-mcpserver-v1alpha1-Server) |  |  |






<a name="koinon-mcpserver-v1alpha1-SetToolSinkRequest"></a>

### SetToolSinkRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| server_id | [string](#string) |  |  |
| tool | [string](#string) |  |  |
| sink | [ToolSink](#koinon-mcpserver-v1alpha1-ToolSink) |  |  |
| content_hash | [string](#string) |  | content_hash is the Tool.content_hash the admin reviewed. Required. |






<a name="koinon-mcpserver-v1alpha1-SetToolSinkResponse"></a>

### SetToolSinkResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| tool | [Tool](#koinon-mcpserver-v1alpha1-Tool) |  |  |






<a name="koinon-mcpserver-v1alpha1-SetToolTierRequest"></a>

### SetToolTierRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| server_id | [string](#string) |  |  |
| tool | [string](#string) |  |  |
| tier | [Tier](#koinon-mcpserver-v1alpha1-Tier) |  |  |






<a name="koinon-mcpserver-v1alpha1-SetToolTierResponse"></a>

### SetToolTierResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| tool | [Tool](#koinon-mcpserver-v1alpha1-Tool) |  |  |






<a name="koinon-mcpserver-v1alpha1-SetToolTwoApproversRequest"></a>

### SetToolTwoApproversRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| server_id | [string](#string) |  |  |
| tool | [string](#string) |  |  |
| required | [bool](#bool) |  |  |






<a name="koinon-mcpserver-v1alpha1-SetToolTwoApproversResponse"></a>

### SetToolTwoApproversResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| tool | [Tool](#koinon-mcpserver-v1alpha1-Tool) |  |  |






<a name="koinon-mcpserver-v1alpha1-Tool"></a>

### Tool



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| server_id | [string](#string) |  |  |
| name | [string](#string) |  |  |
| description | [string](#string) |  |  |
| read_only_hint | [bool](#bool) |  | read_only_hint is the server&#39;s own readOnlyHint annotation: a suggestion for the admin, who decides the tier. |
| tier | [Tier](#koinon-mcpserver-v1alpha1-Tier) |  |  |
| review_reason | [string](#string) |  | review_reason is set while the tool&#39;s description or input schema changed since an admin last reviewed it. It does not block the tool. |
| present | [bool](#bool) |  | present is false once the server stopped listing the tool. |
| first_seen_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| last_seen_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| tier_set_by | [string](#string) |  |  |
| tier_set_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| requires_two_approvers | [bool](#bool) |  | requires_two_approvers: a call in the ask tier needs two approvers. |
| content_hash | [string](#string) |  | content_hash identifies the description and input schema as last published. |
| unavailable | [string](#string) |  | unavailable says why agents are not offered the tool (&#34;unavailable: name too long ...&#34;); empty when they are. |
| description_original_length | [int64](#int64) |  | description is the sanitised text (ADR 0057 amendment, #52). description_original_length is the length in characters of the description as the server published it; description_capped says it, or a description in the input schema, was cut to its cap; description_cleaned says control, invisible or tag characters were removed or harness-lookalike tags escaped. |
| description_capped | [bool](#bool) |  |  |
| description_cleaned | [bool](#bool) |  |  |
| sink | [ToolSink](#koinon-mcpserver-v1alpha1-ToolSink) |  | sink is the tool&#39;s declaration for the leak guard (ADR 0091). A tool nobody declared is external-high with the server&#39;s legs. |






<a name="koinon-mcpserver-v1alpha1-ToolSink"></a>

### ToolSink
ToolSink is what a tool declares about its calls (ADR 0091 decision 2).


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| sink_class | [string](#string) |  | sink_class is &#34;none&#34;, &#34;internal&#34;, &#34;external-low&#34; or &#34;external-high&#34;: the fixed class of the tool. For a derived tool it is the class of a call whose address arguments do not parse, always &#34;external-high&#34;. |
| derivation | [string](#string) |  | derivation is &#34;fixed&#34; (use sink_class), &#34;code_host&#34; (internal only for a non-public repository of the org&#39;s code-host scope; address_args are the owner and repo arguments, in that order) or &#34;fetch&#34; (internal for a fetch-trusted host, otherwise external-low; address_args is the url argument). |
| address_args | [string](#string) | repeated | address_args name the arguments that say where a call goes. Every other argument is payload. |
| result_untrusted_input | [bool](#bool) | optional | result_untrusted_input and result_private_data say what the tool&#39;s results carry for the session legs. Unset is the server&#39;s own leg. |
| result_private_data | [bool](#bool) | optional |  |






<a name="koinon-mcpserver-v1alpha1-UpdateServerRequest"></a>

### UpdateServerRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| server_id | [string](#string) |  |  |
| url | [string](#string) | optional | Fields left unset stay. |
| transport | [string](#string) | optional |  |
| credential_header | [string](#string) | optional | Setting any of the credential fields replaces the credential&#39;s header and prefix; credential_value replaces the secret, and leaving it out keeps the stored one. |
| credential_prefix | [string](#string) | optional |  |
| credential_value | [string](#string) | optional |  |
| remove_credential | [bool](#bool) |  |  |






<a name="koinon-mcpserver-v1alpha1-UpdateServerResponse"></a>

### UpdateServerResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| server | [Server](#koinon-mcpserver-v1alpha1-Server) |  |  |





 


<a name="koinon-mcpserver-v1alpha1-Health"></a>

### Health


| Name | Number | Description |
| ---- | ------ | ----------- |
| HEALTH_UNSPECIFIED | 0 |  |
| HEALTH_UNKNOWN | 1 |  |
| HEALTH_HEALTHY | 2 |  |
| HEALTH_UNHEALTHY | 3 | UNHEALTHY: three probes in a row failed; the server&#39;s tools are withdrawn from agents. |



<a name="koinon-mcpserver-v1alpha1-Tier"></a>

### Tier


| Name | Number | Description |
| ---- | ------ | ----------- |
| TIER_UNSPECIFIED | 0 |  |
| TIER_ASK | 1 | ASK: a human approves each call (ADR 0008, 0042). Every new tool starts here. |
| TIER_AUTO | 2 | AUTO: the agent calls the tool directly. |
| TIER_NEVER | 3 | NEVER: no agent may call the tool. |


 

 


<a name="koinon-mcpserver-v1alpha1-McpServerService"></a>

### McpServerService
McpServerService manages an org&#39;s remote MCP servers (ADR 0057) for the UI. It is the connect-rpc
face of the same service layer the admin API uses, so validation, the upstream URL policy and the
audit trail are the same. Admins and owners only: members do not see servers.

Servers declared by a labelled Kubernetes Secret (source &#34;secret&#34;) are read-only: changing or
deleting a live one fails with FAILED_PRECONDITION and names the Secret. Their tools&#39; tiers can still
be set here. Credentials are write-only; they are never returned.

| Method Name | Request Type | Response Type | Description |
| ----------- | ------------ | ------------- | ------------|
| ListServers | [ListServersRequest](#koinon-mcpserver-v1alpha1-ListServersRequest) | [ListServersResponse](#koinon-mcpserver-v1alpha1-ListServersResponse) |  |
| CreateServer | [CreateServerRequest](#koinon-mcpserver-v1alpha1-CreateServerRequest) | [CreateServerResponse](#koinon-mcpserver-v1alpha1-CreateServerResponse) | CreateServer adds a server. A server that asks each user to sign in with OAuth is refused with INVALID_ARGUMENT (planned for v0.2). |
| UpdateServer | [UpdateServerRequest](#koinon-mcpserver-v1alpha1-UpdateServerRequest) | [UpdateServerResponse](#koinon-mcpserver-v1alpha1-UpdateServerResponse) |  |
| SetServerClass | [SetServerClassRequest](#koinon-mcpserver-v1alpha1-SetServerClassRequest) | [SetServerClassResponse](#koinon-mcpserver-v1alpha1-SetServerClassResponse) | SetServerClass sets what a server exposes: its kind and the legs of the leak guard (ADR 0084) it carries. It works for declared servers too, which the declaring Secret does not describe. Audited as mcp_server.data_class.set. |
| DeleteServer | [DeleteServerRequest](#koinon-mcpserver-v1alpha1-DeleteServerRequest) | [DeleteServerResponse](#koinon-mcpserver-v1alpha1-DeleteServerResponse) | DeleteServer deletes a server entered here, with its tools and the agents&#39; allowances of it. A declared server can be deleted only after its Secret was deleted (removed). |
| ListTools | [ListToolsRequest](#koinon-mcpserver-v1alpha1-ListToolsRequest) | [ListToolsResponse](#koinon-mcpserver-v1alpha1-ListToolsResponse) | ListTools returns the tools of one server, or of all the org&#39;s servers when server_id is empty. |
| SetToolTier | [SetToolTierRequest](#koinon-mcpserver-v1alpha1-SetToolTierRequest) | [SetToolTierResponse](#koinon-mcpserver-v1alpha1-SetToolTierResponse) | SetToolTier moves a tool to a tier and clears its review flag. |
| SetToolTwoApprovers | [SetToolTwoApproversRequest](#koinon-mcpserver-v1alpha1-SetToolTwoApproversRequest) | [SetToolTwoApproversResponse](#koinon-mcpserver-v1alpha1-SetToolTwoApproversResponse) | SetToolTwoApprovers turns on or off the requirement that a call to the tool in the ask tier needs two approvers: distinct admins or owners, neither the ticket&#39;s creator. Off by default. |
| SetToolSink | [SetToolSinkRequest](#koinon-mcpserver-v1alpha1-SetToolSinkRequest) | [SetToolSinkResponse](#koinon-mcpserver-v1alpha1-SetToolSinkResponse) | SetToolSink declares a tool&#39;s sink class, address arguments and result legs (ADR 0091). The request names the content hash of the tool the admin reviewed; if the server republished the tool since, the call fails with ABORTED (HTTP 409) and nothing changes. Audited as mcp_tool.declaration.set. A changed server URL or tool content hash puts the declaration back to the default, audited as mcp_tool.declaration.reset. |
| AcknowledgeToolReview | [AcknowledgeToolReviewRequest](#koinon-mcpserver-v1alpha1-AcknowledgeToolReviewRequest) | [AcknowledgeToolReviewResponse](#koinon-mcpserver-v1alpha1-AcknowledgeToolReviewResponse) | AcknowledgeToolReview clears a tool&#39;s review flag without changing its tier. |
| GetAgentServers | [GetAgentServersRequest](#koinon-mcpserver-v1alpha1-GetAgentServersRequest) | [GetAgentServersResponse](#koinon-mcpserver-v1alpha1-GetAgentServersResponse) | GetAgentServers returns the servers an agent may use. |
| SetAgentServers | [SetAgentServersRequest](#koinon-mcpserver-v1alpha1-SetAgentServersRequest) | [SetAgentServersResponse](#koinon-mcpserver-v1alpha1-SetAgentServersResponse) | SetAgentServers replaces the servers an agent may use. |

 



<a name="koinon_orgsettings_v1alpha1_orgsettings-proto"></a>
<p align="right"><a href="#top">Top</a></p>

## koinon/orgsettings/v1alpha1/orgsettings.proto



<a name="koinon-orgsettings-v1alpha1-GetHomeBackupRetentionRequest"></a>

### GetHomeBackupRetentionRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |






<a name="koinon-orgsettings-v1alpha1-GetHomeBackupRetentionResponse"></a>

### GetHomeBackupRetentionResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| retention | [HomeBackupRetention](#koinon-orgsettings-v1alpha1-HomeBackupRetention) |  |  |






<a name="koinon-orgsettings-v1alpha1-GetRunContentRetentionRequest"></a>

### GetRunContentRetentionRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |






<a name="koinon-orgsettings-v1alpha1-GetRunContentRetentionResponse"></a>

### GetRunContentRetentionResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| retention | [RunContentRetention](#koinon-orgsettings-v1alpha1-RunContentRetention) |  |  |






<a name="koinon-orgsettings-v1alpha1-HomeBackupRetention"></a>

### HomeBackupRetention
HomeBackupRetention is the org&#39;s effective retention of old home backup repositories. A key change
starts a new backup repository for each agent&#39;s home; the old one stays readable for the
checkpoints written to it for days, then it is deleted. The deadline is set when the new repository
starts, so a change applies to later key changes only.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| days | [int32](#int32) |  |  |
| default_days | [int32](#int32) |  |  |
| min_days | [int32](#int32) |  |  |
| max_days | [int32](#int32) |  |  |






<a name="koinon-orgsettings-v1alpha1-RunContentRetention"></a>

### RunContentRetention
RunContentRetention is the org&#39;s effective retention of stored run content. Once a finished run is
older than days, the purge job blanks the payload of its content events (messages, thoughts and tool
calls) and strips the text from prompt, permission and refusal events, whose state fields stay. Other
events, the run&#39;s status, traces and the spend ledger stay.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| days | [int32](#int32) |  |  |
| default_days | [int32](#int32) |  |  |
| min_days | [int32](#int32) |  |  |
| max_days | [int32](#int32) |  |  |






<a name="koinon-orgsettings-v1alpha1-SetHomeBackupRetentionRequest"></a>

### SetHomeBackupRetentionRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| days | [int32](#int32) |  | days to keep an old home backup repository; 0 returns it to the default. |






<a name="koinon-orgsettings-v1alpha1-SetHomeBackupRetentionResponse"></a>

### SetHomeBackupRetentionResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| retention | [HomeBackupRetention](#koinon-orgsettings-v1alpha1-HomeBackupRetention) |  |  |






<a name="koinon-orgsettings-v1alpha1-SetRunContentRetentionRequest"></a>

### SetRunContentRetentionRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| days | [int32](#int32) |  | days to keep a finished run&#39;s content; 0 returns it to the default. |






<a name="koinon-orgsettings-v1alpha1-SetRunContentRetentionResponse"></a>

### SetRunContentRetentionResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| retention | [RunContentRetention](#koinon-orgsettings-v1alpha1-RunContentRetention) |  |  |





 

 

 


<a name="koinon-orgsettings-v1alpha1-OrgSettingsService"></a>

### OrgSettingsService
OrgSettingsService is the UI&#39;s face of the org&#39;s settings that are not owned by another service
(leak guard and trace settings have their own). It is the connect-rpc face of the same service
layer the admin API uses. Admins and owners only. Every change is audited.

| Method Name | Request Type | Response Type | Description |
| ----------- | ------------ | ------------- | ------------|
| GetRunContentRetention | [GetRunContentRetentionRequest](#koinon-orgsettings-v1alpha1-GetRunContentRetentionRequest) | [GetRunContentRetentionResponse](#koinon-orgsettings-v1alpha1-GetRunContentRetentionResponse) |  |
| SetRunContentRetention | [SetRunContentRetentionRequest](#koinon-orgsettings-v1alpha1-SetRunContentRetentionRequest) | [SetRunContentRetentionResponse](#koinon-orgsettings-v1alpha1-SetRunContentRetentionResponse) | SetRunContentRetention sets how long a finished run&#39;s stored content is kept, or returns it to the default (30 days) when days is 0. Audited as org.run_content_retention.set. |
| GetHomeBackupRetention | [GetHomeBackupRetentionRequest](#koinon-orgsettings-v1alpha1-GetHomeBackupRetentionRequest) | [GetHomeBackupRetentionResponse](#koinon-orgsettings-v1alpha1-GetHomeBackupRetentionResponse) |  |
| SetHomeBackupRetention | [SetHomeBackupRetentionRequest](#koinon-orgsettings-v1alpha1-SetHomeBackupRetentionRequest) | [SetHomeBackupRetentionResponse](#koinon-orgsettings-v1alpha1-SetHomeBackupRetentionResponse) | SetHomeBackupRetention sets how long an old home backup repository is kept after a key change, or returns it to the default (7 days) when days is 0. Audited as org.home_backup_retention.set. |

 



<a name="koinon_platform_v1alpha1_platform-proto"></a>
<p align="right"><a href="#top">Top</a></p>

## koinon/platform/v1alpha1/platform.proto


 

 

 


<a name="koinon-platform-v1alpha1-PlatformService"></a>

### PlatformService
PlatformService is the first Koinon API surface. It has no methods yet;
it exists so code generation and `buf breaking` run from day one.

| Method Name | Request Type | Response Type | Description |
| ----------- | ------------ | ------------- | ------------|

 



<a name="koinon_provider_v1alpha1_provider-proto"></a>
<p align="right"><a href="#top">Top</a></p>

## koinon/provider/v1alpha1/provider.proto



<a name="koinon-provider-v1alpha1-CheckProviderEchoRequest"></a>

### CheckProviderEchoRequest
CheckProviderEchoRequest names the provider and one of its models.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| provider_id | [string](#string) |  |  |
| model | [string](#string) |  | A model of the provider that the org serves and has a price for. |






<a name="koinon-provider-v1alpha1-CheckProviderEchoResponse"></a>

### CheckProviderEchoResponse
CheckProviderEchoResponse holds the result.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| echo_check | [EchoCheck](#koinon-provider-v1alpha1-EchoCheck) |  |  |






<a name="koinon-provider-v1alpha1-CreateModelRequest"></a>

### CreateModelRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| provider_id | [string](#string) |  |  |
| name | [string](#string) |  |  |
| guard | [bool](#bool) |  |  |
| rates | [Rates](#koinon-provider-v1alpha1-Rates) |  | **Deprecated.** rates is deprecated: a request that sets it fails with INVALID_ARGUMENT. Add a price with PriceService.SetPrice instead. |
| format | [string](#string) |  | format is the model&#39;s wire format, within what the provider offers. Empty means the default (ADR 0069); a value marks the model as overridden. |






<a name="koinon-provider-v1alpha1-CreateModelResponse"></a>

### CreateModelResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| model | [Model](#koinon-provider-v1alpha1-Model) |  |  |
| warnings | [string](#string) | repeated | warnings are non-fatal notes, for example that the model runs without prompt caching because Koinon has no cache mode for its provider format (ADR 0068). |






<a name="koinon-provider-v1alpha1-CreateProviderRequest"></a>

### CreateProviderRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| name | [string](#string) |  |  |
| type | [string](#string) |  |  |
| url | [string](#string) |  |  |
| api_key | [string](#string) |  | api_key is empty for keyless upstreams such as Ollama. |
| anthropic_workspace_id | [string](#string) |  | anthropic_workspace_id is allowed for type &#34;anthropic&#34; only: wrkspc_ followed by letters and digits. |
| cost_api_key | [string](#string) |  | cost_api_key is the optional cost API key (an Anthropic or OpenAI admin key that may read the cost API; write-only, never returned). anthropic and openai providers at their own API URL only; needs cost_scope. |
| cost_scope | [string](#string) |  | cost_scope is the cost scope of that key (see Provider.cost_scope). |






<a name="koinon-provider-v1alpha1-CreateProviderResponse"></a>

### CreateProviderResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| provider | [Provider](#koinon-provider-v1alpha1-Provider) |  |  |






<a name="koinon-provider-v1alpha1-DefaultModelChoice"></a>

### DefaultModelChoice
DefaultModelChoice is a model the org serves and has a price for.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| name | [string](#string) |  |  |
| provider | [string](#string) |  |  |






<a name="koinon-provider-v1alpha1-DeleteModelRequest"></a>

### DeleteModelRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| model_id | [string](#string) |  |  |






<a name="koinon-provider-v1alpha1-DeleteModelResponse"></a>

### DeleteModelResponse







<a name="koinon-provider-v1alpha1-DeleteProviderRequest"></a>

### DeleteProviderRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| provider_id | [string](#string) |  |  |






<a name="koinon-provider-v1alpha1-DeleteProviderResponse"></a>

### DeleteProviderResponse







<a name="koinon-provider-v1alpha1-EchoCheck"></a>

### EchoCheck
EchoCheck is the result of an echo check (CheckProviderEcho).


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| model | [string](#string) |  | The model the check used. |
| unchanged | [bool](#bool) |  | True when the reply was the line, character for character. |
| sent | [string](#string) |  | The line sent and the reply received. |
| received | [string](#string) |  |  |
| diff | [EchoDiffSegment](#koinon-provider-v1alpha1-EchoDiffSegment) | repeated | The difference between them, in order; every segment of an unchanged result is EQUAL. |
| checked_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |






<a name="koinon-provider-v1alpha1-EchoDiffSegment"></a>

### EchoDiffSegment
EchoDiffSegment is a run of characters in the sent line, the reply or both.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| op | [EchoDiffSegment.Op](#koinon-provider-v1alpha1-EchoDiffSegment-Op) |  |  |
| text | [string](#string) |  |  |






<a name="koinon-provider-v1alpha1-GetDefaultModelRequest"></a>

### GetDefaultModelRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |






<a name="koinon-provider-v1alpha1-GetDefaultModelResponse"></a>

### GetDefaultModelResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| model | [string](#string) |  | model is the org&#39;s default model; empty when the org has none. |
| available | [DefaultModelChoice](#koinon-provider-v1alpha1-DefaultModelChoice) | repeated | available are the models the default may be set to. |






<a name="koinon-provider-v1alpha1-ListModelsRequest"></a>

### ListModelsRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |






<a name="koinon-provider-v1alpha1-ListModelsResponse"></a>

### ListModelsResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| models | [Model](#koinon-provider-v1alpha1-Model) | repeated |  |






<a name="koinon-provider-v1alpha1-ListProvidersRequest"></a>

### ListProvidersRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |






<a name="koinon-provider-v1alpha1-ListProvidersResponse"></a>

### ListProvidersResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| providers | [Provider](#koinon-provider-v1alpha1-Provider) | repeated |  |






<a name="koinon-provider-v1alpha1-Model"></a>

### Model



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| id | [string](#string) |  |  |
| provider_id | [string](#string) |  |  |
| name | [string](#string) |  | name is the plain name agents ask for; the gateway serves it as &#34;&lt;org slug&gt;/&lt;name&gt;&#34;. |
| guard | [bool](#bool) |  | guard is the routing guard, for OpenRouter providers only. |
| rates | [Rates](#koinon-provider-v1alpha1-Rates) |  | **Deprecated.** rates is deprecated and never set; prices are org price entries (PriceService). |
| source | [string](#string) |  |  |
| managed_by_secret | [string](#string) |  |  |
| disabled_reason | [string](#string) |  |  |
| created_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| updated_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| format | [string](#string) |  | format is the model&#39;s wire format: &#34;openai&#34; (chat completions) or &#34;anthropic&#34; (Messages) (ADR 0069). |
| format_overridden | [bool](#bool) |  | format_overridden is set when an admin chose the format; otherwise it follows the default and is re-derived when the default changes. |






<a name="koinon-provider-v1alpha1-ModelFormat"></a>

### ModelFormat
ModelFormat is a model wire format as the server&#39;s format table names it: id is the stored value,
label how it reads to a person. The set is open and grows with the formats Koinon supports.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| id | [string](#string) |  |  |
| label | [string](#string) |  |  |






<a name="koinon-provider-v1alpha1-ModelFormatsRequest"></a>

### ModelFormatsRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| provider_id | [string](#string) |  |  |
| name | [string](#string) |  | name is the model name; the default depends on its family. |






<a name="koinon-provider-v1alpha1-ModelFormatsResponse"></a>

### ModelFormatsResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| default_format | [string](#string) |  | default_format is the id of the format the model gets unless overridden. The formats the provider offers are Provider.formats. |






<a name="koinon-provider-v1alpha1-Provider"></a>

### Provider
Provider is an upstream model API of the org. It never holds the key.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| id | [string](#string) |  |  |
| name | [string](#string) |  |  |
| type | [string](#string) |  | type is the agentgateway provider name, such as openai, openrouter or ollama. |
| url | [string](#string) |  |  |
| has_key | [bool](#bool) |  | has_key is whether an API key is stored. |
| source | [string](#string) |  | source is &#34;ui&#34; for a provider entered here and &#34;secret&#34; for one declared by a Secret. |
| managed_by_secret | [string](#string) |  |  |
| disabled_reason | [string](#string) |  | disabled_reason says why the provider is not served, empty when it is. |
| removed | [bool](#bool) |  | removed is set when the declaring Secret was deleted. |
| created_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| updated_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| anthropic_workspace_id | [string](#string) |  | anthropic_workspace_id is the Anthropic workspace the gateway names for the provider&#39;s models, for an API key not scoped to a workspace. Empty when unset. An identifier, not a credential. |
| formats | [ModelFormat](#koinon-provider-v1alpha1-ModelFormat) | repeated | formats are the model formats the provider&#39;s type offers, preferred first (ADR 0069). |
| echo_check | [EchoCheck](#koinon-provider-v1alpha1-EchoCheck) |  | echo_check is the latest echo check of the provider; unset when none ran. |
| credential_lost_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  | credential_lost_at is set when the API key was lost with the key that encrypted it (`koinon-api keys forget`, ADR 0085 decision 13): the provider is not served until the key is entered again; has_key is false. Unset otherwise. |
| has_cost_key | [bool](#bool) |  | has_cost_key is whether a cost API key is stored: an admin key with which koinon-reconciler reads the provider&#39;s cost API once a day and records the ledger&#39;s difference from it (ADR 0056, amendment 2026-11-06). anthropic and openai providers at their own API URL only. |
| cost_scope | [string](#string) |  | cost_scope is the part of the provider&#39;s account whose cost is this provider key&#39;s: the Anthropic workspace ID (&#34;default&#34; for the default workspace), or the OpenAI project ID or API key ID (key_..., the key&#39;s ID, never the key). |
| cost_key_lost_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  | cost_key_lost_at is set when the cost API key was lost with the key that encrypted it; it must be entered again. Unset otherwise. |
| admin_disabled | [koinon.outward.v1alpha1.AdminDisabled](#koinon-outward-v1alpha1-AdminDisabled) |  | admin_disabled is set while an admin holds the target off (the outward switch, ADR 0110 decision 5): it is not called, probed or offered to runs. Unset otherwise. |






<a name="koinon-provider-v1alpha1-ProviderTypeInfoRequest"></a>

### ProviderTypeInfoRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| type | [string](#string) |  |  |






<a name="koinon-provider-v1alpha1-ProviderTypeInfoResponse"></a>

### ProviderTypeInfoResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| formats | [ModelFormat](#koinon-provider-v1alpha1-ModelFormat) | repeated | formats are the model formats the type offers, preferred first. |
| takes_workspace_id | [bool](#bool) |  | takes_workspace_id is set when the type takes an Anthropic workspace ID. |
| takes_cost_key | [bool](#bool) |  | takes_cost_key is set when Koinon reads the type&#39;s cost API (anthropic and openai, at their own API URL), so the provider takes a cost API key and a cost scope. |






<a name="koinon-provider-v1alpha1-Rates"></a>

### Rates
Rates are deprecated: per-model rates were folded into org prices (PriceService, ADR 0056). The
message stays for wire compatibility; the server never fills it and refuses requests that set it.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| input | [string](#string) |  |  |
| output | [string](#string) |  |  |
| cache_read | [string](#string) |  |  |






<a name="koinon-provider-v1alpha1-SetDefaultModelRequest"></a>

### SetDefaultModelRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| model | [string](#string) |  | model is the new default model; empty clears the default. |






<a name="koinon-provider-v1alpha1-SetDefaultModelResponse"></a>

### SetDefaultModelResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| model | [string](#string) |  | model is the org&#39;s default model after the change; empty when it has none. |






<a name="koinon-provider-v1alpha1-UpdateModelRequest"></a>

### UpdateModelRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| model_id | [string](#string) |  |  |
| guard | [bool](#bool) | optional |  |
| rates | [Rates](#koinon-provider-v1alpha1-Rates) |  | **Deprecated.** rates and clear_rates are deprecated: a request that sets either fails with INVALID_ARGUMENT. |
| clear_rates | [bool](#bool) |  | **Deprecated.**  |
| format | [string](#string) | optional | format overrides the model&#39;s wire format, within what the provider offers; set to &#34;&#34; it goes back to the default and is no longer overridden. |






<a name="koinon-provider-v1alpha1-UpdateModelResponse"></a>

### UpdateModelResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| model | [Model](#koinon-provider-v1alpha1-Model) |  |  |






<a name="koinon-provider-v1alpha1-UpdateProviderRequest"></a>

### UpdateProviderRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| provider_id | [string](#string) |  |  |
| type | [string](#string) | optional | Fields left unset stay. |
| url | [string](#string) | optional |  |
| api_key | [string](#string) | optional | api_key replaces the key; set to &#34;&#34; it removes the key. |
| anthropic_workspace_id | [string](#string) | optional | anthropic_workspace_id replaces the workspace ID; set to &#34;&#34; it removes it. |
| cost_api_key | [string](#string) | optional | cost_api_key replaces the cost API key (an Anthropic or OpenAI admin key that may read the cost API; write-only, never returned); set to &#34;&#34; it removes the key and its scope. |
| cost_scope | [string](#string) | optional | cost_scope replaces the cost scope; required with a cost API key (see Provider.cost_scope). |






<a name="koinon-provider-v1alpha1-UpdateProviderResponse"></a>

### UpdateProviderResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| provider | [Provider](#koinon-provider-v1alpha1-Provider) |  |  |





 


<a name="koinon-provider-v1alpha1-EchoDiffSegment-Op"></a>

### EchoDiffSegment.Op


| Name | Number | Description |
| ---- | ------ | ----------- |
| OP_UNSPECIFIED | 0 |  |
| OP_EQUAL | 1 | In both. |
| OP_DELETED | 2 | In the line sent but not in the reply: the provider removed or replaced it. |
| OP_INSERTED | 3 | In the reply but not in the line sent. |


 

 


<a name="koinon-provider-v1alpha1-ProviderService"></a>

### ProviderService
ProviderService manages an org&#39;s model providers and models (ADR 0056) for the UI. It is the
connect-rpc face of the same service layer the admin API uses, so validation, the upstream URL
policy and the audit trail are the same. Admins and owners only: members do not see providers.

Entries declared by a labelled Kubernetes Secret (source &#34;secret&#34;) are read-only: changing or
deleting a live one fails with FAILED_PRECONDITION and names the Secret. Provider keys are write-only;
they are never returned.

| Method Name | Request Type | Response Type | Description |
| ----------- | ------------ | ------------- | ------------|
| ListProviders | [ListProvidersRequest](#koinon-provider-v1alpha1-ListProvidersRequest) | [ListProvidersResponse](#koinon-provider-v1alpha1-ListProvidersResponse) |  |
| CreateProvider | [CreateProviderRequest](#koinon-provider-v1alpha1-CreateProviderRequest) | [CreateProviderResponse](#koinon-provider-v1alpha1-CreateProviderResponse) |  |
| UpdateProvider | [UpdateProviderRequest](#koinon-provider-v1alpha1-UpdateProviderRequest) | [UpdateProviderResponse](#koinon-provider-v1alpha1-UpdateProviderResponse) |  |
| DeleteProvider | [DeleteProviderRequest](#koinon-provider-v1alpha1-DeleteProviderRequest) | [DeleteProviderResponse](#koinon-provider-v1alpha1-DeleteProviderResponse) | DeleteProvider deletes a provider entered here, with its models. A declared provider can be deleted only after its Secret was deleted (removed). |
| ListModels | [ListModelsRequest](#koinon-provider-v1alpha1-ListModelsRequest) | [ListModelsResponse](#koinon-provider-v1alpha1-ListModelsResponse) |  |
| CreateModel | [CreateModelRequest](#koinon-provider-v1alpha1-CreateModelRequest) | [CreateModelResponse](#koinon-provider-v1alpha1-CreateModelResponse) |  |
| UpdateModel | [UpdateModelRequest](#koinon-provider-v1alpha1-UpdateModelRequest) | [UpdateModelResponse](#koinon-provider-v1alpha1-UpdateModelResponse) |  |
| DeleteModel | [DeleteModelRequest](#koinon-provider-v1alpha1-DeleteModelRequest) | [DeleteModelResponse](#koinon-provider-v1alpha1-DeleteModelResponse) |  |
| ModelFormats | [ModelFormatsRequest](#koinon-provider-v1alpha1-ModelFormatsRequest) | [ModelFormatsResponse](#koinon-provider-v1alpha1-ModelFormatsResponse) | ModelFormats says which formats a model may use on a provider and which one it gets by default (ADR 0069), so the model form offers exactly what the server will accept. |
| GetDefaultModel | [GetDefaultModelRequest](#koinon-provider-v1alpha1-GetDefaultModelRequest) | [GetDefaultModelResponse](#koinon-provider-v1alpha1-GetDefaultModelResponse) | GetDefaultModel returns the org&#39;s default model (ADR 0086), which agents without a model setting run on, and the models it may be set to: the ones the org serves and has a price for. |
| SetDefaultModel | [SetDefaultModelRequest](#koinon-provider-v1alpha1-SetDefaultModelRequest) | [SetDefaultModelResponse](#koinon-provider-v1alpha1-SetDefaultModelResponse) | SetDefaultModel sets the org&#39;s default model, or clears it with an empty model. The model must be one the org serves and has a price for. Audited as org.default_model.set. |
| CheckProviderEcho | [CheckProviderEchoRequest](#koinon-provider-v1alpha1-CheckProviderEchoRequest) | [CheckProviderEchoResponse](#koinon-provider-v1alpha1-CheckProviderEchoResponse) | CheckProviderEcho runs the echo check on one model of the provider: one tiny chat request through the gateway asking the model to repeat a fixed line exactly, to catch masking of names and addresses on the provider&#39;s side. It is metered like any request, runs only when called and stores the result on the provider. Admins and owners only. |
| ProviderTypeInfo | [ProviderTypeInfoRequest](#koinon-provider-v1alpha1-ProviderTypeInfoRequest) | [ProviderTypeInfoResponse](#koinon-provider-v1alpha1-ProviderTypeInfoResponse) | ProviderTypeInfo says what a provider type offers before any provider of that type exists, so the provider form shows only the fields the server will accept. |

 



<a name="koinon_question_v1alpha1_question-proto"></a>
<p align="right"><a href="#top">Top</a></p>

## koinon/question/v1alpha1/question.proto



<a name="koinon-question-v1alpha1-AnswerQuestionRequest"></a>

### AnswerQuestionRequest
AnswerQuestionRequest carries exactly one of choice, accept_draft or text.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| question_id | [string](#string) |  |  |
| choice | [string](#string) |  | One of the question&#39;s choices, verbatim. |
| draft | [string](#string) |  | The draft, possibly edited by the person: send the text to use. An empty text means the draft as offered. |
| text | [string](#string) |  |  |






<a name="koinon-question-v1alpha1-AnswerQuestionResponse"></a>

### AnswerQuestionResponse
AnswerQuestionResponse holds the resolved question.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| question | [Question](#koinon-question-v1alpha1-Question) |  |  |






<a name="koinon-question-v1alpha1-AskableMember"></a>

### AskableMember
AskableMember is a member who can answer questions.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| user_id | [string](#string) |  |  |
| email | [string](#string) |  |  |
| display_name | [string](#string) |  |  |
| role | [koinon.auth.v1alpha1.OrgRole](#koinon-auth-v1alpha1-OrgRole) |  |  |






<a name="koinon-question-v1alpha1-DismissQuestionRequest"></a>

### DismissQuestionRequest
DismissQuestionRequest names the question.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| question_id | [string](#string) |  |  |






<a name="koinon-question-v1alpha1-DismissQuestionResponse"></a>

### DismissQuestionResponse
DismissQuestionResponse holds the resolved question.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| question | [Question](#koinon-question-v1alpha1-Question) |  |  |






<a name="koinon-question-v1alpha1-ListAskableMembersRequest"></a>

### ListAskableMembersRequest
ListAskableMembersRequest names the org.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |






<a name="koinon-question-v1alpha1-ListAskableMembersResponse"></a>

### ListAskableMembersResponse
ListAskableMembersResponse lists them.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| members | [AskableMember](#koinon-question-v1alpha1-AskableMember) | repeated |  |






<a name="koinon-question-v1alpha1-ListQuestionsRequest"></a>

### ListQuestionsRequest
ListQuestionsRequest names the org and optionally a task.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| task_id | [string](#string) |  | Empty lists the caller&#39;s open questions in the org. |






<a name="koinon-question-v1alpha1-ListQuestionsResponse"></a>

### ListQuestionsResponse
ListQuestionsResponse lists questions.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| questions | [Question](#koinon-question-v1alpha1-Question) | repeated |  |






<a name="koinon-question-v1alpha1-Question"></a>

### Question
Question is one question of an agent to a person.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| id | [string](#string) |  |  |
| org_id | [string](#string) |  |  |
| task_id | [string](#string) |  |  |
| run_id | [string](#string) |  |  |
| agent_id | [string](#string) |  |  |
| asked_to | [string](#string) |  | **Deprecated.** Deprecated: use recipients / waiting_on_user_ids. Set to the first recipient, for older clients. |
| question | [string](#string) |  |  |
| choices | [string](#string) | repeated | Answers offered as buttons; empty means free text only. |
| draft | [string](#string) |  | Text the agent proposes; empty when none. |
| default_answer | [string](#string) |  | The safe default, applied when the question is dismissed or expires. |
| state | [QuestionState](#koinon-question-v1alpha1-QuestionState) |  |  |
| answer | [string](#string) |  |  |
| answer_kind | [AnswerKind](#koinon-question-v1alpha1-AnswerKind) |  |  |
| answered_by | [string](#string) |  |  |
| expires_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| opened_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| resolved_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| created_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| can_answer | [bool](#bool) |  | Whether the caller can answer or dismiss it now. |
| target | [koinon.template.v1alpha1.NotifyTarget](#koinon-template-v1alpha1-NotifyTarget) |  | Who the question was addressed to, as resolved when it was asked. |
| target_label | [string](#string) |  | The target in plain words: a person&#39;s name, &#34;the org&#39;s admins&#34;, &#34;the org&#39;s owners&#34;, &#34;the org&#39;s members&#34;. |
| recipients | [Recipient](#koinon-question-v1alpha1-Recipient) | repeated | Everyone it waits on. The first answer wins. |
| waiting_since | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  | When it started waiting on the caller (unset when the caller is not a recipient). The inbox ages items from this; a reminder does not change it. |
| answered_by_name | [string](#string) |  | The display name of answered_by. |






<a name="koinon-question-v1alpha1-Recipient"></a>

### Recipient
Recipient is one person a question waits on.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| user_id | [string](#string) |  |  |
| display_name | [string](#string) |  |  |
| waiting_since | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |





 


<a name="koinon-question-v1alpha1-AnswerKind"></a>

### AnswerKind
AnswerKind is how an answer was given.

| Name | Number | Description |
| ---- | ------ | ----------- |
| ANSWER_KIND_UNSPECIFIED | 0 |  |
| ANSWER_KIND_CHOICE | 1 | One of the question&#39;s choices. |
| ANSWER_KIND_DRAFT | 2 | The draft, accepted as offered or edited. |
| ANSWER_KIND_TEXT | 3 | Free text. |
| ANSWER_KIND_DEFAULT | 4 | The safe default, applied by a dismissal or an expiry. |



<a name="koinon-question-v1alpha1-QuestionState"></a>

### QuestionState
QuestionState is where a question stands.

| Name | Number | Description |
| ---- | ------ | ----------- |
| QUESTION_STATE_UNSPECIFIED | 0 |  |
| QUESTION_STATE_ASKED | 1 | The agent has asked; its turn has not ended, so nobody sees it yet. |
| QUESTION_STATE_OPEN | 2 | Waiting for the person. |
| QUESTION_STATE_ANSWERED | 3 |  |
| QUESTION_STATE_DISMISSED | 4 | Dismissed by the person; the default was applied. |
| QUESTION_STATE_EXPIRED | 5 | Nobody answered in time; the default was applied. |
| QUESTION_STATE_CANCELED | 6 | The run ended (canceled, failed or finished) before anyone answered; nothing was applied. |


 

 


<a name="koinon-question-v1alpha1-QuestionService"></a>

### QuestionService
QuestionService is what an agent asks a person with the ask_user tool (ADR 0065 decisions 8 and
10a). A question is addressed to a target, a person or a role, resolved to its recipients when it
is asked. Every recipient sees it in the inbox and can answer or dismiss it. The first answer
wins. Every request names the org it acts in; IDs from another org are not found.

| Method Name | Request Type | Response Type | Description |
| ----------- | ------------ | ------------- | ------------|
| ListQuestions | [ListQuestionsRequest](#koinon-question-v1alpha1-ListQuestionsRequest) | [ListQuestionsResponse](#koinon-question-v1alpha1-ListQuestionsResponse) | ListQuestions returns the open questions addressed to the caller, newest first, or with a task_id every question of that task (any state) that is visible to the caller. Viewers and above. |
| AnswerQuestion | [AnswerQuestionRequest](#koinon-question-v1alpha1-AnswerQuestionRequest) | [AnswerQuestionResponse](#koinon-question-v1alpha1-AnswerQuestionResponse) | AnswerQuestion answers an open question addressed to the caller and resumes the run. The answer is a choice, the draft (as offered or edited) or free text. A question that is no longer open is refused (Aborted), so an answer never resumes a run twice. Members and above. |
| DismissQuestion | [DismissQuestionRequest](#koinon-question-v1alpha1-DismissQuestionRequest) | [DismissQuestionResponse](#koinon-question-v1alpha1-DismissQuestionResponse) | DismissQuestion dismisses an open question addressed to the caller: its safe default becomes the answer and the agent is told. Members and above. |
| ListAskableMembers | [ListAskableMembersRequest](#koinon-question-v1alpha1-ListAskableMembersRequest) | [ListAskableMembersResponse](#koinon-question-v1alpha1-ListAskableMembersResponse) | ListAskableMembers lists the org&#39;s members who can answer questions (member role or higher), for picking a trigger&#39;s notify target. Admins and above. |

 



<a name="koinon_run_v1alpha1_run-proto"></a>
<p align="right"><a href="#top">Top</a></p>

## koinon/run/v1alpha1/run.proto



<a name="koinon-run-v1alpha1-CancelRunRequest"></a>

### CancelRunRequest
CancelRunRequest names a run.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| run_id | [string](#string) |  |  |
| reason | [string](#string) |  | Shown on the ticket; defaults to &#34;canceled by &lt;user&gt;&#34;. |






<a name="koinon-run-v1alpha1-CancelRunResponse"></a>

### CancelRunResponse
CancelRunResponse holds the canceled run.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| run | [Run](#koinon-run-v1alpha1-Run) |  |  |






<a name="koinon-run-v1alpha1-Cost"></a>

### Cost
Cost is model spend recorded by the spend ledger from agentgateway&#39;s access
log (plan item L1).


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| requests | [int64](#int64) |  | Charged model requests. |
| input_tokens | [int64](#int64) |  |  |
| output_tokens | [int64](#int64) |  |  |
| usd | [string](#string) |  | USD as an exact decimal string with at least two fraction digits, for example &#34;0.0021&#34; or &#34;0.00&#34;. |
| priced | [bool](#bool) |  | False when some request had no price, so usd undercounts. |
| estimated_requests | [int64](#int64) |  | Requests whose usage the provider did not report and Koinon estimated (included in requests, tokens and usd). Zero when every request reported its usage. |
| cache_read_tokens | [int64](#int64) |  | input_tokens is the total input, cache reads and writes included. The split by type: the uncached input is input_tokens - cache_read_tokens - cache_write_tokens. reasoning_tokens is part of output_tokens where the provider reports it. |
| cache_write_tokens | [int64](#int64) |  |  |
| reasoning_tokens | [int64](#int64) |  |  |






<a name="koinon-run-v1alpha1-GetRunRequest"></a>

### GetRunRequest
GetRunRequest names a run.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| run_id | [string](#string) |  |  |






<a name="koinon-run-v1alpha1-GetRunResponse"></a>

### GetRunResponse
GetRunResponse holds the run.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| run | [Run](#koinon-run-v1alpha1-Run) |  |  |






<a name="koinon-run-v1alpha1-Heartbeat"></a>

### Heartbeat
Heartbeat is an empty keep-alive message.






<a name="koinon-run-v1alpha1-LiveChunk"></a>

### LiveChunk
LiveChunk is a piece of the message or thought in progress.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| kind | [string](#string) |  | kind is the run event kind of the item the text belongs to, for example &#34;acp.agent_message_chunk&#34; or &#34;acp.agent_thought_chunk&#34;. |
| text | [string](#string) |  |  |
| item | [string](#string) |  | item names the item the text belongs to. The run event stored for the item carries the same name as &#34;item&#34; in its payload. A client that has that event drops the item&#39;s live text, in whatever order the two arrive. Empty for a sender that does not name items. |






<a name="koinon-run-v1alpha1-LiveChunks"></a>

### LiveChunks
LiveChunks is a batch of live text, in the order it was typed.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| chunks | [LiveChunk](#koinon-run-v1alpha1-LiveChunk) | repeated |  |






<a name="koinon-run-v1alpha1-Run"></a>

### Run
Run is one execution of an agent for a ticket.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| id | [string](#string) |  |  |
| org_id | [string](#string) |  |  |
| ticket_id | [string](#string) |  |  |
| agent_id | [string](#string) |  |  |
| agent_version_id | [string](#string) |  |  |
| state | [RunState](#koinon-run-v1alpha1-RunState) |  |  |
| park_reason | [ParkReason](#koinon-run-v1alpha1-ParkReason) |  |  |
| outcome_reason | [string](#string) |  | Why the run failed or was canceled. |
| max_wall_clock_seconds | [int64](#int64) |  | Limits copied from the agent&#39;s version (or the agent&#39;s override) when the run was queued. |
| max_tokens | [int64](#int64) |  | **Deprecated.** Deprecated: tokens are reported, not enforced (ADR 0081). Zero on new runs. |
| tokens_used | [int64](#int64) |  | Tokens the run&#39;s requests used, input plus output as the gateway reports them (cache reads and writes are part of the input). The split by type is in cost. |
| last_event_seq | [int64](#int64) |  | The seq of the newest event. |
| created_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| started_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| finished_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| cost | [Cost](#koinon-run-v1alpha1-Cost) |  | Model spend recorded for the run by the spend ledger (plan item L1). Set by GetRun and on the runs of GetTicket; unset elsewhere. |
| failure_class | [FailureClass](#koinon-run-v1alpha1-FailureClass) |  | The class of the error that failed or stopped the run. Unspecified when the run did not fail, or failed before classes existed. |
| max_model_requests | [int64](#int64) |  | The run&#39;s request limit: 0 for a run queued before run limits existed (ADR 0081). Progress is cost.requests. |
| max_spend_usd | [string](#string) |  | The run&#39;s spend limit in USD as a decimal string; empty when the run has none. Progress is cost.usd. |






<a name="koinon-run-v1alpha1-RunEvent"></a>

### RunEvent
RunEvent is one session update or lifecycle note of a run.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| run_id | [string](#string) |  |  |
| seq | [int64](#int64) |  | Gap-free and strictly increasing per run, from 1. |
| kind | [string](#string) |  | For example &#34;session.update&#34; or &#34;run.canceled&#34;. |
| payload | [google.protobuf.Struct](#google-protobuf-Struct) |  |  |
| created_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| content_purged | [bool](#bool) |  | True when the org&#39;s retention has passed and the payload was removed; the event itself stays. |






<a name="koinon-run-v1alpha1-WatchRunRequest"></a>

### WatchRunRequest
WatchRunRequest names a run and where to resume.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| run_id | [string](#string) |  |  |
| after_seq | [int64](#int64) |  | Stream events with a greater seq; 0 streams from the start. |






<a name="koinon-run-v1alpha1-WatchRunResponse"></a>

### WatchRunResponse
WatchRunResponse carries a run snapshot, one event, live text or a heartbeat.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| run | [Run](#koinon-run-v1alpha1-Run) |  |  |
| event | [RunEvent](#koinon-run-v1alpha1-RunEvent) |  |  |
| heartbeat | [Heartbeat](#koinon-run-v1alpha1-Heartbeat) |  | Sent every 30 seconds while the stream has sent nothing else, so a proxy or load balancer does not cut a quiet run&#39;s stream as idle. Clients ignore it. |
| live | [LiveChunks](#koinon-run-v1alpha1-LiveChunks) |  | Text the agent is typing, sent about every 100 ms while it types. Live only: never stored, not part of the seq order, and a client that joins late or drops a stream misses it. The event for the finished message or thought follows with its whole text and replaces it; a client that has that event drops the item&#39;s live text (LiveChunk.item). |





 


<a name="koinon-run-v1alpha1-FailureClass"></a>

### FailureClass
FailureClass says what kind of error ended a run (ADR 0030, amendment of 2026-10-26). Retries
and the message to the person follow from it. Unset on a run that did not fail.

| Name | Number | Description |
| ---- | ------ | ----------- |
| FAILURE_CLASS_UNSPECIFIED | 0 |  |
| FAILURE_CLASS_TRANSIENT | 1 | A passing fault (a 5xx, an overloaded provider, a dropped stream); retried with backoff. |
| FAILURE_CLASS_THROTTLED | 2 | The provider rate-limited the request; retried after its Retry-After. |
| FAILURE_CLASS_CONTEXT_OVERFLOW | 3 | The input does not fit the model&#39;s context window. |
| FAILURE_CLASS_BUDGET | 4 | A run limit (model requests or spend) or a spend limit (agent, org or platform) stopped the run. |
| FAILURE_CLASS_POLICY | 5 | A policy denied the request; never retried. |
| FAILURE_CLASS_FATAL | 6 | Anything unknown; never retried. |
| FAILURE_CLASS_SANDBOX_UNAVAILABLE | 7 | The sandbox never became ready within the org&#39;s cap; never retried. |



<a name="koinon-run-v1alpha1-ParkReason"></a>

### ParkReason
ParkReason says why a parked run waits.

| Name | Number | Description |
| ---- | ------ | ----------- |
| PARK_REASON_UNSPECIFIED | 0 |  |
| PARK_REASON_APPROVAL | 1 |  |
| PARK_REASON_BUDGET | 2 |  |
| PARK_REASON_A2A | 3 |  |
| PARK_REASON_INPUT_REQUIRED | 4 |  |



<a name="koinon-run-v1alpha1-RunState"></a>

### RunState
RunState is the ADR 0030 state machine.

| Name | Number | Description |
| ---- | ------ | ----------- |
| RUN_STATE_UNSPECIFIED | 0 |  |
| RUN_STATE_QUEUED | 1 |  |
| RUN_STATE_STARTING | 2 |  |
| RUN_STATE_RUNNING | 3 |  |
| RUN_STATE_PARKED | 4 |  |
| RUN_STATE_SUCCEEDED | 5 |  |
| RUN_STATE_FAILED | 6 |  |
| RUN_STATE_CANCELED | 7 |  |


 

 


<a name="koinon-run-v1alpha1-RunService"></a>

### RunService
RunService reads, cancels and streams runs (ADR 0030). Every request names
the org it acts in; IDs from another org are not found.

| Method Name | Request Type | Response Type | Description |
| ----------- | ------------ | ------------- | ------------|
| GetRun | [GetRunRequest](#koinon-run-v1alpha1-GetRunRequest) | [GetRunResponse](#koinon-run-v1alpha1-GetRunResponse) | GetRun returns one run. Viewers and above. |
| CancelRun | [CancelRunRequest](#koinon-run-v1alpha1-CancelRunRequest) | [CancelRunResponse](#koinon-run-v1alpha1-CancelRunResponse) | CancelRun cancels a run that has not finished. Members and above. |
| WatchRun | [WatchRunRequest](#koinon-run-v1alpha1-WatchRunRequest) | [WatchRunResponse](#koinon-run-v1alpha1-WatchRunResponse) stream | WatchRun streams the run&#39;s events with seq greater than after_seq, first the stored ones and then new ones as they are written, each exactly once and in seq order. To resume after a dropped stream, call again with the last seq received. The first message is a snapshot of the run; another snapshot follows whenever its state changes. The stream ends after the run has finished and every event has been sent. Heartbeat messages keep a quiet stream open. Viewers and above. |

 



<a name="koinon_task_v1alpha1_task-proto"></a>
<p align="right"><a href="#top">Top</a></p>

## koinon/task/v1alpha1/task.proto



<a name="koinon-task-v1alpha1-AppendMessageRequest"></a>

### AppendMessageRequest
AppendMessageRequest is a user message on a task.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| task_id | [string](#string) |  |  |
| text | [string](#string) |  |  |






<a name="koinon-task-v1alpha1-AppendMessageResponse"></a>

### AppendMessageResponse
AppendMessageResponse holds the new event and the queued run, if one was queued by this call.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| event | [TaskEvent](#koinon-task-v1alpha1-TaskEvent) |  |  |
| run | [koinon.run.v1alpha1.Run](#koinon-run-v1alpha1-Run) |  |  |






<a name="koinon-task-v1alpha1-CancelTaskRequest"></a>

### CancelTaskRequest
CancelTaskRequest names the task.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| task_id | [string](#string) |  |  |






<a name="koinon-task-v1alpha1-CancelTaskResponse"></a>

### CancelTaskResponse
CancelTaskResponse holds the canceled task and the queued runs the cancel canceled.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| task | [Task](#koinon-task-v1alpha1-Task) |  |  |
| canceled_runs | [koinon.run.v1alpha1.Run](#koinon-run-v1alpha1-Run) | repeated |  |






<a name="koinon-task-v1alpha1-CreateTaskRequest"></a>

### CreateTaskRequest
CreateTaskRequest describes the task and its first message. A person creating it through this
API is the creator; triggers and agents create tasks inside the control plane.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| agent_id | [string](#string) |  | An active agent of the org; it owns the task. |
| title | [string](#string) |  |  |
| message | [string](#string) |  | The first message; optional. |






<a name="koinon-task-v1alpha1-CreateTaskResponse"></a>

### CreateTaskResponse
CreateTaskResponse holds the task and the run queued for it.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| task | [Task](#koinon-task-v1alpha1-Task) |  |  |
| run | [koinon.run.v1alpha1.Run](#koinon-run-v1alpha1-Run) |  |  |






<a name="koinon-task-v1alpha1-Creator"></a>

### Creator
Creator is who or what created a task. Exactly the field of its kind is set.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| kind | [CreatorKind](#koinon-task-v1alpha1-CreatorKind) |  |  |
| user_id | [string](#string) |  |  |
| agent_id | [string](#string) |  |  |
| trigger_id | [string](#string) |  |  |






<a name="koinon-task-v1alpha1-GetTaskRequest"></a>

### GetTaskRequest
GetTaskRequest names a task.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| task_id | [string](#string) |  |  |






<a name="koinon-task-v1alpha1-GetTaskResponse"></a>

### GetTaskResponse
GetTaskResponse holds the task, its whole timeline and its runs.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| task | [Task](#koinon-task-v1alpha1-Task) |  |  |
| events | [TaskEvent](#koinon-task-v1alpha1-TaskEvent) | repeated |  |
| runs | [koinon.run.v1alpha1.Run](#koinon-run-v1alpha1-Run) | repeated |  |






<a name="koinon-task-v1alpha1-ListTasksRequest"></a>

### ListTasksRequest
ListTasksRequest names the org and an optional state.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| state | [TaskState](#koinon-task-v1alpha1-TaskState) |  | Unspecified lists every state. |






<a name="koinon-task-v1alpha1-ListTasksResponse"></a>

### ListTasksResponse
ListTasksResponse lists tasks.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| tasks | [Task](#koinon-task-v1alpha1-Task) | repeated |  |






<a name="koinon-task-v1alpha1-NotifyTypingRequest"></a>

### NotifyTypingRequest
NotifyTypingRequest names the task.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| task_id | [string](#string) |  |  |






<a name="koinon-task-v1alpha1-NotifyTypingResponse"></a>

### NotifyTypingResponse
NotifyTypingResponse is empty.






<a name="koinon-task-v1alpha1-SendDirectMessageRequest"></a>

### SendDirectMessageRequest
SendDirectMessageRequest is a message to an agent, starting a thread or replying in one.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| agent_id | [string](#string) |  | The agent the message is for. With task_id it must be the task&#39;s agent. |
| task_id | [string](#string) |  | The thread to reply in: a task started by a direct message. Empty starts a new thread. |
| text | [string](#string) |  |  |
| message_id | [string](#string) |  | Chosen by the client for this message (a UUID); sending it again, for example after a lost response, has no second effect and returns the first result. |






<a name="koinon-task-v1alpha1-SendDirectMessageResponse"></a>

### SendDirectMessageResponse
SendDirectMessageResponse holds the thread&#39;s task.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| task | [Task](#koinon-task-v1alpha1-Task) |  |  |
| event | [TaskEvent](#koinon-task-v1alpha1-TaskEvent) |  | The message as it entered the timeline. A replay of an earlier call returns no event when the API runs the delivery inline; with the durable queue it attaches to the finished delivery and returns the original event and created flag. |
| created | [bool](#bool) |  | True when this call created the task. |
| reopened | [bool](#bool) |  | True when this call reopened a completed task as a follow-up. |






<a name="koinon-task-v1alpha1-Task"></a>

### Task
Task is one unit of work and its conversation.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| id | [string](#string) |  |  |
| org_id | [string](#string) |  |  |
| title | [string](#string) |  |  |
| state | [TaskState](#koinon-task-v1alpha1-TaskState) |  |  |
| agent_id | [string](#string) |  | The agent that owns the task. |
| created_by | [Creator](#koinon-task-v1alpha1-Creator) |  |  |
| last_event_seq | [int64](#int64) |  | The seq of the newest timeline event. |
| created_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| updated_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| finished_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  | Set once the task is completed, canceled or failed. |
| cost | [koinon.run.v1alpha1.Cost](#koinon-run-v1alpha1-Cost) |  | Model spend of all the task&#39;s runs (plan item L1). Set by GetTask and ListTasks. |
| agent_summary | [string](#string) |  | The agent&#39;s own note on where the task stands and what comes next, written at the end of its last turn (ADR 0051 amendment). Set by GetTask only. |
| agent_summary_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| waiting_on_user_id | [string](#string) |  | **Deprecated.** Deprecated: use recipients / waiting_on_user_ids. Set to the first recipient, for older clients. |
| waiting_on_name | [string](#string) |  | The target label of the oldest open question. |
| direct_message_thread | [bool](#bool) |  | True when a direct message to the agent started the task: replies in its inbox-chat thread go to SendDirectMessage with this task_id (plan item TR3). |
| waiting_on_user_ids | [string](#string) | repeated | Everyone the task&#39;s open questions wait on. Set by ListTasks and GetTask. The inbox uses it so that a task waiting on someone else&#39;s answer is not shown to its creator as theirs to act on. |






<a name="koinon-task-v1alpha1-TaskEvent"></a>

### TaskEvent
TaskEvent is one entry of the task timeline: a message, a state change or a run note such as
&#34;canceled: wall-clock cap&#34;.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| seq | [int64](#int64) |  | Gap-free and strictly increasing per task, from 1. |
| kind | [string](#string) |  | For example &#34;message&#34;, &#34;task.created&#34;, &#34;status.changed&#34;, &#34;run.queued&#34;, &#34;run.canceled&#34;. |
| actor_kind | [ActorKind](#koinon-task-v1alpha1-ActorKind) |  |  |
| actor_user_id | [string](#string) |  |  |
| actor_agent_id | [string](#string) |  |  |
| run_id | [string](#string) |  | The run the event is about, if any. |
| payload | [google.protobuf.Struct](#google-protobuf-Struct) |  |  |
| created_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |





 


<a name="koinon-task-v1alpha1-ActorKind"></a>

### ActorKind
ActorKind says who wrote a task event.

| Name | Number | Description |
| ---- | ------ | ----------- |
| ACTOR_KIND_UNSPECIFIED | 0 |  |
| ACTOR_KIND_USER | 1 |  |
| ACTOR_KIND_AGENT | 2 |  |
| ACTOR_KIND_SYSTEM | 3 |  |



<a name="koinon-task-v1alpha1-CreatorKind"></a>

### CreatorKind
CreatorKind says what created a task.

| Name | Number | Description |
| ---- | ------ | ----------- |
| CREATOR_KIND_UNSPECIFIED | 0 |  |
| CREATOR_KIND_USER | 1 |  |
| CREATOR_KIND_TRIGGER | 2 |  |
| CREATOR_KIND_AGENT | 3 |  |



<a name="koinon-task-v1alpha1-TaskState"></a>

### TaskState
TaskState is where a task stands (ADR 0065, after A2A).

| Name | Number | Description |
| ---- | ------ | ----------- |
| TASK_STATE_UNSPECIFIED | 0 |  |
| TASK_STATE_SUBMITTED | 1 | Created; not yet picked up as working. |
| TASK_STATE_WORKING | 2 | The agent is working on it. |
| TASK_STATE_INPUT_REQUIRED | 3 | A person has to act: approve, answer or validate. |
| TASK_STATE_COMPLETED | 4 |  |
| TASK_STATE_CANCELED | 5 |  |
| TASK_STATE_FAILED | 6 |  |


 

 


<a name="koinon-task-v1alpha1-TaskService"></a>

### TaskService
TaskService is the work record and the conversation on each task (ADR 0065). A task is
A2A-shaped and owned by one agent; people, triggers and agents create tasks, and nobody moves
them along by hand. Every request names the org it acts in; IDs from another org are not found.

| Method Name | Request Type | Response Type | Description |
| ----------- | ------------ | ------------- | ------------|
| CreateTask | [CreateTaskRequest](#koinon-task-v1alpha1-CreateTaskRequest) | [CreateTaskResponse](#koinon-task-v1alpha1-CreateTaskResponse) | CreateTask creates a task for an active agent and queues a run for it (plan item A4). Members and above. |
| ListTasks | [ListTasksRequest](#koinon-task-v1alpha1-ListTasksRequest) | [ListTasksResponse](#koinon-task-v1alpha1-ListTasksResponse) | ListTasks returns the org&#39;s tasks, oldest first, optionally those in one state. Viewers and above. |
| GetTask | [GetTaskRequest](#koinon-task-v1alpha1-GetTaskRequest) | [GetTaskResponse](#koinon-task-v1alpha1-GetTaskResponse) | GetTask returns a task with its timeline and runs. Viewers and above. |
| AppendMessage | [AppendMessageRequest](#koinon-task-v1alpha1-AppendMessageRequest) | [AppendMessageResponse](#koinon-task-v1alpha1-AppendMessageResponse) | AppendMessage adds a user message to the task and queues a run for the task&#39;s agent unless one is already queued, which will see the message. Members and above. |
| CancelTask | [CancelTaskRequest](#koinon-task-v1alpha1-CancelTaskRequest) | [CancelTaskResponse](#koinon-task-v1alpha1-CancelTaskResponse) | CancelTask ends a task that has not ended: its queued runs are canceled and it moves to canceled. Members and above. |
| NotifyTyping | [NotifyTypingRequest](#koinon-task-v1alpha1-NotifyTypingRequest) | [NotifyTypingResponse](#koinon-task-v1alpha1-NotifyTypingResponse) | NotifyTyping tells Koinon that a person is typing in the task&#39;s conversation, so the task&#39;s agent sandbox is started (or kept up) before the message arrives (plan item LT1, ADR 0066). Clients call it when typing starts; calls for the same task within ten seconds of the last one are ignored. Members and above. |
| SendDirectMessage | [SendDirectMessageRequest](#koinon-task-v1alpha1-SendDirectMessageRequest) | [SendDirectMessageResponse](#koinon-task-v1alpha1-SendDirectMessageResponse) | SendDirectMessage sends a message to an agent in the inbox chat (plan item TR3, ADR 0066). The agent must have a direct_message trigger in its active version, else the call fails with FAILED_PRECONDITION. Without task_id it starts a thread: a new task (created by the trigger, interactive priority) whose first message is the trigger&#39;s instruction plus the text. With task_id it replies in that thread: an open task takes the message and its agent is woken; a completed task is reopened as a follow-up; a canceled or failed one takes no message. The message_id makes a retry or replay a no-op. Members and above. |

 



<a name="koinon_ticket_v1alpha1_ticket-proto"></a>
<p align="right"><a href="#top">Top</a></p>

## koinon/ticket/v1alpha1/ticket.proto



<a name="koinon-ticket-v1alpha1-AppendMessageRequest"></a>

### AppendMessageRequest
AppendMessageRequest is a user message on a ticket.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| ticket_id | [string](#string) |  |  |
| text | [string](#string) |  |  |






<a name="koinon-ticket-v1alpha1-AppendMessageResponse"></a>

### AppendMessageResponse
AppendMessageResponse holds the new event and the queued run, if one was
queued by this call.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| event | [TicketEvent](#koinon-ticket-v1alpha1-TicketEvent) |  |  |
| run | [koinon.run.v1alpha1.Run](#koinon-run-v1alpha1-Run) |  |  |






<a name="koinon-ticket-v1alpha1-CreateTicketRequest"></a>

### CreateTicketRequest
CreateTicketRequest describes the ticket and its first message.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| agent_id | [string](#string) |  | An active agent of the org. |
| title | [string](#string) |  |  |
| message | [string](#string) |  | The first message; optional. |






<a name="koinon-ticket-v1alpha1-CreateTicketResponse"></a>

### CreateTicketResponse
CreateTicketResponse holds the ticket and the run queued for it.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| ticket | [Ticket](#koinon-ticket-v1alpha1-Ticket) |  |  |
| run | [koinon.run.v1alpha1.Run](#koinon-run-v1alpha1-Run) |  |  |






<a name="koinon-ticket-v1alpha1-GetTicketRequest"></a>

### GetTicketRequest
GetTicketRequest names a ticket.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| ticket_id | [string](#string) |  |  |






<a name="koinon-ticket-v1alpha1-GetTicketResponse"></a>

### GetTicketResponse
GetTicketResponse holds the ticket, its whole timeline and its runs.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| ticket | [Ticket](#koinon-ticket-v1alpha1-Ticket) |  |  |
| events | [TicketEvent](#koinon-ticket-v1alpha1-TicketEvent) | repeated |  |
| runs | [koinon.run.v1alpha1.Run](#koinon-run-v1alpha1-Run) | repeated |  |






<a name="koinon-ticket-v1alpha1-ListTicketsRequest"></a>

### ListTicketsRequest
ListTicketsRequest names the org and an optional column.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| status | [TicketStatus](#koinon-ticket-v1alpha1-TicketStatus) |  | Unspecified lists every column. |






<a name="koinon-ticket-v1alpha1-ListTicketsResponse"></a>

### ListTicketsResponse
ListTicketsResponse lists tickets.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| tickets | [Ticket](#koinon-ticket-v1alpha1-Ticket) | repeated |  |






<a name="koinon-ticket-v1alpha1-MoveTicketRequest"></a>

### MoveTicketRequest
MoveTicketRequest names the ticket and the target column.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| ticket_id | [string](#string) |  |  |
| status | [TicketStatus](#koinon-ticket-v1alpha1-TicketStatus) |  |  |






<a name="koinon-ticket-v1alpha1-MoveTicketResponse"></a>

### MoveTicketResponse
MoveTicketResponse holds the moved ticket and the runs the move canceled.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| ticket | [Ticket](#koinon-ticket-v1alpha1-Ticket) |  |  |
| canceled_runs | [koinon.run.v1alpha1.Run](#koinon-run-v1alpha1-Run) | repeated |  |






<a name="koinon-ticket-v1alpha1-Ticket"></a>

### Ticket
Ticket is one unit of work and its conversation.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| id | [string](#string) |  |  |
| org_id | [string](#string) |  |  |
| title | [string](#string) |  |  |
| status | [TicketStatus](#koinon-ticket-v1alpha1-TicketStatus) |  |  |
| agent_id | [string](#string) |  |  |
| created_by | [string](#string) |  |  |
| last_event_seq | [int64](#int64) |  | The seq of the newest timeline event. |
| created_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| updated_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| done_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| cost | [koinon.run.v1alpha1.Cost](#koinon-run-v1alpha1-Cost) |  | Model spend of all the ticket&#39;s runs (plan item L1). Set by GetTicket and ListTickets. |
| agent_summary | [string](#string) |  | The agent&#39;s own note on where the ticket stands and what comes next, written at the end of its last turn (ADR 0051 amendment). Set by GetTicket only; seen by whoever sees the ticket. |
| agent_summary_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |






<a name="koinon-ticket-v1alpha1-TicketEvent"></a>

### TicketEvent
TicketEvent is one entry of the ticket timeline: a message, a column move
or a run note such as &#34;canceled: wall-clock cap&#34;.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| seq | [int64](#int64) |  | Gap-free and strictly increasing per ticket, from 1. |
| kind | [string](#string) |  | For example &#34;message&#34;, &#34;status.changed&#34;, &#34;run.queued&#34;, &#34;run.canceled&#34;. |
| actor_kind | [ActorKind](#koinon-ticket-v1alpha1-ActorKind) |  |  |
| actor_user_id | [string](#string) |  |  |
| actor_agent_id | [string](#string) |  |  |
| run_id | [string](#string) |  | The run the event is about, if any. |
| payload | [google.protobuf.Struct](#google-protobuf-Struct) |  |  |
| created_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |





 


<a name="koinon-ticket-v1alpha1-ActorKind"></a>

### ActorKind
ActorKind says who wrote a ticket event.

| Name | Number | Description |
| ---- | ------ | ----------- |
| ACTOR_KIND_UNSPECIFIED | 0 |  |
| ACTOR_KIND_USER | 1 |  |
| ACTOR_KIND_AGENT | 2 |  |
| ACTOR_KIND_SYSTEM | 3 |  |



<a name="koinon-ticket-v1alpha1-TicketStatus"></a>

### TicketStatus
TicketStatus is the board column (ADR 0049).

DEPRECATED: use koinon.task.v1alpha1.TaskState.

| Name | Number | Description |
| ---- | ------ | ----------- |
| TICKET_STATUS_UNSPECIFIED | 0 |  |
| TICKET_STATUS_TODO | 1 |  |
| TICKET_STATUS_IN_PROGRESS | 2 |  |
| TICKET_STATUS_WAITING_ON_YOU | 3 |  |
| TICKET_STATUS_DONE | 4 |  |


 

 


<a name="koinon-ticket-v1alpha1-TicketService"></a>

### TicketService
TicketService is the board and the conversation on each ticket (ADR 0017).
Every request names the org it acts in; IDs from another org are not found.

DEPRECATED: tickets are tasks now (ADR 0065, plan item TK1). This service is a compatibility
layer over koinon.task.v1alpha1.TaskService, kept until the web UI moves to tasks (TK2). The board
column is derived from the task state: submitted is To do, working is In progress,
input_required is Waiting on you, and completed, canceled and failed are Done.

| Method Name | Request Type | Response Type | Description |
| ----------- | ------------ | ------------- | ------------|
| CreateTicket | [CreateTicketRequest](#koinon-ticket-v1alpha1-CreateTicketRequest) | [CreateTicketResponse](#koinon-ticket-v1alpha1-CreateTicketResponse) | CreateTicket creates a ticket for an active agent and queues a run for it (plan item A4). Members and above. |
| ListTickets | [ListTicketsRequest](#koinon-ticket-v1alpha1-ListTicketsRequest) | [ListTicketsResponse](#koinon-ticket-v1alpha1-ListTicketsResponse) | ListTickets returns the org&#39;s tickets, oldest first, optionally one board column. Viewers and above. |
| GetTicket | [GetTicketRequest](#koinon-ticket-v1alpha1-GetTicketRequest) | [GetTicketResponse](#koinon-ticket-v1alpha1-GetTicketResponse) | GetTicket returns a ticket with its timeline and runs. Viewers and above. |
| AppendMessage | [AppendMessageRequest](#koinon-ticket-v1alpha1-AppendMessageRequest) | [AppendMessageResponse](#koinon-ticket-v1alpha1-AppendMessageResponse) | AppendMessage adds a user message to the ticket and queues a run for the ticket&#39;s agent unless one is already queued, which will see the message. Members and above. |
| MoveTicket | [MoveTicketRequest](#koinon-ticket-v1alpha1-MoveTicketRequest) | [MoveTicketResponse](#koinon-ticket-v1alpha1-MoveTicketResponse) | MoveTicket moves the ticket to a board column. Moving it to Done cancels its queued runs. Members and above. |

 



<a name="koinon_token_v1alpha1_token-proto"></a>
<p align="right"><a href="#top">Top</a></p>

## koinon/token/v1alpha1/token.proto



<a name="koinon-token-v1alpha1-CreateTokenRequest"></a>

### CreateTokenRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| name | [string](#string) |  |  |
| role | [koinon.auth.v1alpha1.OrgRole](#koinon-auth-v1alpha1-OrgRole) |  | role must be ORG_ROLE_ADMIN or ORG_ROLE_OWNER and at most the caller&#39;s. |
| expires_in | [google.protobuf.Duration](#google-protobuf-Duration) |  | expires_in is optional: 30 days by default, between 1 hour and 90 days. |






<a name="koinon-token-v1alpha1-CreateTokenResponse"></a>

### CreateTokenResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| token | [Token](#koinon-token-v1alpha1-Token) |  |  |
| secret | [string](#string) |  | secret is the token to send as &#34;Authorization: Bearer &lt;secret&gt;&#34;. It is shown only here. |






<a name="koinon-token-v1alpha1-ListTokensRequest"></a>

### ListTokensRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |






<a name="koinon-token-v1alpha1-ListTokensResponse"></a>

### ListTokensResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| tokens | [Token](#koinon-token-v1alpha1-Token) | repeated |  |






<a name="koinon-token-v1alpha1-RebindTokenRequest"></a>

### RebindTokenRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| token_id | [string](#string) |  |  |






<a name="koinon-token-v1alpha1-RebindTokenResponse"></a>

### RebindTokenResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| token | [Token](#koinon-token-v1alpha1-Token) |  |  |






<a name="koinon-token-v1alpha1-RevokeTokenRequest"></a>

### RevokeTokenRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| token_id | [string](#string) |  |  |






<a name="koinon-token-v1alpha1-RevokeTokenResponse"></a>

### RevokeTokenResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| token | [Token](#koinon-token-v1alpha1-Token) |  |  |






<a name="koinon-token-v1alpha1-Token"></a>

### Token
Token describes an admin API token. It never holds the secret.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| id | [string](#string) |  |  |
| name | [string](#string) |  |  |
| prefix | [string](#string) |  | prefix is the public part that appears in the token, to tell tokens apart. |
| role | [koinon.auth.v1alpha1.OrgRole](#koinon-auth-v1alpha1-OrgRole) |  | role is ORG_ROLE_ADMIN or ORG_ROLE_OWNER. |
| created_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| expires_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| revoked_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| last_used_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| created_by | [string](#string) |  | created_by is the user at the root of the token&#39;s chain: who created the first token from a session. created_by_name is their display name or email. |
| created_by_name | [string](#string) |  |  |
| parent_token_id | [string](#string) |  | parent_token_id names the token that created this one, empty for a token made in the UI. A token dies with its parent. |





 

 

 


<a name="koinon-token-v1alpha1-TokenService"></a>

### TokenService
TokenService manages an org&#39;s admin API tokens (ADR 0056): bearer tokens for
/admin/v1, scoped to one org, with the role admin or owner, never more than
the creator&#39;s. Admins and owners only. The token itself is returned once, by
CreateToken; Koinon keeps only its hash.

| Method Name | Request Type | Response Type | Description |
| ----------- | ------------ | ------------- | ------------|
| CreateToken | [CreateTokenRequest](#koinon-token-v1alpha1-CreateTokenRequest) | [CreateTokenResponse](#koinon-token-v1alpha1-CreateTokenResponse) | CreateToken creates a token. expires_in defaults to 30 days and is at most 90 days. |
| ListTokens | [ListTokensRequest](#koinon-token-v1alpha1-ListTokensRequest) | [ListTokensResponse](#koinon-token-v1alpha1-ListTokensResponse) | ListTokens returns the org&#39;s tokens, newest first, without their secrets. |
| RevokeToken | [RevokeTokenRequest](#koinon-token-v1alpha1-RevokeTokenRequest) | [RevokeTokenResponse](#koinon-token-v1alpha1-RevokeTokenResponse) | RevokeToken revokes a token. Revoking twice is fine. |
| RebindToken | [RebindTokenRequest](#koinon-token-v1alpha1-RebindTokenRequest) | [RebindTokenResponse](#koinon-token-v1alpha1-RebindTokenResponse) | RebindToken re-enables a token that a restored copy refuses because another install issued it (ADR 0110). Platform admins only: an org owner cannot, the same as for the adopt-restore recovery action. A revoked or expired token is not re-enabled. Audited as token.rebind. |

 



<a name="koinon_trace_v1alpha1_trace-proto"></a>
<p align="right"><a href="#top">Top</a></p>

## koinon/trace/v1alpha1/trace.proto



<a name="koinon-trace-v1alpha1-CreateDestinationRequest"></a>

### CreateDestinationRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| destination | [DestinationInput](#koinon-trace-v1alpha1-DestinationInput) |  |  |






<a name="koinon-trace-v1alpha1-CreateDestinationResponse"></a>

### CreateDestinationResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| destination | [Destination](#koinon-trace-v1alpha1-Destination) |  |  |






<a name="koinon-trace-v1alpha1-DeleteDestinationRequest"></a>

### DeleteDestinationRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| destination_id | [string](#string) |  |  |






<a name="koinon-trace-v1alpha1-DeleteDestinationResponse"></a>

### DeleteDestinationResponse







<a name="koinon-trace-v1alpha1-DeletePlatformDestinationRequest"></a>

### DeletePlatformDestinationRequest







<a name="koinon-trace-v1alpha1-DeletePlatformDestinationResponse"></a>

### DeletePlatformDestinationResponse







<a name="koinon-trace-v1alpha1-Destination"></a>

### Destination
Destination is a destination as stored. It never holds a credential.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| id | [string](#string) |  | id is empty for the platform destination. |
| name | [string](#string) |  |  |
| backend | [Backend](#koinon-trace-v1alpha1-Backend) |  |  |
| endpoint_url | [string](#string) |  |  |
| params | [Destination.ParamsEntry](#koinon-trace-v1alpha1-Destination-ParamsEntry) | repeated | params are the preset&#39;s settings that are not secret, such as a LangSmith project. |
| secret_fields | [string](#string) | repeated | secret_fields names the secrets that are set, never their values. |
| resource_attributes | [Destination.ResourceAttributesEntry](#koinon-trace-v1alpha1-Destination-ResourceAttributesEntry) | repeated |  |
| enabled | [bool](#bool) |  |  |
| last_test | [TestResult](#koinon-trace-v1alpha1-TestResult) |  |  |
| health | [Health](#koinon-trace-v1alpha1-Health) |  |  |
| created_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| updated_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| config_version | [int64](#int64) |  | config_version changes with every change of the configuration and with nothing the router records. Send it back as expected_version when updating. |
| credential_lost_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  | credential_lost_at is set when the credentials were lost with the key that encrypted them (`koinon-api keys forget`, ADR 0085 decision 13): the destination receives nothing until a tested update enters every secret in secret_fields again. Unset otherwise. |
| admin_disabled | [koinon.outward.v1alpha1.AdminDisabled](#koinon-outward-v1alpha1-AdminDisabled) |  | admin_disabled is set while an admin holds the target off (the outward switch, ADR 0110 decision 5): it is not called, probed or offered to runs. Unset otherwise. |






<a name="koinon-trace-v1alpha1-Destination-ParamsEntry"></a>

### Destination.ParamsEntry



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| key | [string](#string) |  |  |
| value | [string](#string) |  |  |






<a name="koinon-trace-v1alpha1-Destination-ResourceAttributesEntry"></a>

### Destination.ResourceAttributesEntry



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| key | [string](#string) |  |  |
| value | [string](#string) |  |  |






<a name="koinon-trace-v1alpha1-DestinationInput"></a>

### DestinationInput
DestinationInput is what a caller writes.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| name | [string](#string) |  | name is a DNS label; the platform destination has none. |
| backend | [Backend](#koinon-trace-v1alpha1-Backend) |  |  |
| endpoint_url | [string](#string) |  | endpoint_url is the OTLP/HTTP traces endpoint, for example https://cloud.langfuse.com/api/public/otel/v1/traces. |
| params | [DestinationInput.ParamsEntry](#koinon-trace-v1alpha1-DestinationInput-ParamsEntry) | repeated |  |
| secrets | [DestinationInput.SecretsEntry](#koinon-trace-v1alpha1-DestinationInput-SecretsEntry) | repeated | secrets are the preset&#39;s secret fields (langfuse: public_key, secret_key; datadog, langsmith and opik: api_key), or the headers of a generic destination. |
| remove_secrets | [string](#string) | repeated |  |
| resource_attributes | [DestinationInput.ResourceAttributesEntry](#koinon-trace-v1alpha1-DestinationInput-ResourceAttributesEntry) | repeated |  |
| enabled | [bool](#bool) |  |  |






<a name="koinon-trace-v1alpha1-DestinationInput-ParamsEntry"></a>

### DestinationInput.ParamsEntry



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| key | [string](#string) |  |  |
| value | [string](#string) |  |  |






<a name="koinon-trace-v1alpha1-DestinationInput-ResourceAttributesEntry"></a>

### DestinationInput.ResourceAttributesEntry



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| key | [string](#string) |  |  |
| value | [string](#string) |  |  |






<a name="koinon-trace-v1alpha1-DestinationInput-SecretsEntry"></a>

### DestinationInput.SecretsEntry



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| key | [string](#string) |  |  |
| value | [string](#string) |  |  |






<a name="koinon-trace-v1alpha1-GetPlatformRequest"></a>

### GetPlatformRequest







<a name="koinon-trace-v1alpha1-GetPlatformResponse"></a>

### GetPlatformResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| destination | [Destination](#koinon-trace-v1alpha1-Destination) |  | destination is unset while the platform has none. |
| ceiling | [ContentLevel](#koinon-trace-v1alpha1-ContentLevel) |  |  |






<a name="koinon-trace-v1alpha1-GetSettingsRequest"></a>

### GetSettingsRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |






<a name="koinon-trace-v1alpha1-GetSettingsResponse"></a>

### GetSettingsResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| settings | [Settings](#koinon-trace-v1alpha1-Settings) |  |  |






<a name="koinon-trace-v1alpha1-Health"></a>

### Health
Health is what the router recorded for the destination (decision 7).


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| last_success_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| last_error_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| last_error | [string](#string) |  |  |
| spans_dropped | [int64](#int64) |  |  |






<a name="koinon-trace-v1alpha1-ListDestinationsRequest"></a>

### ListDestinationsRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |






<a name="koinon-trace-v1alpha1-ListDestinationsResponse"></a>

### ListDestinationsResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| destinations | [Destination](#koinon-trace-v1alpha1-Destination) | repeated |  |






<a name="koinon-trace-v1alpha1-SetPlatformCeilingRequest"></a>

### SetPlatformCeilingRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| ceiling | [ContentLevel](#koinon-trace-v1alpha1-ContentLevel) |  |  |






<a name="koinon-trace-v1alpha1-SetPlatformCeilingResponse"></a>

### SetPlatformCeilingResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| ceiling | [ContentLevel](#koinon-trace-v1alpha1-ContentLevel) |  |  |
| orgs_lowered | [int32](#int32) |  | orgs_lowered is how many orgs were above the new ceiling and were lowered to it. |






<a name="koinon-trace-v1alpha1-SetPlatformDestinationRequest"></a>

### SetPlatformDestinationRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| destination | [DestinationInput](#koinon-trace-v1alpha1-DestinationInput) |  |  |
| expected_version | [int64](#int64) | optional | expected_version is the config_version the form was loaded with (0 when the platform has no destination yet). When set and the destination has changed since, the call fails with ABORTED. |






<a name="koinon-trace-v1alpha1-SetPlatformDestinationResponse"></a>

### SetPlatformDestinationResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| destination | [Destination](#koinon-trace-v1alpha1-Destination) |  |  |






<a name="koinon-trace-v1alpha1-Settings"></a>

### Settings
Settings are an org&#39;s trace settings.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| content_level | [ContentLevel](#koinon-trace-v1alpha1-ContentLevel) |  |  |
| sample_rate | [double](#double) |  | sample_rate is the share of runs traced, 0 to 1; 1 traces every run. |
| platform_copy_level | [ContentLevel](#koinon-trace-v1alpha1-ContentLevel) |  | platform_copy_level is the highest level the platform&#39;s copy may carry for this org; never above content_level. |
| ceiling | [ContentLevel](#koinon-trace-v1alpha1-ContentLevel) |  | ceiling is the highest level the platform lets an org pick. |
| has_platform_destination | [bool](#bool) |  | has_platform_destination says the platform has a destination, so platform_copy_level matters. |
| updated_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |
| updated_by | [string](#string) |  |  |






<a name="koinon-trace-v1alpha1-TestDestinationRequest"></a>

### TestDestinationRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| destination_id | [string](#string) |  |  |






<a name="koinon-trace-v1alpha1-TestDestinationResponse"></a>

### TestDestinationResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| destination | [Destination](#koinon-trace-v1alpha1-Destination) |  |  |
| result | [TestResult](#koinon-trace-v1alpha1-TestResult) |  |  |






<a name="koinon-trace-v1alpha1-TestPlatformDestinationRequest"></a>

### TestPlatformDestinationRequest







<a name="koinon-trace-v1alpha1-TestPlatformDestinationResponse"></a>

### TestPlatformDestinationResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| destination | [Destination](#koinon-trace-v1alpha1-Destination) |  |  |
| result | [TestResult](#koinon-trace-v1alpha1-TestResult) |  |  |






<a name="koinon-trace-v1alpha1-TestResult"></a>

### TestResult
TestResult is what the last synthetic trace showed.


| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| ok | [bool](#bool) |  |  |
| message | [string](#string) |  | message is a short, secret-free explanation; empty when ok. |
| tested_at | [google.protobuf.Timestamp](#google-protobuf-Timestamp) |  |  |






<a name="koinon-trace-v1alpha1-UpdateDestinationRequest"></a>

### UpdateDestinationRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| destination_id | [string](#string) |  |  |
| destination | [DestinationInput](#koinon-trace-v1alpha1-DestinationInput) |  |  |
| expected_version | [int64](#int64) | optional | expected_version is the config_version the form was loaded with. When set and the destination has changed since, the update fails with ABORTED. Left out, the update applies to the current version. |






<a name="koinon-trace-v1alpha1-UpdateDestinationResponse"></a>

### UpdateDestinationResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| destination | [Destination](#koinon-trace-v1alpha1-Destination) |  |  |






<a name="koinon-trace-v1alpha1-UpdateSettingsRequest"></a>

### UpdateSettingsRequest



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| org_id | [string](#string) |  |  |
| content_level | [ContentLevel](#koinon-trace-v1alpha1-ContentLevel) |  |  |
| sample_rate | [double](#double) | optional | sample_rate is the share of runs traced, 0 to 1. Left out, the stored value stays (1 for an org that never set one). |
| platform_copy_level | [ContentLevel](#koinon-trace-v1alpha1-ContentLevel) |  |  |






<a name="koinon-trace-v1alpha1-UpdateSettingsResponse"></a>

### UpdateSettingsResponse



| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| settings | [Settings](#koinon-trace-v1alpha1-Settings) |  |  |





 


<a name="koinon-trace-v1alpha1-Backend"></a>

### Backend


| Name | Number | Description |
| ---- | ------ | ----------- |
| BACKEND_UNSPECIFIED | 0 |  |
| BACKEND_LANGFUSE | 1 |  |
| BACKEND_DATADOG | 2 |  |
| BACKEND_LANGSMITH | 3 |  |
| BACKEND_OPIK | 4 |  |
| BACKEND_GENERIC | 5 | GENERIC sends the headers as given: every key in secrets is a header name. |



<a name="koinon-trace-v1alpha1-ContentLevel"></a>

### ContentLevel


| Name | Number | Description |
| ---- | ------ | ----------- |
| CONTENT_LEVEL_UNSPECIFIED | 0 |  |
| CONTENT_LEVEL_NONE | 1 | NONE: names, IDs, counts, timings, statuses and cost; no content. The default. |
| CONTENT_LEVEL_REDACTED | 2 | REDACTED: content after the scrubber. |
| CONTENT_LEVEL_FULL | 3 | FULL: content as is, apart from credentials. Only an org owner may choose it. |


 

 


<a name="koinon-trace-v1alpha1-TraceDestinationService"></a>

### TraceDestinationService
TraceDestinationService manages where run traces go (ADR 0074): an org&#39;s OTLP destinations, its
content level and sampling, and, for platform admins, the platform&#39;s destination and the ceiling on
content levels.

Org calls need the admin role; only an owner may set the content level full. Platform calls need a
platform admin. Credentials are write-only: they are sealed in the secret store and never returned.
Creating or changing a destination&#39;s connection (backend, endpoint, settings or secrets) first sends
it one synthetic trace and saves only when the backend accepts it (decision 9): a refusal is
FAILED_PRECONDITION and the message says why. A change that touches none of those (enabled, name,
resource attributes) is saved untested, so a destination whose backend is down can still be disabled.
Stored secrets are kept only while backend and endpoint origin stay the same; otherwise every secret
is required again. While the org&#39;s effective content level is full, only an owner may add a
destination or change its connection.

Failures carry a machine-readable reason as a google.rpc.ErrorInfo error detail (domain &#34;koinon.dev&#34;,
reason below, the human message stays in the error). Read it by unpacking the details of the error.

	PERMISSION_DENIED  ADMIN_REQUIRED          the org call needs the admin role or higher
	                   PLATFORM_ADMIN_REQUIRED the platform call needs a platform admin
	                   OWNER_ONLY              only an org owner may set the content level or the platform copy to full
	                   OWNER_AT_FULL           the org&#39;s level is full, so only an owner may add a destination or change its connection
	                   NOT_MEMBER              raising a level needs membership of the org (a platform admin outside it may only lower)
	FAILED_PRECONDITION TEST_FAILED            the test trace was not accepted, nothing was saved; the details also hold a
	                                           koinon.trace.v1alpha1.TestResult with the outcome alone (no prefix)
	                   ABOVE_CEILING           the level is above the platform ceiling
	ABORTED            CHANGED                 the destination changed (or was deleted) since the form was loaded

| Method Name | Request Type | Response Type | Description |
| ----------- | ------------ | ------------- | ------------|
| ListDestinations | [ListDestinationsRequest](#koinon-trace-v1alpha1-ListDestinationsRequest) | [ListDestinationsResponse](#koinon-trace-v1alpha1-ListDestinationsResponse) |  |
| CreateDestination | [CreateDestinationRequest](#koinon-trace-v1alpha1-CreateDestinationRequest) | [CreateDestinationResponse](#koinon-trace-v1alpha1-CreateDestinationResponse) |  |
| UpdateDestination | [UpdateDestinationRequest](#koinon-trace-v1alpha1-UpdateDestinationRequest) | [UpdateDestinationResponse](#koinon-trace-v1alpha1-UpdateDestinationResponse) | UpdateDestination replaces the destination&#39;s settings. Secrets are merged by name: a name in secrets is set, a name in remove_secrets is cleared and the rest stay. |
| DeleteDestination | [DeleteDestinationRequest](#koinon-trace-v1alpha1-DeleteDestinationRequest) | [DeleteDestinationResponse](#koinon-trace-v1alpha1-DeleteDestinationResponse) |  |
| TestDestination | [TestDestinationRequest](#koinon-trace-v1alpha1-TestDestinationRequest) | [TestDestinationResponse](#koinon-trace-v1alpha1-TestDestinationResponse) | TestDestination sends the saved destination a synthetic trace again and keeps the result. |
| GetSettings | [GetSettingsRequest](#koinon-trace-v1alpha1-GetSettingsRequest) | [GetSettingsResponse](#koinon-trace-v1alpha1-GetSettingsResponse) |  |
| UpdateSettings | [UpdateSettingsRequest](#koinon-trace-v1alpha1-UpdateSettingsRequest) | [UpdateSettingsResponse](#koinon-trace-v1alpha1-UpdateSettingsResponse) | UpdateSettings sets the org&#39;s content level, sampling and platform opt-in. |
| GetPlatform | [GetPlatformRequest](#koinon-trace-v1alpha1-GetPlatformRequest) | [GetPlatformResponse](#koinon-trace-v1alpha1-GetPlatformResponse) | GetPlatform returns the platform destination and ceiling. Platform admins only. |
| SetPlatformDestination | [SetPlatformDestinationRequest](#koinon-trace-v1alpha1-SetPlatformDestinationRequest) | [SetPlatformDestinationResponse](#koinon-trace-v1alpha1-SetPlatformDestinationResponse) | SetPlatformDestination tests, then saves, the platform destination. Platform admins only. |
| DeletePlatformDestination | [DeletePlatformDestinationRequest](#koinon-trace-v1alpha1-DeletePlatformDestinationRequest) | [DeletePlatformDestinationResponse](#koinon-trace-v1alpha1-DeletePlatformDestinationResponse) |  |
| TestPlatformDestination | [TestPlatformDestinationRequest](#koinon-trace-v1alpha1-TestPlatformDestinationRequest) | [TestPlatformDestinationResponse](#koinon-trace-v1alpha1-TestPlatformDestinationResponse) |  |
| SetPlatformCeiling | [SetPlatformCeilingRequest](#koinon-trace-v1alpha1-SetPlatformCeilingRequest) | [SetPlatformCeilingResponse](#koinon-trace-v1alpha1-SetPlatformCeilingResponse) | SetPlatformCeiling sets the highest content level an org may pick. Lowering it lowers every org above it in the same transaction. Platform admins only. |

 



## Scalar Value Types

| .proto Type | Notes | C++ | Java | Python | Go | C# | PHP | Ruby |
| ----------- | ----- | --- | ---- | ------ | -- | -- | --- | ---- |
| <a name="double" /> double |  | double | double | float | float64 | double | float | Float |
| <a name="float" /> float |  | float | float | float | float32 | float | float | Float |
| <a name="int32" /> int32 | Uses variable-length encoding. Inefficient for encoding negative numbers – if your field is likely to have negative values, use sint32 instead. | int32 | int | int | int32 | int | integer | Bignum or Fixnum (as required) |
| <a name="int64" /> int64 | Uses variable-length encoding. Inefficient for encoding negative numbers – if your field is likely to have negative values, use sint64 instead. | int64 | long | int/long | int64 | long | integer/string | Bignum |
| <a name="uint32" /> uint32 | Uses variable-length encoding. | uint32 | int | int/long | uint32 | uint | integer | Bignum or Fixnum (as required) |
| <a name="uint64" /> uint64 | Uses variable-length encoding. | uint64 | long | int/long | uint64 | ulong | integer/string | Bignum or Fixnum (as required) |
| <a name="sint32" /> sint32 | Uses variable-length encoding. Signed int value. These more efficiently encode negative numbers than regular int32s. | int32 | int | int | int32 | int | integer | Bignum or Fixnum (as required) |
| <a name="sint64" /> sint64 | Uses variable-length encoding. Signed int value. These more efficiently encode negative numbers than regular int64s. | int64 | long | int/long | int64 | long | integer/string | Bignum |
| <a name="fixed32" /> fixed32 | Always four bytes. More efficient than uint32 if values are often greater than 2^28. | uint32 | int | int | uint32 | uint | integer | Bignum or Fixnum (as required) |
| <a name="fixed64" /> fixed64 | Always eight bytes. More efficient than uint64 if values are often greater than 2^56. | uint64 | long | int/long | uint64 | ulong | integer/string | Bignum |
| <a name="sfixed32" /> sfixed32 | Always four bytes. | int32 | int | int | int32 | int | integer | Bignum or Fixnum (as required) |
| <a name="sfixed64" /> sfixed64 | Always eight bytes. | int64 | long | int/long | int64 | long | integer/string | Bignum |
| <a name="bool" /> bool |  | bool | boolean | boolean | bool | bool | boolean | TrueClass/FalseClass |
| <a name="string" /> string | A string must always contain UTF-8 encoded or 7-bit ASCII text. | string | String | str/unicode | string | string | string | String (UTF-8) |
| <a name="bytes" /> bytes | May contain any arbitrary sequence of bytes. | string | ByteString | str | []byte | ByteString | string | String (ASCII-8BIT) |

