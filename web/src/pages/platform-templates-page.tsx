// SPDX-License-Identifier: Apache-2.0

import { LayoutTemplateIcon } from 'lucide-react'
import { useMemo, useState } from 'react'
import { OrgCell, PlatformResourceFilters } from '@/components/platform-org-filter'
import { DataTable } from '@/components/ui/data-table'
import { EmptyState } from '@/components/ui/empty-state'
import { QueryError } from '@/components/ui/inline-error'
import { PageHeader } from '@/components/ui/page-header'
import { SkeletonRows } from '@/components/ui/skeleton'
import type { OrgTemplate } from '@/gen/koinon/platform/v1alpha1/resources_pb'
import { usePlatformTemplates } from '@/hooks/use-platform-resources'
import { dataTableColumns, type DataTableColumn } from '@/lib/data-table'
import { matchesResourceSearch, platformOwnerName } from '@/lib/platform-resources'

const col = dataTableColumns<OrgTemplate>()

/**
 * PlatformTemplatesPage lists the platform templates and the org templates of every team org (ADR 0126 section 4,
 * issue #1412). A platform template shows "Platform" in the Org column. The page only reads.
 */
export function PlatformTemplatesPage() {
  const [orgId, setOrgId] = useState('')
  const [q, setQ] = useState('')
  const list = usePlatformTemplates(orgId)
  const shown = useMemo(
    () => (list.rows ?? []).filter((r) => matchesResourceSearch(q, r.template?.displayName ?? '', r.template?.slug ?? '', platformOwnerName(r.org))),
    [list.rows, q],
  )
  const columns = useMemo<DataTableColumn<OrgTemplate>[]>(
    () => [
      col.accessor((r) => r.template?.displayName || r.template?.slug || '', {
        id: 'template',
        header: 'Template',
        sortFn: 'text',
        meta: { rowHeader: true },
        cell: (c) => (
          <span className="grid">
            <span className="font-medium">{c.getValue()}</span>
            <code className="text-muted-foreground font-mono text-xs">{c.row.original.template?.slug}</code>
          </span>
        ),
      }),
      col.accessor((r) => platformOwnerName(r.org), {
        id: 'org',
        header: 'Org',
        sortFn: 'text',
        cell: (c) => <OrgCell name={c.getValue()} slug={c.row.original.org?.slug} />,
      }),
      col.accessor((r) => r.template?.defaultHarness ?? '', {
        id: 'harness',
        header: 'Default harness',
        sortFn: 'text',
      }),
      col.accessor((r) => r.template?.model ?? '', {
        id: 'model',
        header: 'Model',
        sortFn: 'text',
        cell: (c) => <code className="font-mono text-xs">{c.getValue()}</code>,
      }),
      col.accessor((r) => r.template?.version ?? '', {
        id: 'version',
        header: 'Version',
      }),
    ],
    [],
  )
  return (
    <div className="mx-auto grid max-w-5xl grid-cols-[minmax(0,1fr)] gap-4">
      <PageHeader
        title="Templates"
        description={orgId ? 'The templates of the org that you selected.' : 'The platform templates and the templates of all the team orgs.'}
      />
      <PlatformResourceFilters
        idPrefix="template"
        orgId={orgId}
        onOrgId={setOrgId}
        q={q}
        onQ={setQ}
        searchLabel="Search templates"
        searchPlaceholder="Name, slug or org"
      />
      <QueryError error={list.error ?? undefined} />
      {list.isPending ? (
        <SkeletonRows label="Loading templates…" />
      ) : list.error && !list.rows ? null : (
        <>
          <DataTable
            caption="Templates"
            columns={columns}
            data={shown}
            getRowId={(r) => r.template?.id ?? ''}
            initialSorting={[{ id: 'org', desc: false }]}
            empty={<EmptyState icon={LayoutTemplateIcon} title={q.trim() ? 'No template matches the search.' : 'No templates yet.'} />}
          />
          {list.isFetchingMore && (
            <p role="status" className="text-muted-foreground text-sm">
              Loading more templates…
            </p>
          )}
        </>
      )}
    </div>
  )
}
