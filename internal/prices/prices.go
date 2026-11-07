// SPDX-License-Identifier: Apache-2.0

// Package prices finds the price of a model request and manages org price overrides and the
// default list's versions (plan item PV2, ADR 0056 amendment 2026-10-24).
//
// Precedence, highest first: the org's own price entries, the install's platform price list
// (ADR 0098), the default list. Within a layer see pricing.Select. The per-model rates of PV1 were folded into dated org entries (migration
// 00014, ADR 0056). Every answer carries a ref ("default@<list version>#<id>", "platform@<version>#<id>" or "org#<id>")
// that the ledger stores, so each ledger entry names the price version it used. Ledger entries
// written before the migration may still cite "model#<id>@<unix time>".
package prices

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/estimate"
	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/pricing"
)

// Layers a price can come from.
const (
	SourceOrg      = "org"
	SourcePlatform = "platform"
	SourceDefault  = "default"
)

// Model is the model a request asked for.
type Model struct {
	ID, Name, ProviderType string
	// ProviderID is the provider that serves the model, at ProviderURL.
	ProviderID  pgtype.UUID
	ProviderURL string
	// Format is the model's wire format (models.format). Pricing ignores it; the cache mode
	// depends on it.
	Format modelformat.Format
}

// Resolution is the price found for a model at a time.
type Resolution struct {
	Ref         string
	Source      string
	ListVersion string
	// PlatformVersion is the platform price version of a SourcePlatform answer.
	PlatformVersion int64
	Entry           pricing.Entry
	Price           pricing.Price
	// Fallback is true when no entry was valid at the requested time and the current one was used.
	Fallback bool
}

// EstimateParams returns the estimation settings of the matched entry, before calibration.
func (r Resolution) EstimateParams() estimate.Params {
	ratio, err := pricing.ParseRatio(r.Entry.Ratio)
	if err != nil {
		ratio = 3.0
	}
	return estimate.Params{
		Tokenizer: r.Entry.Tokenizer, Ratio: ratio, ToolOverheadTokens: r.Entry.ToolOverheadTokens, HiddenThinking: r.Entry.HiddenThinking,
		MaxOutput: int64(r.Entry.MaxOutput),
	}
}

// ErrUnpriced means no price matches the model.
var ErrUnpriced = errors.New("prices: no price for this model")

// Lookup finds the enabled model with the plain name an agent used.
func Lookup(ctx context.Context, q *store.Queries, org pgtype.UUID, name string) (Model, error) {
	row, err := q.GetModelForPricing(ctx, store.GetModelForPricingParams{OrgID: org, Name: name})
	if errors.Is(err, pgx.ErrNoRows) {
		return Model{}, ErrUnpriced
	} else if err != nil {
		return Model{}, err
	}
	return Model{ID: row.ID.String(), Name: row.Name, ProviderType: row.ProviderType, ProviderID: row.ProviderID, ProviderURL: row.ProviderUrl, Format: modelformat.Format(row.Format)}, nil
}

// BadPriceError means a stored price cannot be used (a rate that does not parse, an override
// that no longer validates). It concerns one org's prices only: callers record the request as
// unpriced or skip the model, and never stop work for other orgs.
type BadPriceError struct{ Err error }

func (e *BadPriceError) Error() string { return "prices: unusable stored price: " + e.Err.Error() }
func (e *BadPriceError) Unwrap() error { return e.Err }

func bad(err error) error { return &BadPriceError{Err: err} }

// Resolver finds prices: the org's entries first, then the platform list, then those of its ListSource. Build one with
// NewResolver; the zero Resolver has no source and fails every lookup.
type Resolver struct{ src pricing.ListSource }

// NewResolver returns a Resolver over src (pricing.Embedded in production). The source is
// required: a nil one is a wiring mistake and panics here instead of pricing from a list nobody chose.
func NewResolver(src pricing.ListSource) Resolver {
	if src == nil {
		panic("prices: NewResolver needs a pricing.ListSource")
	}
	return Resolver{src: src}
}

// IsZero reports whether r was not built with NewResolver.
func (rv Resolver) IsZero() bool { return rv.src == nil }

// Resolve finds m's price at time at. A stored price that cannot be used is a *BadPriceError.
func (rv Resolver) Resolve(ctx context.Context, q *store.Queries, org pgtype.UUID, m Model, at time.Time) (Resolution, error) {
	return rv.resolveEntries(ctx, q, org, m, at)
}

// resolveEntries finds the org's or the source's entry for m.
func (rv Resolver) resolveEntries(ctx context.Context, q *store.Queries, org pgtype.UUID, m Model, at time.Time) (Resolution, error) {
	if rv.src == nil {
		return Resolution{}, errors.New("prices: the Resolver has no pricing.ListSource")
	}
	rows, err := q.ListPriceOverrides(ctx, org)
	if err != nil {
		return Resolution{}, err
	}
	now := time.Now()
	for _, fallback := range []bool{false, true} {
		t := at
		if fallback {
			t = now
		}
		var rs []pricing.Resolved
		for _, row := range rows {
			if row.RetiredAt.Valid && !row.RetiredAt.Time.After(t) {
				continue
			}
			var e pricing.Entry
			if err := json.Unmarshal(row.Entry, &e); err != nil {
				return Resolution{}, bad(fmt.Errorf("price override %s: %w", row.ID, err))
			}
			e.ID = row.ID.String()
			r, err := e.Resolve(true)
			if err != nil {
				return Resolution{}, bad(err)
			}
			r.ValidFrom = row.ValidFrom.Time
			rs = append(rs, r)
		}
		if r, ok := pricing.Select(rs, m.ProviderType, m.Name, t); ok {
			return Resolution{Ref: "org#" + r.Entry.ID, Source: SourceOrg, Entry: r.Entry, Price: r.Price, Fallback: fallback}, nil
		}
		// The platform list is a step of its own, after the org's entries and before the default
		// list, so a platform entry beats every default entry however specific the default's
		// pattern is (ADR 0098 decision 1).
		version, plat, err := platformEntries(ctx, q, t)
		if err != nil {
			return Resolution{}, err
		}
		if r, ok := pricing.Select(plat, m.ProviderType, m.Name, t); ok {
			return Resolution{
				Ref: PlatformRef(version, r.Entry.ID), Source: SourcePlatform, PlatformVersion: version,
				Entry: r.Entry, Price: r.Price, Fallback: fallback,
			}, nil
		}
		list, defs := rv.src.Current()
		if r, ok := pricing.Select(defs, m.ProviderType, m.Name, t); ok {
			return Resolution{
				Ref: pricing.Ref(list.Version, r.Entry.ID), Source: SourceDefault, ListVersion: list.Version,
				Entry: r.Entry, Price: r.Price, Fallback: fallback,
			}, nil
		}
	}
	return Resolution{}, ErrUnpriced
}

// Calibrated returns r's estimation settings with the learned p90 of the model, if enough real
// requests exist. Calibration only raises estimates (package estimate).
func Calibrated(ctx context.Context, q *store.Queries, org pgtype.UUID, model string, r Resolution) (estimate.Params, error) {
	p := r.EstimateParams()
	c, err := q.GetCalibration(ctx, store.GetCalibrationParams{OrgID: org, Model: model})
	if err != nil {
		return p, err
	}
	if c.Samples >= MinCalibrationSamples {
		p.LearnedP90 = c.P90
	}
	return p, nil
}

// MinCalibrationSamples is how many real requests a model needs before its p90 is used.
const MinCalibrationSamples = 20

// Service manages overrides and default list versions.
type Service struct {
	pool *pgxpool.Pool
	log  *slog.Logger
	src  pricing.ListSource
	res  Resolver
}

// New returns a Service whose default list is src (pricing.Embedded in production). A nil src
// panics, like NewResolver.
func New(pool *pgxpool.Pool, log *slog.Logger, src pricing.ListSource) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{pool: pool, log: log, src: src, res: NewResolver(src)}
}

// SyncDefaults records the embedded list's version, so admins can later see what an upgrade
// changed. It is idempotent and safe from every replica.
func (s *Service) SyncDefaults(ctx context.Context) error {
	list, _ := s.src.Current()
	raw, err := json.Marshal(list.Entries)
	if err != nil {
		return err
	}
	n, err := store.New(s.pool).InsertDefaultPriceList(ctx, store.InsertDefaultPriceListParams{Version: list.Version, Entries: raw})
	if err != nil {
		return fmt.Errorf("prices: record default price list %s: %w", list.Version, err)
	}
	if n > 0 {
		s.log.Info("default price list recorded", "version", list.Version, "entries", len(list.Entries))
	}
	return nil
}

// InvalidError is a validation failure.
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string { return e.Msg }

// ErrNotFound means the named price or list version does not exist.
var ErrNotFound = errors.New("not found")

// Override is an org price entry as callers see it.
type Override struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	Entry     pricing.Entry `json:"entry"`
	ValidFrom time.Time     `json:"validFrom"`
	RetiredAt *time.Time    `json:"retiredAt,omitempty"`
	CreatedBy string        `json:"createdBy"`
	CreatedAt time.Time     `json:"createdAt"`
}

func toOverride(r store.PriceOverride) (Override, error) {
	var e pricing.Entry
	if err := json.Unmarshal(r.Entry, &e); err != nil {
		return Override{}, err
	}
	e.ID = r.Name
	o := Override{ID: r.ID.String(), Name: r.Name, Entry: e, ValidFrom: r.ValidFrom.Time, CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt.Time}
	if r.RetiredAt.Valid {
		t := r.RetiredAt.Time
		o.RetiredAt = &t
	}
	return o, nil
}

// ListOverrides returns every version of every override, newest first per name.
func (s *Service) ListOverrides(ctx context.Context, org pgtype.UUID) ([]Override, error) {
	rows, err := store.New(s.pool).ListPriceOverrides(ctx, org)
	if err != nil {
		return nil, err
	}
	out := make([]Override, 0, len(rows))
	for _, r := range rows {
		o, err := toOverride(r)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}

// OverrideInput creates a new version of the price called Entry.ID.
type OverrideInput struct {
	Entry pricing.Entry
	// ValidFrom is when the price starts to apply; zero means now. Earlier requests keep the
	// price they were charged at.
	ValidFrom time.Time
}

// SetOverride adds a version of a named org price. It never changes an earlier version.
func (s *Service) SetOverride(ctx context.Context, org pgtype.UUID, actor audit.Actor, in OverrideInput) (Override, error) {
	e := in.Entry
	if e.ValidFrom == "" {
		e.ValidFrom = time.Now().UTC().Format(time.DateOnly)
	}
	if _, err := e.Resolve(true); err != nil {
		return Override{}, &InvalidError{Msg: err.Error()}
	}
	from := in.ValidFrom
	if from.IsZero() {
		from = time.Now().UTC()
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return Override{}, err
	}
	var row store.PriceOverride
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		var err error
		row, err = q.InsertPriceOverride(ctx, store.InsertPriceOverrideParams{
			OrgID: org, ID: newUUID(), Name: e.ID, Entry: raw, ValidFrom: pgtype.Timestamptz{Time: from, Valid: true}, CreatedBy: actor.String(),
		})
		if err != nil {
			return err
		}
		return audit.RecordErr(ctx, q, audit.Event{
			Org: org, Actor: actor, Action: "price.set", TargetKind: "price", TargetID: e.ID,
			After: map[string]any{"name": e.ID, "provider": e.Provider, "match": e.Match, "prices": e.Prices, "tiers": e.Tiers, "validFrom": from.Format(time.RFC3339)},
		})
	})
	if err != nil {
		if isUnique(err) {
			return Override{}, &InvalidError{Msg: "a version of this price already starts at that time; choose a later validFrom"}
		}
		return Override{}, err
	}
	return toOverride(row)
}

// RetireOverride ends the named price now. Its rows stay for the ledger entries that cite them.
func (s *Service) RetireOverride(ctx context.Context, org pgtype.UUID, actor audit.Actor, name string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		n, err := q.RetirePriceOverrides(ctx, store.RetirePriceOverridesParams{OrgID: org, Name: name, At: pgtype.Timestamptz{Time: time.Now(), Valid: true}})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		return audit.RecordErr(ctx, q, audit.Event{Org: org, Actor: actor, Action: "price.retire", TargetKind: "price", TargetID: name})
	})
}

// DefaultsStatus is the installed default list and the versions before it.
type DefaultsStatus struct {
	Current  string          `json:"current"`
	Previous []string        `json:"previous"`
	Entries  []pricing.Entry `json:"entries"`
}

// Defaults returns the embedded list and the list versions this installation has run.
func (s *Service) Defaults(ctx context.Context) (DefaultsStatus, error) {
	list, _ := s.src.Current()
	rows, err := store.New(s.pool).ListDefaultPriceLists(ctx)
	if err != nil {
		return DefaultsStatus{}, err
	}
	st := DefaultsStatus{Current: list.Version, Entries: list.Entries}
	for _, r := range rows {
		if r.Version != list.Version {
			st.Previous = append(st.Previous, r.Version)
		}
	}
	return st, nil
}

// DefaultsDiff lists what changed between two recorded default list versions. An empty from
// means the version before to; an empty to means the current one.
func (s *Service) DefaultsDiff(ctx context.Context, from, to string) (fromV, toV string, changes []pricing.Change, err error) {
	q := store.New(s.pool)
	if to == "" {
		l, _ := s.src.Current()
		to = l.Version
	}
	rows, err := q.ListDefaultPriceLists(ctx)
	if err != nil {
		return "", "", nil, err
	}
	load := func(v string) (pricing.List, error) {
		for _, r := range rows {
			if r.Version == v {
				l := pricing.List{Version: v}
				return l, json.Unmarshal(r.Entries, &l.Entries)
			}
		}
		return pricing.List{}, fmt.Errorf("%w: default price list %s", ErrNotFound, v)
	}
	if from == "" {
		for i, r := range rows {
			if r.Version == to && i+1 < len(rows) {
				from = rows[i+1].Version
			}
		}
		if from == "" {
			return "", to, nil, nil // the first version this installation ran: nothing before it
		}
	}
	a, err := load(from)
	if err != nil {
		return "", "", nil, err
	}
	b, err := load(to)
	if err != nil {
		return "", "", nil, err
	}
	return from, to, pricing.Diff(a, b), nil
}

// Flagged is a price entry whose recomputed cost disagrees with the gateway's by more than 1%.
type Flagged struct {
	PriceRef string `json:"priceRef"`
	Model    string `json:"model"`
	Requests int64  `json:"requests"`
	// Why counts the requests per reason; a request can have several.
	GatewayDiff, UsageMissing, MaxOutputCapped, PriceError int64
	LastSeen                                               time.Time `json:"lastSeen"`
	CostUSD                                                string    `json:"costUsd"`
	GatewayCostUSD                                         string    `json:"gatewayCostUsd"`
}

// ListFlagged lists the entries flagged since a time.
func (s *Service) ListFlagged(ctx context.Context, org pgtype.UUID, since time.Time) ([]Flagged, error) {
	rows, err := store.New(s.pool).ListFlaggedPrices(ctx, store.ListFlaggedPricesParams{OrgID: org, Since: pgtype.Timestamptz{Time: since, Valid: true}})
	if err != nil {
		return nil, err
	}
	out := make([]Flagged, 0, len(rows))
	for _, r := range rows {
		out = append(out, Flagged{PriceRef: r.PriceRef, Model: r.Model, Requests: r.Flagged, GatewayDiff: r.GatewayDiff, UsageMissing: r.UsageMissing, MaxOutputCapped: r.MaxOutputCapped, PriceError: r.PriceError, LastSeen: r.LastSeen.Time, CostUSD: r.CostUsd, GatewayCostUSD: r.GatewayCostUsd})
	}
	return out, nil
}
