// SPDX-License-Identifier: Apache-2.0

import { PlugIcon } from 'lucide-react'
import { useMemo, useState } from 'react'
import { OrgCell, PlatformResourceFilters } from '@/components/platform-org-filter'
import { DataTable } from '@/components/ui/data-table'
import { EmptyState } from '@/components/ui/empty-state'
import { QueryError } from '@/components/ui/inline-error'
import { PageHeader } from '@/components/ui/page-header'
import { SkeletonRows } from '@/components/ui/skeleton'
import type { OrgMcpServer } from '@/gen/koinon/platform/v1alpha1/resources_pb'
import { usePlatformMcpServers } from '@/hooks/use-platform-resources'
import { dataTableColumns, type DataTableColumn } from '@/lib/data-table'
import { transportLabel } from '@/lib/mcp-servers'
import { matchesResourceSearch, platformOwnerName, serverStatusText } from '@/lib/platform-resources'

const col = dataTableColumns<OrgMcpServer>()

/**
 * PlatformMcpServersPage lists the remote MCP servers of every team org (ADR 0126 section 4, issue #1412). It only
 * reads: the tool review and every change stay on the org's own MCP servers page.
 */
export function PlatformMcpServersPage() {
  const [orgId, setOrgId] = useState('')
  const [q, setQ] = useState('')
  const list = usePlatformMcpServers(orgId)
  const shown = useMemo(
    () => (list.rows ?? []).filter((r) => matchesResourceSearch(q, r.server?.name ?? '', r.server?.url ?? '', platformOwnerName(r.org))),
    [list.rows, q],
  )
  const columns = useMemo<DataTableColumn<OrgMcpServer>[]>(
    () => [
      col.accessor((r) => r.server?.name ?? '', {
        id: 'server',
        header: 'Server',
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
      col.accessor((r) => r.server?.url ?? '', {
        id: 'url',
        header: 'URL',
        cell: (c) => <code className="font-mono text-xs break-all">{c.getValue()}</code>,
      }),
      col.accessor((r) => transportLabel(r.server?.transport ?? ''), {
        id: 'transport',
        header: 'Transport',
        sortFn: 'text',
      }),
      col.accessor((r) => (r.server ? serverStatusText(r.server) : ''), {
        id: 'status',
        header: 'Status',
        sortFn: 'text',
      }),
    ],
    [],
  )
  return (
    <div className="mx-auto grid max-w-5xl grid-cols-[minmax(0,1fr)] gap-4">
      <PageHeader title="MCP servers" description="The remote MCP servers of all the team orgs. To review tools or change a server, open its org." />
      <PlatformResourceFilters
        idPrefix="server"
        orgId={orgId}
        onOrgId={setOrgId}
        q={q}
        onQ={setQ}
        searchLabel="Search MCP servers"
        searchPlaceholder="Name, URL or org"
      />
      <QueryError error={list.error ?? undefined} />
      {list.isPending ? (
        <SkeletonRows label="Loading MCP servers…" />
      ) : list.error && !list.rows ? null : (
        <>
          <DataTable
            caption="MCP servers"
            columns={columns}
            data={shown}
            getRowId={(r) => `${r.org?.orgId ?? ''}/${r.server?.id ?? ''}`}
            initialSorting={[{ id: 'org', desc: false }]}
            empty={<EmptyState icon={PlugIcon} title={q.trim() ? 'No MCP server matches the search.' : 'No MCP servers yet.'} />}
          />
          {list.isFetchingMore && (
            <p role="status" className="text-muted-foreground text-sm">
              Loading more MCP servers…
            </p>
          )}
        </>
      )}
    </div>
  )
}
