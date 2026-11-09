// SPDX-License-Identifier: Apache-2.0

package agentd

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/prices"
)

// DefaultModelServeTimeout bounds the wait for the gateway to serve a run's model. A new
// provider's key file takes up to ProviderKeySettle (90 s in standalone mode) to reach the gateway
// pod, then a pass applies the config and agentgateway reloads it; five minutes is several times
// that.
const DefaultModelServeTimeout = 5 * time.Minute

// modelWaitKind is the run event recorded when a run starts waiting for its model (old issue 174).
const modelWaitKind = "run.waiting_for_model"

// waitModel waits until the gateway serves the run's model, so the harness's first model call is
// not refused (old issue 174). koinon-reconciler is the only component that knows: it leaves a new
// provider out of the gateway state until its key file has reached the gateway pod, and records in
// gateway_served_models the models of every state it applied. A model the org's catalog does not
// have is not waited for (the gateway answers for it as before). While the run waits it records
// run.waiting_for_model once, which the task timeline shows, and NOTIFYs the reconciler every
// ReconcileNudge so the pass that applies the provider follows soon after its key file settles.
// A model without a usable price, or whose provider's key was lost, fails the run at once, since the
// gateway never serves it. After
// ModelServeTimeout the run fails with a reason that names the provider. ModelServeTimeout must
// comfortably exceed the reconciler's --provider-key-settle plus a gateway apply.
func (r *runner) waitModel(ctx context.Context, model string) error {
	now := r.p.cfg.Now
	started := now()
	var (
		recorded bool
		nudged   time.Time
		poll     = r.p.cfg.Poll
	)
	for {
		m, err := r.q.GetModelServing(ctx, store.GetModelServingParams{OrgID: r.run.OrgID, Name: model})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			// Not in the catalog (a platform default model, or a name the org does not have).
			return nil
		case err != nil:
			// A database error is waited out like the sandbox wait does, within the same bound.
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if now().Sub(started) >= r.p.cfg.ModelServeTimeout {
				return fmt.Errorf("check whether the gateway serves the model %q: %w", model, err)
			}
			r.log.Warn("checking whether the gateway serves the model failed", "model", model, "err", err)
			if !sleep(ctx, r.p.cfg.Poll) {
				return ctx.Err()
			}
			continue
		case m.CredentialLost:
			// The provider's key was lost with a key-encryption key (ADR 0085 decision 13): the
			// gateway stops serving it and no wait brings it back.
			return &failure{reason: fmt.Sprintf("the API key of the model provider %q was lost, so the gateway does not "+
				"serve the model %q; enter the key again on the Providers page and run the task again", m.ProviderName, model)}
		case m.Served:
			if recorded {
				r.p.cfg.Metrics.ModelWait.Observe(now().Sub(started).Seconds())
				r.log.Info("the gateway serves the model now", "model", model, "provider", m.ProviderName, "waited", now().Sub(started))
			}
			return nil
		}
		if !recorded {
			// A model without a usable price is never served (ADR 0041): fail now, not after the wait.
			pm := prices.Model{ID: m.ModelID.String(), Name: model, ProviderType: m.ProviderType, ProviderURL: m.ProviderUrl}
			var bad *prices.BadPriceError
			switch _, err := r.p.cfg.Prices.Resolve(ctx, r.q, r.run.OrgID, pm, now()); {
			case errors.Is(err, prices.ErrUnpriced):
				return &failure{reason: fmt.Sprintf("the model %q has no price, so the gateway does not serve it; "+
					"add a price for it on the Prices page and run the task again", model)}
			case errors.As(err, &bad):
				return &failure{reason: fmt.Sprintf("the model %q is not served: %v; fix its price on the Prices page", model, err)}
			case err != nil && ctx.Err() != nil:
				return ctx.Err()
			case err != nil:
				// Not knowing the price is no reason to fail: the wait decides.
				r.log.Warn("resolving the model's price failed", "model", model, "err", err)
			}
			recorded = true
			r.log.Info("waiting for the model provider to reach the gateway", "model", model, "provider", m.ProviderName)
			if err := r.own.Record(ctx, modelWaitKind, map[string]any{
				"model": model, "provider": m.ProviderName,
				"reason": fmt.Sprintf("waiting for the model provider %q to reach the gateway", m.ProviderName),
			}); err != nil {
				return err
			}
		}
		if waited := now().Sub(started); waited >= r.p.cfg.ModelServeTimeout {
			r.p.cfg.Metrics.ModelWait.Observe(waited.Seconds())
			return &failure{reason: fmt.Sprintf("the gateway did not serve the model %q within %s: its provider %q has not "+
				"reached the gateway. Check the provider on the Providers page (its key, its URL) and the koinon-reconciler "+
				"log for why it is not served, then run the task again", model, r.p.cfg.ModelServeTimeout, m.ProviderName)}
		}
		if now().Sub(nudged) >= r.p.cfg.ReconcileNudge {
			nudged = now()
			if err := r.q.NotifyReconciler(ctx, r.run.OrgID); err != nil && ctx.Err() == nil {
				r.log.Warn("notifying the reconciler failed", "err", err)
			}
		}
		if !sleep(ctx, poll) {
			return ctx.Err()
		}
		// Back off to a few seconds: a provider takes a minute or more to reach the gateway.
		poll = min(2*poll, max(r.p.cfg.Poll, 5*time.Second))
	}
}
