// SPDX-License-Identifier: Apache-2.0

// Package services implements Koinon's org-facing connect-rpc services
// (plan item A3): AgentService, TicketService, RunService and
// TemplateService, on the sqlc store and with the caller's Principal from the
// auth interceptor.
//
// Every org-owned call names its org in the request. The caller must hold a
// role in that org (platform admins hold every role); every query is scoped
// to it, so an ID from another org is simply not found. Roles per RPC:
//
//	viewer  Get*, List*, WatchRun
//	member  CreateAgentFromTemplate, CreateTicket, AppendMessage, MoveTicket, CancelRun
//	admin   ActivateAgent (approves the version's grants), ArchiveAgent
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

	"github.com/procoduck/koinon/internal/auth"
	"github.com/procoduck/koinon/internal/db/store"
	"github.com/procoduck/koinon/internal/gen/koinon/agent/v1alpha1/agentv1alpha1connect"
	"github.com/procoduck/koinon/internal/gen/koinon/run/v1alpha1/runv1alpha1connect"
	"github.com/procoduck/koinon/internal/gen/koinon/template/v1alpha1/templatev1alpha1connect"
	"github.com/procoduck/koinon/internal/gen/koinon/ticket/v1alpha1/ticketv1alpha1connect"
	"github.com/procoduck/koinon/internal/runwatch"
)

// DefaultWatchPollInterval is how often a WatchRun stream re-reads the run
// without a wake-up, as a backstop for missed notifications.
const DefaultWatchPollInterval = 5 * time.Second

// Options configures a Server.
type Options struct {
	// Hub wakes WatchRun streams. Required.
	Hub *runwatch.Hub
	// WatchPollInterval defaults to DefaultWatchPollInterval.
	WatchPollInterval time.Duration
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// Server implements the four services.
type Server struct {
	pool *pgxpool.Pool
	q    *store.Queries
	hub  *runwatch.Hub
	poll time.Duration
	log  *slog.Logger
}

// New returns a Server backed by pool.
func New(pool *pgxpool.Pool, opts Options) *Server {
	if opts.WatchPollInterval <= 0 {
		opts.WatchPollInterval = DefaultWatchPollInterval
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Server{pool: pool, q: store.New(pool), hub: opts.Hub, poll: opts.WatchPollInterval, log: opts.Logger}
}

// Register registers every service on server.
func (s *Server) Register(server *connect.Server) {
	agentv1alpha1connect.RegisterAgentServiceHandler(server, s)
	ticketv1alpha1connect.RegisterTicketServiceHandler(server, s)
	runv1alpha1connect.RegisterRunServiceHandler(server, s)
	templatev1alpha1connect.RegisterTemplateServiceHandler(server, s)
}

var (
	_ agentv1alpha1connect.AgentServiceHandler       = (*Server)(nil)
	_ ticketv1alpha1connect.TicketServiceHandler     = (*Server)(nil)
	_ runv1alpha1connect.RunServiceHandler           = (*Server)(nil)
	_ templatev1alpha1connect.TemplateServiceHandler = (*Server)(nil)
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

// inSnapshot runs read-only fn in one repeatable-read snapshot.
func (s *Server) inSnapshot(ctx context.Context, fn func(q *store.Queries) error) error {
	return pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly},
		func(tx pgx.Tx) error { return fn(store.New(tx)) })
}
