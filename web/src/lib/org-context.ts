// SPDX-License-Identifier: Apache-2.0

import { createContext, useContext } from 'react'
import type { Membership } from '@/gen/koinon/auth/v1alpha1/auth_pb'

export type OrgContextValue = {
  org: Membership
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
