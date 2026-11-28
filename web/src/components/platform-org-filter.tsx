// SPDX-License-Identifier: Apache-2.0

import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { NativeSelect } from '@/components/ui/native-select'
import { useAllOrgs } from '@/hooks/use-all-orgs'
import { orgSummaryName } from '@/lib/platform-orgs'

/**
 * PlatformResourceFilters are the filters above a list of org resources in the platform console: one org or all of
 * them (the server filters), and a search text (the page filters).
 */
export function PlatformResourceFilters({
  idPrefix,
  orgId,
  onOrgId,
  q,
  onQ,
  searchLabel,
  searchPlaceholder,
}: {
  idPrefix: string
  orgId: string
  onOrgId: (id: string) => void
  q: string
  onQ: (q: string) => void
  searchLabel: string
  searchPlaceholder: string
}) {
  const orgs = useAllOrgs()
  return (
    <div className="flex flex-wrap gap-4">
      <div className="grid w-full max-w-xs gap-1.5">
        <Label htmlFor={`${idPrefix}-org`}>Org</Label>
        <NativeSelect id={`${idPrefix}-org`} value={orgId} onChange={(e) => onOrgId(e.target.value)}>
          <option value="">All orgs</option>
          {(orgs.orgs ?? []).map((o) => (
            <option key={o.orgId} value={o.orgId}>
              {orgSummaryName(o)} ({o.slug})
            </option>
          ))}
        </NativeSelect>
      </div>
      <div className="grid w-full max-w-sm gap-1.5">
        <Label htmlFor={`${idPrefix}-search`}>{searchLabel}</Label>
        <Input id={`${idPrefix}-search`} type="search" placeholder={searchPlaceholder} value={q} onChange={(e) => onQ(e.target.value)} />
      </div>
    </div>
  )
}

/** OrgCell is the Org column of a list of org resources: the org's name and its slug. */
export function OrgCell({ name, slug }: { name: string; slug?: string }) {
  return (
    <span className="grid">
      <span className="font-medium">{name}</span>
      {slug && <code className="text-muted-foreground font-mono text-xs">{slug}</code>}
    </span>
  )
}
