// SPDX-License-Identifier: Apache-2.0

package codehost

import (
	"fmt"
	"slices"

	"github.com/stateracloud/koinon/connectors"
	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/templates"
)

// Registry maps a connector type ID to its Provider.
type Registry struct {
	byID map[string]Provider
	ids  []string
}

// NewRegistry returns a Registry of providers; a later provider with the same ID replaces an
// earlier one.
func NewRegistry(providers ...Provider) *Registry {
	r := &Registry{byID: map[string]Provider{}}
	for _, p := range providers {
		if _, dup := r.byID[p.ID()]; !dup {
			r.ids = append(r.ids, p.ID())
		}
		r.byID[p.ID()] = p
	}
	slices.Sort(r.ids)
	return r
}

// Get returns the provider of a connector type.
func (r *Registry) Get(connector string) (Provider, bool) {
	if r == nil {
		return nil, false
	}
	p, ok := r.byID[connector]
	return p, ok
}

// IDs are the registered connector type IDs, sorted.
func (r *Registry) IDs() []string {
	if r == nil {
		return nil
	}
	return slices.Clone(r.ids)
}

// Type is the connector type of a registered provider.
func (r *Registry) Type(connector string) (*connectors.Type, bool) {
	if _, ok := r.Get(connector); !ok {
		return nil, false
	}
	return connectors.Get(connector)
}

// Grant returns the agent's grant on a code host and the connector it is on. An agent holds at
// most one; if config names several the first in ID order wins.
func (r *Registry) Grant(cfg agentconfig.Config) (connector string, g templates.Grant, ok bool) {
	for _, id := range r.IDs() {
		if g, ok := cfg.Grant(id); ok {
			return id, g, true
		}
	}
	return "", templates.Grant{}, false
}

// Binding returns the agent's binding on a code host and the connector it is on.
func (r *Registry) Binding(cfg agentconfig.Config) (connector string, b agentconfig.Binding, ok bool) {
	for _, id := range r.IDs() {
		if b, ok := cfg.Binding(id); ok {
			return id, b, true
		}
	}
	return "", agentconfig.Binding{}, false
}

// ReadGrant is the agent's forge read grant: a grant on a code host with at least one scope that has
// network rules, on a binding with at least one bound repository. ok is false when cfg has none. It is
// the one rule for "this agent expects a forge credential" (#1086 part C2): koinon-reconciler renders the
// forge routes and the credential from it (reconciler.GitHubReadAccessAt), and koinon-agentd waits for
// the credential when it holds. err is set when the grant names a connector type that is not embedded.
func (r *Registry) ReadGrant(cfg agentconfig.Config) (rg ReadGrant, ok bool, err error) {
	connector, g, found := r.Grant(cfg)
	if !found {
		return ReadGrant{}, false, nil
	}
	ct, found := connectors.Get(connector)
	if !found {
		return ReadGrant{}, false, fmt.Errorf("the %s connector type is not embedded", connector)
	}
	var scopes []string
	for _, s := range g.Scopes {
		if ct.HasNetwork(g.Preset, []string{s}) {
			scopes = append(scopes, s)
		}
	}
	b, bound := cfg.Binding(connector)
	if len(scopes) == 0 || !bound || len(b.Targets) == 0 {
		return ReadGrant{}, false, nil
	}
	return ReadGrant{Connector: connector, Type: ct, Preset: g.Preset, Scopes: scopes, Binding: b}, true, nil
}

// ReadGrant is what Registry.ReadGrant found.
type ReadGrant struct {
	// Connector is the connector type ID.
	Connector string
	Type      *connectors.Type
	Preset    templates.Preset
	// Scopes are the grant's scopes that have network rules.
	Scopes  []string
	Binding agentconfig.Binding
}

// MustGet is Get for a provider the caller registered; it panics when there is none.
func (r *Registry) MustGet(connector string) Provider {
	p, ok := r.Get(connector)
	if !ok {
		panic("codehost: no provider for connector " + connector)
	}
	return p
}
