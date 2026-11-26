// SPDX-License-Identifier: Apache-2.0

import { Link } from '@tanstack/react-router'
import { ChevronsUpDownIcon } from 'lucide-react'
import { Button } from '@/components/ui/button'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { orgRoleLabel } from '@/lib/labels'
import { orgName, useOrg } from '@/lib/org-context'

/**
 * OrgSwitcher is the org menu under the logo (ADR 0126 section 4): the team orgs of the user with the role, and a
 * "Platform console" item for platform admins. The personal org is not in the list (OrgProvider leaves it out), and
 * there is no "Create org" item (ADR 0126 section 7). In an org that a platform admin opened without being a member
 * of it, the role reads "Platform admin" and no item is checked; picking a team org ends the acting state.
 */
export function OrgSwitcher({ platformAdmin }: { platformAdmin: boolean }) {
  const { org, memberships, setOrgId, acting } = useOrg()
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        {/* The name is the visible text with a hidden "Org:" in front (WCAG 2.5.3, label in name). */}
        <Button variant="outline" className="h-auto w-full justify-between px-3 py-1.5 text-left">
          <span className="grid min-w-0">
            <span className="truncate font-medium">
              <span className="sr-only">Org:</span>{' '}
              {orgName(org)}
            </span>
            <span className="text-muted-foreground text-xs font-normal">{acting ? 'Platform admin' : orgRoleLabel(org.role)}</span>
          </span>
          <ChevronsUpDownIcon aria-hidden className="text-muted-foreground" />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="start" className="w-60">
        <DropdownMenuLabel className="text-xs">Orgs</DropdownMenuLabel>
        <DropdownMenuRadioGroup aria-label="Orgs" value={org.orgId} onValueChange={setOrgId}>
          {memberships.map((m) => (
            <DropdownMenuRadioItem key={m.orgId} value={m.orgId}>
              <span className="truncate">{orgName(m)}</span>
              <span className="ml-auto text-xs">{orgRoleLabel(m.role)}</span>
            </DropdownMenuRadioItem>
          ))}
        </DropdownMenuRadioGroup>
        {platformAdmin && (
          <>
            <DropdownMenuSeparator />
            <DropdownMenuItem asChild>
              <Link to="/platform">Platform console</Link>
            </DropdownMenuItem>
          </>
        )}
      </DropdownMenuContent>
    </DropdownMenu>
  )
}
