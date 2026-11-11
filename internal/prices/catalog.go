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
	if trigger == TriggerSchedule {
		listed := make([]string, len(snap.Meta))
		for i, m := range snap.Meta {
			listed[i] = m.Model
		}
		due, err := r.catalog.Due(ctx, snap.FetchedAt, inUse, listed)
		if err != nil {
			r.log.Warn("openrouter catalog: reading the last store", "err", err)
			return
		}
		if !due {
			return
		}
	}
	res, err := r.catalog.Apply(ctx, snap, inUse)
	if err != nil {
		r.log.Warn("openrouter catalog: storing the fetch", "err", err)
		return
	}
	r.log.Info("openrouter catalog stored", "source", orcatalog.SourceTag(snap.FetchedAt), "models", res.Models, "limitProposals", res.Proposals)
	ef, ok := a.(pricefetch.EndpointsFetcher)
	if !ok {
		return
	}
	failed := 0
	for i, m := range inUse {
		if i == orcatalog.MaxEndpointModels || ctx.Err() != nil {
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
		r.log.Warn("openrouter catalog: endpoints could not be fetched for some models", "failed", failed, "of", min(len(inUse), orcatalog.MaxEndpointModels))
	}
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
