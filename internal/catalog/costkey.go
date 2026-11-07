// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/providerusage"
	"github.com/stateracloud/koinon/internal/secretstore"
)

// CostKeyColumn holds a provider's cost API key: a second, optional credential of a direct
// provider (an Anthropic or OpenAI admin key) with which the daily reconciliation reads the
// provider's cost API (ADR 0056, amendment 2026-11-06). It is never sent to the gateway.
const CostKeyColumn = "cost_api_key"

// CostKeyBinding is the AAD binding of a provider's cost API key.
func CostKeyBinding(org, id pgtype.UUID) secretstore.Binding {
	return secretstore.Binding{OrgID: org.String(), Table: ProvidersTable, RowID: id.String(), Column: CostKeyColumn}
}

// CostSources are the provider types whose cost API Koinon reads; a cost key is accepted only for
// a provider one of them serves.
var CostSources = providerusage.DefaultCosts(nil)

// OpenCostKey decrypts a provider's cost API key. Only koinon-reconciler calls it, for the daily
// reconciliation (package ledger).
func OpenCostKey(keys Opener, org, id pgtype.UUID, envelope []byte) (string, error) {
	if len(envelope) == 0 {
		return "", invalid("provider has no cost API key")
	}
	if keys == nil {
		return "", ErrNoKeys
	}
	var env secretstore.Envelope
	if err := json.Unmarshal(envelope, &env); err != nil {
		return "", invalid("provider cost key envelope is unreadable")
	}
	plain, err := keys.Open(CostKeyBinding(org, id), env)
	if err != nil {
		return "", err
	}
	defer clear(plain)
	return string(plain), nil
}

// ValidateCostKey checks the cost key and scope a provider of typ at rawURL would hold after a
// change: none, or a scope the type's cost source can read, for a provider the source serves.
func ValidateCostKey(typ, rawURL string, hasKey bool, scope string) error {
	if !hasKey {
		if scope != "" {
			return invalid("a cost scope needs a cost API key")
		}
		return nil
	}
	src, ok := CostSources.For(typ, rawURL)
	if !ok {
		return invalid("Koinon reads the cost API of anthropic and openai providers at their own API URL only; remove the cost API key first")
	}
	if !src.ValidScope(scope) {
		switch typ {
		case "anthropic":
			return invalid("the cost scope of an anthropic provider is its workspace ID (wrkspc_...) or \"default\"")
		default:
			return invalid("the cost scope of an openai provider is its project ID (proj_...) or its API key's ID (key_...)")
		}
	}
	return nil
}

// sealCostKey seals a cost API key for its provider row; "" is no key.
func (s *Service) sealCostKey(org, id pgtype.UUID, key string) ([]byte, error) {
	if key == "" {
		return nil, nil
	}
	if s.keys == nil {
		return nil, ErrNoKeys
	}
	env, err := s.keys.Seal(CostKeyBinding(org, id), []byte(key))
	if err != nil {
		return nil, fmt.Errorf("catalog: seal provider cost key: %w", err)
	}
	return marshalEnvelope(env)
}

// applyCostKey writes the patch's cost key and scope to out (the provider after the rest of the
// patch), or checks that the provider's stored ones still fit its type and URL. A removed key
// removes the scope with it.
func (s *Service) applyCostKey(ctx context.Context, q *store.Queries, org pgtype.UUID, out store.Provider, patch ProviderPatch) (store.Provider, error) {
	hasKey, scope := len(out.CostApiKey) > 0, out.CostScope.String
	if patch.CostAPIKey != nil {
		hasKey = *patch.CostAPIKey != ""
		if !hasKey && patch.CostScope == nil {
			scope = ""
		}
	}
	if patch.CostScope != nil {
		scope = *patch.CostScope
	}
	if err := ValidateCostKey(out.Type, out.Url, hasKey, scope); err != nil {
		return out, err
	}
	if patch.CostAPIKey == nil && patch.CostScope == nil {
		return out, nil
	}
	var env []byte
	if patch.CostAPIKey != nil {
		var err error
		if env, err = s.sealCostKey(org, out.ID, *patch.CostAPIKey); err != nil {
			return out, err
		}
	}
	return q.SetProviderCostKey(ctx, store.SetProviderCostKeyParams{
		OrgID: org, ID: out.ID, SetKey: patch.CostAPIKey != nil, CostApiKey: env, CostScope: text(scope),
	})
}
