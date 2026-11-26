// SPDX-License-Identifier: Apache-2.0

import type { Membership } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import type { OrgSummary } from '@/gen/koinon/platform/v1alpha1/platform_pb'
import { formatUSD } from '@/lib/cost'
import { orgRoleLabel } from '@/lib/labels'

/** orgSummaryName is the name of an org in the platform console: its display name, else its slug. */
export function orgSummaryName(o: OrgSummary): string {
  return o.displayName || o.slug
}

/** spendText is the spend of an org this month against its monthly budget: "$12.00 of $100.00". */
export function spendText(o: OrgSummary): string {
  const spent = formatUSD(o.spentThisMonthUsd)
  return o.monthlyCapUsd ? `${spent} of ${formatUSD(o.monthlyCapUsd)}` : `${spent}, no monthly budget`
}

/** roleText is the role of the platform admin in an org, or "Not a member". */
export function roleText(o: OrgSummary, memberships: readonly Membership[]): string {
  const m = memberships.find((x) => x.orgId === o.orgId)
  return m ? orgRoleLabel(m.role) : 'Not a member'
}

/** matchesOrgSearch says whether an org matches the search text: a part of its name or its slug, in any letter case. */
export function matchesOrgSearch(o: OrgSummary, q: string): boolean {
  const t = q.trim().toLowerCase()
  return !t || o.displayName.toLowerCase().includes(t) || o.slug.toLowerCase().includes(t)
}
