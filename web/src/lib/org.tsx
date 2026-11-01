// SPDX-License-Identifier: Apache-2.0

import { useMemo, useState, type ReactNode } from 'react'
import type { Membership } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { OrgContext, type OrgContextValue } from '@/lib/org-context'

const storageKey = 'koinon.org'

function stored(): string | null {
  try {
    return localStorage.getItem(storageKey)
  } catch {
    return null
  }
}

/** OrgProvider holds the org the user works in, remembered per browser. */
export function OrgProvider({ memberships, children }: { memberships: readonly Membership[]; children: ReactNode }) {
  const [chosen, setChosen] = useState<string | null>(stored)
  const value = useMemo<OrgContextValue | null>(() => {
    const org = memberships.find((m) => m.orgId === chosen) ?? memberships[0]
    if (!org) return null
    return {
      org,
      memberships,
      setOrgId: (id) => {
        setChosen(id)
        try {
          localStorage.setItem(storageKey, id)
        } catch {
          // private mode: the choice lasts for this page only
        }
      },
    }
  }, [memberships, chosen])
  if (!value) return <p className="p-6">You are not a member of any org yet.</p>
  return <OrgContext.Provider value={value}>{children}</OrgContext.Provider>
}
