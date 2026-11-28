// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"

	connect "connectrpc.com/connect/v2"

	"github.com/stateracloud/koinon/internal/auth"
	"github.com/stateracloud/koinon/internal/gen/koinon/audit/v1alpha1/auditv1alpha1connect"
	"github.com/stateracloud/koinon/internal/gen/koinon/budget/v1alpha1/budgetv1alpha1connect"
	"github.com/stateracloud/koinon/internal/gen/koinon/outward/v1alpha1/outwardv1alpha1connect"
	"github.com/stateracloud/koinon/internal/gen/koinon/platform/v1alpha1/platformv1alpha1connect"
	refusalv1 "github.com/stateracloud/koinon/internal/gen/koinon/refusal/v1"
	"github.com/stateracloud/koinon/internal/gen/koinon/token/v1alpha1/tokenv1alpha1connect"
	"github.com/stateracloud/koinon/internal/gen/koinon/trace/v1alpha1/tracev1alpha1connect"
	"github.com/stateracloud/koinon/internal/gen/koinon/usage/v1alpha1/usagev1alpha1connect"
)

// The platform-admin check of every platform-scope RPC (issue #1292). requirePlatformAdmin is the only
// code in this package that reads Principal.PlatformAdmin to refuse a call or sets the
// PLATFORM_ADMIN_REQUIRED reason; the "platform-admin check" specs fail on a copy. A platform-scope RPC
// calls it first, before it parses or reads anything, so a refusal does not show whether an org or an
// object exists. Every platform-scope RPC is listed in platformAdminRPCs; the "platform-admin registry"
// spec in internal/api refuses each one to an org owner, admin, member and viewer.

// requirePlatformAdmin returns the caller when it is a platform admin, else PermissionDenied with the
// reason PLATFORM_ADMIN_REQUIRED. Membership of an org does not count: an org owner or admin is not a
// platform admin.
func requirePlatformAdmin(ctx context.Context) (*auth.Principal, error) {
	p, ok := auth.PrincipalFrom(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, "login required")
	}
	if !p.PlatformAdmin {
		return nil, withReason(connect.NewError(connect.CodePermissionDenied, "only a platform admin can do this"), refusalv1.Reason_PLATFORM_ADMIN_REQUIRED)
	}
	return p, nil
}

// platformAdminRPCs lists every platform-scope RPC by procedure. The value is empty when the RPC is
// for platform admins only; else it says which requests of the RPC are platform-scope (the other
// requests are org-scope and use authorize).
var platformAdminRPCs = map[string]string{
	platformv1alpha1connect.PlatformServiceListOrgsProcedure:                       "",
	platformv1alpha1connect.PlatformServiceGetOrgProcedure:                         "",
	platformv1alpha1connect.PlatformResourceServiceListProvidersProcedure:          "",
	platformv1alpha1connect.PlatformResourceServiceListTemplatesProcedure:          "",
	platformv1alpha1connect.PlatformResourceServiceListMcpServersProcedure:         "",
	usagev1alpha1connect.UsageServiceGetPlatformUsageProcedure:                     "",
	usagev1alpha1connect.UsageServiceExportPlatformUsageProcedure:                  "",
	budgetv1alpha1connect.BudgetServiceListOrgBudgetsProcedure:                     "",
	budgetv1alpha1connect.BudgetServiceSetPlatformCeilingProcedure:                 "",
	budgetv1alpha1connect.PriceServiceListPriceProposalsProcedure:                  "",
	budgetv1alpha1connect.PriceServiceGetPriceProposalProcedure:                    "",
	budgetv1alpha1connect.PriceServiceAcceptPriceProposalProcedure:                 "",
	budgetv1alpha1connect.PriceServiceDismissPriceProposalProcedure:                "",
	budgetv1alpha1connect.PriceServiceListPriceSourceFetchesProcedure:              "",
	budgetv1alpha1connect.PriceServiceListPlatformPriceVersionsProcedure:           "",
	budgetv1alpha1connect.PriceServiceGetPlatformPriceVersionProcedure:             "",
	budgetv1alpha1connect.PriceServiceDiffPlatformPriceVersionsProcedure:           "",
	budgetv1alpha1connect.PriceServiceRollbackPlatformPricesProcedure:              "",
	budgetv1alpha1connect.PriceServiceGetPriceRefreshSettingsProcedure:             "",
	budgetv1alpha1connect.PriceServiceCheckPriceSourcesProcedure:                   "",
	budgetv1alpha1connect.PriceServiceExplainPlatformPriceProcedure:                "",
	tracev1alpha1connect.TraceDestinationServiceGetPlatformProcedure:               "",
	tracev1alpha1connect.TraceDestinationServiceSetPlatformDestinationProcedure:    "",
	tracev1alpha1connect.TraceDestinationServiceDeletePlatformDestinationProcedure: "",
	tracev1alpha1connect.TraceDestinationServiceTestPlatformDestinationProcedure:   "",
	tracev1alpha1connect.TraceDestinationServiceSetPlatformCeilingProcedure:        "",
	tokenv1alpha1connect.TokenServiceRebindTokenProcedure:                          "",

	auditv1alpha1connect.AuditServiceListProcedure:                         "scope AUDIT_SCOPE_INSTALL, or an empty org_id (every org)",
	outwardv1alpha1connect.OutwardServiceSetOutwardTargetDisabledProcedure: "a platform kind (an install-wide target)",
}

// PlatformAdminRPCs returns a copy of the platform-scope RPC registry: procedure to the condition
// under which the RPC is platform-scope, empty when it always is. Tests walk it.
func PlatformAdminRPCs() map[string]string {
	out := make(map[string]string, len(platformAdminRPCs))
	for k, v := range platformAdminRPCs {
		out[k] = v
	}
	return out
}
