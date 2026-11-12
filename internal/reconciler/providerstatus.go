// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/reconciler/gatewayplan"
)

// failureDetailMax bounds the gateway's own text kept for the UI.
const failureDetailMax = 500

// recordProviderStatus records why the gateway does not serve providers (issue #17), so the UI can
// show failed with a reason instead of a provider that stays saved. The served models themselves are
// recorded by recordServed; this adds only the failures:
//   - the apply failed whole: the config was refused (configRejected, with its reason) or the gateway
//     was unreachable or the reload did not complete (applyErr);
//   - the gateway rejected the org's model objects one by one (a PruneOnly entry).
//
// An org whose apply took effect without a rejection has its failures cleared. Only providers with an
// unserved model get a failure; a fully served provider keeps serving with the previous config.
func (r *Reconciler) recordProviderStatus(ctx context.Context, orgs []gatewayplan.Org, res gatewayplan.ApplyResult, applyErr error, configRejected string, served []gatewayplan.ServedModels) error {
	var errs []error
	set := func(org gatewayplan.Org, reason, detail string) {
		if len(detail) > failureDetailMax {
			detail = detail[:failureDetailMax]
		}
		if err := r.q.SetGatewayProviderFailures(ctx, store.SetGatewayProviderFailuresParams{OrgID: org.ID, Reason: reason, Detail: detail}); err != nil {
			errs = append(errs, fmt.Errorf("reconciler: record provider failures for org %s: %w", org.Slug, err))
		}
	}
	if res.Failed {
		reason, detail := catalog.ReasonUnreachable, "the gateway could not be reached or did not finish reloading"
		if configRejected != "" {
			reason, detail = catalog.ReasonGatewayRejects, configRejected
		} else if applyErr != nil {
			detail = applyErr.Error()
		}
		for _, org := range orgs {
			set(org, reason, detail)
		}
		return errors.Join(errs...)
	}
	rejected := map[string]string{}
	for _, x := range res.Rejections {
		rejected[x.Org] = joinReason(rejected[x.Org], rejectionText(x))
	}
	for _, s := range served {
		if s.PruneOnly {
			set(s.Org, catalog.ReasonGatewayRejects, rejected[s.Org.Slug])
			continue
		}
		if err := r.q.ClearGatewayProviderFailures(ctx, s.Org.ID); err != nil {
			errs = append(errs, fmt.Errorf("reconciler: clear provider failures for org %s: %w", s.Org.Slug, err))
		}
	}
	return errors.Join(errs...)
}

func rejectionText(x gateway.Rejection) string { return x.Kind + " " + x.Name + ": " + x.Reason }

func joinReason(a, b string) string {
	if a == "" {
		return b
	}
	return strings.Join([]string{a, b}, "; ")
}
