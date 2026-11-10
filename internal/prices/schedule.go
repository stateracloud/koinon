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
// (price.refresh.settings_changed, an install event of system koinon-api:price-refresh). The first
// start with the default mode (auto) records and audits nothing. The row then holds the interval
// too, with no event: a recorded interval only holds a replica back (Tighter takes the longer one),
// so it widens nothing (#354). A bound looser than the default
// never gets here: NewRefresher refuses it. No platform admin is needed.
func (r *Refresher) SyncSettings(ctx context.Context, mode RefreshMode, interval time.Duration) error {
	if _, err := ParseRefreshMode(string(mode)); err != nil {
		return err
	}
	_, err := audit.InTx(ctx, r.svc.pool, func(q *store.Queries, b audit.Batch) error {
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
		auditPlatform(b, AutoActor, "price.refresh.settings_changed", "price_refresh", "", before,
			fmtSettings(want.Mode, want.Interval, want.MaxChangePercent, want.MaxAutoPerDay))
		return nil
	})
	return err
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

// modeRank orders the modes by what they fetch: off nothing, auto the sources a provider uses, on all.
func modeRank(m RefreshMode) int {
	switch m {
	case RefreshOff:
		return 0
	case RefreshAuto:
		return 1
	}
	return 2
}

// heldBounds are the auto-apply bounds outside a scheduled tick: this Refresher's, held to the
// recorded ones (StoredBounds; the defaults before any is recorded) wherever those are tighter, the
// bounds rule of Effective.
func (r *Refresher) heldBounds(ctx context.Context) (pricefetch.Bounds, error) {
	stored, err := r.svc.StoredBounds(ctx)
	if err != nil {
		return pricefetch.Bounds{}, err
	}
	return Tighter(Schedule{Bounds: r.bounds}, Schedule{Bounds: stored}).Bounds, nil
}

// Schedule is what one scheduled tick runs with: the mode, the interval since a source's last fetch,
// and the auto-apply bounds.
type Schedule struct {
	Mode     RefreshMode
	Interval time.Duration
	Bounds   pricefetch.Bounds
}

// Configured is the schedule koinon-api was started with: mode, interval and this Refresher's bounds.
func (r *Refresher) Configured(mode RefreshMode, interval time.Duration) Schedule {
	return Schedule{Mode: mode, Interval: interval, Bounds: r.bounds}
}

// Tighter combines a configured schedule with the recorded one, field by field, to what does less:
// the mode that fetches less (off, then auto, then on), the longer interval, and the smaller of each
// auto-apply bound. It is the rule of Effective, without the database.
func Tighter(configured, recorded Schedule) Schedule {
	out := configured
	if modeRank(recorded.Mode) < modeRank(out.Mode) {
		out.Mode = recorded.Mode
	}
	out.Interval = max(out.Interval, recorded.Interval)
	out.Bounds.MaxChangePercent = min(out.Bounds.MaxChangePercent, recorded.Bounds.MaxChangePercent)
	out.Bounds.MaxAutoPerDay = min(out.Bounds.MaxAutoPerDay, recorded.Bounds.MaxAutoPerDay)
	return out
}

// Effective is the schedule a scheduled tick runs with: the configured one, held back to the recorded
// setting (price_refresh_settings, written by SyncSettings together with its install audit event)
// wherever that one does less (Tighter): the mode, the interval and both auto-apply bounds, which
// the row records with the same event. The recorded row is what the install audit chain,
// StoredBounds and the prices commands see, so a replica never fetches or applies more than it says:
// while its SyncSettings is refused (the install audit writer is not granted yet, ADR 0100 decision
// 1.1a) it runs the recorded setting, and switches at the first tick after its own setting and event
// are recorded; in a rolling upgrade an old replica does no more than the setting a new replica
// recorded. A change to doing less (to off, a longer interval, a tighter bound) takes effect at once,
// before its event is recorded: it widens nothing. No row yet: the default, auto at the configured
// interval with the default bounds, which a first start in auto records without an event.
func (r *Refresher) Effective(ctx context.Context, mode RefreshMode, interval time.Duration) (Schedule, error) {
	if _, err := ParseRefreshMode(string(mode)); err != nil {
		return Schedule{}, err
	}
	configured := r.Configured(mode, interval)
	st, err := store.New(r.svc.pool).GetPriceRefreshSettings(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return Tighter(configured, Schedule{Mode: RefreshAuto, Interval: interval, Bounds: pricefetch.DefaultBounds}), nil
	} else if err != nil {
		return Schedule{}, err
	}
	stored, err := ParseRefreshMode(st.Mode)
	if err != nil {
		return Schedule{}, fmt.Errorf("stored price refresh setting: %w", err)
	}
	storedInterval, err := time.ParseDuration(st.Interval)
	if err != nil {
		return Schedule{}, fmt.Errorf("stored price refresh interval: %w", err)
	}
	storedBounds := pricefetch.Bounds{MaxChangePercent: int(st.MaxChangePercent), MaxAutoPerDay: int(st.MaxAutoPerDay)}
	if err := storedBounds.Validate(); err != nil {
		return Schedule{}, fmt.Errorf("stored price refresh bounds: %w", err)
	}
	return Tighter(configured, Schedule{Mode: stored, Interval: storedInterval, Bounds: storedBounds}), nil
}

// Loop runs a tick on a schedule until ctx ends: first a minute after the start (a restart does not
// fetch at once), then every interval, at least a minute and at most an hour apart, so that auto mode
// notices a new provider and the unpriced-model count stays current. koinon-api runs it in every
// mode; in off it only counts unpriced models and makes no request. Each tick runs the Effective
// schedule, read from the recorded setting at that tick; a tick that cannot read it fetches nothing.
func (r *Refresher) Loop(ctx context.Context, mode RefreshMode, interval time.Duration) {
	configured := r.Configured(mode, interval)
	var last Schedule
	effective := func() (Schedule, bool) {
		eff, err := r.Effective(ctx, mode, interval)
		if err != nil {
			if ctx.Err() == nil {
				r.log.Warn("scheduled price refresh: reading the recorded setting", "err", err)
			}
			return Schedule{}, false
		}
		if eff != last {
			if eff != configured {
				r.log.Warn("the scheduled price refresh runs in the recorded mode until koinon-api's setting is recorded",
					"mode", eff.Mode, "configured", mode, "interval", eff.Interval,
					"maxChangePercent", eff.Bounds.MaxChangePercent, "maxAutoPerDay", eff.Bounds.MaxAutoPerDay)
			} else {
				r.log.Info("the scheduled price refresh runs in the configured mode", "mode", eff.Mode)
			}
			last = eff
		}
		return eff, true
	}
	effective() // says at the start which schedule the first tick will run
	tick := time.NewTimer(time.Minute)
	defer tick.Stop()
	every := interval // the effective interval of the last good read; the configured one before any
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if eff, ok := effective(); ok {
			every = eff.Interval
			if _, err := r.runSchedule(ctx, eff); err != nil && ctx.Err() == nil {
				r.log.Warn("scheduled price refresh", "err", err)
			}
		}
		// Off accepts any interval, zero included: never spin.
		tick.Reset(min(max(every, time.Minute), time.Hour))
	}
}

// runSchedule is one round of the schedule, with a schedule Loop read from Effective (unexported, so
// no caller runs a round under a setting the recorded one does not allow): it reports the unpriced models, then fetches each source
// the mode allows that was last fetched at least the interval ago (another replica may have fetched
// it, or this one before a restart), and applies the proposals inside the schedule's bounds.
func (r *Refresher) runSchedule(ctx context.Context, sc Schedule) (RunResult, error) {
	if _, err := r.ReportUnpriced(ctx); err != nil && ctx.Err() == nil {
		r.log.Warn("counting unpriced models", "err", err)
	}
	names, err := r.activeSources(ctx, sc.Mode)
	if err != nil {
		return RunResult{Failed: map[string]string{}}, err
	}
	var due []string
	for _, n := range names {
		ok, err := r.due(ctx, n, sc.Interval)
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
	return r.run(ctx, TriggerSchedule, due, true, sc.Bounds)
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
	// ListModelsOfProviderType does not return the provider URL, so Resolve here sees no host and a
	// `not_hosts` exclusion never applies. That is fine for the types counted (the ones a source
	// prices): none has a not_hosts entry, and the count is by type, not per endpoint.
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
