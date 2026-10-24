// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
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
}

// ProviderPatch changes a provider; nil fields stay.
type ProviderPatch struct {
	Type *string
	URL  *string
	// APIKey replaces the key; a pointer to "" removes it.
	APIKey *string
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
			Source: store.ConfigSourceUi, CreatedBy: creator,
		})
		if err != nil {
			return err
		}
		_, err = audit.Record(ctx, q, audit.Event{
			Org: org, Actor: actor, Action: "provider.create", TargetKind: "provider", TargetID: id.String(),
			After: providerSummary(row),
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
		typ, u, key := cur.Type, cur.Url, cur.ApiKey
		if patch.Type != nil {
			typ = *patch.Type
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
		out, err = q.UpdateProvider(ctx, store.UpdateProviderParams{OrgID: org, ID: pid, Type: typ, Url: u, ApiKey: key})
		if err != nil {
			return err
		}
		after := providerSummary(out)
		after["keyChanged"] = patch.APIKey != nil
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
		return audit.RecordErr(ctx, q, audit.Event{
			Org: org, Actor: actor, Action: "provider.delete", TargetKind: "provider", TargetID: id, Before: providerSummary(cur),
		})
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
	// Rates are refused: a model's price is an org price entry now (ADR 0056).
	Rates *Rates
}

// ModelPatch changes a model; nil fields stay.
type ModelPatch struct {
	Guard *bool
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
		reason := pgtype.Text{}
		if prov.DisabledReason.Valid {
			reason = pgtype.Text{String: ReasonProviderDisabled, Valid: true}
		}
		row, err = q.CreateModel(ctx, store.CreateModelParams{
			OrgID: org, ID: id, ProviderID: pid, Name: in.Name, Guard: in.Guard,
			Source:         store.ConfigSourceUi,
			DisabledReason: reason, CreatedBy: creator,
		})
		if err != nil {
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
		out, err = q.UpdateModel(ctx, store.UpdateModelParams{OrgID: org, ID: mid, Guard: guard})
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
		return audit.RecordErr(ctx, q, audit.Event{
			Org: org, Actor: actor, Action: "model.delete", TargetKind: "model", TargetID: id, Before: modelSummary(cur),
		})
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

// OpenKey decrypts a provider's API key, or returns "" for a keyless provider. Only the
// reconciler calls it, to deliver keys to the gateway.
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
