// SPDX-License-Identifier: Apache-2.0

// Package orcatalog is the per-install OpenRouter model catalog (ADR 0070, amendment of
// 2026-11-11, #543).
//
// Koinon never embeds OpenRouter's catalog. While an enabled openrouter provider exists, the
// price refresh (package prices) fetches OpenRouter's model list with the shared adapter and
// parser (package pricefetch); this package stores what that body says about the models, in the
// install's database, with the fetch time as the source (openrouter@<fetched_at>), and refreshes
// it weekly.
//
// Two rules apply by field:
//
//   - Display fields (name, description, modalities, tokenizer, supported parameters, and the
//     upstream list of a model in use) are effective as soon as they are stored.
//   - Fields that change behaviour (the context and output limits that set compaction
//     thresholds) only become proposals. They change nothing until a platform admin accepts
//     them; until then models.dev's openrouter values apply (Effective reports no limit).
//
// Prices are not handled here: they follow ADR 0098 in package prices.
package orcatalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/pricefetch"
)

// Interval is the refresh interval of the stored catalog: weekly (ADR 0070 amendment, item 2). The
// daily price fetch of ADR 0098 reads the same list; this interval only says how often its
// metadata is stored and the endpoints route is asked.
const Interval = 7 * 24 * time.Hour

// MaxEndpointModels caps how many models get an endpoints request in one refresh.
const MaxEndpointModels = 100

// The behaviour-changing fields.
const (
	FieldContextLength = "context_length"
	FieldMaxOutput     = "max_output"
)

// Proposal states.
const (
	StateOpen       = "open"
	StateAccepted   = "accepted"
	StateDismissed  = "dismissed"
	StateSuperseded = "superseded"
)

// ErrNotFound is a proposal that does not exist.
var ErrNotFound = errors.New("orcatalog: not found")

// InvalidError is a request the catalog refuses, with a message fit for the person.
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string { return e.Msg }

// SourceTag is the source of a stored row: openrouter@<fetched_at>, RFC 3339 UTC.
func SourceTag(fetchedAt time.Time) string {
	return pricefetch.SourceOpenRouter + "@" + fetchedAt.UTC().Format(time.RFC3339)
}

// Store reads and writes the catalog tables.
type Store struct {
	pool *pgxpool.Pool
}

// New returns a Store over pool.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// dueSlack lets a weekly tick fire although the previous store ran a little later in its tick: the
// refresher looks once a day, so without it the interval drifts to eight days.
const dueSlack = 12 * time.Hour

// Due says whether the catalog should be stored from a list fetched now: it has never been stored,
// the last store is (about) a week old, or a model in use that this fetch lists was not in use at
// the last store (it was enabled since). listed is the set of model names of the fetch: a model in
// use that OpenRouter does not list (delisted, retired, misspelled) is never stored as in use and
// must not make every tick due. A restart inside the week with the same models in use is not due.
func (s *Store) Due(ctx context.Context, now time.Time, inUse, listed []string) (bool, error) {
	c, err := store.New(s.pool).GetOpenRouterCatalog(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	} else if err != nil {
		return false, err
	}
	if now.Sub(c.FetchedAt.Time) >= Interval-dueSlack {
		return true, nil
	}
	for _, m := range inUseListed(inUse, listed) {
		if !slices.Contains(c.InUseModels, m) {
			return true, nil
		}
	}
	return false, nil
}

// inUseListed is the sorted set of the models in use that listed holds.
func inUseListed(inUse, listed []string) []string {
	out := []string{}
	for _, m := range inUse {
		if slices.Contains(listed, m) && !slices.Contains(out, m) {
			out = append(out, m)
		}
	}
	slices.Sort(out)
	return out
}

// Result says what one Apply stored.
type Result struct {
	Models    int
	Proposals int
}

// Apply stores a snapshot's model metadata in one transaction: the catalog row, every model's
// display fields and fetched limits, and, for the models in use, a proposal for each fetched limit
// that is not the accepted one. A snapshot with no metadata stores nothing.
func (s *Store) Apply(ctx context.Context, snap pricefetch.Snapshot, inUse []string) (Result, error) {
	var res Result
	if len(snap.Meta) == 0 {
		return res, nil
	}
	at := pgtype.Timestamptz{Time: snap.FetchedAt.UTC(), Valid: true}
	source := SourceTag(snap.FetchedAt)
	use := map[string]bool{}
	for _, m := range inUse {
		use[m] = true
	}
	listed := make([]string, 0, len(snap.Meta))
	for _, m := range snap.Meta {
		listed = append(listed, m.Model)
	}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		// Two replicas can both find the catalog due. The lock makes the second wait until the first
		// commits; each later statement (read committed) then sees the first one's proposals, so the
		// second refreshes them instead of inserting a duplicate that the live index would refuse.
		if err := q.LockOpenRouterCatalog(ctx); err != nil {
			return err
		}
		if err := q.UpsertOpenRouterCatalog(ctx, store.UpsertOpenRouterCatalogParams{
			Source: source, FetchedAt: at, ModelCount: int32(snap.Count), Sha256: snap.SHA256, // #nosec G115 -- bounded by the body cap
			InUseModels: inUseListed(inUse, listed),
		}); err != nil {
			return err
		}
		for _, m := range snap.Meta {
			display, err := json.Marshal(displayOf(m))
			if err != nil {
				return err
			}
			if err := q.UpsertOpenRouterModel(ctx, store.UpsertOpenRouterModelParams{
				Model: m.Model, Display: display, ContextLength: int32(m.ContextLength), MaxOutput: int32(m.MaxOutput), // #nosec G115 -- parser caps at 10M
				Source: source, FetchedAt: at,
			}); err != nil {
				return err
			}
			res.Models++
			if !use[m.Model] {
				continue
			}
			for field, v := range map[string]int{FieldContextLength: m.ContextLength, FieldMaxOutput: m.MaxOutput} {
				made, err := propose(ctx, q, m.Model, field, v, source)
				if err != nil {
					return err
				}
				if made {
					res.Proposals++
				}
			}
		}
		return nil
	})
	return res, err
}

// propose makes the proposal for one model and field from a fetched value and reports whether it
// created one. A value equal to the accepted one closes the open proposals; a dismissed value is
// remembered and proposes nothing again.
func propose(ctx context.Context, q *store.Queries, model, field string, fetched int, source string) (bool, error) {
	if fetched <= 0 {
		return false, nil // the list gave no value: nothing to propose, nothing changes
	}
	var current pgtype.Int4
	if a, err := q.GetOpenRouterLimitValue(ctx, store.GetOpenRouterLimitValueParams{Model: model, Field: field}); err == nil {
		current = pgtype.Int4{Int32: a.Value, Valid: true}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	if current.Valid && int(current.Int32) == fetched {
		_, err := q.SupersedeOpenRouterLimitProposals(ctx, store.SupersedeOpenRouterLimitProposalsParams{Model: model, Field: field, Keep: 0})
		return false, err
	}
	if _, err := q.SupersedeOpenRouterLimitProposals(ctx, store.SupersedeOpenRouterLimitProposalsParams{Model: model, Field: field, Keep: int32(fetched)}); err != nil { // #nosec G115 -- parser caps at 10M
		return false, err
	}
	live, err := q.FindLiveOpenRouterLimitProposal(ctx, store.FindLiveOpenRouterLimitProposalParams{Model: model, Field: field, ProposedValue: int32(fetched)}) // #nosec G115 -- parser caps at 10M
	switch {
	case err == nil && live.State == StateOpen:
		return false, q.RefreshOpenRouterLimitProposal(ctx, store.RefreshOpenRouterLimitProposalParams{ID: live.ID, CurrentValue: current, Source: source})
	case err == nil:
		return false, nil // dismissed: the same value does not come back
	case !errors.Is(err, pgx.ErrNoRows):
		return false, err
	}
	_, err = q.InsertOpenRouterLimitProposal(ctx, store.InsertOpenRouterLimitProposalParams{
		Model: model, Field: field, CurrentValue: current, ProposedValue: int32(fetched), Source: source, // #nosec G115 -- parser caps at 10M
	})
	return true, err
}

// display is what the stored display column holds.
type display struct {
	Name                string   `json:"name,omitempty"`
	Description         string   `json:"description,omitempty"`
	Created             int64    `json:"created,omitempty"`
	InputModalities     []string `json:"inputModalities,omitempty"`
	OutputModalities    []string `json:"outputModalities,omitempty"`
	Tokenizer           string   `json:"tokenizer,omitempty"`
	SupportedParameters []string `json:"supportedParameters,omitempty"`
}

func displayOf(m pricefetch.ModelMeta) display {
	return display{
		Name: m.Name, Description: m.Description, Created: m.Created, InputModalities: m.InputModalities,
		OutputModalities: m.OutputModalities, Tokenizer: m.Tokenizer, SupportedParameters: m.SupportedParameters,
	}
}

// StoreEndpoints stores the upstream list of a model the catalog already has. A model with no row
// (it left the list) is skipped without error.
func (s *Store) StoreEndpoints(ctx context.Context, e pricefetch.ModelEndpoints) error {
	raw, err := json.Marshal(e.Endpoints)
	if err != nil {
		return err
	}
	if e.Endpoints == nil {
		raw = []byte("[]")
	}
	_, err = store.New(s.pool).SetOpenRouterModelEndpoints(ctx, store.SetOpenRouterModelEndpointsParams{
		Model: e.Model, Endpoints: raw, EndpointsSource: SourceTag(e.FetchedAt),
		EndpointsFetchedAt: pgtype.Timestamptz{Time: e.FetchedAt.UTC(), Valid: true},
	})
	return err
}

// Model is what the catalog knows of one model.
type Model struct {
	Model       string
	Name        string
	Description string
	Created     int64
	// Input and OutputModalities, Tokenizer and SupportedParameters are display fields.
	InputModalities     []string
	OutputModalities    []string
	Tokenizer           string
	SupportedParameters []string
	// Source is openrouter@<fetched_at>; FetchedAt is the same time.
	Source    string
	FetchedAt time.Time
	// Endpoints is the upstream list of a model in use; EndpointsSource is empty when none was fetched.
	Endpoints       []pricefetch.Endpoint
	EndpointsSource string
	// ContextLength and MaxOutput are the EFFECTIVE limits: the accepted values, or nil while none
	// is accepted (models.dev's openrouter values apply then). The last fetched values are in
	// FetchedContextLength and FetchedMaxOutput and change nothing.
	ContextLength        *int
	MaxOutput            *int
	FetchedContextLength int
	FetchedMaxOutput     int
}

// Effective returns the catalog's view of a model. ok is false when the model was never fetched.
func (s *Store) Effective(ctx context.Context, model string) (m Model, ok bool, err error) {
	q := store.New(s.pool)
	row, err := q.GetOpenRouterModel(ctx, model)
	if errors.Is(err, pgx.ErrNoRows) {
		return Model{}, false, nil
	} else if err != nil {
		return Model{}, false, err
	}
	var d display
	if err := json.Unmarshal(row.Display, &d); err != nil {
		return Model{}, false, fmt.Errorf("orcatalog: stored display of %s: %w", model, err)
	}
	m = Model{
		Model: row.Model, Name: d.Name, Description: d.Description, Created: d.Created,
		InputModalities: d.InputModalities, OutputModalities: d.OutputModalities, Tokenizer: d.Tokenizer,
		SupportedParameters: d.SupportedParameters, Source: row.Source, FetchedAt: row.FetchedAt.Time,
		EndpointsSource: row.EndpointsSource, FetchedContextLength: int(row.ContextLength), FetchedMaxOutput: int(row.MaxOutput),
	}
	if row.Endpoints != nil {
		if err := json.Unmarshal(row.Endpoints, &m.Endpoints); err != nil {
			return Model{}, false, fmt.Errorf("orcatalog: stored endpoints of %s: %w", model, err)
		}
	}
	for field, dst := range map[string]**int{FieldContextLength: &m.ContextLength, FieldMaxOutput: &m.MaxOutput} {
		v, err := q.GetOpenRouterLimitValue(ctx, store.GetOpenRouterLimitValueParams{Model: model, Field: field})
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		} else if err != nil {
			return Model{}, false, err
		}
		n := int(v.Value)
		*dst = &n
	}
	return m, true, nil
}

// Proposal is a fetched limit that waits for a person.
type Proposal struct {
	ID            string
	Model         string
	Field         string
	CurrentValue  *int
	ProposedValue int
	Source        string
	State         string
	CreatedAt     time.Time
	DecidedBy     string
}

func toProposal(r store.OpenrouterLimitProposal) Proposal {
	p := Proposal{
		ID: r.ID.String(), Model: r.Model, Field: r.Field, ProposedValue: int(r.ProposedValue), Source: r.Source,
		State: r.State, CreatedAt: r.CreatedAt.Time, DecidedBy: r.DecidedBy,
	}
	if r.CurrentValue.Valid {
		n := int(r.CurrentValue.Int32)
		p.CurrentValue = &n
	}
	return p
}

// ListProposals lists proposals, newest first; an empty state means all.
func (s *Store) ListProposals(ctx context.Context, state string, limit int) ([]Proposal, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := store.New(s.pool).ListOpenRouterLimitProposals(ctx, store.ListOpenRouterLimitProposalsParams{State: state, MaxRows: int32(limit)}) // #nosec G115 -- bounded above
	if err != nil {
		return nil, err
	}
	out := make([]Proposal, len(rows))
	for i, r := range rows {
		out[i] = toProposal(r)
	}
	return out, nil
}

// Accept makes a proposal's value the effective limit of its model, from now on. The change and its
// install audit event commit together.
func (s *Store) Accept(ctx context.Context, actor audit.Actor, id string) error {
	return s.decide(ctx, actor, id, StateAccepted)
}

// Dismiss closes a proposal without applying it; the same value for the same model and field does
// not come back.
func (s *Store) Dismiss(ctx context.Context, actor audit.Actor, id string) error {
	return s.decide(ctx, actor, id, StateDismissed)
}

func (s *Store) decide(ctx context.Context, actor audit.Actor, id, state string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return &InvalidError{Msg: "proposal id " + id + " is not a UUID"}
	}
	_, err = audit.InTx(ctx, s.pool, func(q *store.Queries, b audit.Batch) error {
		row, err := q.GetOpenRouterLimitProposalForUpdate(ctx, uid)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: proposal %s", ErrNotFound, id)
		} else if err != nil {
			return err
		}
		if row.State != StateOpen {
			return &InvalidError{Msg: "proposal " + id + " is " + row.State + ", not open"}
		}
		if err := q.DecideOpenRouterLimitProposal(ctx, store.DecideOpenRouterLimitProposalParams{ID: uid, State: state, DecidedBy: actor.String()}); err != nil {
			return err
		}
		after := map[string]any{"model": row.Model, "field": row.Field, "value": row.ProposedValue, "source": row.Source}
		if state == StateAccepted {
			if err := q.UpsertOpenRouterLimitValue(ctx, store.UpsertOpenRouterLimitValueParams{
				Model: row.Model, Field: row.Field, Value: row.ProposedValue, AcceptedBy: actor.String(), ProposalID: uid,
			}); err != nil {
				return err
			}
		}
		b.Install(audit.InstallEvent{Actor: actor, Action: "model_metadata.limit." + state, TargetKind: "openrouter_limit_proposal", TargetID: id, After: after})
		return nil
	})
	return err
}

func parseUUID(s string) (pgtype.UUID, error) {
	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		return u, err
	}
	return u, nil
}

// Names returns the distinct, sorted model names (the helper the refresh uses to build the in-use list).
func Names(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return slices.Compact(out)
}
