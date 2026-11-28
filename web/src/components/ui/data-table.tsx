// SPDX-License-Identifier: Apache-2.0
// The shadcn/ui "data table" pattern on TanStack Table v9 (issue #1270); the source is ours to edit.

import * as React from "react"
import { useTable, type RowData, type SortingState } from "@tanstack/react-table"
import { ArrowDownIcon, ArrowUpDownIcon, ArrowUpIcon } from "lucide-react"
import { Table, TableBody, TableCaption, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { dataTableFeatures, type DataTableColumn } from "@/lib/data-table"
import { cn } from "@/lib/utils"

export type DataTableProps<TData extends RowData> = {
  columns: DataTableColumn<TData>[]
  data: TData[]
  /** The table's accessible name. It is the caption (visually hidden) and names the scroll region. */
  caption: string
  /** A stable id per row (the API id), so React keeps row state when the order changes. */
  getRowId: (row: TData) => string
  initialSorting?: SortingState
  /** What shows in place of the table when `data` is empty. */
  empty?: React.ReactNode
  className?: string
}

/**
 * DataTable shows rows in a real HTML table. A sortable column has a button in its header and `aria-sort` on it.
 * Sorting is on the client; a list that pages on the server sorts in its query instead.
 */
export function DataTable<TData extends RowData>({ columns, data, caption, getRowId, initialSorting = [], empty, className }: DataTableProps<TData>) {
  const table = useTable({
    features: dataTableFeatures,
    columns,
    data,
    getRowId: (row) => getRowId(row),
    initialState: { sorting: initialSorting },
  })

  if (data.length === 0 && empty) return <>{empty}</>

  return (
    <Table regionLabel={`${caption} table`} className={className}>
      <TableCaption className="sr-only">{caption}</TableCaption>
      <TableHeader>
        {table.getHeaderGroups().map((group) => (
          <TableRow key={group.id}>
            {group.headers.map((header) => {
              const sorted = header.column.getIsSorted()
              const canSort = header.column.getCanSort()
              return (
                <TableHead
                  key={header.id}
                  scope="col"
                  className={header.column.columnDef.meta?.className}
                  aria-sort={canSort ? (sorted === "asc" ? "ascending" : sorted === "desc" ? "descending" : "none") : undefined}
                >
                  {header.isPlaceholder ? null : canSort ? (
                    <button
                      type="button"
                      onClick={header.column.getToggleSortingHandler()}
                      className="hover:text-foreground focus-visible:ring-ring/50 -mx-1 inline-flex items-center gap-1 rounded-sm px-1 outline-none focus-visible:ring-[3px]"
                    >
                      <table.FlexRender header={header} />
                      {sorted === "asc" ? (
                        <ArrowUpIcon aria-hidden className="size-3.5" />
                      ) : sorted === "desc" ? (
                        <ArrowDownIcon aria-hidden className="size-3.5" />
                      ) : (
                        <ArrowUpDownIcon aria-hidden className="size-3.5 opacity-60" />
                      )}
                    </button>
                  ) : (
                    <table.FlexRender header={header} />
                  )}
                </TableHead>
              )
            })}
          </TableRow>
        ))}
      </TableHeader>
      <TableBody>
        {table.getRowModel().rows.map((row) => (
          <TableRow key={row.id}>
            {row.getAllCells().map((cell) => {
              const meta = cell.column.columnDef.meta
              return meta?.rowHeader ? (
                <th key={cell.id} scope="row" className={cn("py-2 pr-3 font-medium", meta.className)}>
                  <table.FlexRender cell={cell} />
                </th>
              ) : (
                <TableCell key={cell.id} className={meta?.className}>
                  <table.FlexRender cell={cell} />
                </TableCell>
              )
            })}
          </TableRow>
        ))}
      </TableBody>
    </Table>
  )
}
