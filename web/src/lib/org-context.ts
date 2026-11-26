// SPDX-License-Identifier: Apache-2.0

import { createContext, useContext } from 'react'
import type { Membership } from '@/gen/koinon/auth/v1alpha1/auth_pb'

export type OrgContextValue = {
  org: Membership
  /** The team orgs of the user: the personal org is not in the list (ADR 0126 section 4). */
  memberships: readonly Membership[]
  setOrgId: (id: string) => void
}

export const OrgContext = createContext<OrgContextValue | null>(null)

/** useOrg returns the org the user works in; every RPC names it. */
export function useOrg(): OrgContextValue {
  const v = useContext(OrgContext)
  if (!v) throw new Error('useOrg outside OrgProvider')
  return v
}

/** orgName is the name the UI shows for an org. */
export function orgName(m: Membership): string {
  return m.orgDisplayName || m.orgSlug
}

/**
 * teamMemberships leaves out the personal org of the user. The personal org stays in the data model (ADR 0018,
 * 0019), but the UI does not list it until v0.2 (ADR 0126 section 4).
 */
export function teamMemberships(memberships: readonly Membership[], personalOrgId: string | undefined): Membership[] {
  return memberships.filter((m) => !personalOrgId || m.orgId !== personalOrgId)
}
