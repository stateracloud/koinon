// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/mcpservers"
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

// mcpProbeAgentPrefix starts the name of the per-server probe agent, a key that only the prober holds.
const mcpProbeAgentPrefix = gateway.MCPProbeAgentPrefix

// servedServer is an MCP server a pass renders.
type servedServer struct {
	org    store.ListOrgsToReconcileRow
	row    store.McpServer
	target string
	// file is the credential's data key in the provider keys Secret, "" for a server without one.
	file  string
	tools []store.McpTool
}

func (s *servedServer) probeAgent() gateway.AgentRef {
	return gateway.AgentRef{Org: s.org.Slug, Name: mcpProbeAgentPrefix + s.row.Name}
}

// servedMCP is what a pass renders for MCP servers.
type servedMCP struct {
	servers []*servedServer
	files   map[string][]byte
}

// loadMCP reads every org's enabled servers and their tools. A server that cannot be served (its URL
// breaks the policy, its credential cannot be opened, its target name clashes with another org's) is
// left out and reported, so one broken entry does not stop the others. A database error aborts the
// pass.
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
			s := &servedServer{org: org, row: row, target: name}
			if len(row.Credential) > 0 {
				cred, err := mcpservers.OpenCredential(r.cfg.Keys, org.ID, row.ID, row.Credential)
				if err != nil {
					errs = append(errs, fmt.Errorf("MCP server %s/%s not served: its credential cannot be opened: %w", org.Slug, row.Name, err))
					continue
				}
				sum := sha256.Sum256(row.Credential)
				s.file = "mcp-" + execution.UUIDString(row.ID.Bytes) + "-" + hex.EncodeToString(sum[:4])
				out.files[s.file] = []byte(cred)
			}
			tools, err := r.q.ListMCPToolsOfServer(ctx, store.ListMCPToolsOfServerParams{OrgID: org.ID, ServerID: row.ID})
			if err != nil {
				return out, errs, fmt.Errorf("reconciler: list tools of MCP server %s/%s: %w", org.Slug, row.Name, err)
			}
			s.tools = tools
			owner[name], owner[ask] = org.Slug+"/"+row.Name, org.Slug+"/"+row.Name
			out.servers = append(out.servers, s)
		}
	}
	if len(out.files) > 0 && r.cfg.ProviderKeys == nil {
		return out, errs, errors.New("reconciler: MCP servers have credentials but no provider key store is configured")
	}
	return out, errs, nil
}

// settleMCP leaves out servers whose new credential file is still reaching the gateway pod and have no
// previously applied one; with one, the server keeps using it. It returns the data keys to keep.
func (r *Reconciler) settleMCP(served *servedMCP, settling map[string]bool) map[string]bool {
	keep := map[string]bool{}
	for name := range served.files {
		keep[name] = true
	}
	if len(settling) == 0 {
		return keep
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*servedServer
	for _, s := range served.servers {
		if s.file == "" || !settling[s.file] {
			out = append(out, s)
			continue
		}
		if prev := r.appliedMCPFile[execution.UUIDString(s.row.ID.Bytes)]; prev != "" {
			s.file = prev
			keep[prev] = true
			out = append(out, s)
			r.log.Info("a new MCP server credential is still reaching the gateway; serving with the previous one", "org", s.org.Slug, "server", s.row.Name)
			continue
		}
		r.log.Info("a new MCP server credential is still reaching the gateway; the server is not served until it has", "org", s.org.Slug, "server", s.row.Name)
	}
	served.servers = out
	return keep
}

// target is the server's gateway target.
func (r *Reconciler) mcpTarget(s *servedServer) gateway.MCPTarget {
	t := gateway.MCPTarget{Name: s.target, URL: s.row.Url, SSE: s.row.Transport == store.McpTransportSse, Org: s.org.Slug}
	if s.file != "" {
		t.CredentialHeader = s.row.CredentialHeader.String
		t.CredentialPrefix = s.row.CredentialPrefix
		t.CredentialFile = path.Join(r.cfg.ProviderKeyDir, s.file)
	}
	return t
}

// allowedMCP is one agent's allowance of a server.
type allowedMCP struct {
	server pgtype.UUID
	// tools narrows the allowance to these tools; nil allows all of the server's.
	tools []string
}

// mcpGrants returns the agent's grants on the served servers, by tier, and whether any server gave
// it ask-tier tools. Servers that are unhealthy give nothing.
func mcpGrants(allowed []allowedMCP, served *servedMCP) (direct, ask []gateway.ToolGrant) {
	for _, a := range allowed {
		i := slices.IndexFunc(served.servers, func(s *servedServer) bool { return s.row.ID == a.server })
		if i < 0 {
			continue
		}
		s := served.servers[i]
		if s.row.Health == store.McpHealthUnhealthy {
			continue
		}
		var auto, asked []string
		for _, t := range s.tools {
			if !t.Present || (a.tools != nil && !slices.Contains(a.tools, t.Name)) {
				continue
			}
			if mcpservers.ToolNameProblem(s.org.Slug, s.row.Name, t.Name) != "" {
				continue // agents cannot use a name this long or with these characters
			}
			switch t.Tier {
			case store.McpTierAuto:
				auto = append(auto, t.Name)
			case store.McpTierAsk:
				asked = append(asked, t.Name)
			}
		}
		if len(auto) > 0 {
			direct = append(direct, gateway.ToolGrant{Target: s.target, Tools: auto})
		}
		if len(asked) > 0 {
			asked = append(asked, mcpservers.AskStatusTool)
			ask = append(ask, gateway.ToolGrant{Target: mcpservers.AskTargetName(s.org.Slug, s.row.Name), Tools: asked})
		}
	}
	return direct, ask
}

// mcpAllowances reads the active agents' MCP allowlists of one org.
func (r *Reconciler) mcpAllowances(ctx context.Context, org pgtype.UUID) (map[pgtype.UUID][]allowedMCP, error) {
	rows, err := r.q.ListAgentMCPServers(ctx, org)
	if err != nil {
		return nil, fmt.Errorf("reconciler: list MCP allowlists of org %s: %w", execution.UUIDString(org.Bytes), err)
	}
	out := map[pgtype.UUID][]allowedMCP{}
	for _, row := range rows {
		a := allowedMCP{server: row.ServerID}
		if row.Tools != nil {
			a.tools = slices.Clone(row.Tools)
		}
		out[row.AgentID] = append(out[row.AgentID], a)
	}
	return out, nil
}

// applyMCP adds the served servers, the ask-tier targets, the agents' MCP grants and the prober's
// keys to state. items are the pass's agents. It returns the probes the prober should run once the
// state is applied.
func (r *Reconciler) applyMCP(ctx context.Context, state *gateway.State, served *servedMCP, items []*work) ([]mcpProbe, error) {
	if len(served.servers) == 0 {
		return nil, nil
	}
	allowances := map[string]map[pgtype.UUID][]allowedMCP{}
	askTarget := map[string]bool{}
	for _, w := range items {
		if w.agent.Status != store.AgentStatusActive || w.key.ID == "" {
			continue
		}
		slug := w.org.Slug
		if _, ok := allowances[slug]; !ok {
			a, err := r.mcpAllowances(ctx, w.org.ID)
			if err != nil {
				return nil, err
			}
			allowances[slug] = a
		}
		direct, ask := mcpGrants(allowances[slug][w.agent.ID], served)
		if len(ask) > 0 && r.cfg.MCPAskURL == "" {
			r.log.Warn("an agent is allowed MCP tools in the ask tier but no approval proxy is configured; they are not offered", "org", slug, "agent", execution.UUIDString(w.agent.ID.Bytes))
			ask = nil
		}
		for _, g := range ask {
			askTarget[g.Target] = true
		}
		grants := append(slices.Clone(state.Tools[agentRef(w.org, w.agent.ID)]), append(direct, ask...)...)
		if len(grants) > 0 {
			state.GrantTools(agentRef(w.org, w.agent.ID), grants...)
		}
	}
	var probes []mcpProbe
	execGrants := map[string][]gateway.ToolGrant{}
	execOrg := map[string]store.ListOrgsToReconcileRow{}
	for _, s := range served.servers {
		execGrants[s.org.Slug] = append(execGrants[s.org.Slug], gateway.ToolGrant{Target: s.target, All: true})
		execOrg[s.org.Slug] = s.org
	}
	// Each org with servers gets an executor key, for the calls a human approved: they go through the
	// gateway like any call (its egress policy, its credential injection), so koinon-api never holds
	// a server credential. The key opens that org's servers only (the target conditions).
	for slug, grants := range execGrants {
		ref := gateway.AgentRef{Org: slug, Name: gateway.MCPExecAgentName}
		key, err := r.execKey(ctx, execOrg[slug], ref)
		if err != nil {
			return nil, err
		}
		state.AddKey(key)
		state.GrantTools(ref, grants...)
	}
	for _, s := range served.servers {
		state.MCP = append(state.MCP, r.mcpTarget(s))
		if at := mcpservers.AskTargetName(s.org.Slug, s.row.Name); askTarget[at] {
			state.MCP = append(state.MCP, gateway.MCPTarget{Name: at, URL: strings.TrimRight(r.cfg.MCPAskURL, "/") + "/" + s.row.Name, Org: s.org.Slug})
		}
		// The prober's key: it lists the server's tools through the gateway, as an agent would.
		ref := s.probeAgent()
		key, material, err := r.probeKey(ctx, ref)
		if err != nil {
			return nil, err
		}
		state.AddKey(key)
		state.GrantTools(ref, gateway.ToolGrant{Target: s.target, All: true})
		probes = append(probes, mcpProbe{
			org: s.org.ID, orgSlug: s.org.Slug, server: s.row.ID, name: s.row.Name, target: s.target,
			material: material,
		})
	}
	return probes, nil
}

// execKey returns the org's executor key, issuing it on first use and keeping it in a Secret that
// koinon-api reads (mcpservers.ExecSecretName).
func (r *Reconciler) execKey(ctx context.Context, org store.ListOrgsToReconcileRow, ref gateway.AgentRef) (gateway.Key, error) {
	name := mcpservers.ExecSecretName(org.ID)
	sec, found, err := r.secrets.Get(ctx, name)
	if err != nil {
		return gateway.Key{}, err
	}
	if found && sec.KeyID != "" && gateway.HashKey(sec.Material) == sec.KeyHash {
		return gateway.Key{ID: sec.KeyID, Agent: ref, Hash: sec.KeyHash}, nil
	}
	k, err := r.gw.IssueKey(ctx, ref)
	if err != nil {
		return gateway.Key{}, fmt.Errorf("issue the MCP executor key of %s: %w", ref, err)
	}
	if err := r.secrets.Put(ctx, KeySecret{
		Name: name, KeyID: k.ID, KeyHash: k.Hash, Material: k.Material, Labels: ownerLabels(org.ID, pgtype.UUID{}),
	}); err != nil {
		return gateway.Key{}, err
	}
	return k.Key, nil
}

// probeKey returns the prober's key for ref, issuing it on first use. The material lives in this
// process only: a restart issues new keys and so revokes the old ones.
func (r *Reconciler) probeKey(ctx context.Context, ref gateway.AgentRef) (gateway.Key, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.probeKeys == nil {
		r.probeKeys = map[gateway.AgentRef]gateway.IssuedKey{}
	}
	if k, ok := r.probeKeys[ref]; ok {
		return k.Key, k.Material, nil
	}
	k, err := r.gw.IssueKey(ctx, ref)
	if err != nil {
		return gateway.Key{}, "", fmt.Errorf("issue the MCP probe key of %s: %w", ref, err)
	}
	r.probeKeys[ref] = k
	return k.Key, k.Material, nil
}

// recordAppliedMCP remembers the credential file each served server was applied with and the probes
// the applied state makes possible.
func (r *Reconciler) recordAppliedMCP(served servedMCP, probes []mcpProbe) {
	r.mu.Lock()
	defer r.mu.Unlock()
	next := map[string]string{}
	for _, s := range served.servers {
		if s.file != "" {
			next[execution.UUIDString(s.row.ID.Bytes)] = s.file
		}
	}
	r.appliedMCPFile = next
	r.probes = probes
	live := map[gateway.AgentRef]bool{}
	for _, s := range served.servers {
		live[s.probeAgent()] = true
	}
	for ref := range r.probeKeys {
		if !live[ref] {
			delete(r.probeKeys, ref)
		}
	}
}
