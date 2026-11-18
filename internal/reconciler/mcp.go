// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/reconciler/gatewayplan"
)

// Remote MCP servers (plan item MC1, ADR 0057). Each enabled server of an org becomes a gateway
// target "<org slug>-<server>", visible to that org's keys only. A server's credential reaches the
// gateway as a file in the same Secret as the provider keys, named
// "mcp-<server ID>-<first 8 hex of the SHA-256 of the credential's envelope>", with the same
// settle logic: the target is not served until kubelet has propagated the file, and a changed
// credential keeps the previous file in use meanwhile.
//
// Agents get the tools of the servers they are allowed (agent_mcp_servers), by tier: tools in the
// auto tier on the server's own target; tools in the ask tier on a second target,
// "<org slug>-<server>-ask", that points at koinon-api's approval-gated tool proxy
// (internal/mcpask); tools in the never tier on none. An unhealthy server's tools are left out of
// every agent's grants, while its target stays so the prober can see it recover.

// servedMCP is what a pass renders for MCP servers.
type servedMCP struct {
	servers []gatewayplan.MCPServer
	files   map[string][]byte
}

// loadMCP reads every org's enabled servers and their tools. A server that cannot be served (its URL
// breaks the policy, its credential cannot be opened, its credential header or prefix holds a '$'
// (#668), its target name clashes with another org's) is left out and reported, so one broken entry
// does not stop the others. A database error aborts the pass.
func (r *Reconciler) loadMCP(ctx context.Context, orgs []store.ListOrgsToReconcileRow) (servedMCP, []error, error) {
	out := servedMCP{files: map[string][]byte{}}
	var errs []error
	owner := map[string]string{}
	for _, org := range orgs {
		rows, err := r.q.ListServedMCPServers(ctx, org.ID)
		if err != nil {
			return out, errs, fmt.Errorf("reconciler: list MCP servers of org %s: %w", org.Slug, err)
		}
		for _, row := range rows {
			name := mcpservers.TargetName(org.Slug, row.Name)
			ask := mcpservers.AskTargetName(org.Slug, row.Name)
			if other, ok := owner[name]; ok {
				errs = append(errs, fmt.Errorf("MCP server %s/%s not served: its gateway target %q is also the target of %s", org.Slug, row.Name, name, other))
				continue
			}
			if other, ok := owner[ask]; ok {
				errs = append(errs, fmt.Errorf("MCP server %s/%s not served: its approval target %q is also the target of %s", org.Slug, row.Name, ask, other))
				continue
			}
			if err := r.cfg.URLPolicy.CheckStrict(ctx, row.Url); err != nil {
				errs = append(errs, fmt.Errorf("MCP server %s/%s not served: %w", org.Slug, row.Name, err))
				continue
			}
			s := gatewayplan.MCPServer{
				Org: gatewayplan.Org{ID: org.ID, Slug: org.Slug}, ID: row.ID, Name: row.Name, URL: row.Url,
				SSE: row.Transport == store.McpTransportSse, Unhealthy: row.Health == store.McpHealthUnhealthy,
			}
			if len(row.Credential) > 0 {
				cred, err := mcpservers.OpenCredential(r.cfg.Keys, org.ID, row.ID, row.Credential)
				if err != nil {
					errs = append(errs, fmt.Errorf("MCP server %s/%s not served: its credential cannot be opened: %w", org.Slug, row.Name, err))
					continue
				}
				if err := mcpservers.CheckCredentialLoader(row.CredentialHeader.String, row.CredentialPrefix); err != nil {
					errs = append(errs, fmt.Errorf("MCP server %s/%s not served: %w", org.Slug, row.Name, err))
					continue
				}
				sum := sha256.Sum256(row.Credential)
				s.File = "mcp-" + execution.UUIDString(row.ID.Bytes) + "-" + hex.EncodeToString(sum[:4])
				s.CredentialHeader, s.CredentialPrefix = row.CredentialHeader.String, row.CredentialPrefix
				out.files[s.File] = []byte(cred)
			}
			tools, err := r.q.ListMCPToolsOfServer(ctx, store.ListMCPToolsOfServerParams{OrgID: org.ID, ServerID: row.ID})
			if err != nil {
				return out, errs, fmt.Errorf("reconciler: list tools of MCP server %s/%s: %w", org.Slug, row.Name, err)
			}
			for _, t := range tools {
				s.Tools = append(s.Tools, gatewayplan.MCPTool{Name: t.Name, Present: t.Present, Tier: planTier(t.Tier)})
			}
			owner[name], owner[ask] = org.Slug+"/"+row.Name, org.Slug+"/"+row.Name
			out.servers = append(out.servers, s)
		}
	}
	if len(out.files) > 0 && r.cfg.ProviderKeys == nil {
		return out, errs, errors.New("reconciler: MCP servers have credentials but no provider key store is configured")
	}
	return out, errs, nil
}

// planTier maps a tool's stored tier.
func planTier(t store.McpTier) gatewayplan.Tier {
	switch t {
	case store.McpTierAuto:
		return gatewayplan.TierAuto
	case store.McpTierAsk:
		return gatewayplan.TierAsk
	default:
		return gatewayplan.TierNever
	}
}

// mcpAllowances reads the active agents' MCP allowlists of one org.
func (r *Reconciler) mcpAllowances(ctx context.Context, org pgtype.UUID) (map[pgtype.UUID][]gatewayplan.Allowed, error) {
	rows, err := r.q.ListAgentMCPServers(ctx, org)
	if err != nil {
		return nil, fmt.Errorf("reconciler: list MCP allowlists of org %s: %w", execution.UUIDString(org.Bytes), err)
	}
	out := map[pgtype.UUID][]gatewayplan.Allowed{}
	for _, row := range rows {
		a := gatewayplan.Allowed{Server: row.ServerID}
		if row.Tools != nil {
			a.Tools = slices.Clone(row.Tools)
		}
		out[row.AgentID] = append(out[row.AgentID], a)
	}
	return out, nil
}

// systemKey returns the org's key for a system principal of Koinon's own (the MCP executor, the model
// check), issuing it on first use and keeping it in the Secret name, which koinon-api reads.
func (r *Reconciler) systemKey(ctx context.Context, name string, org pgtype.UUID, ref gateway.AgentRef) (gateway.Key, error) {
	sec, found, err := r.secrets.Get(ctx, name)
	if err != nil {
		return gateway.Key{}, err
	}
	if found && sec.KeyID != "" && gateway.HashKey(sec.Material) == sec.KeyHash {
		return gateway.Key{ID: sec.KeyID, Agent: ref, Hash: sec.KeyHash}, nil
	}
	k, err := r.gw.IssueKey(ctx, ref)
	if err != nil {
		return gateway.Key{}, fmt.Errorf("issue the key of %s: %w", ref, err)
	}
	if err := r.secrets.Put(ctx, KeySecret{
		Name: name, KeyID: k.ID, KeyHash: k.Hash, Material: k.Material, Labels: ownerLabels(org, pgtype.UUID{}),
	}); err != nil {
		return gateway.Key{}, err
	}
	return k.Key, nil
}
