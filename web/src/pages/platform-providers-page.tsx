// SPDX-License-Identifier: Apache-2.0

import { ServerCogIcon } from 'lucide-react'
import { useMemo, useState } from 'react'
import { OrgCell, PlatformResourceFilters } from '@/components/platform-org-filter'
import { DataTable } from '@/components/ui/data-table'
import { EmptyState } from '@/components/ui/empty-state'
import { QueryError } from '@/components/ui/inline-error'
import { PageHeader } from '@/components/ui/page-header'
import { SkeletonRows } from '@/components/ui/skeleton'
import type { OrgProvider } from '@/gen/koinon/platform/v1alpha1/resources_pb'
import { usePlatformProviders } from '@/hooks/use-platform-resources'
import { dataTableColumns, type DataTableColumn } from '@/lib/data-table'
import { matchesResourceSearch, platformOwnerName, providerStatusText } from '@/lib/platform-resources'
import { providerTypeLabel } from '@/lib/providers'

const col = dataTableColumns<OrgProvider>()

/**
 * PlatformProvidersPage lists the model providers of every team org (ADR 0126 section 4, issue #1412). It only reads:
 * a platform admin changes a provider on the org's own Providers page.
 */
export function PlatformProvidersPage() {
  const [orgId, setOrgId] = useState('')
  const [q, setQ] = useState('')
  const list = usePlatformProviders(orgId)
  const shown = useMemo(
    () =>
      (list.rows ?? []).filter((r) =>
        matchesResourceSearch(q, r.provider?.name ?? '', r.provider?.type ?? '', r.provider?.url ?? '', platformOwnerName(r.org)),
      ),
    [list.rows, q],
  )
  const columns = useMemo<DataTableColumn<OrgProvider>[]>(
    () => [
      col.accessor((r) => r.provider?.name ?? '', {
        id: 'provider',
        header: 'Provider',
        sortFn: 'text',
        meta: { rowHeader: true },
        cell: (c) => <span className="font-medium">{c.getValue()}</span>,
      }),
      col.accessor((r) => platformOwnerName(r.org), {
        id: 'org',
        header: 'Org',
        sortFn: 'text',
        cell: (c) => <OrgCell name={c.getValue()} slug={c.row.original.org?.slug} />,
      }),
      col.accessor((r) => providerTypeLabel(r.provider?.type ?? ''), {
        id: 'type',
        header: 'Type',
        sortFn: 'text',
      }),
      col.accessor((r) => r.provider?.url ?? '', {
        id: 'url',
        header: 'URL',
        cell: (c) => <code className="font-mono text-xs break-all">{c.getValue()}</code>,
      }),
      col.accessor((r) => (r.provider ? providerStatusText(r.provider) : ''), {
        id: 'status',
        header: 'Status',
        sortFn: 'text',
      }),
    ],
    [],
  )
  return (
    <div className="mx-auto grid max-w-5xl grid-cols-[minmax(0,1fr)] gap-4">
      <PageHeader title="Providers" description="The model providers of all the team orgs. To change a provider, open its org." />
      <PlatformResourceFilters
        idPrefix="provider"
        orgId={orgId}
        onOrgId={setOrgId}
        q={q}
        onQ={setQ}
        searchLabel="Search providers"
        searchPlaceholder="Name, type, URL or org"
      />
      <QueryError error={list.error ?? undefined} />
      {list.isPending ? (
        <SkeletonRows label="Loading providers…" />
      ) : list.error && !list.rows ? null : (
        <>
          <DataTable
            caption="Providers"
            columns={columns}
            data={shown}
            getRowId={(r) => `${r.org?.orgId ?? ''}/${r.provider?.id ?? ''}`}
            initialSorting={[{ id: 'org', desc: false }]}
            empty={<EmptyState icon={ServerCogIcon} title={q.trim() ? 'No provider matches the search.' : 'No providers yet.'} />}
          />
          {list.isFetchingMore && (
            <p role="status" className="text-muted-foreground text-sm">
              Loading more providers…
            </p>
          )}
        </>
      )}
    </div>
  )
}
