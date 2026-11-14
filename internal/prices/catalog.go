// SPDX-License-Identifier: Apache-2.0

package prices

import (
	"context"
	"errors"
	"slices"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/orcatalog"
	"github.com/stateracloud/koinon/internal/pricefetch"
)

// storeCatalog stores the model metadata of an OpenRouter snapshot in the per-install catalog
// (ADR 0070 amendment of 2026-11-11), then asks the endpoints route about the models in use. It
// runs only for a fetch that already happened: the price refresh decided to make it, so `off`
// never gets here and `auto` only while an openrouter provider is enabled. It adds no request to the list.
//
// A scheduled fetch stores the metadata at most weekly (orcatalog.Due: a restart does not fetch
// again, and neither does the daily price tick). A fetch an operator asked for (the CLI) always
// does. A failure is logged and never fails the price fetch.
func (r *Refresher) storeCatalog(ctx context.Context, a pricefetch.Adapter, snap pricefetch.Snapshot, trigger string) {
	if r.catalog == nil || snap.Source != pricefetch.SourceOpenRouter || len(snap.Meta) == 0 {
		return
	}
	// The catalog is stored only while an enabled openrouter provider exists (ADR 0070 amendment):
	// a provider with no enabled model still counts. This holds for the schedule and the CLI alike.
	exists, err := store.New(r.svc.pool).ProviderTypeInUse(ctx, pricefetch.ProviderTypesOf(pricefetch.SourceOpenRouter)[0])
	if err != nil {
		r.log.Warn("openrouter catalog: looking for an openrouter provider", "err", err)
		return
	}
	if !exists {
		return
	}
	inUse, err := r.openRouterModelsInUse(ctx)
	if err != nil {
		r.log.Warn("openrouter catalog: listing the models in use", "err", err)
		return
	}
	listed := make([]string, len(snap.Meta))
	for i, m := range snap.Meta {
		listed[i] = m.Model
	}
	if trigger == TriggerSchedule {
		// Due is a cheap read without the lock, so most ticks open no transaction; ApplyIfDue checks
		// it again under the lock, since another replica may store in between.
		due, err := r.catalog.Due(ctx, snap.FetchedAt, inUse, listed)
		if err != nil {
			r.log.Warn("openrouter catalog: reading the last store", "err", err)
			return
		}
		if due {
			res, err := r.catalog.ApplyIfDue(ctx, snap, inUse)
			if err != nil {
				r.log.Warn("openrouter catalog: storing the fetch", "err", err)
				return
			}
			if res.Skipped {
				return // another replica stored this week's catalog and asked the endpoints route
			}
			r.logStored(snap, res)
		}
	} else {
		res, err := r.catalog.Apply(ctx, snap, inUse)
		if err != nil {
			r.log.Warn("openrouter catalog: storing the fetch", "err", err)
			return
		}
		r.logStored(snap, res)
	}
	ef, ok := a.(pricefetch.EndpointsFetcher)
	if !ok {
		return
	}
	// Endpoints are due per model, not per store: a round that failed or stopped half-way is asked
	// again on the next tick even when the catalog itself is not due. Only the models in use that the
	// list carries: a name OpenRouter does not list (it comes from an org's provider settings) would
	// fail every week.
	ask, left, err := r.catalog.EndpointsDue(ctx, snap.FetchedAt, inUse, listed)
	if err != nil {
		r.log.Warn("openrouter catalog: reading which endpoints are due", "err", err)
		return
	}
	if len(ask) == 0 {
		return
	}
	failed := 0
	for i, m := range ask {
		if ctx.Err() != nil {
			// A stop half-way: the models not reached keep their old endpoints_fetched_at and stay due.
			r.log.Warn("openrouter catalog: endpoints round stopped", "done", i, "of", len(ask))
			break
		}
		e, err := ef.FetchEndpoints(ctx, m, r.now())
		if err == nil {
			err = r.catalog.StoreEndpoints(ctx, e)
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			failed++
		}
	}
	if failed > 0 {
		r.log.Warn("openrouter catalog: endpoints could not be fetched for some models; the next tick asks again", "failed", failed, "of", len(ask))
	}
	if left > 0 {
		r.log.Info("openrouter catalog: endpoints left for a later round", "left", left, "asked", len(ask))
	}
}

func (r *Refresher) logStored(snap pricefetch.Snapshot, res orcatalog.Result) {
	r.log.Info("openrouter catalog stored", "source", orcatalog.SourceTag(snap.FetchedAt), "models", res.Models, "limitProposals", res.Proposals)
}

// openRouterModelsInUse lists, sorted and once each, the names of the enabled models of enabled
// openrouter providers in every org. Names only: no org, provider or secret leaves this function.
func (r *Refresher) openRouterModelsInUse(ctx context.Context) ([]string, error) {
	rows, err := store.New(r.svc.pool).ListModelsOfProviderType(ctx, pricefetch.ProviderTypesOf(pricefetch.SourceOpenRouter)[0])
	if err != nil {
		return nil, err
	}
	names := make([]string, len(rows))
	for i, row := range rows {
		names[i] = row.Name
	}
	return slices.Clone(orcatalog.Names(names)), nil
}
