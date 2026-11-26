// SPDX-License-Identifier: Apache-2.0

import { createContext, useContext } from 'react'
import type { Membership, WhoAmIResponse } from '@/gen/koinon/auth/v1alpha1/auth_pb'

export type OrgContextValue = {
  org: Membership
  /** The team orgs of the user: the personal org is not in the list (ADR 0126 section 4). */
  memberships: readonly Membership[]
  setOrgId: (id: string) => void
  /**
   * acting is true when a platform admin opened the org from the platform console without being a member of it. The
   * org area then shows the "acting as platform admin" banner on every page (ADR 0126 section 4).
   */
  acting: boolean
}

export const OrgContext = createContext<OrgContextValue | null>(null)

/** useOrg returns the org the user works in; every RPC names it. */
export function useOrg(): OrgContextValue {
  const v = useContext(OrgContext)
  if (!v) throw new Error('useOrg outside the org area')
  return v
}

/**
 * useOptionalOrg returns the org the user works in, or undefined in the platform console, which has no org. Only a
 * component that also serves a platform target (ADR 0126 section 4) uses it; an org page uses useOrg.
 */
export function useOptionalOrg(): OrgContextValue | undefined {
  return useContext(OrgContext) ?? undefined
}

/**
 * OrgSession is the org state of the signed-in user across the org area and the platform console (ADR 0126 section
 * 4). OrgProvider holds it.
 */
export type OrgSession = {
  /** The signed-in user, as WhoAmI says. */
  who: WhoAmIResponse
  /** The team orgs of the user. */
  memberships: readonly Membership[]
  platformAdmin: boolean
  /** The last team org that the user used, or undefined when the user has no team org. */
  lastOrg: Membership | undefined
  /** The org that a platform admin opened without being a member of it, or undefined. */
  actingOrgId: string | undefined
  /** openOrg opens a team org of the user: it becomes the last org used, and acting ends. */
  openOrg: (id: string) => void
  /** openAsPlatformAdmin opens an org of which the platform admin is not a member. A team org opens with openOrg. */
  openAsPlatformAdmin: (id: string) => void
  /** endActing ends the acting state. The platform console calls it when it opens. */
  endActing: () => void
}

export const OrgSessionContext = createContext<OrgSession | null>(null)

/** useOrgSession returns the org state of the signed-in user. */
export function useOrgSession(): OrgSession {
  const v = useContext(OrgSessionContext)
  if (!v) throw new Error('useOrgSession outside OrgProvider')
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
