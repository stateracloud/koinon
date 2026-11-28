// SPDX-License-Identifier: Apache-2.0

import {
  createColumnHelper,
  createSortedRowModel,
  rowSortingFeature,
  sortFn_alphanumeric,
  sortFn_basic,
  sortFn_datetime,
  sortFn_text,
  tableFeatures,
  type ColumnDef,
  type RowData,
} from '@tanstack/react-table'

// The TanStack Table set-up of <DataTable> (components/ui/data-table.tsx), apart so that file exports only components.

/** dataTableFeatures is the one feature set of every DataTable: sorting on the client. */
export const dataTableFeatures = tableFeatures({
  rowSortingFeature,
  sortedRowModel: createSortedRowModel(),
  sortFns: { alphanumeric: sortFn_alphanumeric, text: sortFn_text, datetime: sortFn_datetime, basic: sortFn_basic },
  columnMeta: {} as {
    /** Classes for the header and the cells of the column. */
    className?: string
    /** The column names the row: its cells are `<th scope='row'>`. */
    rowHeader?: boolean
  },
})

export type DataTableFeatures = typeof dataTableFeatures
// A column list mixes value types, as in the TanStack examples.
export type DataTableColumn<TData extends RowData> = ColumnDef<DataTableFeatures, TData, any>

/** dataTableColumns returns the column helper for rows of type TData. */
export function dataTableColumns<TData extends RowData>() {
  return createColumnHelper<DataTableFeatures, TData>()
}
