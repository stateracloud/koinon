// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"fmt"
	"time"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/ledger"
	"github.com/stateracloud/koinon/internal/mcpservers"
)

// applyModelCheck gives each org that serves a priced model its model-check key (ADR 0056
// amendment): the system principal for the provider echo check. The key requests only the models
// the state serves to the org, and carries the net of its small daily budget and the org's
// remaining caps, so a runaway caller is stopped by the gateway (standalone) or by the key being
// taken out (Kubernetes mode) like an agent's key. An org that serves no priced model has no key.
// A budget that cannot be computed fails the key closed to zero for this pass. It returns the
// errors of the orgs it could not serve; none stops the pass.
func (r *Reconciler) applyModelCheck(ctx context.Context, state *gateway.State, orgs []store.ListOrgsToReconcileRow, now time.Time) []error {
	var errs []error
	for _, org := range orgs {
		names := state.ServedModels(org.Slug)
		if len(names) == 0 {
			continue
		}
		ref := gateway.AgentRef{Org: org.Slug, Name: gateway.ModelCheckAgentName}
		key, err := r.systemKey(ctx, mcpservers.ModelCheckSecretName(org.ID), org, ref)
		if err != nil {
			r.m.PassErrors.WithLabelValues(errKindProvider).Inc()
			errs = append(errs, fmt.Errorf("model-check key of org %s: %w", org.Slug, err))
			continue
		}
		state.AddKey(key)
		state.RestrictModels(ref, names...)
		l, err := ledger.SystemLimit(ctx, r.q, org.ID, now)
		if err != nil {
			r.m.PassErrors.WithLabelValues(errKindSpendLimit).Inc()
			errs = append(errs, fmt.Errorf("model-check budget of org %s not computed (key fails closed): %w", org.Slug, err))
			l = &gateway.Limit{Unit: gateway.USD, Amount: 0, Window: ledger.Window, Generation: "failed"}
		}
		state.SetLimit(ref, l)
	}
	return errs
}
