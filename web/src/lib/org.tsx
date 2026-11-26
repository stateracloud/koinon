// SPDX-License-Identifier: Apache-2.0

import { useCallback, useMemo, useState, type ReactNode } from 'react'
import type { WhoAmIResponse } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { OrgSessionContext, teamMemberships, type OrgSession } from '@/lib/org-context'
import { actingKey, lastOrgKey, local, read, session, write } from '@/lib/org-storage'


/**
 * OrgProvider holds the org state of the signed-in user (OrgSession). The personal org is not one of the orgs (ADR
 * 0126 section 4). A platform admin can also act in an org of which they are not a member; the state is ignored for
 * any other user.
 */
export function OrgProvider({ who, children }: { who: WhoAmIResponse; children: ReactNode }) {
  const memberships = useMemo(() => teamMemberships(who.memberships, who.user?.personalOrgId), [who.memberships, who.user?.personalOrgId])
  const platformAdmin = who.platformAdmin
  const [chosen, setChosen] = useState(() => read(local, lastOrgKey))
  const [acting, setActing] = useState(() => read(session, actingKey))
  // The callbacks keep their identity, so a page can run an effect once with them.
  const openOrg = useCallback((id: string) => {
    setChosen(id)
    write(local, lastOrgKey, id)
    setActing(undefined)
    write(session, actingKey, undefined)
  }, [])
  const openAsPlatformAdmin = useCallback((id: string) => {
    setActing(id)
    write(session, actingKey, id)
  }, [])
  const endActing = useCallback(() => {
    setActing(undefined)
    write(session, actingKey, undefined)
  }, [])
  const value = useMemo<OrgSession>(() => {
    const lastOrg = memberships.find((m) => m.orgId === chosen) ?? memberships[0]
    // A platform admin who became a member of the org since works in it as a member.
    const actingOrgId = platformAdmin && acting && !memberships.some((m) => m.orgId === acting) ? acting : undefined
    return { who, memberships, platformAdmin, lastOrg, actingOrgId, openOrg, openAsPlatformAdmin, endActing }
  }, [who, memberships, platformAdmin, chosen, acting, openOrg, openAsPlatformAdmin, endActing])
  return <OrgSessionContext.Provider value={value}>{children}</OrgSessionContext.Provider>
}
