// SPDX-License-Identifier: Apache-2.0

package prices

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/pricefetch"
)

// RefreshMode is the scheduled price refresh setting (ADR 0098 decision 2.2, maintainer decision
// 2026-11-07): the chart's prices.refresh.enabled unset, false or true.
type RefreshMode string

// Refresh modes.
const (
	// RefreshAuto fetches a source only while some org has a provider of a type the source prices
	// (OpenRouter while an openrouter provider exists), checked again at every tick. The default.
	RefreshAuto RefreshMode = "auto"
	// RefreshOff never fetches: koinon-api makes no request to any price source.
	RefreshOff RefreshMode = "off"
	// RefreshOn fetches every source, whatever providers exist.
	RefreshOn RefreshMode = "on"
)

// ParseRefreshMode parses auto, off or on.
func ParseRefreshMode(s string) (RefreshMode, error) {
	switch m := RefreshMode(s); m {
	case RefreshAuto, RefreshOff, RefreshOn:
		return m, nil
	}
	return "", fmt.Errorf("price refresh mode %q: want auto, off or on", s)
}

// SyncSettings records the refresh setting koinon-api started with and audits a change
// (price.refresh.settings_changed). The first start with the default mode (auto) records and
// audits nothing. A bound looser than the default never gets here: NewRefresher refuses it. With
// no platform admin yet the change is recorded and logged, not refused (auditPlatform).
func (r *Refresher) SyncSettings(ctx context.Context, mode RefreshMode, interval time.Duration) error {
	if _, err := ParseRefreshMode(string(mode)); err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, r.svc.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		want := store.UpsertPriceRefreshSettingsParams{
			Mode: string(mode), Interval: interval.String(), MaxChangePercent: int32(r.bounds.MaxChangePercent), MaxAutoPerDay: int32(r.bounds.MaxAutoPerDay), // #nosec G115 -- validated bounds
		}
		old, err := q.GetPriceRefreshSettings(ctx)
		first := errors.Is(err, pgx.ErrNoRows)
		if err != nil && !first {
			return err
		}
		if !first && old.Mode == want.Mode && old.Interval == want.Interval && old.MaxChangePercent == want.MaxChangePercent && old.MaxAutoPerDay == want.MaxAutoPerDay {
			return nil
		}
		if err := q.UpsertPriceRefreshSettings(ctx, want); err != nil {
			return err
		}
		if first && mode == RefreshAuto {
			return nil
		}
		fmtSettings := func(mode, iv string, pct, perDay int32) map[string]any {
			return map[string]any{"mode": mode, "interval": iv, "maxChangePercent": pct, "maxAutoPerDay": perDay}
		}
		var before any
		if !first {
			before = fmtSettings(old.Mode, old.Interval, old.MaxChangePercent, old.MaxAutoPerDay)
		}
		return auditPlatform(ctx, q, r.log, audit.System(), "price.refresh.settings_changed", "price_refresh", "", before,
			fmtSettings(want.Mode, want.Interval, want.MaxChangePercent, want.MaxAutoPerDay))
	})
}

// StoredBounds returns the auto-apply bounds koinon-api last started with (SyncSettings), or the
// defaults before it ever started. The prices commands classify with them, so a CLI fetch does not
// mark as auto what the server's tighter bounds would hold for review.
func (s *Service) StoredBounds(ctx context.Context) (pricefetch.Bounds, error) {
	st, err := store.New(s.pool).GetPriceRefreshSettings(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return pricefetch.DefaultBounds, nil
	} else if err != nil {
		return pricefetch.Bounds{}, err
	}
	b := pricefetch.Bounds{MaxChangePercent: int(st.MaxChangePercent), MaxAutoPerDay: int(st.MaxAutoPerDay)}
	if err := b.Validate(); err != nil {
		return pricefetch.Bounds{}, fmt.Errorf("stored price refresh bounds: %w", err)
	}
	return b, nil
}

// Loop runs Tick on a schedule until ctx ends: first a minute after the start (a restart does not
// fetch at once), then every interval, at most an hour apart, so that auto mode notices a new
// provider and the unpriced-model count stays current. koinon-api runs it in every mode; in off it
// only counts unpriced models and makes no request.
func (r *Refresher) Loop(ctx context.Context, mode RefreshMode, interval time.Duration) {
	tick := time.NewTimer(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if _, err := r.Tick(ctx, mode, interval); err != nil && ctx.Err() == nil {
			r.log.Warn("scheduled price refresh", "err", err)
		}
		tick.Reset(min(interval, time.Hour))
	}
}

// Tick is one round of the schedule: it reports the unpriced models, then fetches each source the
// mode allows that was last fetched at least interval ago (another replica may have fetched it, or
// this one before a restart), and applies the proposals inside the bounds.
func (r *Refresher) Tick(ctx context.Context, mode RefreshMode, interval time.Duration) (RunResult, error) {
	if _, err := r.ReportUnpriced(ctx); err != nil && ctx.Err() == nil {
		r.log.Warn("counting unpriced models", "err", err)
	}
	names, err := r.activeSources(ctx, mode)
	if err != nil {
		return RunResult{Failed: map[string]string{}}, err
	}
	var due []string
	for _, n := range names {
		ok, err := r.due(ctx, n, interval)
		if err != nil {
			return RunResult{Failed: map[string]string{}}, err
		}
		if ok {
			due = append(due, n)
		}
	}
	if len(due) == 0 {
		return RunResult{Failed: map[string]string{}}, nil
	}
	return r.run(ctx, TriggerSchedule, due, true)
}

// activeSources are the adapters the mode fetches now.
func (r *Refresher) activeSources(ctx context.Context, mode RefreshMode) ([]string, error) {
	switch mode {
	case RefreshOff:
		return nil, nil
	case RefreshOn:
		return r.adapterNames(), nil
	case RefreshAuto:
	default:
		return nil, fmt.Errorf("price refresh mode %q: want auto, off or on", mode)
	}
	q := store.New(r.svc.pool)
	var out []string
	for _, name := range r.adapterNames() {
		for _, pt := range pricefetch.ProviderTypesOf(name) {
			inUse, err := q.ProviderTypeInUse(ctx, pt)
			if err != nil {
				return nil, err
			}
			if inUse {
				out = append(out, name)
				break
			}
		}
	}
	return out, nil
}

// due says whether a source was last fetched at least interval ago, or never.
func (r *Refresher) due(ctx context.Context, name string, interval time.Duration) (bool, error) {
	f, err := store.New(r.svc.pool).GetLastPriceSourceFetch(ctx, name)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	} else if err != nil {
		return false, err
	}
	return r.now().Sub(f.FetchedAt.Time) >= interval, nil
}

// ReportUnpriced counts, for each provider type a source prices, the enabled catalog models that no
// price covers (no org, platform or default entry), sets koinon_price_unpriced_models and, while
// any is unpriced, logs a warning. Counts only: never a model name, org or provider id.
func (r *Refresher) ReportUnpriced(ctx context.Context) (map[string]int, error) {
	q := store.New(r.svc.pool)
	now := r.now()
	out := map[string]int{}
	for _, pt := range pricefetch.PricedProviderTypes() {
		rows, err := q.ListModelsOfProviderType(ctx, pt)
		if err != nil {
			return out, err
		}
		n := 0
		for _, row := range rows {
			_, err := r.svc.res.Resolve(ctx, q, row.OrgID, Model{ID: row.ID.String(), Name: row.Name, ProviderType: pt}, now)
			var bp *BadPriceError
			switch {
			case errors.Is(err, ErrUnpriced):
				n++
			case err != nil && !errors.As(err, &bp):
				return out, err
			}
		}
		out[pt] = n
		r.metrics.unpriced(pt, n)
		if n > 0 {
			hint := "the price refresh fetches the source's prices; until then requests to these models cannot be estimated"
			if slices.Contains(pricefetch.ProviderTypesOf(pricefetch.SourceOpenRouter), pt) {
				hint = "a new install has no OpenRouter prices until two fetches 24 hours apart agree; set an org price or accept a proposal (koinon-api prices proposals list) to price them sooner"
			}
			r.log.Warn("models have no price", "provider_type", pt, "models", n, "hint", hint)
		}
	}
	return out, nil
}
