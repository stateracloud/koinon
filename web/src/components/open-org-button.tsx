// SPDX-License-Identifier: Apache-2.0

import { useNavigate } from '@tanstack/react-router'
import { Button } from '@/components/ui/button'
import type { OrgSummary } from '@/gen/koinon/platform/v1alpha1/platform_pb'
import { useOrgSession } from '@/lib/org-context'
import { orgSummaryName } from '@/lib/platform-orgs'

/**
 * OpenOrgButton opens an org from the platform console (ADR 0126 section 4). A team org of the admin opens as usual
 * ("Open"). Any other org opens with the "acting as platform admin" banner ("Open as platform admin"). The accessible
 * name is the visible text and then the org name, so each button in a list has its own name (WCAG 2.5.3).
 */
export function OpenOrgButton({ org }: { org: OrgSummary }) {
  const s = useOrgSession()
  const navigate = useNavigate()
  const member = s.memberships.some((m) => m.orgId === org.orgId)
  const name = orgSummaryName(org)
  const label = member ? 'Open' : 'Open as platform admin'
  function open() {
    if (member) s.openOrg(org.orgId)
    else s.openAsPlatformAdmin(org.orgId)
    void navigate({ to: '/' })
  }
  return (
    <Button variant="outline" size="sm" onClick={open} aria-label={`${label}: ${name}`}>
      {label}
    </Button>
  )
}
