// SPDX-License-Identifier: Apache-2.0

// Package agentmodel decides which model an agent's run uses and manages the agent's own model
// setting (rc.6).
//
// The model of a run comes from, in order: the agent's setting (a mutable setting an admin
// changes without a new version), the active version's model (a template's spec.model), then
// the org default. Choose is that order; koinon-agentd applies it at run start and the UI shows
// its result with the layer it came from.
//
// A model is usable when the org serves it (an enabled provider and model) and has a price for it
// (the gateway serves priced models only, ADR 0041). Check is that test. The setting is checked
// when it is set and again when a run starts, because providers and prices change afterwards; a
// set model that fails the check fails the run with a reason that says so. It never falls back
// to another layer, because a silent fallback would run the agent on a model nobody chose.
//
// The model's wire format (ADR 0069) belongs to the model, not to the agent, so a model chosen
// here needs nothing more: the format is looked up by the resolved model name.
//
// The agent has a second setting of the same kind, whether its model thinks (thinking.go, ADR 0069
// amendment 2026-10-29): on unless turned off, resolved for the effective model.
package agentmodel

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/internal/prices"
)

// Default is the setting value that follows the version and the org default.
const Default = "default"

// Source is the layer a model comes from.
type Source string

// The layers, in order of precedence.
const (
	SourceAgent      Source = "agent"
	SourceVersion    Source = "version"
	SourceOrgDefault Source = "org_default"
	SourceNone       Source = ""
)

// Normalize returns the stored form of a requested setting: "" when it is the default.
func Normalize(m string) string {
	m = strings.TrimSpace(m)
	if m == Default {
		return ""
	}
	return m
}

// Choose applies the precedence to the agent's setting and the version's model. A layer that is
// empty or "default" is skipped; platformDefault is the operator's override of the org default
// (koinon-agentd --default-model), "" for none. It returns "" and SourceNone when no layer names a
// model: the caller then takes the org default (OrgDefault).
func Choose(setting, versionModel, platformDefault string) (string, Source) {
	if m := Normalize(setting); m != "" {
		return m, SourceAgent
	}
	if m := Normalize(versionModel); m != "" {
		return m, SourceVersion
	}
	if m := Normalize(platformDefault); m != "" {
		return m, SourceOrgDefault
	}
	return "", SourceNone
}

// Served is a model the org serves and has a price for.
type Served struct {
	Name     string
	Provider string
}

// UnusableError means a model is not served or not priced.
type UnusableError struct {
	Model string
	// Reason is "not_served", "unpriced" or "bad_price".
	Reason string
	// Detail carries the price error of "bad_price".
	Detail error
}

func (e *UnusableError) Error() string {
	switch e.Reason {
	case "unpriced":
		return fmt.Sprintf("the model %q has no price, so the gateway does not serve it", e.Model)
	case "bad_price":
		return fmt.Sprintf("the model %q is not served: %v", e.Model, e.Detail)
	}
	return fmt.Sprintf("the model %q is not served in this org", e.Model)
}

func (e *UnusableError) Unwrap() error { return e.Detail }

// RunReason is the run's failure reason for an agent whose set model is unusable.
func (e *UnusableError) RunReason() string {
	switch e.Reason {
	case "unpriced":
		return fmt.Sprintf("the agent's model %q has no price, so the gateway does not serve it; "+
			"add a price for it on the Prices page or pick another model on the agent page", e.Model)
	case "bad_price":
		return fmt.Sprintf("the agent's model %q is not served: %v; fix its price on the Prices page "+
			"or pick another model on the agent page", e.Model, e.Detail)
	}
	return fmt.Sprintf("the agent's model %q is not served in this org; pick another on the agent page", e.Model)
}

// Check reports whether the org serves the model and has a usable price for it. A nil result means
// yes; *UnusableError means no; any other error is a failure to find out.
func Check(ctx context.Context, rv prices.Resolver, q *store.Queries, org pgtype.UUID, model string, at time.Time) error {
	served, err := q.ListServedCatalog(ctx, org)
	if err != nil {
		return err
	}
	for _, m := range served {
		if m.ModelName != model {
			continue
		}
		pm := prices.Model{ID: m.ModelID.String(), Name: m.ModelName, ProviderType: m.ProviderType, Format: modelformat.Format(m.ModelFormat)}
		_, err := rv.Resolve(ctx, q, org, pm, at)
		var bad *prices.BadPriceError
		switch {
		case err == nil:
			return nil
		case errors.Is(err, prices.ErrUnpriced):
			return &UnusableError{Model: model, Reason: "unpriced"}
		case errors.As(err, &bad):
			return &UnusableError{Model: model, Reason: "bad_price", Detail: err}
		}
		return err
	}
	return &UnusableError{Model: model, Reason: "not_served"}
}

// ListServed returns the org's models that Check accepts, by provider and name. A model whose
// price cannot be told is left out.
func ListServed(ctx context.Context, rv prices.Resolver, q *store.Queries, org pgtype.UUID, at time.Time) ([]Served, error) {
	rows, err := q.ListServedCatalog(ctx, org)
	if err != nil {
		return nil, err
	}
	out := make([]Served, 0, len(rows))
	for _, m := range rows {
		pm := prices.Model{ID: m.ModelID.String(), Name: m.ModelName, ProviderType: m.ProviderType, Format: modelformat.Format(m.ModelFormat)}
		if _, err := rv.Resolve(ctx, q, org, pm, at); err == nil {
			out = append(out, Served{Name: m.ModelName, Provider: m.ProviderName})
		}
	}
	return out, nil
}

// OrgDefault is the org's stored default model (orgs.default_model, old issue 256): the model agents with
// no model setting run on. It is set by the org's first model or by an admin (Service.SetDefault)
// and nothing else changes it. "" when the org has none.
func OrgDefault(ctx context.Context, q *store.Queries, org pgtype.UUID) (string, error) {
	m, err := q.GetOrgDefaultModel(ctx, org)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return m, err
}

// NoDefaultMessage says what to do when an agent names no model and the org has no default model.
const NoDefaultMessage = "the agent names no model and its org has no default model; add a provider and a model with a price, or set the org's default model"

// GetDefault returns the org's default model, "" for none.
func (s *Service) GetDefault(ctx context.Context, org pgtype.UUID) (string, error) {
	return OrgDefault(ctx, store.New(s.pool), org)
}

// ListServed returns the models the org's default may be set to: the ones it serves and has a
// price for.
func (s *Service) ListServed(ctx context.Context, org pgtype.UUID, at time.Time) ([]Served, error) {
	return ListServed(ctx, s.prices, store.New(s.pool), org, at)
}

// SetDefault sets the org's default model, or clears it with "". The model must pass Check. The
// change and its audit event, with the old and new value, commit together; setting the value it
// already has writes no event.
func (s *Service) SetDefault(ctx context.Context, org pgtype.UUID, actor audit.Actor, model string, at time.Time) error {
	if len(model) > 256 {
		return &InvalidError{Msg: "the model name is longer than 256 characters"}
	}
	_, err := audit.InTx(ctx, s.pool, func(q *store.Queries, b audit.Batch) error {
		if model != "" {
			var u *UnusableError
			if err := Check(ctx, s.prices, q, org, model, at); errors.As(err, &u) && u.Reason == "not_served" {
				// Another org's model reads exactly like one that does not exist (org isolation).
				return fmt.Errorf("%w: the org serves no model named %q", ErrNotFound, model)
			} else if errors.As(err, &u) {
				return &InvalidError{Msg: u.Error() + "; pick a model the org serves and has a price for"}
			} else if err != nil {
				return err
			}
		}
		old, err := q.SetOrgDefaultModel(ctx, store.SetOrgDefaultModelParams{OrgID: org, DefaultModel: model})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if old == model {
			return nil
		}
		b.Org(audit.Event{
			Org: org, Actor: actor, Action: "org.default_model.set", TargetKind: "org", TargetID: org.String(),
			Before: map[string]any{"model": old}, After: map[string]any{"model": model},
		})
		return nil
	})
	return err
}

// View is an agent's model setting and what it resolves to.
type View struct {
	// Setting is the agent's own setting, "" for the default.
	Setting string
	// VersionModel is the active version's model, "" when it names none.
	VersionModel string
	Effective    string
	Source       Source
	// Problem is set when Setting is no longer usable.
	Problem   string
	Available []Served
	// Thinking is the agent's thinking setting for Effective (thinking.go).
	Thinking ThinkingView
}

// Describe builds the view of an agent. versionModel and versionThinking are the active version's
// ("" or "default" for no model; for an agent not yet activated, the latest version's).
func Describe(ctx context.Context, rv prices.Resolver, q *store.Queries, a store.Agent, versionModel, versionThinking, platformDefault string, at time.Time) (View, error) {
	v, err := describeModel(ctx, rv, q, a, versionModel, platformDefault, at)
	if err != nil {
		return View{}, err
	}
	v.Thinking, err = describeThinking(ctx, q, a, versionThinking, v.Effective)
	return v, err
}

func describeModel(ctx context.Context, rv prices.Resolver, q *store.Queries, a store.Agent, versionModel, platformDefault string, at time.Time) (View, error) {
	v := View{Setting: a.Model.String, VersionModel: Normalize(versionModel)}
	var err error
	if v.Available, err = ListServed(ctx, rv, q, a.OrgID, at); err != nil {
		return View{}, err
	}
	v.Effective, v.Source = Choose(v.Setting, versionModel, platformDefault)
	if v.Source == SourceAgent {
		var u *UnusableError
		switch err := Check(ctx, rv, q, a.OrgID, v.Setting, at); {
		case errors.As(err, &u):
			v.Problem = u.RunReason()
		case err != nil:
			return View{}, err
		}
		return v, nil
	}
	if v.Source == SourceVersion {
		// A template's model the org does not serve (or cannot price) would meet the gateway's 404
		// at the first request: say so here, where a person can pick another.
		var u *UnusableError
		switch err := Check(ctx, rv, q, a.OrgID, v.Effective, at); {
		case errors.As(err, &u):
			v.Problem = "The agent's version names a model the gateway will not serve (" + u.Error() + "); pick a model below."
		case err != nil:
			return View{}, err
		}
	}
	if v.Source == SourceNone {
		def, err := OrgDefault(ctx, q, a.OrgID)
		if err != nil {
			return View{}, err
		}
		if def == "" {
			v.Problem = NoDefaultMessage
			return v, nil
		}
		v.Effective, v.Source = def, SourceOrgDefault
		var u *UnusableError
		switch err := Check(ctx, rv, q, a.OrgID, def, at); {
		case errors.As(err, &u):
			v.Problem = "The org's default model is not usable (" + u.Error() + "); pick another default model."
		case err != nil:
			return View{}, err
		}
	}
	return v, nil
}

// Load reads an agent and describes its model: the version's model is the active version's, or the
// latest version's for an agent not yet activated. ErrNotFound: no such agent in the org.
func (s *Service) Load(ctx context.Context, org, agent pgtype.UUID, at time.Time) (store.Agent, View, error) {
	q := store.New(s.pool)
	a, err := q.GetAgent(ctx, store.GetAgentParams{OrgID: org, ID: agent})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Agent{}, View{}, ErrNotFound
	} else if err != nil {
		return store.Agent{}, View{}, err
	}
	versionModel, versionThinking, err := versionSettings(ctx, q, a)
	if err != nil {
		return store.Agent{}, View{}, err
	}
	view, err := Describe(ctx, s.prices, q, a, versionModel, versionThinking, s.platformDefault, at)
	return a, view, err
}

// versionSettings returns the model and thinking setting of agent a's active version, or of its
// latest version when it is not yet activated.
func versionSettings(ctx context.Context, q *store.Queries, a store.Agent) (model, thinking string, err error) {
	versions, err := q.ListAgentVersions(ctx, store.ListAgentVersionsParams{OrgID: a.OrgID, AgentID: a.ID})
	if err != nil {
		return "", "", err
	}
	for _, v := range versions {
		if v.ID == a.ActiveVersionID || (!a.ActiveVersionID.Valid && v.Version == a.LatestVersion) {
			cfg, err := agentconfig.Parse(v.Config)
			if err != nil {
				return "", "", err
			}
			model, thinking = cfg.Model, cfg.Thinking
		}
	}
	return model, thinking, nil
}

// ErrNotFound means the agent does not exist in the org.
var ErrNotFound = errors.New("not found")

// InvalidError is a refused setting; the message is safe to show.
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string { return e.Msg }

// Service changes agents' model settings.
type Service struct {
	pool   *pgxpool.Pool
	prices prices.Resolver
	// platformDefault is koinon-agentd's --default-model, which the API cannot see; the chart leaves
	// it unset, so the org default is the stored one (OrgDefault).
	platformDefault string
}

// New returns a Service on pool.
func New(pool *pgxpool.Pool, rv prices.Resolver) *Service { return &Service{pool: pool, prices: rv} }

// Set sets the agent's model; "default" or "" clears it. The model must pass Check. The change and
// its audit event, with the old and new value, commit together. Setting the value it already has
// is not a change and writes no event. It returns the updated agent.
func (s *Service) Set(ctx context.Context, org, agent pgtype.UUID, actor audit.Actor, model string, at time.Time) (store.Agent, error) {
	want := Normalize(model)
	if len(want) > 200 {
		return store.Agent{}, &InvalidError{Msg: "the model name is longer than 200 characters"}
	}
	var out store.Agent
	_, err := audit.InTx(ctx, s.pool, func(q *store.Queries, b audit.Batch) error {
		cur, err := q.GetAgentForUpdate(ctx, store.GetAgentForUpdateParams{OrgID: org, ID: agent})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if cur.Status == store.AgentStatusArchived {
			return &InvalidError{Msg: "the agent is archived"}
		}
		if want != "" {
			var u *UnusableError
			if err := Check(ctx, s.prices, q, org, want, at); errors.As(err, &u) {
				return &InvalidError{Msg: u.Error() + "; pick a model the org serves and has a price for"}
			} else if err != nil {
				return err
			}
		}
		if cur.Model.String == want {
			out = cur
			return nil
		}
		out, err = q.SetAgentModel(ctx, store.SetAgentModelParams{OrgID: org, ID: agent, Model: pgtype.Text{String: want, Valid: want != ""}})
		if err != nil {
			return err
		}
		b.Org(audit.Event{
			Org: org, Actor: actor, Action: "agent.model.set", TargetKind: "agent", TargetID: agent.String(),
			Before: map[string]any{"model": settingLabel(cur.Model.String)},
			After:  map[string]any{"model": settingLabel(want)},
		})
		return nil
	})
	return out, err
}

func settingLabel(m string) string {
	if m == "" {
		return Default
	}
	return m
}

// BackfillDefaults gives each org that existed when migration 00045 ran the default model it ran on
// before the default was stored: the first model ListServed returns (the first one Check accepts,
// by provider and model name). It runs once per org (the migration's default_model_pending mark),
// is safe from every replica, and records org.default_model.set with the reason "migrated". An org
// with no usable model yet gets none and falls back to its first model (catalog adoptDefaultModel).
// It returns how many defaults it set.
func BackfillDefaults(ctx context.Context, pool *pgxpool.Pool, rv prices.Resolver, at time.Time) (int, error) {
	ids, err := store.New(pool).ListOrgsPendingDefaultModel(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, org := range ids {
		set := false
		_, err := audit.InPgTx(ctx, pool, func(tx pgx.Tx, b audit.Batch) error {
			set = false
			q := store.New(tx)
			if claimed, err := q.ClaimOrgDefaultModelBackfill(ctx, org); err != nil || claimed == 0 {
				return err
			}
			list, err := ListServed(ctx, rv, q, org, at)
			if err != nil || len(list) == 0 {
				return err
			}
			rows, err := q.SetOrgDefaultModelIfUnset(ctx, store.SetOrgDefaultModelIfUnsetParams{OrgID: org, DefaultModel: list[0].Name})
			if err != nil || rows == 0 {
				return err
			}
			set = true
			b.Org(audit.Event{
				Org: org, Actor: audit.System("koinon-api:agent-model"), Action: "org.default_model.set", TargetKind: "org", TargetID: org.String(),
				Before: map[string]any{"model": ""}, After: map[string]any{"model": list[0].Name, "reason": "migrated"},
			})
			return nil
		})
		if err != nil {
			return n, err
		}
		if set {
			n++
		}
	}
	return n, nil
}
