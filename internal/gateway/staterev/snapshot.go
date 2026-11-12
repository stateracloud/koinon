// SPDX-License-Identifier: Apache-2.0

// Package staterev is the durable side of the State stream (ADR 0097 decision 2.1): it turns a
// gateway.State into the wire message of a revision, stores a revision when the state changed, and
// records and reads the confirmations of gateway copies.
//
// Hard rule: a stored revision and a StateUpdate carry no secret material. gateway.State holds none
// (a key is its id and hash; a provider key or MCP credential is the path it is mounted at), the
// conversion copies named fields only, and the spec in this package fails when a field that could
// carry one is added to State or to the message without a review.
package staterev

import (
	"cmp"
	"slices"
	"time"

	"github.com/stateracloud/koinon/internal/gateway"
	gwv1 "github.com/stateracloud/koinon/internal/gen/koinon/gateway/v1alpha1"
	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/internal/money"
	"github.com/stateracloud/koinon/internal/thinking"
)

// Snapshot returns the StateUpdate of s without a revision number. It is deterministic: every set
// is sorted, so two equal States give equal messages whatever order a pass built them in, and a
// pass that changes nothing publishes nothing.
func Snapshot(s gateway.State) *gwv1.StateUpdate {
	return &gwv1.StateUpdate{
		Keys:  keysToProto(s.Keys),
		Model: modelToProto(s.ModelState),
		Mcp:   mcpToProto(s.MCPState),
	}
}

// FromProto is the inverse of Snapshot, for the gateway side of the stream. The revision is not
// part of a State.
func FromProto(u *gwv1.StateUpdate) gateway.State {
	var s gateway.State
	for _, k := range u.GetKeys() {
		s.Keys = append(s.Keys, gateway.Key{ID: k.GetId(), Agent: agentFromProto(k.GetAgent()), Hash: k.GetHash()})
	}
	m := u.GetModel()
	for _, l := range m.GetLimits() {
		if s.Limits == nil {
			s.Limits = map[gateway.AgentRef]gateway.Limit{}
		}
		lim := l.GetLimit()
		s.Limits[agentFromProto(l.GetAgent())] = gateway.Limit{
			Unit: gateway.LimitUnit(lim.GetUnit()), Amount: money.USD(lim.GetAmountPicoUsd()),
			Window: time.Duration(lim.GetWindowSeconds()) * time.Second, Generation: lim.GetGeneration(),
		}
	}
	for _, p := range m.GetProviders() {
		s.Providers = append(s.Providers, providerFromProto(p))
	}
	for _, am := range m.GetModels() {
		if s.Models == nil {
			s.Models = map[gateway.AgentRef][]string{}
		}
		s.Models[agentFromProto(am.GetAgent())] = slices.Clone(am.GetModels())
	}
	for _, t := range u.GetMcp().GetTargets() {
		s.MCP = append(s.MCP, gateway.MCPTarget{
			Name: t.GetName(), URL: t.GetUrl(), SSE: t.GetSse(), Org: t.GetOrg(),
			CredentialHeader: t.GetCredentialHeader(), CredentialPrefix: t.GetCredentialPrefix(), CredentialFile: t.GetCredentialFile(),
		})
	}
	for _, at := range u.GetMcp().GetTools() {
		if s.Tools == nil {
			s.Tools = map[gateway.AgentRef][]gateway.ToolGrant{}
		}
		a := agentFromProto(at.GetAgent())
		for _, g := range at.GetGrants() {
			s.Tools[a] = append(s.Tools[a], gateway.ToolGrant{Target: g.GetTarget(), Tools: slices.Clone(g.GetTools()), All: g.GetAll()})
		}
	}
	return s
}

func agentToProto(a gateway.AgentRef) *gwv1.AgentRef { return &gwv1.AgentRef{Org: a.Org, Name: a.Name} }

func agentFromProto(a *gwv1.AgentRef) gateway.AgentRef {
	return gateway.AgentRef{Org: a.GetOrg(), Name: a.GetName()}
}

func cmpAgent(a, b gateway.AgentRef) int {
	return cmp.Or(cmp.Compare(a.Org, b.Org), cmp.Compare(a.Name, b.Name))
}

func sortedAgents[V any](m map[gateway.AgentRef]V) []gateway.AgentRef {
	out := make([]gateway.AgentRef, 0, len(m))
	for a := range m {
		out = append(out, a)
	}
	slices.SortFunc(out, cmpAgent)
	return out
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func sortedCopy(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}

func keysToProto(keys []gateway.Key) []*gwv1.Key {
	out := make([]*gwv1.Key, 0, len(keys))
	for _, k := range keys {
		// Named fields only: the id, the owner and the hash. Never a field added to Key later.
		out = append(out, &gwv1.Key{Id: k.ID, Agent: agentToProto(k.Agent), Hash: k.Hash})
	}
	slices.SortFunc(out, func(a, b *gwv1.Key) int { return cmp.Compare(a.GetId(), b.GetId()) })
	return out
}

func modelToProto(m gateway.ModelState) *gwv1.ModelState {
	out := &gwv1.ModelState{}
	for _, a := range sortedAgents(m.Limits) {
		l := m.Limits[a]
		out.Limits = append(out.Limits, &gwv1.AgentLimit{Agent: agentToProto(a), Limit: &gwv1.Limit{
			Unit: string(l.Unit), AmountPicoUsd: int64(l.Amount), WindowSeconds: int64(l.AppliedWindow() / time.Second), Generation: l.Generation,
		}})
	}
	for _, p := range m.Providers {
		out.Providers = append(out.Providers, providerToProto(p))
	}
	slices.SortFunc(out.Providers, func(a, b *gwv1.Provider) int {
		return cmp.Or(cmp.Compare(a.GetOrg(), b.GetOrg()), cmp.Compare(a.GetName(), b.GetName()))
	})
	for _, a := range sortedAgents(m.Models) {
		out.Models = append(out.Models, &gwv1.AgentModels{Agent: agentToProto(a), Models: sortedCopy(m.Models[a])})
	}
	return out
}

func providerToProto(p gateway.Provider) *gwv1.Provider {
	out := &gwv1.Provider{
		Org: p.Org, Name: p.Name, Type: p.Type, BaseUrl: p.BaseURL,
		// The path the key is mounted at, not the key.
		ApiKeyFile:           p.APIKeyFile,
		AnthropicWorkspaceId: p.AnthropicWorkspaceID,
	}
	for _, m := range p.Models {
		pm := &gwv1.Model{
			Name: m.Name, Format: string(m.Format), Guard: m.Guard,
			Thinking: &gwv1.Thinking{
				Thinks: string(m.Thinking.Thinks), On: string(m.Thinking.On), Off: string(m.Thinking.Off),
				Display: string(m.Thinking.Display), Betas: sortedCopy(m.Thinking.Betas), UsageSplit: string(m.Thinking.UsageSplit),
			},
		}
		if m.Price != nil {
			pm.Price = &gwv1.Price{
				Input: m.Price.Input, Output: m.Price.Output, CacheRead: m.Price.CacheRead,
				CacheWrite: m.Price.CacheWrite, Reasoning: m.Price.Reasoning,
			}
			for _, t := range m.Price.Tiers {
				pm.Price.Tiers = append(pm.Price.Tiers, &gwv1.PriceTier{
					ContextOver: t.ContextOver, Input: t.Input, Output: t.Output,
					CacheRead: t.CacheRead, CacheWrite: t.CacheWrite, Reasoning: t.Reasoning,
				})
			}
			slices.SortStableFunc(pm.Price.Tiers, func(a, b *gwv1.PriceTier) int { return cmp.Compare(a.GetContextOver(), b.GetContextOver()) })
		}
		out.Models = append(out.Models, pm)
	}
	slices.SortFunc(out.Models, func(a, b *gwv1.Model) int { return cmp.Compare(a.GetName(), b.GetName()) })
	return out
}

func providerFromProto(p *gwv1.Provider) gateway.Provider {
	out := gateway.Provider{
		Org: p.GetOrg(), Name: p.GetName(), Type: p.GetType(), BaseURL: p.GetBaseUrl(),
		APIKeyFile: p.GetApiKeyFile(), AnthropicWorkspaceID: p.GetAnthropicWorkspaceId(),
	}
	for _, m := range p.GetModels() {
		t := m.GetThinking()
		gm := gateway.Model{
			Name: m.GetName(), Format: modelformat.Format(m.GetFormat()), Guard: m.GetGuard(),
			Thinking: thinking.Capability{
				Thinks: thinking.Thinks(t.GetThinks()), On: thinking.Mode(t.GetOn()), Off: thinking.Mode(t.GetOff()),
				Display: thinking.Display(t.GetDisplay()), Betas: slices.Clone(t.GetBetas()), UsageSplit: thinking.UsageSplit(t.GetUsageSplit()),
			},
		}
		if pr := m.GetPrice(); pr != nil {
			gm.Price = &gateway.Price{
				Input: pr.GetInput(), Output: pr.GetOutput(), CacheRead: pr.GetCacheRead(),
				CacheWrite: pr.GetCacheWrite(), Reasoning: pr.GetReasoning(),
			}
			for _, pt := range pr.GetTiers() {
				gm.Price.Tiers = append(gm.Price.Tiers, gateway.PriceTier{
					ContextOver: pt.GetContextOver(), Input: pt.GetInput(), Output: pt.GetOutput(),
					CacheRead: pt.GetCacheRead(), CacheWrite: pt.GetCacheWrite(), Reasoning: pt.GetReasoning(),
				})
			}
		}
		out.Models = append(out.Models, gm)
	}
	return out
}

func mcpToProto(m gateway.MCPState) *gwv1.MCPState {
	out := &gwv1.MCPState{}
	for _, t := range m.MCP {
		out.Targets = append(out.Targets, &gwv1.MCPTarget{
			Name: t.Name, Url: t.URL, Sse: t.SSE, Org: t.Org,
			CredentialHeader: t.CredentialHeader, CredentialPrefix: t.CredentialPrefix,
			// The path the credential is mounted at, not the credential.
			CredentialFile: t.CredentialFile,
		})
	}
	slices.SortFunc(out.Targets, func(a, b *gwv1.MCPTarget) int {
		return cmp.Or(cmp.Compare(a.GetOrg(), b.GetOrg()), cmp.Compare(a.GetName(), b.GetName()))
	})
	for _, a := range sortedAgents(m.Tools) {
		at := &gwv1.AgentTools{Agent: agentToProto(a)}
		for _, g := range m.Tools[a] {
			at.Grants = append(at.Grants, &gwv1.ToolGrant{Target: g.Target, Tools: sortedCopy(g.Tools), All: g.All})
		}
		slices.SortStableFunc(at.Grants, func(x, y *gwv1.ToolGrant) int {
			return cmp.Or(cmp.Compare(x.GetTarget(), y.GetTarget()), slices.Compare(x.GetTools(), y.GetTools()), cmp.Compare(boolInt(x.GetAll()), boolInt(y.GetAll())))
		})
		out.Tools = append(out.Tools, at)
	}
	return out
}
