// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgtype"

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
// A provider with an enabled model that has no price is failed as unknown ("no price: model"): the
// gateway does not serve it by design (ADR 0041), and the page must not show it as waiting forever.
//
// A provider the pass left out of the render (its URL is not allowed, its key cannot be opened, a
// price is unusable) is failed as unknown with the reason as detail.
//
// An org whose apply took effect without a rejection has its failures cleared. Only providers with an
// unserved model get a failure; a fully served provider keeps serving with the previous config.
func (r *Reconciler) recordProviderStatus(ctx context.Context, orgs []gatewayplan.Org, res gatewayplan.ApplyResult, applyErr error, configRejected string, served []gatewayplan.ServedModels, skipped []skippedProvider) error {
	var errs []error
	// set writes the skipped providers' own rows first, then the org-wide reason for the others.
	set := func(org gatewayplan.Org, reason, detail string) {
		skip := []pgtype.UUID{} // never nil: a NULL array would match no provider
		for _, k := range skipped {
			if k.orgID != org.ID {
				continue
			}
			skip = append(skip, k.providerID)
			if err := r.q.UpsertGatewayProviderFailure(ctx, store.UpsertGatewayProviderFailureParams{OrgID: k.orgID, ProviderID: k.providerID, Reason: k.reason, Detail: truncateUTF8(k.detail, failureDetailMax)}); err != nil {
				errs = append(errs, fmt.Errorf("reconciler: record skipped provider for org %s: %w", org.Slug, err))
			}
		}
		if len(detail) > failureDetailMax {
			detail = truncateUTF8(detail, failureDetailMax)
		}
		if err := r.q.SetGatewayProviderFailures(ctx, store.SetGatewayProviderFailuresParams{OrgID: org.ID, Reason: reason, Detail: detail, SkipIds: skip}); err != nil {
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
			continue
		}
		// Providers the pass left out of the render are in no Apply result: record them here.
		for _, k := range skipped {
			if k.orgID != s.Org.ID {
				continue
			}
			if err := r.q.SetGatewayProviderFailure(ctx, store.SetGatewayProviderFailureParams{OrgID: k.orgID, ProviderID: k.providerID, Reason: k.reason, Detail: truncateUTF8(k.detail, failureDetailMax)}); err != nil {
				errs = append(errs, fmt.Errorf("reconciler: record skipped provider for org %s: %w", s.Org.Slug, err))
			}
		}
		if len(s.Unpriced) == 0 {
			continue
		}
		if err := r.q.SetGatewayUnpricedFailures(ctx, store.SetGatewayUnpricedFailuresParams{OrgID: s.Org.ID, ModelIds: s.Unpriced}); err != nil {
			errs = append(errs, fmt.Errorf("reconciler: record unpriced models for org %s: %w", s.Org.Slug, err))
		}
	}
	return errors.Join(errs...)
}

// truncateUTF8 cuts s to at most n bytes without splitting a rune.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func rejectionText(x gateway.Rejection) string { return x.Kind + " " + x.Name + ": " + x.Reason }

func joinReason(a, b string) string {
	if a == "" {
		return b
	}
	return strings.Join([]string{a, b}, "; ")
}
