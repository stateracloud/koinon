// SPDX-License-Identifier: Apache-2.0

package apiassembly

import (
	"context"
	"net/http"

	connect "connectrpc.com/connect/v2"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/a2aedge"
	"github.com/stateracloud/koinon/internal/adminapi"
	"github.com/stateracloud/koinon/internal/api"
	"github.com/stateracloud/koinon/internal/approvals"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/codehost"
	"github.com/stateracloud/koinon/internal/codehost/scopesync"
	"github.com/stateracloud/koinon/internal/connections"
	"github.com/stateracloud/koinon/internal/declarative"
	"github.com/stateracloud/koinon/internal/githublink"
	"github.com/stateracloud/koinon/internal/installbinding"
	"github.com/stateracloud/koinon/internal/mcpask"
	"github.com/stateracloud/koinon/internal/mcpguard"
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/outward"
	"github.com/stateracloud/koinon/internal/restorehold"
	"github.com/stateracloud/koinon/internal/restorestatus"
	"github.com/stateracloud/koinon/internal/services"
	"github.com/stateracloud/koinon/internal/tokens"
	"github.com/stateracloud/koinon/internal/tracedest"
	"github.com/stateracloud/koinon/pricing"
	"github.com/stateracloud/koinon/web"
)

// Assembly is the request path and the parts of it that serve still runs loops and listeners for.
type Assembly struct {
	// Handler is the API handler: Connect services, admin REST, the web UI, /healthz and /readyz.
	Handler http.Handler
	// Server is the Connect service implementation.
	Server *services.Server

	Admin     *adminapi.Server
	Approvals *approvals.Service
	Tokens    *tokens.Service
	Catalog   *catalog.Service
	MCP       *mcpservers.Service
	Traces    *tracedest.Service
	Scope     *scopesync.Syncer
	Hold      *restorehold.Guard
	Hosts     *codehost.Registry
	URLPolicy catalog.URLPolicy
	// Opener opens connection secrets; nil without a keyring.
	Opener connections.Opener

	// Guard, GuardMetrics, Ask and ExecuteAllowed are the MCP leak guard (ADR 0091), the ask-tier
	// proxy and the run-at-once callback the listeners of serve still mount.
	Guard          *mcpguard.Guard
	GuardMetrics   *mcpguard.Metrics
	Ask            *mcpask.Server
	ExecuteAllowed func(ctx context.Context, org, id pgtype.UUID)
}

// buildSurfaces mounts the domain: services.Server, admin REST and the API handler.
func buildSurfaces(out Outside, set Settings, pre Prebuilt, d *domain) (*Assembly, error) {
	// A nil *Syncer in the interface would not read as nil, so it is set only when it exists.
	var scopeSync services.ScopeSyncer
	if d.scope != nil {
		scopeSync = d.scope
	}
	links := githublink.New(out.Pool, githublink.Options{Keys: d.opener, Hosts: d.hosts, PublicURL: set.PublicURL, Auth: pre.Auth, Logger: out.Logger})
	svc := services.New(out.Pool, services.Options{
		SandboxResources: &set.SandboxResources, ScopeSync: scopeSync, ModelCheck: d.echo, Traces: d.traces, RestoreHold: d.hold,
		Hub: pre.Hub, WatchPollInterval: set.WatchPoll, WatchHeartbeatInterval: set.WatchHeartbeat, WatchCancelGrace: set.WatchCancelGrace, Keys: d.keys, Tokens: d.tokens, Hosts: d.hosts, Approvals: d.approvals, GitHubLinks: links, Catalog: d.catalog, MCP: d.mcp, RemoteAgents: d.remoteAgents, Notifier: pre.Notifier, DirectMessages: pre.DirectMessages, RunNow: pre.RunNow, URLPolicy: d.urlPolicy, Prices: pricing.Embedded(), Logger: out.Logger,
	})
	pool, kube := out.Pool, out.Kube
	admin := adminapi.New(pool, d.catalog, d.tokens, out.Logger, pricing.Embedded()).WithMCP(d.mcp).WithOutward(outward.New(pool).WithRestoreHold(d.hold)).WithTriggers(svc).WithModelCheck(d.echo).
		WithRestoreStatus(func(ctx context.Context) (any, error) {
			opt := restorestatus.Options{AcceptImporterDeletions: set.AcceptImporterDeletions, Configured: restorestatus.ConfiguredTargets{
				PublicURL: installbinding.Origin(set.PublicURL), OIDCIssuer: set.OIDCIssuer, OIDCRedirectURL: installbinding.Origin(set.OIDCRedirectURL),
			}}
			if kube != nil {
				opt.PlanRemovals = func(ctx context.Context) ([]installbinding.Removal, error) {
					return declarative.PlanRemovals(ctx, pool, kube, set.Namespace)
				}
			}
			return restorestatus.Build(ctx, pool, out.BindingOwner, opt)
		})
	ui := out.UI
	if ui == nil && !out.NoUI {
		ui = web.Dist()
	}
	edge := a2aedge.New(a2aedge.Options{
		Backend: a2aedge.PG{Pool: pool, Svc: svc}, PublicURL: installbinding.Origin(set.PublicURL),
		Gate: admin.Gate, Principal: adminapi.PrincipalFrom,
	})
	handler, err := api.NewHandler(api.Options{
		Auth:           pre.Auth,
		Metrics:        pre.Metrics,
		TrustedOrigins: set.TrustedOrigins,
		Services:       append(append([]func(*connect.Server){}, out.Services...), svc.Register),
		UI:             ui,
		Mounts:         []func(*http.ServeMux){admin.Mount, edge.Mount, links.Mount},
		Ready:          out.Ready,
	})
	if err != nil {
		return nil, err
	}
	return &Assembly{
		Handler: handler, Server: svc, Admin: admin, Approvals: d.approvals, Tokens: d.tokens, Catalog: d.catalog, MCP: d.mcp,
		Traces: d.traces, Scope: d.scope, Hold: d.hold, Hosts: d.hosts, URLPolicy: d.urlPolicy, Opener: d.opener,
		Guard: d.guard, GuardMetrics: d.guardMetrics, Ask: d.ask, ExecuteAllowed: d.executeAllowed,
	}, nil
}
