// SPDX-License-Identifier: Apache-2.0

import { BuildingIcon } from 'lucide-react'
import { useMemo, useState } from 'react'
import { OpenOrgButton } from '@/components/open-org-button'
import { DataTable } from '@/components/ui/data-table'
import { EmptyState } from '@/components/ui/empty-state'
import { QueryError } from '@/components/ui/inline-error'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { PageHeader } from '@/components/ui/page-header'
import { SkeletonRows } from '@/components/ui/skeleton'
import type { OrgSummary } from '@/gen/koinon/platform/v1alpha1/platform_pb'
import { useAllOrgs } from '@/hooks/use-all-orgs'
import { dataTableColumns, type DataTableColumn } from '@/lib/data-table'
import { useOrgSession } from '@/lib/org-context'
import { matchesOrgSearch, orgSummaryName, roleText, spendText } from '@/lib/platform-orgs'

const col = dataTableColumns<OrgSummary>()

/**
 * PlatformOrgsPage lists every team org of the platform (ADR 0126 section 4; board "Platform console: orgs"). A
 * platform admin opens a team org of their own with "Open", and any other org with "Open as platform admin".
 * Personal orgs are not in the list until v0.2. There is no "Create org", "Archived" or "Near budget" yet.
 */
export function PlatformOrgsPage() {
  const { memberships } = useOrgSession()
  const list = useAllOrgs()
  const [q, setQ] = useState('')
  const shown = useMemo(() => (list.orgs ?? []).filter((o) => matchesOrgSearch(o, q)), [list.orgs, q])
  const columns = useMemo<DataTableColumn<OrgSummary>[]>(
    () => [
      col.accessor(orgSummaryName, {
        id: 'org',
        header: 'Org',
        sortFn: 'text',
        meta: { rowHeader: true },
        cell: (c) => (
          <span className="grid">
            <span className="font-medium">{c.getValue()}</span>
            <code className="text-muted-foreground font-mono text-xs">{c.row.original.slug}</code>
          </span>
        ),
      }),
      col.accessor('memberCount', { header: 'Members', sortFn: 'basic' }),
      col.accessor('agentCount', { header: 'Agents', sortFn: 'basic' }),
      col.accessor((o) => Number(o.spentThisMonthUsd) || 0, { id: 'spend', header: 'Spend this month', sortFn: 'basic', cell: (c) => spendText(c.row.original) }),
      col.accessor((o) => roleText(o, memberships), { id: 'role', header: 'Your role', sortFn: 'text' }),
      col.display({
        id: 'actions',
        header: () => <span className="sr-only">Actions</span>,
        cell: (c) => <OpenOrgButton org={c.row.original} />,
      }),
    ],
    [memberships],
  )
  return (
    <div className="mx-auto grid max-w-5xl grid-cols-[minmax(0,1fr)] gap-4">
      <PageHeader title="Orgs" description="All the team orgs of this Koinon. Personal orgs are not in this list." />
      <div className="grid max-w-sm gap-1.5">
        <Label htmlFor="org-search">Search orgs</Label>
        <Input id="org-search" type="search" placeholder="Name or slug" value={q} onChange={(e) => setQ(e.target.value)} />
      </div>
      <QueryError error={list.error ?? undefined} />
      {list.isPending ? (
        <SkeletonRows label="Loading orgs…" />
      ) : list.error && !list.orgs ? null : (
        <>
          <DataTable
            caption="Orgs"
            columns={columns}
            data={shown}
            getRowId={(o) => o.orgId}
            initialSorting={[{ id: 'org', desc: false }]}
            empty={<EmptyState icon={BuildingIcon} title={q.trim() ? 'No org matches the search.' : 'No orgs yet.'} />}
          />
          {list.isFetchingMore && (
            <p role="status" className="text-muted-foreground text-sm">
              Loading more orgs…
            </p>
          )}
        </>
      )}
    </div>
  )
}
