// SPDX-License-Identifier: Apache-2.0

package prices

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/pricefetch"
	"github.com/stateracloud/koinon/pricing"
)

// This file is the read side of the platform price list for the API (ADR 0098 slice 3): what the
// prices commands print, as values. The writes (Accept, Dismiss, Rollback, Run) are the ones the
// commands call, so the audit events are the same.

// GetProposal returns one proposal.
func (s *Service) GetProposal(ctx context.Context, id string) (Proposal, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return Proposal{}, &InvalidError{Msg: "proposal id " + id + " is not a UUID"}
	}
	row, err := store.New(s.pool).GetPriceProposal(ctx, uid)
	if errors.Is(err, pgx.ErrNoRows) {
		return Proposal{}, fmt.Errorf("%w: proposal %s", ErrNotFound, id)
	} else if err != nil {
		return Proposal{}, err
	}
	return toProposal(row)
}

// SourceFetch is one row of the fetch log.
type SourceFetch struct {
	ID         string    `json:"id"`
	Source     string    `json:"source"`
	URL        string    `json:"url,omitempty"`
	FetchedAt  time.Time `json:"fetchedAt"`
	Result     string    `json:"result"`
	Error      string    `json:"error,omitempty"`
	SHA256     string    `json:"sha256,omitempty"`
	ETag       string    `json:"etag,omitempty"`
	CommitSHA  string    `json:"commitSha,omitempty"`
	ModelCount int       `json:"modelCount"`
	Trigger    string    `json:"trigger"`
}

// SourceFetches lists the fetch log, newest first.
func (s *Service) SourceFetches(ctx context.Context, limit int) ([]SourceFetch, error) {
	rows, err := store.New(s.pool).ListPriceSourceFetches(ctx, int32(min(max(limit, 1), 1000))) // #nosec G115 -- bounded
	if err != nil {
		return nil, err
	}
	out := make([]SourceFetch, 0, len(rows))
	for _, r := range rows {
		out = append(out, SourceFetch{
			ID: r.ID.String(), Source: r.Source, URL: r.Url, FetchedAt: r.FetchedAt.Time, Result: r.Result, Error: r.Error,
			SHA256: r.Sha256, ETag: r.Etag, CommitSHA: r.CommitSha, ModelCount: int(r.ModelCount), Trigger: r.Trigger,
		})
	}
	return out, nil
}

// PlatformVersion returns one platform version with its entries. Version 0 means the newest.
func (s *Service) PlatformVersion(ctx context.Context, version int64) (PlatformVersion, error) {
	q := store.New(s.pool)
	if version == 0 {
		vs, err := s.PlatformVersions(ctx, 1)
		if err != nil {
			return PlatformVersion{}, err
		}
		if len(vs) == 0 {
			return PlatformVersion{}, fmt.Errorf("%w: no platform price version yet", ErrNotFound)
		}
		version = vs[0].Version
	}
	v, err := q.GetPlatformPriceVersion(ctx, version)
	if errors.Is(err, pgx.ErrNoRows) {
		return PlatformVersion{}, fmt.Errorf("%w: platform price version %d", ErrNotFound, version)
	} else if err != nil {
		return PlatformVersion{}, err
	}
	rows, err := q.ListPlatformPriceEntries(ctx, version)
	if err != nil {
		return PlatformVersion{}, err
	}
	return toPlatformVersion(v, rows)
}

// DiffPlatform lists what changed from version from to version to, sorted by entry. A from of 0 is
// the version before to (an empty list when to is the first version); a to of 0 is the newest.
func (s *Service) DiffPlatform(ctx context.Context, from, to int64) (fromV, toV int64, changes []pricing.Change, err error) {
	newer, err := s.PlatformVersion(ctx, to)
	if err != nil {
		return 0, 0, nil, err
	}
	var older PlatformVersion
	if from == 0 {
		from = newer.Version - 1
	}
	if from >= 1 {
		if older, err = s.PlatformVersion(ctx, from); err != nil {
			return 0, 0, nil, err
		}
	}
	return older.Version, newer.Version, pricing.Diff(platformList(older), platformList(newer)), nil
}

func platformList(v PlatformVersion) pricing.List {
	l := pricing.List{}
	for _, e := range v.Entries {
		l.Entries = append(l.Entries, e.Entry)
	}
	return l
}

// RefreshSettings is the scheduled refresh setting koinon-api last started with.
type RefreshSettings struct {
	Mode             RefreshMode
	Interval         time.Duration
	MaxChangePercent int
	MaxAutoPerDay    int
	// Recorded is false before any start recorded a setting; the rest are then the defaults.
	Recorded  bool
	UpdatedAt time.Time
}

// Settings reads the recorded refresh setting. Settings are Helm-owned (chart values), so there is
// no write here.
func (s *Service) Settings(ctx context.Context) (RefreshSettings, error) {
	st, err := store.New(s.pool).GetPriceRefreshSettings(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return RefreshSettings{
			Mode: RefreshAuto, MaxChangePercent: pricefetch.DefaultBounds.MaxChangePercent, MaxAutoPerDay: pricefetch.DefaultBounds.MaxAutoPerDay,
		}, nil
	} else if err != nil {
		return RefreshSettings{}, err
	}
	mode, err := ParseRefreshMode(st.Mode)
	if err != nil {
		return RefreshSettings{}, err
	}
	iv, err := time.ParseDuration(st.Interval)
	if err != nil {
		return RefreshSettings{}, fmt.Errorf("stored price refresh interval: %w", err)
	}
	return RefreshSettings{
		Mode: mode, Interval: iv, MaxChangePercent: int(st.MaxChangePercent), MaxAutoPerDay: int(st.MaxAutoPerDay),
		Recorded: true, UpdatedAt: st.UpdatedAt.Time,
	}, nil
}

// Explain finds the price in use now for a model of a provider type, without an org: the platform
// layer, then the default list. The Resolution says which and carries the price_ref.
func (s *Service) Explain(ctx context.Context, providerType, model string) (Resolution, error) {
	m := Model{Name: model, ProviderType: providerType}
	return s.res.Resolve(ctx, store.New(s.pool), pgtype.UUID{}, m, time.Now())
}
