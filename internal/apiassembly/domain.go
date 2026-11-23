// SPDX-License-Identifier: Apache-2.0

package apiassembly

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/approvals"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/codehost"
	"github.com/stateracloud/koinon/internal/codehost/providers"
	"github.com/stateracloud/koinon/internal/codehost/scopesync"
	"github.com/stateracloud/koinon/internal/connections"
	"github.com/stateracloud/koinon/internal/connectors/github"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/mcpask"
	"github.com/stateracloud/koinon/internal/mcpguard"
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/modelcheck"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/project"
	"github.com/stateracloud/koinon/internal/remoteagents"
	"github.com/stateracloud/koinon/internal/restorehold"
	"github.com/stateracloud/koinon/internal/services"
	"github.com/stateracloud/koinon/internal/tokens"
	"github.com/stateracloud/koinon/internal/tracedest"
	"github.com/stateracloud/koinon/pricing"
)

// domain is the services of the request path, before they are mounted.
type domain struct {
	keys    services.Keys
	opener  connections.Opener
	catKeys catalog.Keys

	urlPolicy catalog.URLPolicy
	hosts     *codehost.Registry
	hold      *restorehold.Guard

	guardMetrics   *mcpguard.Metrics
	guard          *mcpguard.Guard
	ask            *mcpask.Server
	executeAllowed func(ctx context.Context, org, id pgtype.UUID)
	approvals      *approvals.Service

	tokens       *tokens.Service
	catalog      *catalog.Service
	mcp          *mcpservers.Service
	remoteAgents *remoteagents.Service
	echo         *modelcheck.Service
	traces       *tracedest.Service
	scope        *scopesync.Syncer
}

// buildDomain is the wiring serve had in line: hold guard, egress policy, code hosts, approvals,
// tokens with the binding, catalog, MCP, echo check, trace destinations and scope sync.
func buildDomain(out Outside, set Settings) (*domain, error) {
	pool, kube, keyring := out.Pool, out.Kube, out.Keyring
	d := &domain{}
	if keyring != nil {
		d.opener, d.keys, d.catKeys = keyring, keyring, keyring
	}
	// One egress policy for everything an org points the gateway or Koinon at: model providers and
	// remote MCP servers.
	// The guest's resolver follows the cluster's DNS domain (#1054).
	search := catalog.SubstrateSearchFor(set.ClusterDomain)
	d.urlPolicy = catalog.URLPolicy{Namespace: set.Namespace, AllowedHosts: set.EgressAllowedHosts, Search: &search}
	// One GitHub client, so connection checks and approved calls share its token cache. It and the
	// Gitea provider dial Connection base URLs through the egress policy.
	gh := out.GitHub
	if gh == nil {
		gh = github.NewClient(github.Options{BaseURL: set.GitHubAPIURL, DialContext: d.urlPolicy.SandboxDialContext(&net.Dialer{Timeout: 5 * time.Second})})
	}
	d.hosts = providers.New(gh)
	// The MCP leak guard (ADR 0091) and the ask-tier proxy refer to each other: the guard records its
	// approvals through the proxy, and the proxy asks the guard for the risk on its one card.
	d.guardMetrics = mcpguard.NewMetrics(nil)
	// approvals is created below, after the guard. executeAllowed runs a call recorded approved
	// (a Trusted preset, an approval scope) at once instead of on the next sweep.
	d.executeAllowed = func(ctx context.Context, org, id pgtype.UUID) {
		ctx, cancel := context.WithTimeout(ctx, approvals.DefaultExecutionLease)
		defer cancel()
		if _, err := d.approvals.Execute(ctx, org, id); errors.Is(err, restorehold.ErrHeld) {
			out.Logger.Info("restore-hold: an allowed call waits for the release", "approval", id.String())
		} else if err != nil {
			out.Logger.Warn("executing an allowed call failed; the sweeper retries", "approval", id.String(), "err", err)
		}
	}
	d.ask = mcpask.New(pool, mcpask.Options{
		Execute: d.executeAllowed,
		Assess: func(ctx context.Context, org store.Org, agent store.Agent, srv store.McpServer, tool string, args json.RawMessage) (map[string]any, error) {
			return d.guard.Assess(ctx, org, agent, srv, tool, args)
		},
	})
	d.guard = mcpguard.New(pool, mcpguard.Options{
		Logger: out.Logger, Metrics: d.guardMetrics, ByteCap: set.GuardByteCap,
		Approve:    d.ask.RequestApproval,
		OwnTargets: append(providers.Default().IDs(), gateway.KoinonTarget),
	})
	// One guard for every service that reaches outward (issue #462, ADR 0107).
	d.hold = restorehold.NewGuard(set.RestoreHold)
	approvalOpts := approvals.Options{RestoreHold: d.hold, Keys: d.opener, Hosts: d.hosts, GatewayMCPURL: set.GatewayMCPURL, PublicURL: set.PublicURL, Logger: out.Logger, OnMCPResult: d.guard.RecordApproved, OnMCPSent: d.guard.RecordSent, DialContext: d.urlPolicy.SandboxDialContext(&net.Dialer{Timeout: 5 * time.Second})}
	if kube != nil {
		// Approved calls to remote MCP servers go through the gateway with the org's executor key,
		// which the reconciler keeps in a Secret; koinon-api never holds a server credential.
		approvalOpts.ExecKeys = mcpservers.KubeExecKeys{Client: kube, Namespace: set.Namespace}
	}
	approvalOpts.SavedWorkKeys = keyring // opens the work saved from stuck sandboxes (ADR 0054)
	d.approvals = approvals.New(pool, approvalOpts)
	d.tokens = tokens.New(pool, nil).WithBinding(out.Binding)
	d.catalog = catalog.New(pool, d.catKeys, out.Logger).WithURLPolicy(d.urlPolicy)
	// A server that asks each user to sign in is refused when it is added (v0.2 brings per-user OAuth).
	d.mcp = mcpservers.New(pool, d.catKeys, out.Logger).WithURLPolicy(d.urlPolicy).WithPreflight(mcpservers.OAuthPreflight(d.urlPolicy)).WithRestoreHold(d.hold)
	if set.GatewayMode == project.GatewayKubernetes {
		// The Kubernetes driver cannot render a per-target TLS policy (gateway.MCPTargetCA).
		d.mcp.WithoutTargetTLS()
	}
	if set.MCPRecipient != nil {
		d.mcp.WithRecipient(set.MCPRecipient)
	}
	d.remoteAgents = remoteagents.New(pool, d.catKeys, out.Logger).WithURLPolicy(d.urlPolicy).WithRestoreHold(d.hold)
	if set.GatewayMode == project.GatewayKubernetes {
		d.remoteAgents.WithoutTargetTLS()
	}
	// The provider echo check sends one request through the gateway with the org's model-check key,
	// which the reconciler keeps in a Secret (mcpservers.ModelCheckSecretName). Without a gateway URL or Kubernetes
	// access the check answers that it is not configured.
	if out.ModelChat != nil {
		d.echo = modelcheck.New(pool, out.ModelChat, prices.NewResolver(pricing.Embedded()))
	} else if set.GatewayURL != "" && kube != nil {
		d.echo = modelcheck.New(pool, modelcheck.GatewayChat{
			URL: set.GatewayURL, Keys: mcpservers.KubeExecKeys{Client: kube, Namespace: set.Namespace, SecretName: mcpservers.ModelCheckSecretName},
			HTTP: &http.Client{Timeout: 90 * time.Second},
		}, prices.NewResolver(pricing.Embedded()))
	} else {
		d.echo = modelcheck.New(pool, nil, prices.NewResolver(pricing.Embedded()))
	}
	// Run-trace destinations (ADR 0074). The client dials only what the egress policy allows.
	var err error
	if d.traces, err = tracedest.New(pool, d.catKeys, d.urlPolicy, out.Logger); err != nil {
		return nil, err
	}
	d.traces.WithRestoreHold(d.hold)
	d.echo.WithRestoreHold(d.hold)
	// The org's code-host scope (ADR 0091): each code-host connection's repositories and their
	// visibility, read when a connection is created or tested and again every few minutes, so the
	// leak guard calls a write into the org's own private repository internal only on a fresh reading.
	if !out.NoScopeSync {
		d.scope = scopesync.New(pool, d.hosts, d.opener, out.Logger).WithRestoreHold(d.hold)
	}
	return d, nil
}
