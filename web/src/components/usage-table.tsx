// SPDX-License-Identifier: Apache-2.0

import { ChartColumnIcon, DownloadIcon } from 'lucide-react'
import { useMemo, useState } from 'react'
import { Button } from '@/components/ui/button'
import { DataTable } from '@/components/ui/data-table'
import { EmptyState } from '@/components/ui/empty-state'
import { InlineError } from '@/components/ui/inline-error'
import { useFeedback } from '@/hooks/use-feedback'
import { formatUSD } from '@/lib/cost'
import { dataTableColumns, type DataTableColumn } from '@/lib/data-table'
import { downloadText } from '@/lib/download'
import { cacheReadShare, formatCount, formatShare, tokensOf, type UsageGroup } from '@/lib/usage'

const col = dataTableColumns<UsageGroup>()
const num = { className: 'text-right tabular-nums' }

/**
 * UsageTable shows usage groups (by org, model, provider, agent or day) with their requests, tokens, cache read share
 * and cost (issue #1281). `name` is the header of the first column.
 */
export function UsageTable({ caption, name, groups, sortByName = false }: { caption: string; name: string; groups: UsageGroup[]; sortByName?: boolean }) {
  const columns = useMemo<DataTableColumn<UsageGroup>[]>(
    () => [
      col.accessor('label', {
        id: 'name',
        header: name,
        sortFn: 'text',
        meta: { rowHeader: true },
        cell: (c) => (
          <span className="grid">
            <span className="font-medium">{c.getValue()}</span>
            {c.row.original.detail && <code className="text-muted-foreground font-mono text-xs">{c.row.original.detail}</code>}
          </span>
        ),
      }),
      col.accessor((g) => Number(g.requests), { id: 'requests', header: 'Requests', sortFn: 'basic', meta: num, cell: (c) => formatCount(c.row.original.requests) }),
      col.accessor((g) => Number(tokensOf(g)), { id: 'tokens', header: 'Tokens', sortFn: 'basic', meta: num, cell: (c) => formatCount(tokensOf(c.row.original)) }),
      col.accessor((g) => cacheReadShare(g), { id: 'cache', header: 'Cache read share', sortFn: 'basic', meta: num, cell: (c) => formatShare(c.getValue()) }),
      // Sort on the exact bigint: a Number loses the far fraction digits above about $9,007.
      col.accessor('costPico', {
        id: 'cost',
        header: 'Cost',
        sortFn: (a, b) => {
          const x = a.original.costPico
          const y = b.original.costPico
          return x === y ? 0 : x > y ? 1 : -1
        },
        meta: num,
        cell: (c) => formatUSD(c.row.original.costUsd),
      }),
    ],
    [name],
  )
  return (
    <DataTable
      caption={caption}
      columns={columns}
      data={groups}
      getRowId={(g) => g.key}
      initialSorting={[sortByName ? { id: 'name', desc: true } : { id: 'cost', desc: true }]}
      empty={<EmptyState icon={ChartColumnIcon} title="No usage in this period." />}
    />
  )
}

/**
 * ExportUsageButton saves the CSV export of the period that the page shows. `load` calls the matching Export method
 * of UsageService with the same request as the page's Get call, so the file holds the same rows and totals.
 */
export function ExportUsageButton({ load, disabled }: { load: () => Promise<{ filename: string; csv: string }>; disabled?: boolean }) {
  const fb = useFeedback()
  const [busy, setBusy] = useState(false)
  return (
    <div className="grid justify-items-end gap-2">
      <Button
        variant="outline"
        size="sm"
        disabled={disabled || busy}
        onClick={async () => {
          setBusy(true)
          const out = await fb.run(load, 'Export ready.')
          setBusy(false)
          if (out) downloadText(out.filename, out.csv)
        }}
      >
        <DownloadIcon aria-hidden />
        {busy ? 'Exporting…' : 'Export CSV'}
      </Button>
      <InlineError message={fb.error} />
    </div>
  )
}
