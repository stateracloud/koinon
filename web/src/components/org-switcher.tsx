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
 * there is no "Create org" item (ADR 0126 section 7).
 */
export function OrgSwitcher({ platformAdmin }: { platformAdmin: boolean }) {
  const { org, memberships, setOrgId } = useOrg()
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button variant="outline" className="h-auto w-full justify-between px-3 py-1.5 text-left" aria-label={`Org: ${orgName(org)}`}>
          <span className="grid min-w-0">
            <span className="truncate font-medium">{orgName(org)}</span>
            <span className="text-muted-foreground text-xs font-normal">{orgRoleLabel(org.role)}</span>
          </span>
          <ChevronsUpDownIcon aria-hidden className="text-muted-foreground" />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="start" className="w-60">
        <DropdownMenuLabel className="text-muted-foreground text-xs">Orgs</DropdownMenuLabel>
        <DropdownMenuRadioGroup aria-label="Orgs" value={org.orgId} onValueChange={setOrgId}>
          {memberships.map((m) => (
            <DropdownMenuRadioItem key={m.orgId} value={m.orgId}>
              <span className="truncate">{orgName(m)}</span>
              <span className="text-muted-foreground ml-auto text-xs">{orgRoleLabel(m.role)}</span>
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
