// SPDX-License-Identifier: Apache-2.0

// Package services implements Koinon's org-facing connect-rpc services
// (plan item A3): AgentService, TicketService, RunService,
// TemplateService, ConnectionService, ApprovalService, TokenService, ProviderService and TraceDestinationService, on the sqlc store and with the caller's Principal from the
// auth interceptor.
//
// Every org-owned call names its org in the request. The caller must hold a
// role in that org (platform admins hold every role); every query is scoped
// to it, so an ID from another org is simply not found. Roles per RPC:
//
//	viewer  Get*, List*, WatchRun
//	member  CreateAgentFromTemplate, CreateTicket, AppendMessage, MoveTicket, CancelRun
//	admin   TraceDestinationService (org calls; the content level full needs an owner; the platform calls need a platform admin), AuditService.List (a platform admin may also read every org), ProviderService (every call), ActivateAgent (approves the version's grants), ArchiveAgent,
//	        CreateGitHubConnection, CreateGiteaConnection, TestConnection, CreateToken, ListTokens, RevokeToken, Approve, Deny (ADR 0049:
//	        approvers are admins and owners)
//
// A caller without any role in the org gets NotFound, so org IDs cannot be
// probed; a member without the required role gets PermissionDenied.
package services

import (
	"context"
	"errors"
	"log/slog"
	"time"

	connect "connectrpc.com/connect/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/agentmodel"
	"github.com/stateracloud/koinon/internal/approvals"
	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/auth"
	"github.com/stateracloud/koinon/internal/budget"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/codehost"
	"github.com/stateracloud/koinon/internal/codehost/providers"
	"github.com/stateracloud/koinon/internal/connections"
	"github.com/stateracloud/koinon/internal/connectors/github"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/durable"
	"github.com/stateracloud/koinon/internal/gen/koinon/agent/v1alpha1/agentv1alpha1connect"
	"github.com/stateracloud/koinon/internal/gen/koinon/approval/v1alpha1/approvalv1alpha1connect"
	"github.com/stateracloud/koinon/internal/gen/koinon/audit/v1alpha1/auditv1alpha1connect"
	"github.com/stateracloud/koinon/internal/gen/koinon/budget/v1alpha1/budgetv1alpha1connect"
	"github.com/stateracloud/koinon/internal/gen/koinon/connection/v1alpha1/connectionv1alpha1connect"
	"github.com/stateracloud/koinon/internal/gen/koinon/leakguard/v1alpha1/leakguardv1alpha1connect"
	"github.com/stateracloud/koinon/internal/gen/koinon/mcpserver/v1alpha1/mcpserverv1alpha1connect"
	"github.com/stateracloud/koinon/internal/gen/koinon/orgsettings/v1alpha1/orgsettingsv1alpha1connect"
	"github.com/stateracloud/koinon/internal/gen/koinon/outward/v1alpha1/outwardv1alpha1connect"
	"github.com/stateracloud/koinon/internal/gen/koinon/provider/v1alpha1/providerv1alpha1connect"
	"github.com/stateracloud/koinon/internal/gen/koinon/question/v1alpha1/questionv1alpha1connect"
	"github.com/stateracloud/koinon/internal/gen/koinon/run/v1alpha1/runv1alpha1connect"
	"github.com/stateracloud/koinon/internal/gen/koinon/task/v1alpha1/taskv1alpha1connect"
	"github.com/stateracloud/koinon/internal/gen/koinon/template/v1alpha1/templatev1alpha1connect"
	"github.com/stateracloud/koinon/internal/gen/koinon/ticket/v1alpha1/ticketv1alpha1connect"
	"github.com/stateracloud/koinon/internal/gen/koinon/token/v1alpha1/tokenv1alpha1connect"
	"github.com/stateracloud/koinon/internal/gen/koinon/trace/v1alpha1/tracev1alpha1connect"
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/modelcheck"
	"github.com/stateracloud/koinon/internal/orgsettings"
	"github.com/stateracloud/koinon/internal/outward"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/restorehold"
	"github.com/stateracloud/koinon/internal/runwatch"
	"github.com/stateracloud/koinon/internal/tokens"
	"github.com/stateracloud/koinon/internal/tracedest"
	"github.com/stateracloud/koinon/pricing"
)

// ScopeSyncer is the code-host scope module (codehost/scopesync.Syncer), which owns every reading
// of a connection's repositories. The services only tell it that a connection changed (Request);
// the one-in-flight rule, the timeout and the credential record are its policy, not theirs.
type ScopeSyncer interface {
	Request(ctx context.Context, row store.Connection, actor audit.Actor) <-chan struct{}
	Drain(ctx context.Context)
}

// Keys is the secret store as the services use it.
type Keys interface {
	connections.Opener
	connections.Sealer
}

// DefaultWatchPollInterval is how often a WatchRun stream re-reads the run
// without a wake-up, as a backstop for missed notifications.
const DefaultWatchPollInterval = 5 * time.Second

// DefaultWatchHeartbeatInterval is how often a WatchRun stream that has sent nothing else sends
// a heartbeat, so proxies and load balancers keep a quiet run's stream open (old issue 141).
const DefaultWatchHeartbeatInterval = 30 * time.Second

// DefaultWatchCancelGrace is how long WatchRun keeps a canceled, once-claimed run's stream open for
// the item its worker was typing when the cancel landed (old issue 344, ADR 0074).
const DefaultWatchCancelGrace = 10 * time.Second

// Options configures a Server.
type Options struct {
	// Hub wakes WatchRun streams. Required.
	Hub *runwatch.Hub
	// WatchPollInterval defaults to DefaultWatchPollInterval.
	WatchPollInterval time.Duration
	// WatchHeartbeatInterval defaults to DefaultWatchHeartbeatInterval.
	WatchHeartbeatInterval time.Duration
	// WatchCancelGrace defaults to DefaultWatchCancelGrace; negative turns the grace off.
	WatchCancelGrace time.Duration
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// Prices is the default price list; required (pricing.Embedded in production).
	Prices pricing.ListSource
	// Keys seals and opens connections' secret fields (the secret store). Nil means Koinon
	// runs without key-encryption keys, and creating connections fails.
	Keys Keys
	// Tokens issues admin API tokens; defaults to a service on the pool.
	Tokens *tokens.Service
	// ScopeSync is the code-host scope module. The services ask it for a reading of a code-host
	// connection's repositories when the connection is created, given a new secret or tested, and
	// drain it at shutdown (the periodic refresh is the module's own). Nil skips that.
	ScopeSync ScopeSyncer
	// Hosts are the code-host providers that run connection checks and list repositories. Without
	// it the shipped providers are used, with GitHub as the client below.
	Hosts *codehost.Registry
	// GitHub is the client of the shipped GitHub provider when Hosts is nil; it defaults to a
	// client for api.github.com.
	GitHub *github.Client
	// Approvals decides and executes approval requests. Nil means Approve and
	// Deny are unavailable; listing still works.
	Approvals *approvals.Service
	// Catalog manages model providers and models (ADR 0056). Nil means ProviderService answers
	// UNIMPLEMENTED.
	Catalog *catalog.Service
	// MCP manages remote MCP servers (ADR 0057). Nil means McpServerService answers UNIMPLEMENTED.
	MCP *mcpservers.Service
	// Notifier delivers the notifications that follow a commit (ADR 0066, practice B2). Nil means
	// an inline one: best effort, no retry. koinon-api passes a DBOS-backed one with --durable.
	Notifier *durable.Notifier
	// DirectMessages delivers direct messages through the org-partitioned DBOS queue (TR3). Nil
	// means inline, in one transaction. koinon-api passes one with --durable.
	DirectMessages *DirectMessageQueue
	// RunNow fires schedule triggers by hand through the org queue (api.durable.enabled). Nil means
	// inline.
	RunNow *RunNowQueue
	// ModelCheck runs the provider echo check (rc.6). Nil means CheckProviderEcho answers
	// UNIMPLEMENTED.
	ModelCheck *modelcheck.Service
	// URLPolicy decides which hosts a connection's base URL may name (the platform's egress
	// policy, as for providers and MCP servers). The zero policy still denies private and internal
	// addresses.
	URLPolicy catalog.URLPolicy
	// Traces manages run-trace destinations, content levels and the platform ceiling (ADR 0074). Nil
	// means TraceDestinationService answers UNIMPLEMENTED.
	Traces *tracedest.Service
	// RestoreHold is the install's restore-hold guard (issue #462, ADR 0107). The services that reach
	// outward hold the same guard and refuse with restorehold.ErrHeld (FailedPrecondition here); the
	// Server keeps it for the code-host calls it makes itself (connections, bindings) and for
	// RunScheduleTrigger. Nil means not held.
	RestoreHold *restorehold.Guard
}

// Server implements the four services.
type Server struct {
	pool *pgxpool.Pool
	q    *store.Queries
	hub  *runwatch.Hub
	poll time.Duration
	// heartbeat is the idle interval after which WatchRun sends a heartbeat.
	heartbeat time.Duration
	// cancelGrace is how long WatchRun waits for late events after a canceled run's snapshot.
	cancelGrace time.Duration
	log         *slog.Logger

	keys      Keys
	scopeSync ScopeSyncer
	tokens    *tokens.Service
	budgets   *budget.Service
	prices    *prices.Service
	hosts     *codehost.Registry
	approvals *approvals.Service
	catalog   *catalog.Service
	mcp       *mcpservers.Service
	typing    typingGate
	notifier  *durable.Notifier
	dm        *DirectMessageQueue
	urlPolicy catalog.URLPolicy
	// agentModels changes agents' model settings; modelCheck runs the provider echo check.
	agentModels *agentmodel.Service
	modelCheck  *modelcheck.Service
	runNow      *RunNowQueue
	traces      *tracedest.Service
	// leakGuard changes the org's leak-guard threshold and fetch-trusted host list (old issue 350).
	leakGuard *orgsettings.Service
	// hold refuses the code-host calls the handlers make themselves (refuseWhileHeld).
	hold *restorehold.Guard
	// outward is the one switch for outward targets (ADR 0110 decision 5).
	outward *outward.Service
}

// New returns a Server backed by pool.
func New(pool *pgxpool.Pool, opts Options) *Server {
	if opts.WatchPollInterval <= 0 {
		opts.WatchPollInterval = DefaultWatchPollInterval
	}
	if opts.WatchHeartbeatInterval <= 0 {
		opts.WatchHeartbeatInterval = DefaultWatchHeartbeatInterval
	}
	if opts.WatchCancelGrace == 0 {
		opts.WatchCancelGrace = DefaultWatchCancelGrace
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Hosts == nil {
		opts.Hosts = providers.New(opts.GitHub)
	}
	if opts.Tokens == nil {
		opts.Tokens = tokens.New(pool, nil)
	}
	if opts.Notifier == nil {
		opts.Notifier = durable.NewNotifier(nil, opts.Logger)
		RegisterNotifications(opts.Notifier, pool)
	}
	s := &Server{
		dm:       opts.DirectMessages,
		notifier: opts.Notifier, urlPolicy: opts.URLPolicy,
		pool: pool, q: store.New(pool), hub: opts.Hub, poll: opts.WatchPollInterval, heartbeat: opts.WatchHeartbeatInterval, cancelGrace: opts.WatchCancelGrace, log: opts.Logger,
		keys: opts.Keys, scopeSync: opts.ScopeSync, tokens: opts.Tokens, hosts: opts.Hosts, approvals: opts.Approvals, catalog: opts.Catalog, mcp: opts.MCP,
		budgets: budget.New(pool), prices: prices.New(pool, opts.Logger, opts.Prices),
		agentModels: agentmodel.New(pool, prices.NewResolver(opts.Prices)), modelCheck: opts.ModelCheck, runNow: opts.RunNow, traces: opts.Traces, leakGuard: orgsettings.NewService(pool),
		hold: opts.RestoreHold, outward: outward.New(pool).WithRestoreHold(opts.RestoreHold),
	}
	if s.dm != nil {
		s.dm.bind(s)
	}
	if s.runNow != nil {
		s.runNow.bind(s)
	}
	return s
}

// Register registers every service on server.
func (s *Server) Register(server *connect.Server) {
	agentv1alpha1connect.RegisterAgentServiceHandler(server, s)
	ticketv1alpha1connect.RegisterTicketServiceHandler(server, s)
	taskv1alpha1connect.RegisterTaskServiceHandler(server, taskAPI{s})
	questionv1alpha1connect.RegisterQuestionServiceHandler(server, s)
	runv1alpha1connect.RegisterRunServiceHandler(server, s)
	templatev1alpha1connect.RegisterTemplateServiceHandler(server, s)
	connectionv1alpha1connect.RegisterConnectionServiceHandler(server, s)
	tokenv1alpha1connect.RegisterTokenServiceHandler(server, s)
	budgetv1alpha1connect.RegisterBudgetServiceHandler(server, s)
	budgetv1alpha1connect.RegisterPriceServiceHandler(server, s)
	approvalv1alpha1connect.RegisterApprovalServiceHandler(server, s)
	providerv1alpha1connect.RegisterProviderServiceHandler(server, s)
	mcpserverv1alpha1connect.RegisterMcpServerServiceHandler(server, s)
	leakguardv1alpha1connect.RegisterLeakGuardServiceHandler(server, s)
	outwardv1alpha1connect.RegisterOutwardServiceHandler(server, s)
	orgsettingsv1alpha1connect.RegisterOrgSettingsServiceHandler(server, s)
	auditv1alpha1connect.RegisterAuditServiceHandler(server, s)
	tracev1alpha1connect.RegisterTraceDestinationServiceHandler(server, traceAPI{s})
}

var (
	_ agentv1alpha1connect.AgentServiceHandler             = (*Server)(nil)
	_ ticketv1alpha1connect.TicketServiceHandler           = (*Server)(nil)
	_ runv1alpha1connect.RunServiceHandler                 = (*Server)(nil)
	_ templatev1alpha1connect.TemplateServiceHandler       = (*Server)(nil)
	_ connectionv1alpha1connect.ConnectionServiceHandler   = (*Server)(nil)
	_ approvalv1alpha1connect.ApprovalServiceHandler       = (*Server)(nil)
	_ tokenv1alpha1connect.TokenServiceHandler             = (*Server)(nil)
	_ budgetv1alpha1connect.BudgetServiceHandler           = (*Server)(nil)
	_ budgetv1alpha1connect.PriceServiceHandler            = (*Server)(nil)
	_ providerv1alpha1connect.ProviderServiceHandler       = (*Server)(nil)
	_ mcpserverv1alpha1connect.McpServerServiceHandler     = (*Server)(nil)
	_ leakguardv1alpha1connect.LeakGuardServiceHandler     = (*Server)(nil)
	_ outwardv1alpha1connect.OutwardServiceHandler         = (*Server)(nil)
	_ orgsettingsv1alpha1connect.OrgSettingsServiceHandler = (*Server)(nil)
	_ auditv1alpha1connect.AuditServiceHandler             = (*Server)(nil)
)

// caller is an authorised request in one org.
type caller struct {
	org pgtype.UUID
	p   *auth.Principal
}

func (c caller) actor() pgtype.UUID { return c.p.UserID }

// authorize checks that the caller holds at least minRole in the named org.
func authorize(ctx context.Context, orgID string, minRole store.OrgRole) (caller, error) {
	p, ok := auth.PrincipalFrom(ctx)
	if !ok {
		return caller{}, connect.NewError(connect.CodeUnauthenticated, "login required")
	}
	org, err := parseID("org_id", orgID)
	if err != nil {
		return caller{}, err
	}
	if _, member := p.RoleIn(org); !member && !p.PlatformAdmin {
		return caller{}, connect.NewError(connect.CodeNotFound, "org not found")
	}
	if !p.HasRole(org, minRole) {
		return caller{}, connect.NewError(connect.CodePermissionDenied, "this needs the "+string(minRole)+" role or higher")
	}
	return caller{org: org, p: p}, nil
}

func parseID(field, s string) (pgtype.UUID, error) {
	var id pgtype.UUID
	if s == "" {
		return id, connect.NewError(connect.CodeInvalidArgument, field+" is required")
	}
	if err := id.Scan(s); err != nil || !id.Valid {
		return id, connect.NewError(connect.CodeInvalidArgument, field+" is not a UUID")
	}
	return id, nil
}

// dbError maps a store error: no rows is NotFound for what, anything else is
// logged and Internal.
func (s *Server) dbError(err error, what string) error {
	var ce *connect.Error
	switch {
	case errors.As(err, &ce):
		return err
	case errors.Is(err, restorehold.ErrHeld):
		return connect.NewError(connect.CodeFailedPrecondition, err.Error())
	case errors.Is(err, pgx.ErrNoRows):
		return connect.NewError(connect.CodeNotFound, what+" not found")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeCanceled, "request canceled")
	default:
		s.log.Error("database error", "what", what, "err", err)
		return connect.NewError(connect.CodeInternal, "internal error")
	}
}

// inTx runs fn in a transaction.
func (s *Server) inTx(ctx context.Context, fn func(q *store.Queries) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error { return fn(store.New(tx)) })
}

// inAuditTx runs fn in a transaction like inTx, with the audit batch of the change: fn adds its audit
// events to b and the audit module writes them just before commit, install event first and org events
// by org ID (ADR 0100 decision 1.7), so the event commits with its change or not at all. The batch stamps
// the request ID of ctx on every event, so inAuditTx gives ctx one (requestCtx) when the caller has not.
func (s *Server) inAuditTx(ctx context.Context, fn func(q *store.Queries, b audit.Batch) error) error {
	_, err := audit.InTx(requestCtx(ctx), s.pool, fn)
	return err
}

// inSnapshot runs read-only fn in one repeatable-read snapshot.
func (s *Server) inSnapshot(ctx context.Context, fn func(q *store.Queries) error) error {
	return pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly},
		func(tx pgx.Tx) error { return fn(store.New(tx)) })
}
