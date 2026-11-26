// SPDX-License-Identifier: Apache-2.0

import { TagIcon } from 'lucide-react'
import { useMemo, useState } from 'react'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { DataTable } from '@/components/ui/data-table'
import { EmptyState } from '@/components/ui/empty-state'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import type { Rates } from '@/gen/koinon/budget/v1alpha1/price_pb'
import { dataTableColumns, type DataTableColumn } from '@/lib/data-table'
import {
  matchesPriceSearch,
  rateSortValue,
  rateText,
  sourceLabels,
  sourceLegend,
  unpricedText,
  type PriceRow,
  type PriceSource,
  type RateKey,
} from '@/lib/price-table'

const col = dataTableColumns<PriceRow>()

const rateColumns: { key: RateKey; header: string }[] = [
  { key: 'input', header: 'Input' },
  { key: 'output', header: 'Output' },
  { key: 'cacheRead', header: 'Cache read' },
  { key: 'cacheWrite', header: 'Cache write' },
]

const sourceOrder: Record<PriceSource | '', number> = { org: 0, platform: 1, default: 2, '': 3 }

/** SourceBadge is the badge of one price layer (ADR 0126 section 6). */
export function SourceBadge({ source }: { source: PriceSource }) {
  return <Badge variant={source === 'org' ? 'default' : source === 'platform' ? 'secondary' : 'outline'}>{sourceLabels[source]}</Badge>
}

/** PriceLegend is the one-line legend above the table: what each badge means. */
export function PriceLegend({ sources = ['org', 'platform', 'default'] }: { sources?: PriceSource[] }) {
  return (
    <ul aria-label="Price sources" className="text-muted-foreground flex flex-wrap gap-x-4 gap-y-1 text-sm">
      {sourceLegend
        .filter((l) => sources.includes(l.source))
        .map((l) => (
          <li key={l.source} className="flex items-center gap-1.5">
            <SourceBadge source={l.source} /> <span>{l.help}</span>
          </li>
        ))}
    </ul>
  )
}

function RateCell({ row, k }: { row: PriceRow; k: RateKey }) {
  if (!row.source) return null
  const now = row.rates?.[k]
  const was = row.replaced?.[k]
  const replacedLabel = row.replacedSource ? sourceLabels[row.replacedSource] : ''
  const replacedTitle = replacedLabel ? `${replacedLabel} price: ${rateText(was)}` : undefined
  const changed = row.replaced !== undefined && (was ?? '') !== (now ?? '')
  return (
    <span className="whitespace-nowrap tabular-nums">
      {changed && (
        <>
          <del className="text-muted-foreground mr-1.5" title={replacedTitle}>
            <span className="sr-only">{replacedLabel ? `${replacedLabel} price was ` : 'was '}</span>
            {rateText(was)}
          </del>
          <span className="sr-only">, now </span>
        </>
      )}
      {rateText(now)}
    </span>
  )
}

function rateOf(r: PriceRow, k: RateKey): Rates[RateKey] | undefined {
  return r.source ? r.rates?.[k] : undefined
}

export type PriceTableProps = {
  rows: PriceRow[]
  /** The table's accessible name. */
  caption: string
  /** The search field's id, unique on the page. */
  searchId: string
  /** Adds an "Override" button to Platform and Default rows (and rows with no price). Org rows have none. */
  onOverride?: (row: PriceRow) => void
  /** What shows when there are no rows at all. */
  emptyTitle: string
}

/**
 * PriceTable is the model prices table with three source badges (ADR 0126 section 6, issue #1277): Model, Provider,
 * Source, Input, Output, Cache read and Cache write, in USD per million tokens. An org override row shows the price
 * it replaces, struck through. Search finds a row by model or provider; every column sorts. The org prices page and
 * the platform prices page use it.
 */
export function PriceTable({ rows, caption, searchId, onOverride, emptyTitle }: PriceTableProps) {
  const [q, setQ] = useState('')
  const shown = useMemo(() => rows.filter((r) => matchesPriceSearch(r, q)), [rows, q])
  const columns = useMemo<DataTableColumn<PriceRow>[]>(() => {
    const cols: DataTableColumn<PriceRow>[] = [
      col.accessor('model', {
        header: 'Model',
        sortFn: 'text',
        meta: { rowHeader: true, className: 'max-w-72' },
        cell: (c) => <code className="font-mono text-xs break-all">{c.getValue()}</code>,
      }),
      col.accessor('provider', {
        header: 'Provider',
        sortFn: 'text',
        cell: (c) => (
          <span className="grid">
            <span>{c.getValue()}</span>
            {c.row.original.providerType && c.row.original.providerType !== c.getValue() && (
              <span className="text-muted-foreground text-xs">{c.row.original.providerType}</span>
            )}
          </span>
        ),
      }),
      col.accessor((r) => sourceOrder[r.source], {
        id: 'source',
        header: 'Source',
        sortFn: 'basic',
        sortDescFirst: false,
        cell: (c) => {
          const r = c.row.original
          return r.source ? <SourceBadge source={r.source} /> : <span className="text-destructive text-sm">{unpricedText(r.unpricedReason)}</span>
        },
      }),
      ...rateColumns.map((rc) =>
        col.accessor((r) => rateSortValue(rateOf(r, rc.key)), {
          id: rc.key,
          header: rc.header,
          sortFn: 'basic',
          sortDescFirst: false,
          sortUndefined: 'last',
          meta: { className: 'text-right' },
          cell: (c) => <RateCell row={c.row.original} k={rc.key} />,
        }),
      ),
    ]
    if (onOverride) {
      cols.push(
        col.display({
          id: 'actions',
          header: () => <span className="sr-only">Actions</span>,
          cell: (c) => {
            const r = c.row.original
            if (r.source === 'org') return null
            return (
              <Button variant="outline" size="sm" aria-label={`Override the price of ${r.model} (${r.provider})`} onClick={() => onOverride(r)}>
                Override
              </Button>
            )
          },
        }),
      )
    }
    return cols
  }, [onOverride])

  return (
    <div className="grid gap-3">
      <div className="grid max-w-sm gap-1.5">
        <Label htmlFor={searchId}>Search models</Label>
        <Input id={searchId} type="search" placeholder="Model or provider" value={q} onChange={(e) => setQ(e.target.value)} autoComplete="off" />
      </div>
      <p className="text-muted-foreground text-xs">Prices are in USD per million tokens.</p>
      <DataTable
        caption={caption}
        columns={columns}
        data={shown}
        getRowId={(r) => r.id}
        initialSorting={[{ id: 'model', desc: false }]}
        empty={<EmptyState icon={TagIcon} title={q.trim() && rows.length > 0 ? 'No model matches the search.' : emptyTitle} />}
      />
    </div>
  )
}
