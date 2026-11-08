// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/internal/secretstore"
)

// Opener decrypts envelopes; *secretstore.Keyring is one.
type Opener interface {
	Open(b secretstore.Binding, e secretstore.Envelope) ([]byte, error)
}

func marshalEnvelope(e secretstore.Envelope) ([]byte, error) { return json.Marshal(e) }

// ProviderInput creates a provider.
type ProviderInput struct {
	Name string
	Type string
	URL  string
	// APIKey is empty for keyless upstreams such as Ollama.
	APIKey string
	// AnthropicWorkspaceID is for type anthropic only.
	AnthropicWorkspaceID string
	// CostAPIKey is the optional cost API key (an admin key that reads the provider's cost API, for
	// the daily reconciliation); CostScope names the workspace, project or API key whose cost is
	// the provider's. A key needs a scope.
	CostAPIKey string
	CostScope  string
}

// ProviderPatch changes a provider; nil fields stay.
type ProviderPatch struct {
	Type *string
	URL  *string
	// APIKey replaces the key; a pointer to "" removes it.
	APIKey *string
	// AnthropicWorkspaceID replaces the workspace ID; a pointer to "" removes it.
	AnthropicWorkspaceID *string
	// CostAPIKey replaces the cost API key (an admin key that reads the provider's cost API, for the
	// daily reconciliation); a pointer to "" removes it, and its scope with it.
	CostAPIKey *string
	// CostScope replaces the cost scope: the workspace (anthropic), or the project or API key ID
	// (openai), whose cost is the provider key's.
	CostScope *string
}

// ListProviders returns the org's providers, declared and entered.
func (s *Service) ListProviders(ctx context.Context, org pgtype.UUID) ([]Provider, error) {
	rows, err := store.New(s.pool).ListProviders(ctx, org)
	if err != nil {
		return nil, err
	}
	out := make([]Provider, 0, len(rows))
	for _, r := range rows {
		out = append(out, toProvider(r))
	}
	return out, nil
}

// GetProvider returns one provider.
func (s *Service) GetProvider(ctx context.Context, org pgtype.UUID, id string) (Provider, error) {
	pid, err := parseID(id)
	if err != nil {
		return Provider{}, err
	}
	row, err := store.New(s.pool).GetProvider(ctx, store.GetProviderParams{OrgID: org, ID: pid})
	if err != nil {
		return Provider{}, mapErr(err)
	}
	return toProvider(row), nil
}

// CreateProvider adds a provider entered by a person.
func (s *Service) CreateProvider(ctx context.Context, org pgtype.UUID, actor audit.Actor, creator pgtype.UUID, in ProviderInput) (Provider, error) {
	if err := ValidateProvider(in.Name, in.Type, in.URL); err != nil {
		return Provider{}, err
	}
	if err := ValidateWorkspaceID(in.Type, in.AnthropicWorkspaceID); err != nil {
		return Provider{}, err
	}
	if err := s.policy.Check(ctx, in.URL); err != nil {
		return Provider{}, err
	}
	id, err := newID()
	if err != nil {
		return Provider{}, err
	}
	key, err := s.seal(org, id, in.APIKey)
	if err != nil {
		return Provider{}, err
	}
	var row store.Provider
	err = s.tx(ctx, func(q *store.Queries) error {
		row, err = q.CreateProvider(ctx, store.CreateProviderParams{
			OrgID: org, ID: id, Name: in.Name, Type: in.Type, Url: in.URL, ApiKey: key,
			AnthropicWorkspaceID: text(in.AnthropicWorkspaceID),
			Source:               store.ConfigSourceUi, CreatedBy: creator,
		})
		if err != nil {
			return err
		}
		if in.CostAPIKey != "" || in.CostScope != "" {
			// The same checks and sealing as an update; a failure rolls the provider back.
			if row, err = s.applyCostKey(ctx, q, org, row, ProviderPatch{CostAPIKey: &in.CostAPIKey, CostScope: &in.CostScope}); err != nil {
				return err
			}
		}
		after := providerSummary(row)
		after["costKeyChanged"] = in.CostAPIKey != ""
		_, err = audit.Record(ctx, q, audit.Event{
			Org: org, Actor: actor, Action: "provider.create", TargetKind: "provider", TargetID: id.String(),
			After: after,
		})
		return err
	})
	if err != nil {
		return Provider{}, mapErr(err)
	}
	return toProvider(row), nil
}

// UpdateProvider changes a provider entered by a person.
func (s *Service) UpdateProvider(ctx context.Context, org pgtype.UUID, actor audit.Actor, id string, patch ProviderPatch) (Provider, error) {
	pid, err := parseID(id)
	if err != nil {
		return Provider{}, err
	}
	var out store.Provider
	err = s.tx(ctx, func(q *store.Queries) error {
		cur, err := q.GetProvider(ctx, store.GetProviderParams{OrgID: org, ID: pid})
		if err != nil {
			return err
		}
		if cur.Source != store.ConfigSourceUi {
			return readOnly(cur.ManagedBySecret.String)
		}
		typ, u, key, ws := cur.Type, cur.Url, cur.ApiKey, cur.AnthropicWorkspaceID.String
		if patch.Type != nil {
			typ = *patch.Type
		}
		if patch.AnthropicWorkspaceID != nil {
			ws = *patch.AnthropicWorkspaceID
		}
		if err := ValidateWorkspaceID(typ, ws); err != nil {
			return err
		}
		if patch.URL != nil {
			u = *patch.URL
		}
		if err := validateTypeURL(typ, u); err != nil {
			return err
		}
		if err := s.policy.Check(ctx, u); err != nil {
			return err
		}
		if patch.APIKey != nil {
			if key, err = s.seal(org, pid, *patch.APIKey); err != nil {
				return err
			}
		}
		if typ != GuardProviderType {
			models, err := q.ListModels(ctx, org)
			if err != nil {
				return err
			}
			for _, m := range models {
				if m.ProviderID == pid && m.Guard {
					return invalid("model %q has the routing guard on, which exists for %s providers only", m.Name, GuardProviderType)
				}
			}
		}
		// The key is rewritten only when the patch sets it: cur was read without a lock, and a
		// concurrent `keys rewrap` or `reencrypt` may have replaced the envelope since.
		out, err = q.UpdateProvider(ctx, store.UpdateProviderParams{
			OrgID: org, ID: pid, Type: typ, Url: u, SetApiKey: patch.APIKey != nil, ApiKey: key,
			AnthropicWorkspaceID: text(ws),
		})
		if err != nil {
			return err
		}
		if typ != cur.Type {
			if err := refitModelFormats(ctx, q, org, actor, out); err != nil {
				return err
			}
		}
		if out, err = s.applyCostKey(ctx, q, org, out, patch); err != nil {
			return err
		}
		after := providerSummary(out)
		after["keyChanged"] = patch.APIKey != nil
		after["costKeyChanged"] = patch.CostAPIKey != nil
		_, err = audit.Record(ctx, q, audit.Event{
			Org: org, Actor: actor, Action: "provider.update", TargetKind: "provider", TargetID: id,
			Before: providerSummary(cur), After: after,
		})
		return err
	})
	if err != nil {
		return Provider{}, mapErr(err)
	}
	return toProvider(out), nil
}

// DeleteProvider removes a provider entered by a person, with its models.
func (s *Service) DeleteProvider(ctx context.Context, org pgtype.UUID, actor audit.Actor, id string) error {
	pid, err := parseID(id)
	if err != nil {
		return err
	}
	err = s.tx(ctx, func(q *store.Queries) error {
		cur, err := q.GetProvider(ctx, store.GetProviderParams{OrgID: org, ID: pid})
		if err != nil {
			return err
		}
		// A declared provider whose Secret is gone may be deleted for good; a live one only changes
		// in its Secret.
		if cur.Source != store.ConfigSourceUi && !cur.RemovedAt.Valid {
			return readOnly(cur.ManagedBySecret.String)
		}
		if _, err := q.DeleteProvider(ctx, store.DeleteProviderParams{OrgID: org, ID: pid}); err != nil {
			return err
		}
		if err := audit.RecordErr(ctx, q, audit.Event{
			Org: org, Actor: actor, Action: "provider.delete", TargetKind: "provider", TargetID: id, Before: providerSummary(cur),
		}); err != nil {
			return err
		}
		return clearDefaultIfGone(ctx, q, org, actor)
	})
	return mapErr(err)
}

// ListModels returns the org's models.
func (s *Service) ListModels(ctx context.Context, org pgtype.UUID) ([]Model, error) {
	rows, err := store.New(s.pool).ListModels(ctx, org)
	if err != nil {
		return nil, err
	}
	out := make([]Model, 0, len(rows))
	for _, r := range rows {
		out = append(out, toModel(r))
	}
	return out, nil
}

// GetModel returns one model.
func (s *Service) GetModel(ctx context.Context, org pgtype.UUID, id string) (Model, error) {
	mid, err := parseID(id)
	if err != nil {
		return Model{}, err
	}
	row, err := store.New(s.pool).GetModel(ctx, store.GetModelParams{OrgID: org, ID: mid})
	if err != nil {
		return Model{}, mapErr(err)
	}
	return toModel(row), nil
}

// ModelInput creates a model.
type ModelInput struct {
	ProviderID string
	Name       string
	Guard      bool
	// Format overrides the model's wire format, within what the provider offers; "" is the
	// default (ADR 0069).
	Format string
	// Rates are refused: a model's price is an org price entry now (ADR 0056).
	Rates *Rates
}

// ModelPatch changes a model; nil fields stay.
type ModelPatch struct {
	Guard *bool
	// Format overrides the format; a pointer to "" goes back to the default.
	Format *string
	// Rates and ClearRates are refused, as in ModelInput.
	Rates      *Rates
	ClearRates bool
}

// CreateModel adds a model to a provider entered by a person.
func (s *Service) CreateModel(ctx context.Context, org pgtype.UUID, actor audit.Actor, creator pgtype.UUID, in ModelInput) (Model, error) {
	if in.Rates != nil {
		return Model{}, invalid(ratesRemoved)
	}
	pid, err := parseID(in.ProviderID)
	if err != nil {
		return Model{}, err
	}
	id, err := newID()
	if err != nil {
		return Model{}, err
	}
	var row store.Model
	err = s.tx(ctx, func(q *store.Queries) error {
		prov, err := q.GetProvider(ctx, store.GetProviderParams{OrgID: org, ID: pid})
		if err != nil {
			return err
		}
		if prov.Source != store.ConfigSourceUi {
			return readOnly(prov.ManagedBySecret.String)
		}
		if err := ValidateModel(in.Name, in.Guard, prov.Type); err != nil {
			return err
		}
		format, overridden, err := modelformat.Resolve(prov.Type, in.Name, in.Format)
		if err != nil {
			return invalid("%s", err.Error())
		}
		reason := pgtype.Text{}
		if prov.DisabledReason.Valid {
			reason = pgtype.Text{String: ReasonProviderDisabled, Valid: true}
		}
		row, err = q.CreateModel(ctx, store.CreateModelParams{
			OrgID: org, ID: id, ProviderID: pid, Name: in.Name, Guard: in.Guard,
			Format: string(format), FormatOverridden: overridden,
			Source:         store.ConfigSourceUi,
			DisabledReason: reason, CreatedBy: creator,
		})
		if err != nil {
			return err
		}
		if err := adoptDefaultModel(ctx, q, org, actor, row.Name); err != nil {
			return err
		}
		return audit.RecordErr(ctx, q, audit.Event{
			Org: org, Actor: actor, Action: "model.create", TargetKind: "model", TargetID: id.String(), After: modelSummary(row),
		})
	})
	if err != nil {
		return Model{}, mapErr(err)
	}
	return toModel(row), nil
}

// UpdateModel changes a model entered by a person.
func (s *Service) UpdateModel(ctx context.Context, org pgtype.UUID, actor audit.Actor, id string, patch ModelPatch) (Model, error) {
	if patch.Rates != nil || patch.ClearRates {
		return Model{}, invalid(ratesRemoved)
	}
	mid, err := parseID(id)
	if err != nil {
		return Model{}, err
	}
	var out store.Model
	err = s.tx(ctx, func(q *store.Queries) error {
		cur, err := q.GetModel(ctx, store.GetModelParams{OrgID: org, ID: mid})
		if err != nil {
			return err
		}
		if cur.Source != store.ConfigSourceUi {
			return readOnly(cur.ManagedBySecret.String)
		}
		prov, err := q.GetProvider(ctx, store.GetProviderParams{OrgID: org, ID: cur.ProviderID})
		if err != nil {
			return err
		}
		guard := cur.Guard
		if patch.Guard != nil {
			guard = *patch.Guard
		}
		if err := ValidateModel(cur.Name, guard, prov.Type); err != nil {
			return err
		}
		format, overridden := cur.Format, cur.FormatOverridden
		if patch.Format != nil {
			f, o, err := modelformat.Resolve(prov.Type, cur.Name, *patch.Format)
			if err != nil {
				return invalid("%s", err.Error())
			}
			format, overridden = string(f), o
		}
		out, err = q.UpdateModel(ctx, store.UpdateModelParams{OrgID: org, ID: mid, Guard: guard, Format: format, FormatOverridden: overridden})
		if err != nil {
			return err
		}
		return audit.RecordErr(ctx, q, audit.Event{
			Org: org, Actor: actor, Action: "model.update", TargetKind: "model", TargetID: id,
			Before: modelSummary(cur), After: modelSummary(out),
		})
	})
	if err != nil {
		return Model{}, mapErr(err)
	}
	return toModel(out), nil
}

// DeleteModel removes a model entered by a person.
func (s *Service) DeleteModel(ctx context.Context, org pgtype.UUID, actor audit.Actor, id string) error {
	mid, err := parseID(id)
	if err != nil {
		return err
	}
	err = s.tx(ctx, func(q *store.Queries) error {
		cur, err := q.GetModel(ctx, store.GetModelParams{OrgID: org, ID: mid})
		if err != nil {
			return err
		}
		if cur.Source != store.ConfigSourceUi {
			return readOnly(cur.ManagedBySecret.String)
		}
		if _, err := q.DeleteModel(ctx, store.DeleteModelParams{OrgID: org, ID: mid}); err != nil {
			return err
		}
		if err := audit.RecordErr(ctx, q, audit.Event{
			Org: org, Actor: actor, Action: "model.delete", TargetKind: "model", TargetID: id, Before: modelSummary(cur),
		}); err != nil {
			return err
		}
		return clearDefaultIfGone(ctx, q, org, actor)
	})
	return mapErr(err)
}

func parseID(s string) (pgtype.UUID, error) {
	var id pgtype.UUID
	if err := id.Scan(s); err != nil || !id.Valid {
		return id, ErrNotFound // an ID that is no UUID names nothing
	}
	return id, nil
}

// OpenKey decrypts a provider's API key, or returns "" for a keyless provider. Only
// koinon-reconciler calls it: to deliver keys to the gateway, and for the spend settler to ask a
// provider what it billed for a response (package ledger).
func OpenKey(keys Opener, org, id pgtype.UUID, envelope []byte) (string, error) {
	if len(envelope) == 0 {
		return "", nil
	}
	if keys == nil {
		return "", ErrNoKeys
	}
	var env secretstore.Envelope
	if err := json.Unmarshal(envelope, &env); err != nil {
		return "", invalid("provider key envelope is unreadable")
	}
	plain, err := keys.Open(KeyBinding(org, id), env)
	if err != nil {
		return "", err
	}
	defer clear(plain)
	return string(plain), nil
}

// text is a nullable text column: NULL for "".
func text(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

// refitModelFormats follows a provider's type change (ADR 0069): a model whose format an admin set
// must still be offered by the new type, or the change is refused; every other model takes the
// new type's default.
func refitModelFormats(ctx context.Context, q *store.Queries, org pgtype.UUID, actor audit.Actor, prov store.Provider) error {
	models, err := q.ListModels(ctx, org)
	if err != nil {
		return err
	}
	for _, m := range models {
		if m.ProviderID != prov.ID {
			continue
		}
		if m.FormatOverridden {
			if err := modelformat.Validate(prov.Type, modelformat.Format(m.Format)); err != nil {
				return invalid("model %q is set to the %s format, which a %s provider does not offer; reset the model to the default, or choose a format both types offer", m.Name, m.Format, prov.Type)
			}
			continue
		}
		want := string(modelformat.Default(prov.Type, m.Name))
		if want == m.Format {
			continue
		}
		after, err := q.UpdateModel(ctx, store.UpdateModelParams{OrgID: org, ID: m.ID, Guard: m.Guard, Format: want})
		if err != nil {
			return err
		}
		if err := audit.RecordErr(ctx, q, audit.Event{
			Org: org, Actor: actor, Action: "model.update", TargetKind: "model", TargetID: m.ID.String(),
			Before: modelSummary(m), After: modelSummary(after),
		}); err != nil {
			return err
		}
	}
	return nil
}

// clearDefaultIfGone clears the org's default model when the model it names no longer exists, in
// the transaction that deleted the model, and audits it (old issue 256). It never picks another model: that
// silent change is what the stored default prevents. The agents without a model setting then fail
// with agentmodel.NoDefaultMessage until an admin sets a default.
func clearDefaultIfGone(ctx context.Context, q *store.Queries, org pgtype.UUID, actor audit.Actor) error {
	old, err := q.ClearOrgDefaultModelIfGone(ctx, org)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // the default stays, or there was none
	} else if err != nil {
		return err
	}
	return audit.RecordErr(ctx, q, audit.Event{
		Org: org, Actor: actor, Action: "org.default_model.set", TargetKind: "org", TargetID: org.String(),
		Before: map[string]any{"model": old}, After: map[string]any{"model": "", "reason": "model deleted"},
	})
}

// adoptDefaultModel makes the org's first model its default model (old issue 256). An org that has a default
// keeps it: a later provider or model never moves the agents that run on it. The change is audited.
func adoptDefaultModel(ctx context.Context, q *store.Queries, org pgtype.UUID, actor audit.Actor, model string) error {
	n, err := q.SetOrgDefaultModelIfUnset(ctx, store.SetOrgDefaultModelIfUnsetParams{OrgID: org, DefaultModel: model})
	if err != nil || n == 0 {
		return err
	}
	return audit.RecordErr(ctx, q, audit.Event{
		Org: org, Actor: actor, Action: "org.default_model.set", TargetKind: "org", TargetID: org.String(),
		Before: map[string]any{"model": ""}, After: map[string]any{"model": model, "reason": "first model"},
	})
}
