// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/internal/outward"
)

// DeclaredModel is a model of a declared provider.
type DeclaredModel struct {
	Name  string
	Guard bool
	// Format overrides the model's wire format; "" means the default (ADR 0069).
	Format string
}

// DeclaredProvider is a provider declared by a Kubernetes Secret of type model-provider.
type DeclaredProvider struct {
	// SecretName is the declaring Secret; it is the audit actor.
	SecretName string
	// Version identifies the Secret's content ("<uid>/<resourceVersion>"). Applying the same
	// version again changes nothing.
	Version string
	Name    string
	Type    string
	URL     string
	APIKey  string
	// AnthropicWorkspaceID is for type anthropic only.
	AnthropicWorkspaceID string
	// CostAPIKey and CostScope are the optional cost API key and its scope (anthropic and openai at
	// their own API URL). The Secret is the source of truth: leaving them out removes a stored one.
	CostAPIKey string
	CostScope  string
	Models     []DeclaredModel
	// Disabled is the Secret's disabled key, applied in the transaction of the upsert (ADR 0110 decision 5).
	Disabled bool
	// Warnings are things the Secret declares that Koinon ignores (the removed per-model rates).
	Warnings []string
}

// ApplyDeclaredProvider creates or updates the provider and models a Secret declares, and
// reports whether anything changed. Declared entries win name collisions: UI entries of the
// same names are disabled until the declared ones go. A provider name declared by another
// Secret is an error; so is a model name.
func (s *Service) ApplyDeclaredProvider(ctx context.Context, org pgtype.UUID, d DeclaredProvider) (changed bool, err error) {
	if err := ValidateProvider(d.Name, d.Type, d.URL); err != nil {
		return false, err
	}
	if err := ValidateWorkspaceID(d.Type, d.AnthropicWorkspaceID); err != nil {
		return false, err
	}
	if err := s.policy.Check(ctx, d.URL); err != nil {
		return false, err
	}
	if err := ValidateCostKey(d.Type, d.URL, d.CostAPIKey != "", d.CostScope); err != nil {
		return false, err
	}
	seen := map[string]bool{}
	for _, m := range d.Models {
		if err := ValidateModel(m.Name, m.Guard, d.Type); err != nil {
			return false, fmt.Errorf("model %q: %w", m.Name, err)
		}
		if err := ValidateModelFormat(d.Type, m.Format); err != nil {
			return false, fmt.Errorf("model %q: %w", m.Name, err)
		}
		if seen[m.Name] {
			return false, invalid("model %q is declared twice", m.Name)
		}
		seen[m.Name] = true
	}
	actor := audit.Secret(d.SecretName)
	_, err = audit.InPgTx(ctx, s.pool, func(tx pgx.Tx, b audit.Batch) error {
		q := store.New(tx)
		all, err := q.ListProviders(ctx, org)
		if err != nil {
			return err
		}
		var cur *store.Provider
		for i, p := range all {
			if p.Source != store.ConfigSourceSecret {
				continue
			}
			switch {
			case p.Name == d.Name && p.ManagedBySecret.String != d.SecretName:
				return invalid("provider %q is already declared by the Secret %s", d.Name, p.ManagedBySecret.String)
			case p.Name == d.Name:
				cur = &all[i]
			case p.ManagedBySecret.String == d.SecretName:
				// The Secret renamed its provider: the old entry goes.
				if err := s.removeProvider(ctx, q, b, org, p); err != nil {
					return err
				}
			}
		}
		// A lost key or cost key (keys forget) is written again from the Secret, which still holds it.
		if cur != nil && cur.DeclaredVersion.String == d.Version && !cur.RemovedAt.Valid && !cur.CredentialLostAt.Valid && !cur.CostApiKeyLostAt.Valid {
			return nil
		}
		if err := q.ShadowUIProviders(ctx, store.ShadowUIProvidersParams{OrgID: org, Name: d.Name, Reason: pgtype.Text{String: ReasonShadowedProvider, Valid: true}}); err != nil {
			return err
		}
		var id pgtype.UUID
		var row store.Provider
		var before map[string]any
		if cur == nil {
			if id, err = newID(); err != nil {
				return err
			}
			key, err := s.seal(org, id, d.APIKey)
			if err != nil {
				return err
			}
			row, err = q.CreateProvider(ctx, store.CreateProviderParams{
				OrgID: org, ID: id, Name: d.Name, Type: d.Type, Url: d.URL, ApiKey: key, Source: store.ConfigSourceSecret,
				AnthropicWorkspaceID: text(d.AnthropicWorkspaceID),
				ManagedBySecret:      pgtype.Text{String: d.SecretName, Valid: true},
				DeclaredVersion:      pgtype.Text{String: d.Version, Valid: true},
			})
			if err != nil {
				return err
			}
		} else {
			id, before = cur.ID, providerSummary(*cur)
			key, err := s.seal(org, id, d.APIKey)
			if err != nil {
				return err
			}
			row, err = q.UpdateDeclaredProvider(ctx, store.UpdateDeclaredProviderParams{
				OrgID: org, ID: id, Type: d.Type, Url: d.URL, ApiKey: key, AnthropicWorkspaceID: text(d.AnthropicWorkspaceID),
				DeclaredVersion: pgtype.Text{String: d.Version, Valid: true},
			})
			if err != nil {
				return err
			}
		}
		// Always applied: a Secret that no longer declares a cost key removes the stored one.
		if row, err = s.applyCostKey(ctx, q, org, row, ProviderPatch{CostAPIKey: &d.CostAPIKey, CostScope: &d.CostScope}); err != nil {
			return err
		}
		if err := s.syncDeclaredModels(ctx, q, b, org, actor, row, d); err != nil {
			return err
		}
		if err := refreshShadows(ctx, q, org); err != nil {
			return err
		}
		after := providerSummary(row)
		names := make([]string, 0, len(d.Models))
		for _, m := range d.Models {
			names = append(names, m.Name)
		}
		slices.Sort(names)
		after["models"] = names
		changed = true
		if d.Disabled {
			if _, err := outward.DisableDeclared(ctx, tx, b, org, outward.ModelProvider, d.SecretName); err != nil {
				return err
			}
		}
		b.Org(audit.Event{
			Org: org, Actor: actor, Action: "provider.declare", TargetKind: "provider", TargetID: id.String(),
			Before: before, After: after,
		})
		return nil
	})
	if err != nil {
		return false, mapErr(err)
	}
	return changed, nil
}

func (s *Service) syncDeclaredModels(ctx context.Context, q *store.Queries, b audit.Batch, org pgtype.UUID, actor audit.Actor, prov store.Provider, d DeclaredProvider) error {
	existing, err := q.ListModels(ctx, org)
	if err != nil {
		return err
	}
	have := map[string]store.Model{}
	for _, m := range existing {
		if m.ProviderID == prov.ID {
			have[m.Name] = m
		}
	}
	for _, m := range d.Models {
		format, overridden, err := modelformat.Resolve(prov.Type, m.Name, m.Format)
		if err != nil {
			return invalid("model %q: %s", m.Name, err.Error())
		}
		if err := q.ShadowUIModels(ctx, store.ShadowUIModelsParams{OrgID: org, Name: m.Name, Reason: pgtype.Text{String: ReasonShadowedModel, Valid: true}}); err != nil {
			return err
		}
		if cur, ok := have[m.Name]; ok {
			delete(have, m.Name)
			if _, err := q.UpdateModel(ctx, store.UpdateModelParams{OrgID: org, ID: cur.ID, Guard: m.Guard, Format: string(format), FormatOverridden: overridden}); err != nil {
				return err
			}
			// A model of a provider that was removed and came back is served again.
			if err := q.EnableModel(ctx, store.EnableModelParams{OrgID: org, ID: cur.ID}); err != nil {
				return err
			}
			continue
		}
		id, err := newID()
		if err != nil {
			return err
		}
		if _, err := q.CreateModel(ctx, store.CreateModelParams{
			OrgID: org, ID: id, ProviderID: prov.ID, Name: m.Name, Guard: m.Guard,
			Format: string(format), FormatOverridden: overridden,
			Source:          store.ConfigSourceSecret,
			ManagedBySecret: pgtype.Text{String: d.SecretName, Valid: true},
		}); err != nil {
			if isUnique(err) {
				return invalid("model %q is already declared by another Secret", m.Name)
			}
			return err
		}
		if err := adoptDefaultModel(ctx, q, b, org, actor, m.Name); err != nil {
			return err
		}
	}
	for _, gone := range have {
		if _, err := q.DeleteModel(ctx, store.DeleteModelParams{OrgID: org, ID: gone.ID}); err != nil {
			return err
		}
	}
	if len(have) > 0 {
		return clearDefaultIfGone(ctx, q, b, org, actor)
	}
	return nil
}

// refreshShadows recomputes which UI entries a declared entry disables.
func refreshShadows(ctx context.Context, q *store.Queries, org pgtype.UUID) error {
	if err := q.ReenableUIProviders(ctx, org); err != nil {
		return err
	}
	if err := q.DisableModelsOfDisabledProviders(ctx, store.DisableModelsOfDisabledProvidersParams{OrgID: org, Reason: pgtype.Text{String: ReasonProviderDisabled, Valid: true}}); err != nil {
		return err
	}
	return q.ReenableUIModels(ctx, org)
}

// removeProvider marks a declared provider removed: it and its models stay as rows, disabled, so
// the audit trail and anything that refers to them keep a target, and the Secret coming back
// restores them.
func (s *Service) removeProvider(ctx context.Context, q *store.Queries, b audit.Batch, org pgtype.UUID, p store.Provider) error {
	if p.RemovedAt.Valid {
		return nil
	}
	reason := pgtype.Text{String: ReasonRemoved, Valid: true}
	if err := q.MarkProviderRemoved(ctx, store.MarkProviderRemovedParams{OrgID: org, ID: p.ID, Reason: reason}); err != nil {
		return err
	}
	if err := q.DisableModelsOfProvider(ctx, store.DisableModelsOfProviderParams{OrgID: org, ProviderID: p.ID, Reason: reason}); err != nil {
		return err
	}
	b.Org(audit.Event{
		Org: org, Actor: audit.Secret(p.ManagedBySecret.String), Action: "provider.undeclare", TargetKind: "provider",
		TargetID: p.ID.String(), Before: providerSummary(p), After: map[string]any{"removed": true},
	})
	return nil
}

// DeclaredProviderSecrets returns the names of the Secrets that declare providers in org.
func (s *Service) DeclaredProviderSecrets(ctx context.Context, org pgtype.UUID) ([]string, error) {
	rows, err := store.New(s.pool).ListProviders(ctx, org)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range rows {
		if p.Source == store.ConfigSourceSecret && !p.RemovedAt.Valid && !slices.Contains(out, p.ManagedBySecret.String) {
			out = append(out, p.ManagedBySecret.String)
		}
	}
	return out, nil
}

// RemoveDeclared marks what the Secret declared in org removed (its Secret was deleted), and
// re-enables the UI entries it shadowed. The rows stay until the Secret returns or an admin
// deletes them.
func (s *Service) RemoveDeclared(ctx context.Context, org pgtype.UUID, secretName string) error {
	return mapErr(s.tx(ctx, func(q *store.Queries, b audit.Batch) error {
		all, err := q.ListProviders(ctx, org)
		if err != nil {
			return err
		}
		for _, p := range all {
			if p.Source == store.ConfigSourceSecret && p.ManagedBySecret.String == secretName {
				if err := s.removeProvider(ctx, q, b, org, p); err != nil {
					return err
				}
			}
		}
		return refreshShadows(ctx, q, org)
	}))
}
